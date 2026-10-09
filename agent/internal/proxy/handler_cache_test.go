package proxy

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- 单区间 Range 解析 --------------------------------------------------------

func TestParseByteRange(t *testing.T) {
	cases := []struct {
		header string
		want   byteRange
		ok     bool
	}{
		{"bytes=0-1023", byteRange{start: 0, end: 1023}, true},
		{"bytes=100-", byteRange{start: 100, end: -1}, true},
		{"bytes=0-", byteRange{start: 0, end: -1}, true},
		{"bytes=007-010", byteRange{start: 7, end: 10}, true},
		{"bytes= 5 - 10 ", byteRange{start: 5, end: 10}, true},
		{"bytes=10-10", byteRange{start: 10, end: 10}, true},
		// 以下形态一律不走缓存分支（由上游决定语义）。
		{"", byteRange{}, false},
		{"bytes=-1023", byteRange{}, false},            // 后缀区间
		{"bytes=-", byteRange{}, false},                // 空区间
		{"bytes=0-1023,2048-3071", byteRange{}, false}, // 多区间
		{"items=0-1023", byteRange{}, false},           // 非 bytes 单位
		{"bytes=abc-def", byteRange{}, false},
		{"bytes=200-100", byteRange{}, false}, // end < start
		{"bytes=0--5", byteRange{}, false},
		{"bytes=1-2-3", byteRange{}, false},
	}
	for _, tc := range cases {
		got, ok := parseByteRange(tc.header)
		if ok != tc.ok {
			t.Errorf("%q：ok 应为 %v，实际 %v", tc.header, tc.ok, ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("%q：解析为 %+v，期望 %+v", tc.header, got, tc.want)
		}
	}
}

// --- 头号验收：字节一致性 ------------------------------------------------------

// compareCachedAgainstPassthrough 把同一个 Range 分别在"缓存关（v0.2 路径）"与
// "缓存开"两条数据面上跑一遍，断言状态码、白名单响应头与响应体 sha256 完全一致。
//
// localOnly = true 时还断言缓存开的这一次完全没有触上游（局部命中/全命中的证据）。
// 返回"缓存开"这一次的上游请求数，供调用方断言混合/回退的实际请求次数。
func compareCachedAgainstPassthrough(t *testing.T, cold, warm *cacheStack, g *googleFile, label, rng string, localOnly bool) int64 {
	t.Helper()
	coldResp := mustGet(t, cold.url("file-1", time.Now().Add(time.Hour)), rangeHeader(rng))
	coldBody := readBody(t, coldResp)

	before := g.hits.Load()
	warmResp := mustGet(t, warm.url("file-1", time.Now().Add(time.Hour)), rangeHeader(rng))
	warmBody := readBody(t, warmResp)
	after := g.hits.Load()

	if coldResp.StatusCode != warmResp.StatusCode {
		t.Fatalf("[%s] 状态码不一致：缓存关 %d，缓存开 %d", label, coldResp.StatusCode, warmResp.StatusCode)
	}
	for _, name := range passthroughHeaderNames {
		if got, want := warmResp.Header.Get(name), coldResp.Header.Get(name); got != want {
			t.Fatalf("[%s] 响应头 %s 不一致：缓存关 %q，缓存开 %q", label, name, want, got)
		}
	}
	if got, want := sha256Hex(warmBody), sha256Hex(coldBody); got != want {
		t.Fatalf("[%s] 响应体 sha256 不一致：缓存关 %s（%d 字节），缓存开 %s（%d 字节）",
			label, want, len(coldBody), got, len(warmBody))
	}
	if localOnly && after != before {
		t.Fatalf("[%s] 该用例应由本地块服务，实际触了上游：%d → %d", label, before, after)
	}
	if !localOnly && after == before {
		t.Fatalf("[%s] 该用例应走上游透传，实际没有上游请求", label)
	}
	// 本地拼装的响应头必须是白名单子集：绝不外泄上游凭据/其它头。
	for _, name := range []string{"Authorization", "Set-Cookie", "X-Goog-Hash"} {
		if got := warmResp.Header.Get(name); got != "" {
			t.Fatalf("[%s] 缓存响应不应带 %s：%q", label, name, got)
		}
	}
	return after - before
}

func TestCacheServingIsByteIdenticalToPassthrough(t *testing.T) {
	// 3 整块 + 12345 字节尾块；预热后整文件都进缓存。
	const tail = 12345
	content := randomContent(3*blockSize+tail, 42)
	size := int64(len(content))
	g := newGoogleFile(t, content)

	cold := newCacheStack(t, g, 0, 0, 0)               // 缓存关 = v0.2 路径
	warm := newCacheStack(t, g, 64<<20, 16<<20, 4<<20) // 头 16MiB 覆盖整文件

	// 预热：首触点触发预取，等到整文件就位。
	compareCachedAgainstPassthrough(t, cold, warm, g, "预热请求（未命中透传）", "bytes=0-999", false)
	waitBlockLens(t, warm.cache, "file-1", map[int64]int{
		0: blockSize, 1: blockSize, 2: blockSize, 3: tail,
	})
	waitPrefetchIdle(t, warm.prefetch)
	compareCachedAgainstPassthrough(t, cold, warm, g, "预热后复查（全命中）", "bytes=0-999", true)

	last := size - 1
	cases := []struct {
		name      string
		rng       string
		localOnly bool
	}{
		{"块内小段", "bytes=0-1023", true},
		{"单字节", "bytes=0-0", true},
		{"跨块边界（块0→块1）", "bytes=4194200-4194400", true},
		{"正好骑在块边界上", "bytes=4194303-4194304", true},
		{"跨块边界（块1→块2）", "bytes=8388607-8388608", true},
		{"尾不足一块的全部", fmt.Sprintf("bytes=%d-%d", size-tail, last), true},
		{"尾不足一块的最后一个字节", fmt.Sprintf("bytes=%d-", last), true},
		{"跨块与短块的长区间", "bytes=5000000-12590000", true},
		{"整个文件", fmt.Sprintf("bytes=0-%d", last), true},
		{"开区间到 EOF", "bytes=1000000-", true},
		{"开区间跨块", "bytes=4194304-", true},
		{"端点超 size（上游 clamp）", "bytes=0-99999999", false},
		{"起点越界（上游 416）", fmt.Sprintf("bytes=%d-", size), false},
		{"起点越界且定长（上游 416）", fmt.Sprintf("bytes=%d-9999999", size), false},
		{"远越界（上游 416）", "bytes=99999999-", false},
	}
	for _, tc := range cases {
		compareCachedAgainstPassthrough(t, cold, warm, g, tc.name, tc.rng, tc.localOnly)
	}
}

// --- 混合服务（前缀本地 + 上游续传） ------------------------------------------

func TestCacheMixedServingIsByteIdentical(t *testing.T) {
	const tail = 12345
	content := randomContent(3*blockSize+tail, 43)
	size := int64(len(content))
	g := newGoogleFile(t, content)

	cold := newCacheStack(t, g, 0, 0, 0)
	warm := newCacheStack(t, g, 64<<20, 4<<20, 0) // 头只有 1 块：更长的请求必然走混合

	// 预热：只填块 0。
	doGet(t, warm, "bytes=0-999")
	waitBlockLens(t, warm.cache, "file-1", map[int64]int{0: blockSize})
	waitPrefetchIdle(t, warm.prefetch)

	last := size - 1
	// 这些请求的前缀都在块 0 里，余下必须来自上游：断言恰好 1 次上游请求
	// （既不能把整段推给上游，也不能重复请求）。
	cases := []struct {
		name string
		rng  string
	}{
		{"整文件开区间", "bytes=0-"},
		{"块内偏移起头", "bytes=100-"},
		{"起点贴近块尾", fmt.Sprintf("bytes=%d-%d", blockSize-4, last)},
		{"中部起头", "bytes=2000000-"},
		{"整文件闭区间", fmt.Sprintf("bytes=0-%d", last)},
	}
	for _, tc := range cases {
		if got := compareCachedAgainstPassthrough(t, cold, warm, g, tc.name, tc.rng, false); got != 1 {
			t.Fatalf("[%s] 混合服务应恰好 1 次上游请求（余段），实际 %d", tc.name, got)
		}
	}

	// 缓存只由首触预取写入：路过的字节一个都不许进缓存。
	assertBlockLens(t, warm.cache, "file-1", map[int64]int{0: blockSize})
}

// 混合服务必须"先打开上游、拿到响应头，再写第一个字节"：上游失败时完整回退透传，
// 客户端拿到的仍是完整、正确的 206。
func TestCacheMixedFallsBackWhenUpstreamFails(t *testing.T) {
	content := randomContent(3*blockSize+777, 44)
	g := newGoogleFile(t, content)
	// 只让"从块边界开始的余段请求"失败：混合服务必须先看到它失败再决定回退，
	// 因此客户端请求本身（bytes=0-）仍能被完整透传。
	g.setHook(func(w http.ResponseWriter, r *http.Request, g *googleFile) bool {
		if strings.HasPrefix(r.Header.Get("Range"), fmt.Sprintf("bytes=%d-", blockSize)) {
			http.Error(w, "boom", http.StatusInternalServerError)
			return true
		}
		return false
	})

	cold := newCacheStack(t, g, 0, 0, 0)
	warm := newCacheStack(t, g, 64<<20, 4<<20, 0)
	doGet(t, warm, "bytes=0-999")
	waitBlockLens(t, warm.cache, "file-1", map[int64]int{0: blockSize})
	waitPrefetchIdle(t, warm.prefetch)

	// 1 次失败的余段请求 + 1 次完整透传；客户端拿到的仍是完整、正确的 206。
	if got := compareCachedAgainstPassthrough(t, cold, warm, g, "上游失败回退", "bytes=0-", false); got != 2 {
		t.Fatalf("应为 1 次失败的上游余段 + 1 次完整透传，实际 %d", got)
	}
}

// 上游余段的 ETag 与本地前缀不符 = 文件已换版本：必须整体回退，
// 绝不能把两个版本的字节拼在一起。
func TestCacheMixedFallsBackWhenIdentityMismatch(t *testing.T) {
	size := 3*blockSize + 999
	v1 := randomContent(size, 45)
	v2 := randomContent(size, 46)
	g := newGoogleFile(t, v1)
	g.setETag(`"v1"`) // 表现 ETag 身份路径：真实 Google 主形态没有 ETag

	cold := newCacheStack(t, g, 0, 0, 0)
	warm := newCacheStack(t, g, 64<<20, 4<<20, 0)
	doGet(t, warm, "bytes=0-999")
	waitBlockLens(t, warm.cache, "file-1", map[int64]int{0: blockSize})
	waitPrefetchIdle(t, warm.prefetch)

	g.set(v2, `"v2"`) // 上游换版本
	if got := compareCachedAgainstPassthrough(t, cold, warm, g, "身份不符整体回退", "bytes=0-", false); got != 2 {
		t.Fatalf("应为 1 次身份不符的上游余段 + 1 次完整透传，实际 %d", got)
	}

	// 透传观察到新 ETag → v1 的块已被清掉（此刻还没有新请求，状态是确定的）。
	assertBlockLens(t, warm.cache, "file-1", nil)

	// 后续请求必须拿到 v2 的字节（前缀若混了 v1 就会在这里现形）；
	// 同时首触预取会按新版本重建缓存。
	warmResp := mustGet(t, warm.url("file-1", time.Now().Add(time.Hour)), rangeHeader("bytes=0-"))
	warmBody := readBody(t, warmResp)
	if sha256Hex(warmBody) != sha256Hex(v2) {
		t.Fatal("身份不符时必须整段回退，不得混版本")
	}
	waitPrefetchIdle(t, warm.prefetch)
	if meta, ok := warm.cache.Meta("file-1"); !ok || meta.etag != `"v2"` {
		t.Fatalf("重新预取后元数据应更新为 v2：%+v", meta)
	}
	assertBlockLens(t, warm.cache, "file-1", map[int64]int{0: blockSize})
}

// 上游忽略 Range 直接回 200 整文件时，余段的响应体不是请求的那一段：
// 混合服务必须整体回退透传，绝不能把"本地前缀 + 错位数据"拼出去（那样客户端
// 拿到的字节是静默损坏的）。预取路径对同一情况也是放弃（fetchBlock 的区间校验）。
func TestCacheMixedFallsBackWhenUpstreamIgnoresRange(t *testing.T) {
	content := randomContent(3*blockSize+888, 47)
	g := newGoogleFile(t, content)
	// 只让"从块边界起的余段请求"被忽略：上游回 200 + 整文件；其余请求正常。
	g.setHook(func(w http.ResponseWriter, r *http.Request, g *googleFile) bool {
		if strings.HasPrefix(r.Header.Get("Range"), fmt.Sprintf("bytes=%d-", blockSize)) {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Type", "video/x-matroska")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(content)
			return true
		}
		return false
	})

	cold := newCacheStack(t, g, 0, 0, 0)
	warm := newCacheStack(t, g, 64<<20, 4<<20, 0)
	doGet(t, warm, "bytes=0-999")
	waitBlockLens(t, warm.cache, "file-1", map[int64]int{0: blockSize})
	waitPrefetchIdle(t, warm.prefetch)

	// 1 次"忽略 Range 的余段响应" + 1 次完整透传；客户端拿到的仍是完整、正确的 206。
	if got := compareCachedAgainstPassthrough(t, cold, warm, g, "上游忽略 Range 回退", "bytes=0-", false); got != 2 {
		t.Fatalf("应为 1 次错位余段 + 1 次完整透传，实际 %d", got)
	}
}

// --- ETag / Last-Modified 失效 ------------------------------------------------

func TestCacheETagChangeInvalidatesBlocks(t *testing.T) {
	size := 3 << 20 // 单块覆盖整文件
	v1 := randomContent(size, 51)
	v2 := randomContent(size, 52)
	g := newGoogleFile(t, v1)
	g.setETag(`"v1"`) // 表现 ETag 身份路径：真实 Google 主形态没有 ETag
	st := newCacheStack(t, g, 64<<20, 4<<20, 0)

	doGet(t, st, "bytes=0-999")
	waitBlockLens(t, st.cache, "file-1", map[int64]int{0: size})
	waitPrefetchIdle(t, st.prefetch)

	// 上游换版本；一次必须走上游的请求（端点超 size → 本地不服务）会观察到新 ETag。
	g.set(v2, `"v2"`)
	resp, body := doGet(t, st, "bytes=0-99999999")
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("clamp 请求应回 206，实际 %d", resp.StatusCode)
	}
	if sha256Hex(body) != sha256Hex(v2) {
		t.Fatal("透传应返回新版本字节")
	}
	assertBlockLens(t, st.cache, "file-1", nil) // v1 的块已被 SetEtag 清掉

	// 后续请求必须拿到新字节（不许再命中 v1 的块）。
	resp, body = doGet(t, st, "bytes=0-1023")
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("应为 206，实际 %d", resp.StatusCode)
	}
	if !bytes.Equal(body, v2[:1024]) {
		t.Fatal("ETag 变化后不得再返回旧字节")
	}
}

