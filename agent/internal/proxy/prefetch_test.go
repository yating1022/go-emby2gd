package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- 缓存相关测试的公共脚手架 -------------------------------------------------
//
// 上层（prefetch / handler 缓存分支）的测试都用"真 HTTP 上游 + 假 master + 真 Handler"
// 的全链路：只有这样才能同时验证"响应字节"与"上游被打了几次"。

// googleFile 是假的 Google 直链端点：支持 Range/HEAD，内容可中途更换（模拟上游
// 换版本），可注入故障。
//
// **主形态复刻真实 Google 直链的响应头（2026-10-09 实测）**：206 只带
// Content-Range + Content-Length + Content-Type，**没有 ETag、没有 Last-Modified**；
// 200（完整下载）带 Content-Length，另带 x-goog-hash（range 不带）。这正是
// "内容身份只能退化到总字节数"的形态——字节一致性矩阵必须先在它上面跑通，
// 否则验出来的缓存收益是虚的（此前 mock 恰好带了 Last-Modified，掩盖了缺口）。
//
// etag 非空 / modTime 非零时才发送对应头：ETag / Last-Modified 路径的补充用例
// 用 setETag / setModTime 显式打开，主形态默认不含它们。
type googleFile struct {
	srv *httptest.Server

	mu      sync.Mutex
	content []byte
	etag    string    // 非空才发送 ETag
	modTime time.Time // 非零才发送 Last-Modified
	// hook 返回 true 表示已自行写响应（用于注入故障/畸形响应）。
	hook func(w http.ResponseWriter, r *http.Request, g *googleFile) bool

	// hits / rangedHits 是上游被请求的次数（后者只数带 Range 的）。
	hits       atomic.Int64
	rangedHits atomic.Int64
}

func newGoogleFile(t *testing.T, content []byte) *googleFile {
	t.Helper()
	g := &googleFile{content: content}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.hits.Add(1)
		if r.Header.Get("Range") != "" {
			g.rangedHits.Add(1)
		}
		g.mu.Lock()
		hook, content, etag, modTime := g.hook, g.content, g.etag, g.modTime
		g.mu.Unlock()
		if hook != nil && hook(w, r, g) {
			return
		}
		serveGoogleShape(w, r, content, etag, modTime)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// serveGoogleShape 按真实 Google 直链的头部形态写响应（见 googleFile 的注释）。
func serveGoogleShape(w http.ResponseWriter, r *http.Request, content []byte, etag string, modTime time.Time) {
	size := int64(len(content))
	hdr := w.Header()
	hdr.Set("Content-Type", "video/x-matroska")
	if etag != "" {
		hdr.Set("ETag", etag)
	}
	if !modTime.IsZero() {
		hdr.Set("Last-Modified", modTime.UTC().Format(http.TimeFormat))
	}

	start, end, ranged, unsatisfiable := fakeRange(r.Header.Get("Range"), size)
	switch {
	case unsatisfiable:
		// 起点越界：416 + `bytes */size`（与 Go ServeContent 同语义）。
		hdr.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	case !ranged:
		// 无 Range（或解析不出的 Range，RFC 允许忽略）：200 整文件 + x-goog-hash。
		hdr.Set("Content-Length", strconv.FormatInt(size, 10))
		hdr.Set("x-goog-hash", "crc32c=dGVzdA==,md5=dGVzdA==")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(content)
		}
	default:
		body := content[start : end+1]
		hdr.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		hdr.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	}
}

// fakeRange 解析单区间 `bytes=<start>-<end?>` 并按 size 钳制：
//   - 无 Range / 解析不出（非 bytes= 前缀、多区间、非法值）→ ranged=false（回 200）；
//   - 起点越界 → unsatisfiable=true（回 416）；
//   - 其余 → ranged=true，end 钳到 size-1（端点在 size 之外即"上游 clamp"）。
func fakeRange(spec string, size int64) (start, end int64, ranged, unsatisfiable bool) {
	s, found := strings.CutPrefix(strings.TrimSpace(spec), "bytes=")
	if !found || strings.Contains(s, ",") {
		return 0, 0, false, false
	}
	startStr, endStr, found := strings.Cut(strings.TrimSpace(s), "-")
	if !found {
		return 0, 0, false, false
	}
	start, err := strconv.ParseInt(strings.TrimSpace(startStr), 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false, false
	}
	if start >= size {
		return 0, 0, true, true
	}
	end = size - 1
	if tail := strings.TrimSpace(endStr); tail != "" {
		parsed, perr := strconv.ParseInt(tail, 10, 64)
		if perr != nil || parsed < start {
			return 0, 0, false, false
		}
		if parsed < end {
			end = parsed
		}
	}
	return start, end, true, false
}

func (g *googleFile) linkURL() string { return g.srv.URL + "/gdrive/file-1" }

// set 换内容与 ETag（同一文件的"新版本"）；etag 非空时该形态开始发送 ETag。
func (g *googleFile) set(content []byte, etag string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.content, g.etag = content, etag
}

// setETag 让假上游开始发送 ETag（ETag 身份路径的补充用例用）。
func (g *googleFile) setETag(etag string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.etag = etag
}

// setModTime 换 Last-Modified（模拟"无 ETag 的文件换了版本"）；非零值才开始发送它。
func (g *googleFile) setModTime(modTime time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.modTime = modTime
}

func (g *googleFile) setHook(hook func(w http.ResponseWriter, r *http.Request, g *googleFile) bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hook = hook
}

// cacheStack 是"开启读前缓存"的一整条数据面（复用 handler_test.go 的 stack）。
type cacheStack struct {
	*stack
	google   *googleFile
	cache    *BlockCache
	prefetch *Prefetcher
}

