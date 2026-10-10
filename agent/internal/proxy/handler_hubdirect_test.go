package proxy

// v2 hub 直连数据面（v0.4.2 N3/N4，任务 10-10-agent-hub-direct-v2）的端到端测试：
// 真 Hub 数据面（复用 hub_test.go 的 fixture）+ 假 master（请求计数 + stale 参数
// 记录）+ 真 Handler + 计数拨号器。
//
// 验收口径：
//   - 稳态（hub 健康且已 warm）：客户端直连 hub 取流，master 请求数为 0；
//   - hub 连接级失败 / 409 not_warmed：**恰好一次**回访 master，且必带 stale=1
//     （N4），master 侧据此现场重预热（R1 自愈）；
//   - 首触预取走签名 URL 里的同一个 hub 上游，绝不触发 master 回访；
//   - v1 回归：换链请求里没有 stale 参数，日志行没有 hub_direct（线上形状不变）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// hubDirectURL 拼一条 v2 签名 URL：u/f 按查询参数转义，签名覆盖**解码后**的原值
// （与 master 侧 signClientURLV2 的拼法逐字一致）。
func hubDirectURL(agentURL, fileID, hubBase, driveID string, key []byte, expires time.Time) string {
	e := strconv.FormatInt(expires.Unix(), 10)
	return agentURL + "/dl/" + fileID +
		"?e=" + e +
		"&u=" + url.QueryEscape(hubBase) +
		"&f=" + url.QueryEscape(driveID) +
		"&s=" + SignV2(key, fileID, e, hubBase, driveID)
}

// hubDirectStack 是 node 侧的一条数据面：假 master（记录 stale）+ 真 Handler。
type hubDirectStack struct {
	t       *testing.T
	agent   *httptest.Server
	master  *fakeMaster
	signKey []byte
	dial    *countingDialer
	logs    *lockedLogBuffer
	source  *LinkSource
	cache   *BlockCache
	prefet  *Prefetcher

	mu    sync.Mutex
	stale []string // 每次 master 请求实际收到的 stale 查询参数
}

// newHubDirectStack 组装数据面。cacheBytes != 0 时接线读前缓存与首触预取
// （headBytes 为预取头窗口，tail 恒为 0 保持请求序列确定）。
func newHubDirectStack(t *testing.T, respond func(w http.ResponseWriter, r *http.Request, n int64), cacheBytes, headBytes int64) *hubDirectStack {
	t.Helper()
	st := &hubDirectStack{t: t, signKey: testSignKey(t), logs: &lockedLogBuffer{}}
	st.master = newFakeMaster(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		st.mu.Lock()
		st.stale = append(st.stale, r.URL.Query().Get("stale"))
		st.mu.Unlock()
		respond(w, r, n)
	})
	st.source = newTestLinkSource(t, st.master.srv.URL, nil)
	st.dial = newCountingDialer()
	client := &http.Client{Transport: upstreamTransport(st.dial.DialContext)}
	logger := newInfoLogger(st.logs)

	cfg := Config{
		SignKey:       st.signKey,
		MaxConcurrent: 8,
		Links:         st.source,
		Logger:        logger,
		Client:        client,
	}
	if cacheBytes != 0 {
		st.cache = NewBlockCache(cacheBytes)
		st.prefet = NewPrefetcher(PrefetcherConfig{
			Cache: st.cache, Links: st.source, Client: client, Logger: logger,
			HeadBytes: headBytes, TailBytes: 0,
		})
		cfg.Cache = st.cache
		cfg.Prefetch = st.prefet
	}
	st.agent = httptest.NewServer(NewHandler(cfg))
	t.Cleanup(st.agent.Close)
	return st
}

// staleSeen 返回 master 收到的 stale 参数序列（一次请求一项）。
func (s *hubDirectStack) staleSeen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.stale...)
}