// 没有 ETag 时内容身份退化为 Last-Modified：版本变化同样必须被透传观测到。
// 只观察 ETag（空值 = 无操作）会让旧身份的块被本地服务一直用下去——包括混合
// 服务的"身份不符回退"之后（回退后的透传观测同样是空操作），陈旧字节永不消失。
func TestCacheLastModifiedIdentityChangeInvalidatesBlocks(t *testing.T) {
	size := 3 << 20 // 单块覆盖整文件
	v1 := randomContent(size, 53)
	v2 := randomContent(size, 54)
	g := newGoogleFile(t, v1)
	g.setModTime(time.Unix(1700000000, 0)) // 表现 Last-Modified 身份路径（无 ETag）
	st := newCacheStack(t, g, 64<<20, 4<<20, 0)

	doGet(t, st, "bytes=0-999")
	waitBlockLens(t, st.cache, "file-1", map[int64]int{0: size})
	waitPrefetchIdle(t, st.prefetch)
	if meta, ok := st.cache.Meta("file-1"); !ok || meta.identity() == "" {
		t.Fatalf("无 ETag 时应记录 Last-Modified 身份：%+v（ok=%v）", meta, ok)
	}

	// 上游换版本（新内容 + 新 Last-Modified，仍无 ETag）；一次必须走上游的请求
	// （端点超 size → 本地不服务）会观察到新的 Last-Modified。
	g.set(v2, "")
	g.setModTime(time.Unix(1700009999, 0))
	resp, body := doGet(t, st, "bytes=0-99999999")
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("clamp 请求应回 206，实际 %d", resp.StatusCode)
	}
	if sha256Hex(body) != sha256Hex(v2) {
		t.Fatal("透传应返回新版本字节")
	}
	assertBlockLens(t, st.cache, "file-1", nil) // 旧身份的块已被清除

	// 后续请求必须拿到新字节（不许再命中旧身份的块）。
	resp, body = doGet(t, st, "bytes=0-1023")
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("应为 206，实际 %d", resp.StatusCode)
	}
	if !bytes.Equal(body, v2[:1024]) {
		t.Fatal("Last-Modified 变化后不得再返回旧字节")
	}
	// 身份更新后首触预取会按新版本重建缓存。
	waitPrefetchIdle(t, st.prefetch)
	wantLM := time.Unix(1700009999, 0).UTC().Format(http.TimeFormat)
	if meta, ok := st.cache.Meta("file-1"); !ok || meta.lastModified != wantLM {
		t.Fatalf("重新预取后身份应更新为 %q：%+v（ok=%v）", wantLM, meta, ok)
	}
}