// newCacheStack 组装数据面；cacheBytes == 0 即"缓存关"（v0.2 行为，供逐字节对照）。
func newCacheStack(t *testing.T, g *googleFile, cacheBytes, headBytes, tailBytes int64) *cacheStack {
	t.Helper()
	return newCacheStackWithLogger(t, g, cacheBytes, headBytes, tailBytes, discardLogger())
}

// newCacheStackWithLogger 同 newCacheStack，但把数据面（Handler 与预取器）的日志
// 接到指定 logger——断言 WARN 文案的用例用（混合态"断开连接"语义看日志最直接）。
func newCacheStackWithLogger(t *testing.T, g *googleFile, cacheBytes, headBytes, tailBytes int64, logger *slog.Logger) *cacheStack {
	t.Helper()
	signKey := testSignKey(t)
	master := newFakeMaster(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		_, _ = w.Write([]byte(linkJSON(g.linkURL(), time.Hour)))
	})
	source := newTestLinkSource(t, master.srv.URL, nil)
	cache := NewBlockCache(cacheBytes)
	prefetch := NewPrefetcher(PrefetcherConfig{
		Cache:     cache,
		Links:     source,
		Logger:    logger,
		HeadBytes: headBytes,
		TailBytes: tailBytes,
	})
	handler := NewHandler(Config{
		SignKey:       signKey,
		MaxConcurrent: 8,
		Links:         source,
		Logger:        logger,
		Cache:         cache,
		Prefetch:      prefetch,
	})
	agent := httptest.NewServer(handler)
	t.Cleanup(agent.Close)
	return &cacheStack{
		stack:    &stack{master: master, source: source, handler: handler, agentURL: agent.URL, signKey: signKey},
		google:   g,
		cache:    cache,
		prefetch: prefetch,
	}
}

// newInfoLogger 把 INFO 及以上日志接到共享缓冲（断言预取时间线/文案用）。
func newInfoLogger(buf *lockedLogBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// rangeHeader 组装"只带 Range 的请求头"。
func rangeHeader(spec string) http.Header {
	if spec == "" {
		return nil
	}
	return http.Header{"Range": []string{spec}}
}

// blockLens 返回某文件已缓存块号 → 字节数（测试观测口）。
func blockLens(c *BlockCache, fileID string) map[int64]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[int64]int)
	for key, el := range c.index {
		if key.fileID == fileID {
			out[key.idx] = len(el.Value.(*blockEntry).data)
		}
	}
	return out
}

// assertBlockLens 断言某文件的缓存块集合完全等于 want（块号 → 字节数）。
func assertBlockLens(t *testing.T, c *BlockCache, fileID string, want map[int64]int) {
	t.Helper()
	got := blockLens(c, fileID)
	if len(got) != len(want) {
		t.Fatalf("缓存块数量不符：期望 %v，实际 %v", want, got)
	}
	for idx, n := range want {
		if got[idx] != n {
			t.Fatalf("块 %d 长度不符：期望 %d，实际 %d（全部：%v）", idx, n, got[idx], got)
		}
	}
}

// waitBlockLens 轮询等待缓存块集合达到 want（预取是异步的）。
func waitBlockLens(t *testing.T, c *BlockCache, fileID string, want map[int64]int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		got := blockLens(c, fileID)
		if len(got) == len(want) {
			ok := true
			for idx, n := range want {
				if got[idx] != n {
					ok = false
					break
				}
			}
			if ok {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待缓存块超时：期望 %v，实际 %v", want, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitPrefetchIdle 等待所有在途预取结束（触发是异步的，断言前必须先落地）。
func waitPrefetchIdle(t *testing.T, p *Prefetcher) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		p.mu.Lock()
		n := len(p.inflight)
		p.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("预取未在超时内结束")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func prefetchInflight(p *Prefetcher) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.inflight)
}

// sha256Hex 是字节一致性的比较口径（头号验收）。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// randomContent 生成确定性的伪随机内容：视频数据不可压缩、块边界无规律，
// 用零填充测不出"拼接错位"这类缺陷。
func randomContent(n int, seed int64) []byte {
	buf := make([]byte, n)
	rnd := rand.New(rand.NewSource(seed))
	for i := range buf {
		buf[i] = byte(rnd.Intn(256))
	}
	return buf
}

// --- 首触预取 ---------------------------------------------------------------

func TestPrefetcherFillsHeadAndTailOnFirstTouch(t *testing.T) {
	const chunk = int64(blockSize)
	content := randomContent(8*int(chunk)+1000, 7)
	size := int64(len(content))
	g := newGoogleFile(t, content)
	st := newCacheStack(t, g, 64<<20, 8<<20, 4<<20) // 头 8MiB（2 块）+ 尾 4MiB

	// 首触：客户端第一条 Range（起播探头的形状），必须原样透传。
	resp := mustGet(t, st.url("file-1", time.Now().Add(time.Hour)), rangeHeader("bytes=0-65535"))
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("首触请求应为 206，实际 %d", resp.StatusCode)
	}
	if !bytes.Equal(body, content[:65536]) {
		t.Fatal("首触请求的响应字节不符")
	}

	// 预取落地：头 2 块 + 尾部[7]=整块、[8]=文件最后一块（1000 字节）。
	waitBlockLens(t, st.cache, "file-1", map[int64]int{0: blockSize, 1: blockSize, 7: blockSize, 8: 1000})
	waitPrefetchIdle(t, st.prefetch)
	if got := st.master.requests.Load(); got != 1 {
		t.Fatalf("预取应复用直链缓存，master 只该被请求 1 次，实际 %d", got)
	}
	// v0.3.2 单条大流：头段 1 条（0..2 块一发到位）+ 尾段 1 条 = 预取 2 次上游。
	if got := g.hits.Load(); got != 3 {
		t.Fatalf("上游请求数应为 1（客户端）+ 2（预取：一条头流 + 一条尾流），实际 %d", got)
	}

	// 尾探：完全落在本地块里 —— 上游零新增请求（A2 的核心断言）。
	before := g.hits.Load()
	start := size - 100
	resp = mustGet(t, st.url("file-1", time.Now().Add(time.Hour)),
		rangeHeader(fmt.Sprintf("bytes=%d-%d", start, size-1)))
	body = readBody(t, resp)
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("尾探应为 206，实际 %d", resp.StatusCode)
	}
	if !bytes.Equal(body, content[start:]) {
		t.Fatal("尾探的响应字节不符")
	}
	if got := g.hits.Load(); got != before {
		t.Fatalf("全命中不应触上游：%d → %d", before, got)
	}
}