// noMasterCallbacks 让 master 一旦被请求就立刻报错（稳态断言用）。
func noMasterCallbacks(t *testing.T) func(w http.ResponseWriter, r *http.Request, n int64) {
	return func(w http.ResponseWriter, r *http.Request, n int64) {
		t.Errorf("v2 稳态不应回访 master，实际收到第 %d 次请求", n)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// rewarmHub 模拟 master 的 stale 自愈动作：向 hub 的 /warm 同步重发一次指令
// （v0.4.2 N4 的生产行为）。它在 httptest 的处理器 goroutine 里被调用，
// 只允许 t.Errorf，禁止 Fatalf。
func rewarmHub(t *testing.T, hubBase, fileID, directURL string, auth map[string]string) {
	payload, err := json.Marshal(map[string]any{
		"file_id":     fileID,
		"file_token":  "tok-" + fileID,
		"direct_link": directURL,
		"auth":        auth,
	})
	if err != nil {
		t.Errorf("marshal warm 载荷失败: %v", err)
		return
	}
	resp, err := http.Post(hubBase+hubWarmPath, "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Errorf("重发 /warm 失败: %v", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("重发 /warm 状态 = %d，应 200", resp.StatusCode)
	}
}

// TestHubDirectSteadyStateZeroMasterCallbacks：hub 健康且已 warm 时，v2 请求
// 完全由 node 直连 hub 供流——字节正确、hub 本地命中零出网、master 零回访；
// 连续两次请求都是稳态。
func TestHubDirectSteadyStateZeroMasterCallbacks(t *testing.T) {
	size := 2*blockSize + 12345
	payload := makePayload(size)
	hf := newHubFixture(t, payload, nil)
	// hub 本地命中：块 0 预置 + 元数据（完全不出网的最强形态）。
	identity := fmt.Sprintf("size:%d", size)
	hf.cache.Observe(hf.fileID, fileMeta{size: int64(size)})
	if !hf.cache.Put(hf.fileID, identity, 0, payload[:blockSize]) {
		t.Fatal("预置 hub 块 0 失败")
	}
	hf.installLink() // stopped：不跑预热状态机，上游请求计数保持确定
	hubSrv := httptest.NewServer(hf.hub)
	t.Cleanup(hubSrv.Close)

	st := newHubDirectStack(t, noMasterCallbacks(t), 0, 0)

	// 第一次：块 0 内的区间 → hub 纯本地 206。
	resp := mustGet(t, hubDirectURL(st.agent.URL, "node-token-1", hubSrv.URL, hf.fileID,
		st.signKey, time.Now().Add(time.Hour)), rangeHeader("bytes=100-999"))
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("稳态请求应 206，实际 %d", resp.StatusCode)
	}
	if !bytes.Equal(body, payload[100:1000]) {
		t.Fatalf("稳态字节不符：得到 %d 字节", len(body))
	}

	// 第二次：仍在块 0 内 → 同样零回访。
	resp = mustGet(t, hubDirectURL(st.agent.URL, "node-token-1", hubSrv.URL, hf.fileID,
		st.signKey, time.Now().Add(time.Hour)), rangeHeader("bytes=0-4095"))
	body = readBody(t, resp)
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, payload[:4096]) {
		t.Fatalf("第二次稳态请求不符：status=%d len=%d", resp.StatusCode, len(body))
	}

	if got := st.master.requests.Load(); got != 0 {
		t.Fatalf("稳态下 master 请求数应为 0，实际 %d", got)
	}
	if seen := st.staleSeen(); len(seen) != 0 {
		t.Fatalf("稳态下不应有任何换链，实际 %v", seen)
	}
	if got := hf.up.count(); got != 0 {
		t.Fatalf("hub 本地命中不应触达其上游，实际 %d 次", got)
	}
	// 日志形态：v2 请求带 hub_direct=true（v1 的负向断言在 v1 回归用例里）。
	waitLogContains(t, st.logs, "hub_direct=true")
}