// 真实 Google 主形态：没有 ETag/Last-Modified，身份 = 总字节数。同 id 换文件
// 只要大小不同就必须失效：透传 Observe 到新大小后，旧块立即不可用。
func TestCacheTotalSizeIdentityChangeInvalidatesBlocks(t *testing.T) {
	v1 := randomContent(3<<20, 55)
	v2 := randomContent(5<<20, 56)
	g := newGoogleFile(t, v1)
	st := newCacheStack(t, g, 64<<20, 4<<20, 0)

	doGet(t, st, "bytes=0-999")
	waitBlockLens(t, st.cache, "file-1", map[int64]int{0: len(v1)})
	waitPrefetchIdle(t, st.prefetch)
	if meta, ok := st.cache.Meta("file-1"); !ok || meta.identity() != fmt.Sprintf("size:%d", len(v1)) {
		t.Fatalf("身份应记录为总字节数：%+v（ok=%v）", meta, ok)
	}

	// 上游换成"同 id、不同大小"的文件；一次必须走上游的请求（端点超 size →
	// 本地不服务）会观察到新的总大小。
	g.set(v2, "")
	resp, body := doGet(t, st, "bytes=0-99999999")
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("clamp 请求应回 206，实际 %d", resp.StatusCode)
	}
	if sha256Hex(body) != sha256Hex(v2) {
		t.Fatal("透传应返回新文件字节")
	}
	assertBlockLens(t, st.cache, "file-1", nil) // 旧大小的块已被清除

	// 后续请求必须拿新字节（不许再命中旧文件大小的块）。
	resp, body = doGet(t, st, "bytes=0-1023")
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, v2[:1024]) {
		t.Fatalf("大小变化后不得再返回旧字节：status=%d len=%d", resp.StatusCode, len(body))
	}
}