func TestPrefetcherSingleFlight(t *testing.T) {
	content := randomContent(2<<20, 11) // 2MiB：小于头窗口，整文件就是块 0
	g := newGoogleFile(t, content)
	st := newCacheStack(t, g, 64<<20, 8<<20, 4<<20)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st.prefetch.MaybeStart("file-1")
		}()
	}
	wg.Wait()
	waitPrefetchIdle(t, st.prefetch)

	// 16 次并发触发只允许一轮预取：块 0 = 整文件（短块），上游只被打 1 次。
	assertBlockLens(t, st.cache, "file-1", map[int64]int{0: len(content)})
	if got := g.hits.Load(); got != 1 {
		t.Fatalf("单飞失效：期望 1 次上游请求，实际 %d", got)
	}
}

func TestPrefetcherSkipsWhenBlocksExist(t *testing.T) {
	content := randomContent(1<<20, 13)
	g := newGoogleFile(t, content)
	st := newCacheStack(t, g, 64<<20, 8<<20, 4<<20)

	st.prefetch.MaybeStart("file-1")
	waitPrefetchIdle(t, st.prefetch)
	hits := g.hits.Load()

	// 已有块 → 不再预取（这是"首触"的定义）。
	for i := 0; i < 3; i++ {
		st.prefetch.MaybeStart("file-1")
	}
	time.Sleep(30 * time.Millisecond)
	if got := g.hits.Load(); got != hits {
		t.Fatalf("已有块时不应再预取：%d → %d", hits, got)
	}
}

func TestPrefetcherDisabledCacheDoesNothing(t *testing.T) {
	g := newGoogleFile(t, randomContent(4096, 17))
	st := newCacheStack(t, g, 0, 8<<20, 4<<20) // 预算 0 = 关闭

	if st.cache.Enabled() {
		t.Fatal("预算 0 应是关闭状态")
	}
	st.prefetch.MaybeStart("file-1")
	time.Sleep(50 * time.Millisecond)
	if got := g.hits.Load(); got != 0 {
		t.Fatalf("缓存关闭时不应有任何预取请求，实际 %d", got)
	}
}

func TestPrefetcherNilSafe(t *testing.T) {
	var p *Prefetcher
	p.MaybeStart("file-1")  // 未接线（缓存关闭）时不应 panic
	p.ClientBegin("file-1") // 让路登记/注销同样 nil 安全
	p.ClientEnd("file-1")
}

// 真实 Google 直链没有 ETag / Last-Modified（2026-10-09 实测）：身份链必须落到
// **总字节数**（206 的 Content-Range /total），预取照常落块、照常可被本地服务——
// 绝不能像旧版那样"身份缺失即放弃"（那会让生产环境零缓存空转）。R1 修订回归点。
func TestPrefetcherUsesTotalSizeIdentity(t *testing.T) {
	content := randomContent(2<<20, 23) // 2MiB：小于头窗口，整文件就是块 0（短块）
	g := newGoogleFile(t, content)      // 主形态：无 ETag、无 Last-Modified
	st := newCacheStack(t, g, 64<<20, 8<<20, 4<<20)

	st.prefetch.MaybeStart("file-1")
	waitPrefetchIdle(t, st.prefetch)
	assertBlockLens(t, st.cache, "file-1", map[int64]int{0: len(content)})

	meta, ok := st.cache.Meta("file-1")
	if !ok {
		t.Fatal("预取应记录文件元数据")
	}
	if meta.etag != "" || meta.lastModified != "" {
		t.Fatalf("主形态不应有 ETag/Last-Modified：%+v", meta)
	}
	want := fmt.Sprintf("size:%d", len(content))
	if meta.identity() != want {
		t.Fatalf("身份应退化为总字节数 %q，实际 %q", want, meta.identity())
	}
	if _, ok := st.cache.Get("file-1", meta.identity(), 0); !ok {
		t.Fatal("按总大小身份应能命中块 0")
	}

	// 端到端再确认一次：命中区间由本地服务，不再触上游。
	before := g.hits.Load()
	resp, body := doGet(t, st, "bytes=0-1023")
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, content[:1024]) {
		t.Fatalf("全命中应返回 206 + 原始字节：status=%d", resp.StatusCode)
	}
	if got := g.hits.Load(); got != before {
		t.Fatalf("全命中不应触上游：%d → %d", before, got)
	}
}