// TestHubDirectDeadHubRefreshesOnceWithStale：hub 进程死了（connection refused）
// → 恰好一次换链、恰带 stale=1 → master 回退 Google 直链 → 重试成功；
// 每个地址的拨号次数精确，日志不带完整 URL。
func TestHubDirectDeadHubRefreshesOnceWithStale(t *testing.T) {
	content := randomContent(48*1024, 71)
	g := newGoogleFile(t, content)
	deadURL := closedPortURL(t)
	deadAddr := addrOf(deadURL)

	st := newHubDirectStack(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		if n != 1 {
			t.Errorf("死 hub 自愈应恰好回访 master 一次，实际第 %d 次", n)
		}
		_, _ = w.Write([]byte(linkJSON(g.linkURL(), time.Hour)))
	}, 0, 0)

	resp := mustGet(t, hubDirectURL(st.agent.URL, "node-token-1", deadURL, "drive-file-1",
		st.signKey, time.Now().Add(time.Hour)), nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("换链后应 200，实际 %d", resp.StatusCode)
	}
	if !bytes.Equal(body, content) {
		t.Fatal("换链后字节与源不一致")
	}
	if got := st.master.requests.Load(); got != 1 {
		t.Fatalf("应恰好回访 master 一次，实际 %d", got)
	}
	if seen := st.staleSeen(); len(seen) != 1 || seen[0] != "1" {
		t.Fatalf("v2 失败换链必须带 stale=1，实际 %v", seen)
	}
	if got := st.dial.count(deadAddr); got != 1 {
		t.Fatalf("死 hub 地址应恰好拨号 1 次（绝不原样重试），实际 %d", got)
	}
	if got := st.dial.count(addrOf(g.srv.URL)); got != 1 {
		t.Fatalf("换链后的重试应恰好 1 次，实际 %d", got)
	}
	if got := st.dial.total(); got != 2 {
		t.Fatalf("总上游请求应恰好 2 次，实际 %d", got)
	}
	waitLogContains(t, st.logs, "连接上游失败，重拉直链后重试一次")
	waitLogContains(t, st.logs, "代理请求")
	if s := st.logs.String(); strings.Contains(s, "http://") {
		t.Fatalf("日志不应包含完整 URL：\n%s", s)
	}
}

// TestHubDirect409RewarmsViaMasterAndRecovers：hub 返回 409 not_warmed（重启丢
// 内存的 R1 场景）→ 恰好一次回访，带 stale=1；master 现场重发 /warm 后再次
// 应答 hub 上游 → 节点重试拿到 206 正确字节（不把 409 透传给客户端）。
func TestHubDirect409RewarmsViaMasterAndRecovers(t *testing.T) {
	size := 2*blockSize + 12345
	payload := makePayload(size)
	hf := newHubFixture(t, payload, nil)
	hubSrv := httptest.NewServer(hf.hub)
	t.Cleanup(hubSrv.Close)

	st := newHubDirectStack(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		if n != 1 {
			t.Errorf("409 自愈应恰好回访 master 一次，实际第 %d 次", n)
		}
		// master 的 stale 动作：先同步重发 /warm，再应答 hub 上游。
		rewarmHub(t, hubSrv.URL, hf.fileID, hf.directURL, hf.auth)
		_, _ = fmt.Fprintf(w, `{"url":%q,"headers":{},"expires_at":%q}`,
			hubSrv.URL+"/f/"+hf.fileID, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}, 0, 0)

	// 未 warm 的 hub：第一次 /f/ 必然 409。
	resp := mustGet(t, hubDirectURL(st.agent.URL, "node-token-1", hubSrv.URL, hf.fileID,
		st.signKey, time.Now().Add(time.Hour)), rangeHeader("bytes=0-65535"))
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("重预热后应 206（409 不得透传），实际 %d", resp.StatusCode)
	}
	if !bytes.Equal(body, payload[:65536]) {
		t.Fatalf("重预热后字节不符：得到 %d 字节", len(body))
	}
	if got := st.master.requests.Load(); got != 1 {
		t.Fatalf("应恰好回访 master 一次，实际 %d", got)
	}
	if seen := st.staleSeen(); len(seen) != 1 || seen[0] != "1" {
		t.Fatalf("409 换链必须带 stale=1，实际 %v", seen)
	}
	waitLogContains(t, st.logs, "hub 报告区段未预热，带 stale 提示换链后重试一次")
	// 客户端 Range 确实到达 hub（重试路由正确）。
	clientRangeSeen := false
	for _, req := range hf.up.snapshot() {
		if req.rangeHeader == "bytes=0-65535" {
			clientRangeSeen = true
			break
		}
	}
	if !clientRangeSeen {
		t.Fatal("重试请求应携带原 Range 打到 hub 数据面")
	}
	hf.waitRunDone(hf.fileID)
}