// CACHE_MAX_AGE_MINUTES：块龄超过最大年龄后一律不服务——回到透传（与缓存关
// 逐字节一致）；过期块被回收后该文件重新触发首触预取，随后又是纯本地命中。
func TestCacheExpiredBlocksAreNotServed(t *testing.T) {
	const tail = 4096
	content := randomContent(2*blockSize+tail, 57)
	g := newGoogleFile(t, content)

	cold := newCacheStack(t, g, 0, 0, 0)
	warm := newCacheStack(t, g, 64<<20, 16<<20, 4<<20)
	base := time.Now()
	warm.cache.setAgeForTest(24*time.Hour, func() time.Time { return base })

	doGet(t, warm, "bytes=0-999")
	waitBlockLens(t, warm.cache, "file-1", map[int64]int{0: blockSize, 1: blockSize, 2: tail})
	waitPrefetchIdle(t, warm.prefetch)

	// 注入时钟推进 25h（> 24h 上限）：整文件都过期。
	warm.cache.setAgeForTest(24*time.Hour, func() time.Time { return base.Add(25 * time.Hour) })

	// 过期后必须走上游透传：响应与"缓存关"逐字节一致。
	compareCachedAgainstPassthrough(t, cold, warm, g, "TTL 过期不服务", "bytes=0-1023", false)

	// 过期块被回收后预取重新拉起；重新就位后又是纯本地命中。
	waitPrefetchIdle(t, warm.prefetch)
	waitBlockLens(t, warm.cache, "file-1", map[int64]int{0: blockSize, 1: blockSize, 2: tail})
	before := g.hits.Load()
	resp, body := doGet(t, warm, "bytes=0-1023")
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, content[:1024]) {
		t.Fatalf("重新预取后应本地命中：status=%d", resp.StatusCode)
	}
	if got := g.hits.Load(); got != before {
		t.Fatalf("重新预取后不应再触上游：%d → %d", before, got)
	}
}