// 身份三项全缺只可能是畸形上游（真实 Google 至少给总字节数）。v0.3.2 起头预取是
// **一条流**：整条流共享同一个响应身份，继续读只会白费带宽——弃掉这条流
// （不写空身份块：空键永远不可命中），等待后续请求再试；畸形窗口结束后必须能
// 在下一次触发里自愈，把整个期望块集补齐。
func TestPrefetcherSkipsBlocksWithoutAnyIdentity(t *testing.T) {
	content := randomContent(3*blockSize, 31) // 12MiB：头 2 块 + 尾 1 块
	g := newGoogleFile(t, content)

	var malformed atomic.Bool
	malformed.Store(true)
	// 畸形形态：206 的 Content-Range 缺 `/total`，也不带 ETag/Last-Modified
	// —— 三类身份来源全缺。
	g.setHook(func(w http.ResponseWriter, r *http.Request, g *googleFile) bool {
		if !malformed.Load() {
			return false // 畸形窗口结束：按标准形态服务，验证自愈
		}
		start, end, ranged, unsatisfiable := fakeRange(r.Header.Get("Range"), int64(len(content)))
		if !ranged || unsatisfiable {
			return false
		}
		w.Header().Set("Content-Type", "video/x-matroska")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d", start, end)) // 缺 "/total"
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content[start : end+1])
		return true
	})
	st := newCacheStack(t, g, 64<<20, 8<<20, 4<<20) // 头窗口 2 块 + 尾 1 块

	st.prefetch.MaybeStart("file-1")
	waitPrefetchIdle(t, st.prefetch)

	// 缺身份的流打一次就被弃（不再逐块空转），且一个空身份块都不写。
	if got := g.hits.Load(); got != 1 {
		t.Fatalf("缺身份的流应一次弃用而非逐块空转：期望 1 次上游请求，实际 %d", got)
	}
	assertBlockLens(t, st.cache, "file-1", nil)

	// 自愈：畸形结束后下一次触发把期望块集（头 2 块 + 尾 1 块）补齐。
	malformed.Store(false)
	st.prefetch.MaybeStart("file-1")
	waitPrefetchIdle(t, st.prefetch)
	waitBlockLens(t, st.cache, "file-1", map[int64]int{0: blockSize, 1: blockSize, 2: blockSize})
	if got := g.hits.Load(); got != 3 {
		t.Fatalf("自愈应为 1 条头流 + 1 条尾流（总 3），实际 %d", got)
	}
}

// 预取在途换版本竞态（v0.3.1 语义推广到流）：流首 Observe 一次身份，另有逐块
// 新鲜度检查兜底。若在响应在途的窗口里同 id 文件恰被替换、且另一请求已把
// **新身份**观测进元数据表：
//   - size 形态：预取的旧响应会把 meta 的总大小回退成旧值，旧字节落到旧身份键下
//     ——与回退后的 meta 恰好匹配，下一个请求就会命中陈旧字节（本地服务旧版本）；
//   - ETag 形态：旧字节甚至会被写到"新身份"键下。
//
// 修法：流首 Observe 之前先读一次 Meta——原身份非空且与合并后身份不同即弃用
// 整条流；流**进行中**再变化由 putBlock 的逐块新鲜度检查兜底（长流可能跑很久，
// 只靠流首一次检查不够）。本用例用可控时序精确构造：预取响应体被假上游扣住 →
// 另一请求观测到新版本 → 放行旧响应体。
func TestPrefetcherDropsBlockWhenIdentityChangesInFlight(t *testing.T) {
	v1 := randomContent(3<<20, 81) // 旧版本 3MiB：小于头窗口 → 一轮预取只有块 0
	v2 := randomContent(5<<20, 82) // 新版本 5MiB：大小不同 → 身份必变（size 形态）
	g := newGoogleFile(t, v1)

	var (
		hookMu  sync.Mutex
		holding bool
	)
	entered := make(chan struct{})
	release := make(chan struct{})
	var relOnce sync.Once
	releaseFn := func() { relOnce.Do(func() { close(release) }) }
	defer releaseFn() // 任何失败路径都要放行，否则清理时 httptest.Server.Close 会等挂

	g.setHook(func(w http.ResponseWriter, r *http.Request, g *googleFile) bool {
		hookMu.Lock()
		first := !holding
		if first {
			holding = true
		}
		hookMu.Unlock()
		if !first {
			return false // 后续请求（另一请求与自愈）按当前内容正常服务
		}
		// 第一个上游请求 = 预取的块 0：先发旧版本的 206 响应头并 flush，再扣住 body
		// ——精确构造"响应在途"。放行后写的仍是旧版本的字节。
		start, end, _, _ := fakeRange(r.Header.Get("Range"), int64(len(v1)))
		hdr := w.Header()
		hdr.Set("Content-Type", "video/x-matroska")
		hdr.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(v1)))
		hdr.Set("Content-Length", strconv.Itoa(int(end-start+1)))
		w.WriteHeader(http.StatusPartialContent)
		_ = http.NewResponseController(w).Flush()
		close(entered)
		<-release
		_, _ = w.Write(v1)
		return true
	})

	st := newCacheStack(t, g, 64<<20, 4<<20, 0) // 头 4MiB（1 块）、不预取尾

	// 起第一触预取；它的首个（也是唯一一个）块请求会被假上游扣住。
	st.prefetch.MaybeStart("file-1")
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("预取请求未在超时内到达假上游")
	}

	// 响应在途：上游已换版本，另一请求把它观测进缓存（新身份 = size:5MiB）。
	g.set(v2, "")
	bResp := mustGet(t, st.url("file-1", time.Now().Add(time.Hour)), rangeHeader("bytes=0-1023"))
	if body := readBody(t, bResp); sha256Hex(body) != sha256Hex(v2[:1024]) {
		t.Fatal("在途期间的另一请求应看到新版本字节")
	}

	// 放行旧响应体：预取读到的是旧版本字节，但此刻身份已经是新版本。
	releaseFn()
	waitPrefetchIdle(t, st.prefetch)

	// 竞态块必须一个都不落：既不能挂在新身份键下（旧字节写到新身份 = 陈旧字节可被
	// 本地服务），也不能挂在旧身份键下（它会被回退后的 meta 命中）。
	assertBlockLens(t, st.cache, "file-1", nil)
	if _, ok := st.cache.Get("file-1", fmt.Sprintf("size:%d", len(v1)), 0); ok {
		t.Fatal("旧身份键下不应有块：在途的旧字节不得落盘")
	}
	if _, ok := st.cache.Get("file-1", fmt.Sprintf("size:%d", len(v2)), 0); ok {
		t.Fatal("新身份键下不应有块：在途的旧字节不得写到新身份键下")
	}

	// 紧随的请求必须拿到新版本字节（若旧字节落盘且 meta 被回退，这里会命中陈旧字节）。
	resp, body := doGet(t, st, "bytes=0-1023")
	if sha256Hex(body) != sha256Hex(v2[:1024]) {
		t.Fatalf("竞态后请求不得命中旧字节：status=%d len=%d", resp.StatusCode, len(body))
	}

	// 自愈：上面那次请求触发的预取可能与它自己的 Observe 竞态而被弃（弃用整条流
	// 的正当情形），因此这里无条件再触达一次——两种顺序下缓存最终都必须就位。
	doGet(t, st, "bytes=0-1023")
	waitPrefetchIdle(t, st.prefetch)
	waitBlockLens(t, st.cache, "file-1", map[int64]int{0: blockSize})

	// 就位后回到纯本地命中（新版本字节）。
	before := g.hits.Load()
	resp, body = doGet(t, st, "bytes=0-1023")
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, v2[:1024]) {
		t.Fatalf("自愈后应本地命中新版本：status=%d len=%d", resp.StatusCode, len(body))
	}
	if got := g.hits.Load(); got != before {
		t.Fatalf("自愈后不应再触上游：%d → %d", before, got)
	}
	if meta, ok := st.cache.Meta("file-1"); !ok || meta.identity() != fmt.Sprintf("size:%d", len(v2)) {
		t.Fatalf("meta 应自愈为新身份：%+v（ok=%v）", meta, ok)
	}
}