// TestHubDirectPrefetchStreamsFromHubWithoutMaster：首触预取走签名 URL 里的
// hub 上游（MaybeStartWithLink），稳态零回访；预取块的字节正确，且随后同一
// 区间由本地缓存直接服务。
func TestHubDirectPrefetchStreamsFromHubWithoutMaster(t *testing.T) {
	size := 2*blockSize + 100
	payload := makePayload(size)
	hf := newHubFixture(t, payload, nil)
	hf.installLink() // stopped：hub 自己不做任何主动抓取，请求序列由测试驱动
	hubSrv := httptest.NewServer(hf.hub)
	t.Cleanup(hubSrv.Close)

	st := newHubDirectStack(t, noMasterCallbacks(t), 64<<20, blockSize)

	// 首触：客户端区间原样透传，同时按 v2 契约用 hub 上游起预取。
	resp := mustGet(t, hubDirectURL(st.agent.URL, "node-token-1", hubSrv.URL, hf.fileID,
		st.signKey, time.Now().Add(time.Hour)), rangeHeader("bytes=0-1023"))
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, payload[:1024]) {
		t.Fatalf("首触响应不符：status=%d len=%d", resp.StatusCode, len(body))
	}

	// 预取落地：块 0 完整（头窗口 = 1 块）。
	waitPrefetchIdle(t, st.prefet)
	waitBlockLens(t, st.cache, "node-token-1", map[int64]int{0: blockSize})

	// 预取块的字节必须与源一致。
	meta, ok := st.cache.Meta("node-token-1")
	if !ok {
		t.Fatal("预取应记录元数据")
	}
	block, ok := st.cache.Get("node-token-1", meta.identity(), 0)
	if !ok || !bytes.Equal(block, payload[:blockSize]) {
		t.Fatal("预取块 0 的字节与源不一致")
	}

	if got := st.master.requests.Load(); got != 0 {
		t.Fatalf("预取 + 数据面都不得回访 master，实际 %d 次", got)
	}
	if seen := st.staleSeen(); len(seen) != 0 {
		t.Fatalf("不应有任何换链，实际 %v", seen)
	}
	// hub 收到过预取的头窗口请求（请求形状 = 块对齐大流）。
	prefetchRangeSeen := false
	for _, req := range hf.up.snapshot() {
		if req.rangeHeader == fmt.Sprintf("bytes=0-%d", blockSize-1) {
			prefetchRangeSeen = true
			break
		}
	}
	if !prefetchRangeSeen {
		t.Fatalf("hub 未收到预取的块对齐头窗口请求：%+v", hf.up.snapshot())
	}

	// 预取就绪后，同一区间纯本地 206：hub 与 master 都不再被打扰。
	beforeUp := hf.up.count()
	resp = mustGet(t, hubDirectURL(st.agent.URL, "node-token-1", hubSrv.URL, hf.fileID,
		st.signKey, time.Now().Add(time.Hour)), rangeHeader("bytes=0-1023"))
	body = readBody(t, resp)
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, payload[:1024]) {
		t.Fatalf("本地命中响应不符：status=%d len=%d", resp.StatusCode, len(body))
	}
	if got := hf.up.count(); got != beforeUp {
		t.Fatalf("本地命中不应触达 hub 上游：%d → %d", beforeUp, got)
	}
	if got := st.master.requests.Load(); got != 0 {
		t.Fatalf("master 请求数应保持 0，实际 %d", got)
	}
}