// --- 其它形态不受缓存影响 ------------------------------------------------------

func TestHeadAndNoRangeBypassCache(t *testing.T) {
	const tail = 4096
	content := randomContent(2*blockSize+tail, 61)
	size := len(content)
	g := newGoogleFile(t, content)

	cold := newCacheStack(t, g, 0, 0, 0)
	warm := newCacheStack(t, g, 64<<20, 16<<20, 4<<20)

	doGet(t, warm, "bytes=0-999")
	waitBlockLens(t, warm.cache, "file-1", map[int64]int{0: blockSize, 1: blockSize, 2: tail})
	waitPrefetchIdle(t, warm.prefetch)

	// HEAD 直通上游：拿到 Content-Length 与空响应体。
	before := g.hits.Load()
	headResp := mustHead(t, warm.url("file-1", time.Now().Add(time.Hour)))
	headBody := readBody(t, headResp)
	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD 应为 200，实际 %d", headResp.StatusCode)
	}
	if got := headResp.Header.Get("Content-Length"); got != strconv.Itoa(size) {
		t.Fatalf("HEAD 应透传 Content-Length=%d，实际 %q", size, got)
	}
	if len(headBody) != 0 {
		t.Fatalf("HEAD 不应有响应体，实际 %d 字节", len(headBody))
	}
	if g.hits.Load() != before+1 {
		t.Fatal("HEAD 必须直通上游")
	}

	// 无 Range 的 GET：整文件透传（不缓存、不倒腾本地块），与缓存关逐字节一致。
	coldResp := mustGet(t, cold.url("file-1", time.Now().Add(time.Hour)), nil)
	coldBody := readBody(t, coldResp)
	before = g.hits.Load()
	warmResp := mustGet(t, warm.url("file-1", time.Now().Add(time.Hour)), nil)
	warmBody := readBody(t, warmResp)
	if g.hits.Load() != before+1 {
		t.Fatal("无 Range 的 GET 必须直通上游")
	}
	if coldResp.StatusCode != warmResp.StatusCode || sha256Hex(coldBody) != sha256Hex(warmBody) {
		t.Fatal("无 Range 的 GET 在缓存开/关下必须逐字节一致")
	}
	if resp := warmResp; resp.StatusCode != http.StatusOK {
		t.Fatalf("无 Range 的 GET 应为 200，实际 %d", resp.StatusCode)
	}
	// 真实 Google 的完整下载带 x-goog-hash（range 不带）：它不在白名单里，必须被过滤。
	if got := warmResp.Header.Get("X-Goog-Hash"); got != "" {
		t.Fatalf("非白名单头 X-Goog-Hash 不得外泄：%q", got)
	}
}