// 上游出错（500/连接失败）只 WARN 放弃：不重试、不堆请求（防重试风暴）。
func TestPrefetcherAbandonsOnUpstreamError(t *testing.T) {
	g := newGoogleFile(t, nil)
	g.setHook(func(w http.ResponseWriter, r *http.Request, g *googleFile) bool {
		http.Error(w, "boom", http.StatusInternalServerError)
		return true
	})
	st := newCacheStack(t, g, 64<<20, 8<<20, 4<<20)

	st.prefetch.MaybeStart("file-1")
	waitPrefetchIdle(t, st.prefetch)
	if got := g.hits.Load(); got != 1 {
		t.Fatalf("上游失败应放弃且不重试，实际 %d 次请求", got)
	}
	assertBlockLens(t, st.cache, "file-1", nil)
}

// 预取与真实播放共享出口：全局同时最多 2 个文件的预取在跑，满了直接跳过（不排队）。
func TestPrefetcherRespectsGlobalConcurrencyGate(t *testing.T) {
	g := newGoogleFile(t, randomContent(4096, 19))
	st := newCacheStack(t, g, 64<<20, 8<<20, 4<<20)

	st.prefetch.sem <- struct{}{}
	st.prefetch.sem <- struct{}{}
	st.prefetch.MaybeStart("file-1")
	if got := prefetchInflight(st.prefetch); got != 0 {
		t.Fatalf("并发已满时不应登记在途预取，实际 %d", got)
	}
	time.Sleep(30 * time.Millisecond)
	if got := g.hits.Load(); got != 0 {
		t.Fatalf("并发已满时不应发起预取，实际 %d 次请求", got)
	}

	<-st.prefetch.sem // 腾出一个槽位
	st.prefetch.MaybeStart("file-1")
	waitPrefetchIdle(t, st.prefetch)
	if got := g.hits.Load(); got == 0 {
		t.Fatal("槽位释放后应能预取")
	}
}