// TestHubDirectRejectsBadSignaturesAndV1WithHF：v2 篡改矩阵（handler 层）+ v1 签名
// 夹带 u/f —— 全部 403 统一文案，且不产生任何上游拨号与 master 回访。
func TestHubDirectRejectsBadSignaturesAndV1WithHF(t *testing.T) {
	hf := newHubFixture(t, makePayload(4096), nil)
	hubSrv := httptest.NewServer(hf.hub)
	t.Cleanup(hubSrv.Close)

	st := newHubDirectStack(t, noMasterCallbacks(t), 0, 0)

	valid := hubDirectURL(st.agent.URL, "node-token-1", hubSrv.URL, hf.fileID,
		st.signKey, time.Now().Add(time.Hour))
	otherKey := make([]byte, 32)
	for i := range otherKey {
		otherKey[i] = byte(0xA0 + i)
	}
	cases := []struct {
		name string
		url  string
	}{
		{"u 被篡改（签名不动）", strings.Replace(valid,
			url.QueryEscape(hubSrv.URL), url.QueryEscape(hubSrv.URL+"x"), 1)},
		{"f 被篡改（签名不动）", strings.Replace(valid,
			url.QueryEscape(hf.fileID), url.QueryEscape(hf.fileID+"x"), 1)},
		{"签名被篡改", valid[:len(valid)-1] + "0"},
		{"已过期", hubDirectURL(st.agent.URL, "node-token-1", hubSrv.URL, hf.fileID,
			st.signKey, time.Now().Add(-time.Minute))},
		{"跨 agent 密钥", hubDirectURL(st.agent.URL, "node-token-1", hubSrv.URL, hf.fileID,
			otherKey, time.Now().Add(time.Hour))},
		{"v1 签名夹带 u/f", signedURLFor(st.signKey, st.agent.URL, "node-token-1") +
			"&u=" + url.QueryEscape(hubSrv.URL) + "&f=" + url.QueryEscape(hf.fileID)},
		{"v1 签名只夹带 u", signedURLFor(st.signKey, st.agent.URL, "node-token-1") +
			"&u=" + url.QueryEscape(hubSrv.URL)},
		{"v1 签名只夹带 f", signedURLFor(st.signKey, st.agent.URL, "node-token-1") +
			"&f=" + url.QueryEscape(hf.fileID)},
	}

	var bodies []string
	for _, tc := range cases {
		resp := mustGet(t, tc.url, nil)
		body := string(readBody(t, resp))
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s：应回 403，实际 %d", tc.name, resp.StatusCode)
		}
		if !strings.Contains(body, forbiddenText) {
			t.Fatalf("%s：403 文案应为统一中文提示：%s", tc.name, body)
		}
		bodies = append(bodies, body)
	}
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Fatalf("403 文案必须逐字一致：\n%s\nvs\n%s", bodies[0], bodies[i])
		}
	}
	if got := st.master.requests.Load(); got != 0 {
		t.Fatalf("验签失败的请求不得回访 master，实际 %d 次", got)
	}
	if got := st.dial.total(); got != 0 {
		t.Fatalf("验签失败的请求不得触达任何上游，实际 %d 次拨号", got)
	}
	if got := hf.up.count(); got != 0 {
		t.Fatalf("hub 上游不应被打扰，实际 %d 次", got)
	}
}

// TestV1RetryWireHasNoStaleParam：v1 回归——连接级失败换链的线上形状逐字不变：
// 请求里没有 stale，日志里没有 hub_direct；重试语义（恰一次）保持。
func TestV1RetryWireHasNoStaleParam(t *testing.T) {
	content := randomContent(32*1024, 73)
	g := newGoogleFile(t, content)
	deadURL := closedPortURL(t)
	deadAddr := addrOf(deadURL)

	st := newHubDirectStack(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		if n == 1 {
			_, _ = w.Write([]byte(linkJSON(deadURL+"/f/file-1", time.Hour)))
			return
		}
		_, _ = w.Write([]byte(linkJSON(g.linkURL(), time.Hour)))
	}, 0, 0)

	resp := mustGet(t, signedURLFor(st.signKey, st.agent.URL, "file-1"), nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("v1 换链重试后应 200 + 完整字节：status=%d len=%d", resp.StatusCode, len(body))
	}
	if got := st.master.requests.Load(); got != 2 {
		t.Fatalf("v1 应为 1 拉链 + 1 换链 = 2 次 master 请求，实际 %d", got)
	}
	if seen := st.staleSeen(); len(seen) != 2 || seen[0] != "" || seen[1] != "" {
		t.Fatalf("v1 换链不得携带 stale 参数，实际 %v", seen)
	}
	if got := st.dial.count(deadAddr); got != 1 {
		t.Fatalf("死链地址应恰好拨号 1 次，实际 %d", got)
	}
	if got := st.dial.count(addrOf(g.srv.URL)); got != 1 {
		t.Fatalf("换链后的重试应恰好 1 次，实际 %d", got)
	}
	waitLogContains(t, st.logs, "代理请求")
	if s := st.logs.String(); strings.Contains(s, "hub_direct") {
		t.Fatalf("v1 请求的日志不得出现 hub_direct：\n%s", s)
	}
}