// 缓存关闭时预取不触发、不落块，行为与不带缓存的版本一致。
func TestCacheOffLeavesNoBlocks(t *testing.T) {
	content := randomContent(3<<20, 71)
	g := newGoogleFile(t, content)
	st := newCacheStack(t, g, 0, 8<<20, 4<<20)

	resp, body := doGet(t, st, "bytes=0-1023")
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, content[:1024]) {
		t.Fatalf("缓存关时请求应原样透传：status=%d len=%d", resp.StatusCode, len(body))
	}
	time.Sleep(50 * time.Millisecond) // 给（不该发生的）预取留点机会
	assertBlockLens(t, st.cache, "file-1", nil)
	if got := g.hits.Load(); got != 1 {
		t.Fatalf("缓存关闭时不应有任何预取请求，实际 %d", got)
	}
	if _, ok := st.cache.Meta("file-1"); ok {
		t.Fatal("缓存关闭时不应记录元数据")
	}
}

// Accept-Ranges 的本地拼装只镜像上游观测到的值。真实 Google 主形态不发该头
// （"本地也不许凭空合成"由默认形态的字节一致性矩阵钉死）；这条单独钉**镜像**：
// 上游发了就必须照抄，否则同一请求在缓存开/关下响应头会不一致。
func TestCacheMirrorsUpstreamAcceptRanges(t *testing.T) {
	content := randomContent(3<<20, 59) // 单块覆盖整文件
	g := newGoogleFile(t, content)
	g.setHook(func(w http.ResponseWriter, r *http.Request, g *googleFile) bool {
		w.Header().Set("Accept-Ranges", "bytes")
		serveGoogleShape(w, r, content, "", time.Time{})
		return true
	})
	st := newCacheStack(t, g, 64<<20, 4<<20, 0)

	doGet(t, st, "bytes=0-999") // 透传观测到 Accept-Ranges: bytes
	waitBlockLens(t, st.cache, "file-1", map[int64]int{0: len(content)})
	waitPrefetchIdle(t, st.prefetch)

	before := g.hits.Load()
	resp, body := doGet(t, st, "bytes=0-1023")
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, content[:1024]) {
		t.Fatalf("本地命中应回 206 + 原始字节：status=%d len=%d", resp.StatusCode, len(body))
	}
	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("本地拼装必须镜像上游 Accept-Ranges=bytes，实际 %q", got)
	}
	if got := g.hits.Load(); got != before {
		t.Fatalf("全命中不应触上游：%d → %d", before, got)
	}
}

// --- 小工具 -------------------------------------------------------------------

// doGet 走一次带 Range 的客户端请求（拿签名 URL）。
func doGet(t *testing.T, st *cacheStack, spec string) (*http.Response, []byte) {
	t.Helper()
	resp := mustGet(t, st.url("file-1", time.Now().Add(time.Hour)), rangeHeader(spec))
	return resp, readBody(t, resp)
}

func mustHead(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	return resp
}