// 块 0 早熟（design §3）：单条大流下，块 0 在收到第一个 blockSize 时就绪，
// 不随整段头预取结束才可见。用慢速滴流把时间线拉开：上游写满第一个块后扣住
// 连接——此时块 0 必须已可 Get，且「首块就绪」日志先于「首触预取完成」。
func TestPrefetcherFirstBlockReadyBeforeStreamEnd(t *testing.T) {
	const (
		fileSize = 12 << 20 // 3 个整块
		headSize = 8 << 20
		tailSize = 4 << 20
	)
	content := randomContent(fileSize, 41)
	g := newGoogleFile(t, content)

	blockReady := make(chan struct{})
	release := make(chan struct{})
	var relOnce sync.Once
	releaseFn := func() { relOnce.Do(func() { close(release) }) }
	defer releaseFn() // 失败路径也要放行，否则清理时 httptest.Server.Close 会等挂

	g.setHook(func(w http.ResponseWriter, r *http.Request, g *googleFile) bool {
		if r.Header.Get("Range") != "bytes=0-8388607" { // headSize-1
			return false // 尾流/其它请求按标准形态正常服务
		}
		hdr := w.Header()
		hdr.Set("Content-Type", "video/x-matroska")
		hdr.Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", headSize-1, fileSize))
		hdr.Set("Content-Length", strconv.FormatInt(headSize, 10))
		w.WriteHeader(http.StatusPartialContent)
		const chunk = 512 << 10
		for written := 0; written < headSize; {
			n := chunk
			if headSize-written < n {
				n = headSize - written
			}
			if _, err := w.Write(content[written : written+n]); err != nil {
				return true
			}
			_ = http.NewResponseController(w).Flush()
			written += n
			if written == headSize/2 {
				close(blockReady)
				<-release // 扣住连接：块 0 已到齐（4MiB），流还没结束
			}
		}
		return true
	})

	buf := &lockedLogBuffer{}
	st := newCacheStackWithLogger(t, g, 64<<20, headSize, tailSize, newInfoLogger(buf))
	st.prefetch.MaybeStart("file-1")

	select {
	case <-blockReady:
	case <-time.After(30 * time.Second):
		t.Fatal("假上游未在超时内写满第一个块")
	}

	// 流还扣着的时候：块 0 已就绪（早熟），块 1 不能出现。
	waitBlockLens(t, st.cache, "file-1", map[int64]int{0: blockSize})
	meta, ok := st.cache.Meta("file-1")
	if !ok {
		t.Fatal("流首应已观测到元数据")
	}
	if block, ok := st.cache.Get("file-1", meta.identity(), 0); !ok || len(block) != blockSize {
		t.Fatalf("流未结束前块 0 应可 Get：ok=%v len=%d", ok, len(block))
	}
	if _, ok := st.cache.Get("file-1", meta.identity(), 1); ok {
		t.Fatal("流未结束前块 1 不应就绪")
	}

	waitLogContains(t, buf, "首块就绪")
	if strings.Contains(buf.String(), "首触预取完成") {
		t.Fatal("整段预取尚未结束，不应出现完成日志")
	}

	// 放行：头流收尾（块 1）+ 尾流（块 2）补齐；首块日志必须先于完成日志。
	releaseFn()
	waitPrefetchIdle(t, st.prefetch)
	waitBlockLens(t, st.cache, "file-1", map[int64]int{0: blockSize, 1: blockSize, 2: blockSize})
	if got := g.hits.Load(); got != 2 {
		t.Fatalf("一条头流 + 一条尾流应只打 2 次上游，实际 %d", got)
	}
	logs := buf.String()
	first, done := strings.Index(logs, "首块就绪"), strings.Index(logs, "首触预取完成")
	if first < 0 || done < 0 || first > done {
		t.Fatalf("首块就绪应先于首触预取完成：first=%d done=%d", first, done)
	}
}

// 让路（design §2.2/§3）：同文件客户端在途时预取的读取循环暂停（活跃期无新字节
// 落块），客户端结束后自动恢复。用一条"混合服务"的客户端请求把活跃期钉住：
// 块 0 作本地前缀先行，余段的上游请求被假上游扣住。
func TestPrefetcherYieldsToActiveClientAndResumes(t *testing.T) {
	const (
		fileSize = 12 << 20
		headSize = 8 << 20
		tailSize = 4 << 20
	)
	content := randomContent(fileSize, 43)
	g := newGoogleFile(t, content)

	clientHold := make(chan struct{})
	releaseClient := make(chan struct{})
	var relOnce sync.Once
	releaseFn := func() { relOnce.Do(func() { close(releaseClient) }) }
	defer releaseFn() // 失败路径也要放行，否则客户端 goroutine 与清理都会挂住

	g.setHook(func(w http.ResponseWriter, r *http.Request, g *googleFile) bool {
		switch r.Header.Get("Range") {
		case "bytes=0-8388607": // 头流：慢速滴流（128KB/20ms ≈ 6.4MB/s）
			hdr := w.Header()
			hdr.Set("Content-Type", "video/x-matroska")
			hdr.Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", headSize-1, fileSize))
			hdr.Set("Content-Length", strconv.FormatInt(headSize, 10))
			w.WriteHeader(http.StatusPartialContent)
			for written := 0; written < headSize; written += 128 << 10 {
				if _, err := w.Write(content[written : written+(128<<10)]); err != nil {
					return true
				}
				_ = http.NewResponseController(w).Flush()
				time.Sleep(20 * time.Millisecond)
			}
			return true
		case "bytes=4194304-8388607": // 客户端混合服务的余段：扣住 = 客户端在途
			close(clientHold)
			<-releaseClient
			return false
		default:
			return false
		}
	})

	buf := &lockedLogBuffer{}
	st := newCacheStackWithLogger(t, g, 64<<20, headSize, tailSize, newInfoLogger(buf))
	st.prefetch.MaybeStart("file-1")
	waitBlockLens(t, st.cache, "file-1", map[int64]int{0: blockSize}) // 块 0 就绪

	// 客户端请求整个头窗口：块 0 混合服务（本地前缀 + 上游余段），余段被扣住。
	clientDone := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, st.url("file-1", time.Now().Add(time.Hour)), nil)
		if err != nil {
			clientDone <- err
			return
		}
		req.Header = rangeHeader("bytes=0-8388607")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			clientDone <- err
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			clientDone <- err
			return
		}
		if resp.StatusCode != http.StatusPartialContent {
			clientDone <- fmt.Errorf("客户端应得 206，实际 %d", resp.StatusCode)
			return
		}
		if !bytes.Equal(body, content[:headSize]) {
			clientDone <- fmt.Errorf("客户端字节不符：len=%d", len(body))
			return
		}
		clientDone <- nil
	}()

	select {
	case <-clientHold:
	case <-time.After(30 * time.Second):
		t.Fatal("客户端请求未在超时内到达假上游")
	}

	// 让路：预取必须在检查周期内看到客户端在途并暂停。
	waitLogContains(t, buf, "预取让路")
	if held := blockLens(st.cache, "file-1"); len(held) != 1 || held[0] != blockSize {
		t.Fatalf("让路开始时应只有块 0，实际 %v", held)
	}
	time.Sleep(800 * time.Millisecond)
	if now := blockLens(st.cache, "file-1"); len(now) != 1 || now[0] != blockSize {
		t.Fatalf("客户端在途期间预取不得继续落块，实际 %v", now)
	}

	// 放行客户端：结束后预取恢复；头流收尾 + 尾流补齐。
	releaseFn()
	if err := <-clientDone; err != nil {
		t.Fatalf("客户端请求失败：%v", err)
	}
	waitLogContains(t, buf, "预取恢复")
	waitPrefetchIdle(t, st.prefetch)
	waitBlockLens(t, st.cache, "file-1", map[int64]int{0: blockSize, 1: blockSize, 2: blockSize})
	if got := g.hits.Load(); got != 3 {
		t.Fatalf("上游请求数应为 1 客户端余段 + 1 头流 + 1 尾流 = 3，实际 %d", got)
	}
}

// 断点续取（design §2.3/§3）：头流被上游掐断（短读 EOF）后，已齐块保留、未齐块
// 缺失；再次触发只补缺失部分——头段从第一条缺失块起一条续流 + 尾块单独一条，
// 上游请求序列精确到条；期望块集齐全后不再触发。
func TestPrefetcherResumesMissingBlocksAfterInterruption(t *testing.T) {
	const (
		fileSize = 20 << 20 // 5 个整块
		headSize = 8 << 20  // 头 2 块
		tailSize = 4 << 20  // 尾块（块 4）
	)
	content := randomContent(fileSize, 47)
	g := newGoogleFile(t, content)

	var (
		mu     sync.Mutex
		ranges []string
		cuts   int
	)
	g.setHook(func(w http.ResponseWriter, r *http.Request, g *googleFile) bool {
		rng := r.Header.Get("Range")
		mu.Lock()
		ranges = append(ranges, rng)
		cut := false
		if cuts == 0 && rng == "bytes=0-8388607" {
			cuts++
			cut = true
		}
		mu.Unlock()
		if !cut {
			return false // 后续请求按标准形态正常服务
		}
		// 被掐断的头流：chunked（不给 Content-Length）只写 5MiB 就干净收尾
		// ——精确制造"短读 EOF"；块 1 只收到 1MiB，不完整、绝不写入。
		hdr := w.Header()
		hdr.Set("Content-Type", "video/x-matroska")
		hdr.Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", headSize-1, fileSize))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content[:5<<20])
		return true
	})

	st := newCacheStack(t, g, 64<<20, headSize, tailSize)

	// 第一轮：头流被掐断 → 只有块 0 落地。
	st.prefetch.MaybeStart("file-1")
	waitPrefetchIdle(t, st.prefetch)
	assertBlockLens(t, st.cache, "file-1", map[int64]int{0: blockSize})
	if got := g.hits.Load(); got != 1 {
		t.Fatalf("首轮只应有一条被掐断的头流，实际 %d 次上游请求", got)
	}

	// 第二轮：只补缺失——头段从块 1 起的续流（不重拉块 0）+ 尾块流。
	st.prefetch.MaybeStart("file-1")
	waitPrefetchIdle(t, st.prefetch)
	waitBlockLens(t, st.cache, "file-1", map[int64]int{0: blockSize, 1: blockSize, 4: blockSize})
	if got := g.hits.Load(); got != 3 {
		t.Fatalf("续取应精确为 1 条续流 + 1 条尾流（累计 3），实际 %d", got)
	}
	mu.Lock()
	got := append([]string(nil), ranges...)
	mu.Unlock()
	want := []string{"bytes=0-8388607", "bytes=4194304-8388607", "bytes=16777216-20971519"}
	if len(got) != len(want) {
		t.Fatalf("上游请求序列不符：期望 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条上游请求应为 %s，实际 %s", i+1, want[i], got[i])
		}
	}

	// 期望块集齐全后不再触发。
	before := g.hits.Load()
	for i := 0; i < 3; i++ {
		st.prefetch.MaybeStart("file-1")
	}
	time.Sleep(50 * time.Millisecond)
	if got := g.hits.Load(); got != before {
		t.Fatalf("期望块集齐全后不应再触发：%d → %d", before, got)
	}

	// 已齐块内容必须与源字节一致（块 0 来自被掐断的流、块 1/4 来自续取）。
	meta, ok := st.cache.Meta("file-1")
	if !ok {
		t.Fatal("应有元数据")
	}
	for _, idx := range []int64{0, 1, 4} {
		data, ok := st.cache.Get("file-1", meta.identity(), idx)
		if !ok {
			t.Fatalf("块 %d 应可 Get", idx)
		}
		start := idx * blockSize
		if !bytes.Equal(data, content[start:start+int64(len(data))]) {
			t.Fatalf("块 %d 字节与源不一致", idx)
		}
	}
}

// 200 忽略 Range 边界（design §2.1）：上游不按 Range 回 200 整文件时，按响应的
// **实际起点**尽力填块——块号 = 文件偏移 / blockSize；不完整块照旧不写，不放弃整轮。
func TestPrefetcherBestEffortWhenUpstreamIgnoresRange(t *testing.T) {
	const (
		fileSize = 12 << 20
		headSize = 8 << 20
	)
	content := randomContent(fileSize, 53)
	g := newGoogleFile(t, content)

	var (
		mu    sync.Mutex
		calls int
	)
	g.setHook(func(w http.ResponseWriter, r *http.Request, g *googleFile) bool {
		mu.Lock()
		calls++
		order := calls
		mu.Unlock()
		if order == 1 {
			// 第一条头流：声明 8MiB 却只写 4MiB 就返回（传输被掐断）。
			hdr := w.Header()
			hdr.Set("Content-Type", "video/x-matroska")
			hdr.Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", headSize-1, fileSize))
			hdr.Set("Content-Length", strconv.FormatInt(headSize, 10))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(content[:blockSize])
			return true
		}
		// 第二条（块 1 的续流）：无视 Range 回 200 整文件。响应体从实际文件偏移 0
		// 开始切块：块 0 已存在（Put 首写胜出跳过），块 1 取到 4MiB 处的正确字节。
		hdr := w.Header()
		hdr.Set("Content-Type", "video/x-matroska")
		hdr.Set("Content-Length", strconv.FormatInt(fileSize, 10))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
		return true
	})

	buf := &lockedLogBuffer{}
	st := newCacheStackWithLogger(t, g, 64<<20, headSize, 0, newInfoLogger(buf))

	// 第一轮：被掐断 → 块 0 落地、块 1 缺失（不完整块绝不写入）。
	st.prefetch.MaybeStart("file-1")
	waitPrefetchIdle(t, st.prefetch)
	assertBlockLens(t, st.cache, "file-1", map[int64]int{0: blockSize})

	// 第二轮：200 整文件 → 按实际起点尽力填块，块 1 从 4MiB 处切出。
	st.prefetch.MaybeStart("file-1")
	waitPrefetchIdle(t, st.prefetch)
	waitBlockLens(t, st.cache, "file-1", map[int64]int{0: blockSize, 1: blockSize})
	if got := g.hits.Load(); got != 2 {
		t.Fatalf("两轮共应 2 次上游请求，实际 %d", got)
	}
	waitLogContains(t, buf, "未按 Range 返回")
	meta, ok := st.cache.Meta("file-1")
	if !ok {
		t.Fatal("应有元数据")
	}
	data, ok := st.cache.Get("file-1", meta.identity(), 1)
	if !ok || !bytes.Equal(data, content[blockSize:2*blockSize]) {
		t.Fatal("块 1 必须是从实际流里切出的正确字节")
	}
}

// 非块对齐起点边界：上游回报的区间起点不在块边界上时，按块号取整会把错位字节
// 挂到相邻块上（本地服务出来的字节静默错位）。宁可弃掉这条流也不写入错位字节，
// 后续请求会再试（与"不完整块绝不写入"同一优先级：字节一致性第一）。
func TestPrefetcherDropsMisalignedRangeStart(t *testing.T) {
	const fileSize = 12 << 20
	content := randomContent(fileSize, 59)
	g := newGoogleFile(t, content)

	g.setHook(func(w http.ResponseWriter, r *http.Request, g *googleFile) bool {
		if r.Header.Get("Range") != "bytes=0-8388607" {
			return false // 尾流等按标准形态服务（本用例不应走到）
		}
		// 起点 1000：不是块边界（块边界是 blockSize 的整数倍）
		hdr := w.Header()
		hdr.Set("Content-Type", "video/x-matroska")
		hdr.Set("Content-Range", fmt.Sprintf("bytes 1000-%d/%d", fileSize-1, fileSize))
		hdr.Set("Content-Length", strconv.Itoa(fileSize-1000))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content[1000:])
		return true
	})

	buf := &lockedLogBuffer{}
	st := newCacheStackWithLogger(t, g, 64<<20, 8<<20, 4<<20, newInfoLogger(buf))
	st.prefetch.MaybeStart("file-1")
	waitPrefetchIdle(t, st.prefetch)

	// 错位流必须被弃：一个块都不写（写了就是错位字节）。
	assertBlockLens(t, st.cache, "file-1", nil)
	waitLogContains(t, buf, "起点不在块边界")
}

// 401/403 单次刷新重试在单条流里保留（v2 检查修订点，不许回归）：头流首次撞 401
// 时重拉直链并重试一次；重试用新直链成功，整轮预取不受影响，且全轮只刷新一次。
func TestPrefetcherRefreshesLinkOnceOn401(t *testing.T) {
	content := randomContent(2<<20, 61) // 2MiB：小于头窗口 → 整文件就是块 0（短块）
	g := newGoogleFile(t, content)

	var first atomic.Bool
	first.Store(true)
	g.setHook(func(w http.ResponseWriter, r *http.Request, g *googleFile) bool {
		if first.CompareAndSwap(true, false) {
			w.WriteHeader(http.StatusUnauthorized) // 只掐第一条流，重试按标准形态服务
			return true
		}
		return false
	})

	st := newCacheStack(t, g, 64<<20, 8<<20, 4<<20)
	st.prefetch.MaybeStart("file-1")
	waitPrefetchIdle(t, st.prefetch)

	waitBlockLens(t, st.cache, "file-1", map[int64]int{0: len(content)})
	if got := g.hits.Load(); got != 2 {
		t.Fatalf("401 后应重试恰好一次：期望 2 次上游请求，实际 %d", got)
	}
	// 预取自身的 Link() 1 次 + Refresh 1 次 = master 恰好被请求 2 次（数据面不在场）。
	if got := st.master.requests.Load(); got != 2 {
		t.Fatalf("401 后应重拉直链恰好一次：期望 2 次 master 请求，实际 %d", got)
	}
}
