package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// hub 数据面/控制面的单测（design §7）：
// 三态字节一致性矩阵、白名单 fail-closed、409 not_warmed、区域集请求形状、
// 3 分钟规则时间线、cancel、401 换链、同步落盘。

// upstreamReq 记录一次假 Google 收到的请求。
type upstreamReq struct {
	path        string
	rangeHeader string
	method      string
	auth        string
}

// fakeUpstream 是假 Google：按 Range 回 206、无 Range 回 200；记录请求与
// 凭据头；支持预置状态序列（401 重试等场景）。
type fakeUpstream struct {
	mu          sync.Mutex
	payload     []byte
	requests    []upstreamReq
	statuses    []int         // 非空时逐个弹出直接回状态（先于正常服务）
	gate        chan struct{} // 非 nil 时每个请求先等门打开或请求取消
	ignoreRange bool
	hook        func(w http.ResponseWriter, r *http.Request) bool // 命中即接管响应（自定义区间/节流）
}

func newFakeUpstream(t *testing.T, payload []byte) (*fakeUpstream, *httptest.Server) {
	t.Helper()
	up := &fakeUpstream{payload: payload}
	server := httptest.NewServer(up)
	t.Cleanup(server.Close)
	return up, server
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, upstreamReq{
		path: r.URL.Path, rangeHeader: r.Header.Get("Range"),
		method: r.Method, auth: r.Header.Get("Authorization"),
	})
	gate := f.gate
	var status int
	if len(f.statuses) > 0 {
		status = f.statuses[0]
		f.statuses = f.statuses[1:]
	}
	ignore := f.ignoreRange
	payload := f.payload
	hook := f.hook
	f.mu.Unlock()

	if hook != nil && hook(w, r) {
		return
	}

	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	if ignore || r.Header.Get("Range") == "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
		return
	}
	// 用与生产同一套 size 感知解析：常规/open-ended 原语义不变，后缀区间
	// bytes=-N 按 RFC 映射为 [max(0,size-N), size-1]（N=0 仍是 416）。
	rng, ok := parseByteRangeSized(r.Header.Get("Range"), int64(len(payload)))
	if !ok {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	start, end := rng.start, rng.end
	if rng.openEnded() || end >= int64(len(payload)) {
		end = int64(len(payload)) - 1
	}
	if start > end || start >= int64(len(payload)) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(payload[start : end+1])
}

func (f *fakeUpstream) snapshot() []upstreamReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]upstreamReq(nil), f.requests...)
}

func (f *fakeUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeUpstream) pushStatus(code int) {
	f.mu.Lock()
	f.statuses = append(f.statuses, code)
	f.mu.Unlock()
}

func (f *fakeUpstream) setGate(gate chan struct{}) {
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()
}

func (f *fakeUpstream) setHook(hook func(w http.ResponseWriter, r *http.Request) bool) {
	f.mu.Lock()
	f.hook = hook
	f.mu.Unlock()
}

// hubMasterStub 是假 master 的 download-link 通道（换链用）。
type hubMasterStub struct {
	mu    sync.Mutex
	links map[string]string
	calls []string
}

func (m *hubMasterStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/agent/download-link" {
		http.NotFound(w, r)
		return
	}
	key := r.URL.Query().Get("file_id")
	m.mu.Lock()
	m.calls = append(m.calls, key)
	target := m.links[key]
	m.mu.Unlock()
	if target == "" {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"NOT_FOUND","message":"no link"}`))
		return
	}
	resp := map[string]any{
		"url":        target,
		"headers":    map[string]string{"Authorization": "Bearer refreshed"},
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (m *hubMasterStub) callSnapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// hubFixture 组装"假 Google + 假 master + 磁盘缓存 + Hub"。
type hubFixture struct {
	t       *testing.T
	hub     *Hub
	cache   *DiskCache
	up      *fakeUpstream
	google  *httptest.Server
	master  *httptest.Server
	masterS *hubMasterStub

	fileID    string
	payload   []byte
	directURL string
	auth      map[string]string
}

// newHubFixture 造一个默认区域小、窗口短的 hub；mutate 可覆写配置。
func newHubFixture(t *testing.T, payload []byte, mutate func(*HubConfig)) *hubFixture {
	t.Helper()
	up, google := newFakeUpstream(t, payload)
	masterS := &hubMasterStub{links: map[string]string{}}
	master := httptest.NewServer(masterS)
	t.Cleanup(master.Close)

	cache, err := NewDiskCache(DiskCacheConfig{
		Dir:         t.TempDir(),
		BudgetBytes: 1 << 30,
		MaxAge:      48 * time.Hour,
		Logger:      discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	links := newTestLinkSource(t, master.URL, nil)
	directURL := google.URL + "/gfile"
	fileID := "drive-file-1"
	masterS.links[fileID] = directURL
	masterS.links["tok-"+fileID] = directURL

	cfg := HubConfig{
		AllowIPs:              "127.0.0.1",
		Cache:                 cache,
		Links:                 links,
		Logger:                discardLogger(),
		WarmHeadBytes:         4 * 1024,
		WarmTailBytes:         2 * 1024,
		WarmResumeWindowBytes: 1 * 1024,
		WarmWait:              40 * time.Millisecond,
		WarmPollInterval:      2 * time.Millisecond,
		StallTimeout:          2 * time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	hf := &hubFixture{
		t:         t,
		hub:       NewHub(cfg),
		cache:     cache,
		up:        up,
		google:    google,
		master:    master,
		masterS:   masterS,
		fileID:    fileID,
		payload:   payload,
		directURL: directURL,
		auth:      map[string]string{"Authorization": "Bearer warm-token"},
	}
	return hf
}

// request 直接调用 ServeHTTP（可控制 RemoteAddr）。
func (hf *hubFixture) request(method, target, rangeHeader string, body []byte) *httptest.ResponseRecorder {
	hf.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	req.RemoteAddr = "127.0.0.1:40000"
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	rec := httptest.NewRecorder()
	hf.hub.ServeHTTP(rec, req)
	return rec
}

// warm 发一条 /warm。
func (hf *hubFixture) warm(regions *warmRegionsPayload, extra map[string]any) *httptest.ResponseRecorder {
	hf.t.Helper()
	payload := map[string]any{
		"file_id":     hf.fileID,
		"file_token":  "tok-" + hf.fileID,
		"direct_link": hf.directURL,
		"auth":        hf.auth,
	}
	if regions != nil {
		payload["regions"] = regions
	}
	for k, v := range extra {
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		hf.t.Fatalf("marshal warm: %v", err)
	}
	return hf.request(http.MethodPost, hubWarmPath, "", body)
}

// installLink 白盒注入一条直链（stopped=true 防 maybeContinue 起轮），
// 供只验证数据面三态、不验证预热状态机的用例使用。
func (hf *hubFixture) installLink() {
	hf.t.Helper()
	hf.hub.mu.Lock()
	hf.hub.states[hf.fileID] = &warmState{
		link: warmLink{
			link:  Link{URL: hf.directURL, Headers: hf.auth},
			token: "tok-" + hf.fileID,
		},
		spec:    hf.hub.defaultSpec(),
		stopped: true,
	}
	hf.hub.mu.Unlock()
}

// waitRunDone 等该文件的预热轮结束（含区域集 + 窗口）。
func (hf *hubFixture) waitRunDone(fileID string) {
	hf.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		hf.hub.mu.Lock()
		st := hf.hub.states[fileID]
		done := st == nil || st.run == nil
		hf.hub.mu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	hf.t.Fatal("等待预热轮结束超时")
}

func makePayload(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i % 251)
	}
	return data
}

func TestHubWhitelistFailClosed(t *testing.T) {
	hf := newHubFixture(t, makePayload(1024), nil)

	// 白名单内：/f/ 未 warm 得 409（说明已经过访问控制进入路由）。
	rec := hf.request(http.MethodGet, "/f/"+hf.fileID, "", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("白名单内 /f/ 未 warm 状态 = %d，应 409", rec.Code)
	}
	// 白名单外：一切路径 403，且不泄露路径存在性差异。
	for _, path := range []string{"/f/" + hf.fileID, hubWarmPath, hubCancelPath, "/whatever"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "10.9.9.9:1234"
		rec := httptest.NewRecorder()
		hf.hub.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s 白名单外状态 = %d，应 403", path, rec.Code)
		}
		if body := rec.Body.String(); body != `{"error":"forbidden"}` {
			t.Fatalf("403 响应体 = %s", body)
		}
	}

	// CIDR 条目生效。
	hf2 := newHubFixture(t, makePayload(1024), func(c *HubConfig) { c.AllowIPs = "10.0.0.0/8" })
	req := httptest.NewRequest(http.MethodGet, "/f/x", nil)
	req.RemoteAddr = "10.1.2.3:9"
	rec = httptest.NewRecorder()
	hf2.hub.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatal("CIDR 内地址被拒")
	}
}

func TestHubNotWarmed409(t *testing.T) {
	hf := newHubFixture(t, makePayload(1024), nil)
	rec := hf.request(http.MethodGet, "/f/"+hf.fileID, "", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态 = %d，应 409", rec.Code)
	}
	if body := rec.Body.String(); body != `{"error":"not_warmed"}` {
		t.Fatalf("响应体 = %s，应为冻结形状", body)
	}
	// HEAD 同理；非 GET/HEAD 405。
	if rec := hf.request(http.MethodHead, "/f/"+hf.fileID, "", nil); rec.Code != http.StatusConflict {
		t.Fatalf("HEAD 状态 = %d，应 409", rec.Code)
	}
	if rec := hf.request(http.MethodPost, "/f/"+hf.fileID, "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /f/ 状态 = %d，应 405", rec.Code)
	}
	if hf.up.count() != 0 {
		t.Fatalf("未 warm 的请求不该触达上游（%d 次）", hf.up.count())
	}
}

// TestHubByteMatrix 是三态服务的字节一致性矩阵：全命中（纯本地，不出网）、
// 部分命中（本地前缀 + 上游精确余段）、未命中（透传 + 同步落盘）。
func TestHubByteMatrix(t *testing.T) {
	size := 2*blockSize + 12345
	hf := newHubFixture(t, makePayload(size), nil)
	fileID := hf.fileID
	identity := fmt.Sprintf("size:%d", size)

	// 预置块 0：为"全命中"与"部分命中"提供本地前缀。
	hf.cache.Observe(fileID, fileMeta{size: int64(size)})
	if !hf.cache.Put(fileID, identity, 0, hf.payload[:blockSize]) {
		t.Fatal("预置块 0 失败")
	}
	// 注入直链（不跑预热状态机，让上游请求计数保持确定）。
	hf.installLink()

	// ① 全命中：请求完全落在块 0 内，不触上游。
	before := hf.up.count()
	rec := hf.request(http.MethodGet, "/f/"+fileID, "bytes=100-999", nil)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("全命中状态 = %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), hf.payload[100:1000]) {
		t.Fatal("全命中字节与源不一致")
	}
	if got := rec.Header().Get("Content-Range"); got != fmt.Sprintf("bytes 100-999/%d", size) {
		t.Fatalf("全命中 Content-Range = %q", got)
	}
	if hf.up.count() != before {
		t.Fatalf("全命中不该出网（新增 %d 次请求）", hf.up.count()-before)
	}

	// ② 部分命中：跨块 0 尾部的请求；上游必须收到精确余段请求。
	start := int64(blockSize - 100)
	end := int64(blockSize + 2000)
	before = hf.up.count()
	rec = hf.request(http.MethodGet, "/f/"+fileID, fmt.Sprintf("bytes=%d-%d", start, end), nil)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("混合状态 = %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), hf.payload[start:end+1]) {
		t.Fatal("混合字节与源不一致")
	}
	reqs := hf.up.snapshot()[before:]
	if len(reqs) != 1 {
		t.Fatalf("混合应恰好 1 条上游请求，实际 %d", len(reqs))
	}
	wantRange := fmt.Sprintf("bytes=%d-%d", blockSize, end)
	if reqs[0].rangeHeader != wantRange {
		t.Fatalf("混合上游 Range = %q，应 %q", reqs[0].rangeHeader, wantRange)
	}
	if reqs[0].auth != "Bearer warm-token" {
		t.Fatalf("混合上游未带 warm 凭据：%q", reqs[0].auth)
	}

	// ③ 未命中：无 Range 整文件透传，边传边同步落盘。
	before = hf.up.count()
	rec = hf.request(http.MethodGet, "/f/"+fileID, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("透传状态 = %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), hf.payload) {
		t.Fatal("透传字节与源不一致")
	}
	if hf.up.count() != before+1 {
		t.Fatalf("透传上游请求数 = %d", hf.up.count()-before)
	}
	// 同步落盘：整文件块（含最后一块）就绪。
	for idx := int64(0); idx < 3; idx++ {
		if !hf.cache.Has(fileID, identity, int64(size), idx) {
			t.Fatalf("透传后块 %d 未落盘", idx)
		}
	}

	// ④ 落盘后同一请求纯本地（断网也不影响命中）。
	before = hf.up.count()
	rec = hf.request(http.MethodGet, "/f/"+fileID, fmt.Sprintf("bytes=%d-%d", blockSize-100, 2*blockSize+500), nil)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("二次命中状态 = %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), hf.payload[blockSize-100:2*blockSize+501]) {
		t.Fatal("二次命中字节与源不一致")
	}
	if hf.up.count() != before {
		t.Fatal("全落盘后仍出网")
	}
}

// TestHubWarmRegionsShape 验证区域集抓取的请求数与精确区间（design §4）：
// ①头段单流 bytes=0-(head-1)；②尾段 bytes=(块对齐起点)-size-1；③续播点窗口。
func TestHubWarmRegionsShape(t *testing.T) {
	size := 2*blockSize + 12345
	headBytes := int64(blockSize + 100)
	tailBytes := int64(5000)
	resumeOffset := int64(size / 2)
	hf := newHubFixture(t, makePayload(size), func(c *HubConfig) {
		c.WarmHeadBytes = headBytes
		c.WarmTailBytes = tailBytes
		c.WarmResumeWindowBytes = 1000
	})

	rec := hf.warm(&warmRegionsPayload{
		HeadBytes:         headBytes,
		TailBytes:         tailBytes,
		ResumeOffsetBytes: &resumeOffset,
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/warm 状态 = %d", rec.Code)
	}
	if body := rec.Body.String(); body != `{"status":"warming"}` {
		t.Fatalf("/warm 响应体 = %s", body)
	}
	hf.waitRunDone(hf.fileID)

	reqs := hf.up.snapshot()
	if len(reqs) != 3 {
		t.Fatalf("区域集应 3 条请求（头/尾/续播点），实际 %d：%+v", len(reqs), reqs)
	}
	wantHead := fmt.Sprintf("bytes=0-%d", headBytes-1)
	if reqs[0].rangeHeader != wantHead {
		t.Fatalf("头段 Range = %q，应 %q", reqs[0].rangeHeader, wantHead)
	}
	wantTail := fmt.Sprintf("bytes=%d-%d", 2*blockSize, size-1)
	if reqs[1].rangeHeader != wantTail {
		t.Fatalf("尾段 Range = %q，应 %q", reqs[1].rangeHeader, wantTail)
	}
	wantResumeStart := int64(blockSize) // (resumeOffset-window) 对齐到块 1 起点
	wantResumeEnd := resumeOffset + (1000 - 1)
	wantResume := fmt.Sprintf("bytes=%d-%d", wantResumeStart, wantResumeEnd)
	if reqs[2].rangeHeader != wantResume {
		t.Fatalf("续播点 Range = %q，应 %q", reqs[2].rangeHeader, wantResume)
	}
	for _, r := range reqs {
		if r.method != http.MethodGet {
			t.Fatalf("预热请求方法 = %s", r.method)
		}
		if r.auth != "Bearer warm-token" {
			t.Fatalf("预热请求未带 warm 凭据：%q", r.auth)
		}
	}

	// 头段覆盖块 0、尾段覆盖最后一块（块 2）→ 两者完整落盘；
	// 续播窗口（2KB）只覆盖块 1 的一小段，凑不满 → 块 1 不落（凑满即落语义）。
	identity := fmt.Sprintf("size:%d", size)
	if !hf.cache.Has(hf.fileID, identity, int64(size), 0) {
		t.Fatal("头段后块 0 未落盘")
	}
	if !hf.cache.Has(hf.fileID, identity, int64(size), 2) {
		t.Fatal("尾段后块 2 未落盘")
	}
	if hf.cache.Has(hf.fileID, identity, int64(size), 1) {
		t.Fatal("续播窗口不该凑满块 1")
	}
}

// TestHubWarmIdempotentAndUnknownFields 验证重复 warm 均 200（master 依赖
// 200=已接受）、未知字段被容忍、resume_offset 别名兼容。
func TestHubWarmIdempotentAndUnknownFields(t *testing.T) {
	hf := newHubFixture(t, makePayload(1<<20), nil)
	if rec := hf.warm(nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("首次 warm = %d", rec.Code)
	}
	// 重复 warm（含未来新增的未知字段与 resume_offset 别名）必须仍是 200。
	alias := int64(1234)
	rec := hf.warm(&warmRegionsPayload{ResumeOffset: &alias}, map[string]any{
		"future_field": map[string]any{"k": "v"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("重复 warm = %d，应 200", rec.Code)
	}
	if body := rec.Body.String(); body != `{"status":"warming"}` {
		t.Fatalf("重复 warm 响应体 = %s", body)
	}
	// 校验解析：别名生效且 *_bytes 优先。
	spec := hf.hub.resolveRegions(&warmRegionsPayload{ResumeOffset: &alias})
	if spec.resumeOffset != 1234 {
		t.Fatalf("resume_offset 别名未生效：%d", spec.resumeOffset)
	}
	primary := int64(4321)
	spec = hf.hub.resolveRegions(&warmRegionsPayload{ResumeOffset: &alias, ResumeOffsetBytes: &primary})
	if spec.resumeOffset != 4321 {
		t.Fatalf("resume_offset_bytes 应优先：%d", spec.resumeOffset)
	}
	// 缺省回落：载荷给 0 → 用配置值。
	spec = hf.hub.resolveRegions(&warmRegionsPayload{})
	if spec.headBytes != hf.hub.cfg.WarmHeadBytes || spec.tailBytes != hf.hub.cfg.WarmTailBytes {
		t.Fatalf("区域缺省回落 = %+v", spec)
	}
}

// TestHubCancelStopsRunAndIsSticky 验证 /cancel 立即停轮、粘性停止，
// 下一次 /warm 清除。
func TestHubCancelStopsRunAndIsSticky(t *testing.T) {
	hf := newHubFixture(t, makePayload(1<<20), nil)
	gate := make(chan struct{})
	hf.up.setGate(gate)

	if rec := hf.warm(nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("warm = %d", rec.Code)
	}
	// 等第一条上游请求被"门"挡住（说明预热轮在途）。
	deadline := time.Now().Add(3 * time.Second)
	for hf.up.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hf.up.count() == 0 {
		t.Fatal("预热请求未发出")
	}

	body, _ := json.Marshal(map[string]string{"file_id": hf.fileID})
	rec := hf.request(http.MethodPost, hubCancelPath, "", body)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"stopped"}` {
		t.Fatalf("/cancel = %d %s", rec.Code, rec.Body.String())
	}
	hf.waitRunDone(hf.fileID)

	// 粘性：即使有元数据、有缺失块，maybeContinue 也不得再起轮。
	hf.cache.Observe(hf.fileID, fileMeta{size: 1 << 20})
	hf.hub.maybeContinue(hf.fileID)
	time.Sleep(20 * time.Millisecond)
	hf.hub.mu.Lock()
	st := hf.hub.states[hf.fileID]
	running := st != nil && st.run != nil
	stopped := st != nil && st.stopped
	hf.hub.mu.Unlock()
	if running {
		t.Fatal("cancel 后 maybeContinue 又起了轮")
	}
	if !stopped {
		t.Fatal("cancel 的停止位未保持")
	}

	// 下一次 warm 清除停止位并可再起轮（放行"门"，让本轮能收尾）。
	close(gate)
	if rec := hf.warm(nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("再次 warm = %d", rec.Code)
	}
}

// TestHubPlaybackTriggersFullFill 验证 3 分钟规则：warm 被接受后出现播放
// （/f/ 请求即播放判据）→ 取消计时、转全量续取，最终整文件落盘。
func TestHubPlaybackTriggersFullFill(t *testing.T) {
	payload := makePayload(300000) // < blockSize：单块文件，便于断言"整文件落盘"
	hf := newHubFixture(t, payload, func(c *HubConfig) {
		c.WarmHeadBytes = 4096
		c.WarmTailBytes = 1024
		c.WarmWait = 3 * time.Second // 靠播放立即触发，而不是靠超时
	})
	if rec := hf.warm(nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("warm = %d", rec.Code)
	}
	// 播放：一次 /f/ 请求（未命中透传），随后后台应转全量续取。
	rec := hf.request(http.MethodGet, "/f/"+hf.fileID, "bytes=0-1023", nil)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("播放请求状态 = %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload[:1024]) {
		t.Fatal("播放请求字节与源不一致")
	}

	identity := fmt.Sprintf("size:%d", len(payload))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hf.cache.Has(hf.fileID, identity, int64(len(payload)), 0) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !hf.cache.Has(hf.fileID, identity, int64(len(payload)), 0) {
		t.Fatal("播放后全量续取未完成")
	}
	// 后续播放请求纯本地（带 Range 的 GET 才走缓存三态）。
	before := hf.up.count()
	rec = hf.request(http.MethodGet, "/f/"+hf.fileID, "bytes=0-1023", nil)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("全量后请求状态 = %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload[:1024]) {
		t.Fatal("全量后本地服务字节与源不一致")
	}
	if hf.up.count() != before {
		t.Fatal("全量后仍出网")
	}
}

// TestHubUpstream401Refresh 验证直链过期（401）时经 master 换链重试一次。
func TestHubUpstream401Refresh(t *testing.T) {
	payload := makePayload(50000)
	hf := newHubFixture(t, payload, nil)
	hf.installLink()
	hf.up.pushStatus(http.StatusUnauthorized)

	rec := hf.request(http.MethodGet, "/f/"+hf.fileID, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("换链后状态 = %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatal("换链后字节与源不一致")
	}
	if calls := hf.masterS.callSnapshot(); len(calls) != 1 || calls[0] != "tok-"+hf.fileID {
		t.Fatalf("master 换链调用 = %v，应以 file_token 为键", calls)
	}
	reqs := hf.up.snapshot()
	if len(reqs) != 2 {
		t.Fatalf("上游请求数 = %d，应 401 + 重试 = 2", len(reqs))
	}
	if reqs[1].auth != "Bearer refreshed" {
		t.Fatalf("重试未用新链凭据：%q", reqs[1].auth)
	}
}

// TestHubFillerAlignmentAndIdentityGuard 单测填充器：非块边界起点丢弃前导、
// 只落完整块；身份在途变化立即停手。
func TestHubFillerAlignmentAndIdentityGuard(t *testing.T) {
	cache := testDiskCache(t, 1<<30, 0, time.Now)
	fileID := "filler-file"
	size := int64(3 * blockSize)
	cache.Observe(fileID, fileMeta{etag: "A", size: size})

	// 起点 = 块 1 内偏移 100：流首的 blockSize-100 字节是块 1 的残缺尾部，
	// 必须丢弃；从下一个块边界（块 2）开始整块收集。
	filler := newHubFiller(cache, discardLogger(), fileID, "A", size, blockSize+100)
	if filler.idx != 2 || filler.skip != blockSize-100 {
		t.Fatalf("填充器初始化 idx=%d skip=%d", filler.idx, filler.skip)
	}
	_, _ = filler.Write(makeBlock(11, blockSize))
	if filler.blocks != 0 {
		t.Fatalf("非块边界起点不该先落块（blocks=%d）", filler.blocks)
	}
	// 跳过 blockSize-100 后，还需收满一整块（blockSize）才凑满：第一段留下 100，
	// 第二段补 blockSize-100。
	_, _ = filler.Write(makeBlock(12, blockSize-100))
	if filler.blocks != 1 {
		t.Fatalf("补齐后应落 1 块（blocks=%d）", filler.blocks)
	}
	if !cache.Has(fileID, "A", size, 2) || cache.Has(fileID, "A", size, 1) {
		t.Fatal("落盘块号与预期不符（应只落块 2）")
	}

	// 身份在途变化：Observe 换身份清块，填充器再写必须停手不落。
	cache.Observe(fileID, fileMeta{etag: "B", size: size})
	filler2 := newHubFiller(cache, discardLogger(), fileID, "A", size, 0)
	_, _ = filler2.Write(makeBlock(21, blockSize))
	if filler2.blocks != 0 || !filler2.failed {
		t.Fatalf("身份变化后填充器未停手：blocks=%d failed=%v", filler2.blocks, filler2.failed)
	}
	if cache.Has(fileID, "A", size, 0) {
		t.Fatal("身份变化后旧块不该在缓存里")
	}

	// Write 永不报错（透传主线不受落盘影响）。
	n, err := filler2.Write(makeBlock(22, 100))
	if err != nil || n != 100 {
		t.Fatalf("Write 返回 (%d,%v)，应 (100,nil)", n, err)
	}
}

// TestHubWarmCancelValidationAndRouting 覆盖控制面的参数校验与路由边界。
func TestHubWarmCancelValidationAndRouting(t *testing.T) {
	hf := newHubFixture(t, makePayload(1024), nil)

	if rec := hf.request(http.MethodGet, hubWarmPath, "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /warm = %d，应 405", rec.Code)
	}
	if rec := hf.request(http.MethodPost, hubWarmPath, "", []byte("{not json")); rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 JSON = %d，应 400", rec.Code)
	}
	bad := map[string]any{"file_id": "a/b", "direct_link": "http://x/y"}
	body, _ := json.Marshal(bad)
	if rec := hf.request(http.MethodPost, hubWarmPath, "", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("file_id 含斜杠 = %d，应 400", rec.Code)
	}
	bad = map[string]any{"file_id": "ok", "direct_link": ""}
	body, _ = json.Marshal(bad)
	if rec := hf.request(http.MethodPost, hubWarmPath, "", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("空直链 = %d，应 400", rec.Code)
	}
	if rec := hf.request(http.MethodGet, hubCancelPath, "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /cancel = %d，应 405", rec.Code)
	}
	if rec := hf.request(http.MethodPost, hubCancelPath, "", []byte(`{"file_id":""}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("空 file_id cancel = %d，应 400", rec.Code)
	}
	if rec := hf.request(http.MethodGet, "/nope", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("未知路径 = %d，应 404", rec.Code)
	}
	// 幂等 cancel：没有该文件的状态也回 200。
	if rec := hf.request(http.MethodPost, hubCancelPath, "", []byte(`{"file_id":"ghost"}`)); rec.Code != http.StatusOK {
		t.Fatalf("未知文件 cancel = %d，应 200", rec.Code)
	}
}

// TestHubHeadPassthroughNoTee HEAD 透传不写盘、无响应体。
func TestHubHeadPassthroughNoTee(t *testing.T) {
	payload := makePayload(12345)
	hf := newHubFixture(t, payload, nil)
	hf.installLink()
	rec := hf.request(http.MethodHead, "/f/"+hf.fileID, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD 状态 = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD 响应体 = %d 字节", rec.Body.Len())
	}
	if n := hf.up.count(); n != 1 && n != 0 {
		t.Fatalf("HEAD 上游请求数 = %d", n)
	}
	if hf.cache.Has(hf.fileID, fmt.Sprintf("size:%d", len(payload)), int64(len(payload)), 0) {
		t.Fatal("HEAD 不应写入缓存")
	}
}

// TestHubLocalServeWithoutLink 重启后可复用：磁盘里块齐全但 warm 状态为空
// （没有直链）时，全命中的 Range 请求必须仍走纯本地 206——serveFile 的判定
// 顺序是"先缓存三态、未命中才因无直链 409"；别把 409 提前到缓存判定之前，
// 那会砸掉重启后（以及 warm 状态被清后）的全部磁盘缓存收益。
func TestHubLocalServeWithoutLink(t *testing.T) {
	payload := makePayload(int(2*blockSize) + 100)
	size := int64(len(payload))
	hf := newHubFixture(t, payload, nil)
	// 预置块 0（模拟上一轮运行留下的磁盘缓存），不装任何 warm 状态。
	identity := fmt.Sprintf("size:%d", size)
	if !hf.cache.Put(hf.fileID, identity, 0, payload[:blockSize]) {
		t.Fatal("预置块 0 失败")
	}
	if _, ok := hf.hub.stateLink(hf.fileID); ok {
		t.Fatal("前置条件：不应存在直链状态")
	}

	// ① 块内全命中：纯本地 206，零上游请求。
	rec := hf.request(http.MethodGet, "/f/"+hf.fileID, "bytes=100-199", nil)
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), payload[100:200]) {
		t.Fatalf("全命中 = %d（%d 字节），应 206 本地供流", rec.Code, rec.Body.Len())
	}
	if n := hf.up.count(); n != 0 {
		t.Fatalf("全命中不应出网，上游请求数 = %d", n)
	}

	// ② 部分命中 + 无直链：明确 409 not_warmed（等 master 重新 warm 补齐），
	// 不给出"半本地半无源"的响应。
	rec = hf.request(http.MethodGet, "/f/"+hf.fileID, fmt.Sprintf("bytes=%d-%d", blockSize-10, blockSize+10), nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("部分命中且无直链 = %d，应 409", rec.Code)
	}
	if n := hf.up.count(); n != 0 {
		t.Fatalf("409 路径不应出网，上游请求数 = %d", n)
	}
}

// TestHubWarmRegionMisalignedStartDropped 钉住区域集流的块对齐闸门：上游 206
// 的 Content-Range 起点不在块边界上时整条流弃用（Observe 之前就挡下，一个块
// 都不落）——按"尽力填块"继续会把这类流当正常续取，字节一致性失去锚点。
// 正常 200 整文件（起点 0）与块对齐的 206 不受影响（其余用例已覆盖）。
func TestHubWarmRegionMisalignedStartDropped(t *testing.T) {
	const fileSize = 3 * blockSize
	payload := makePayload(fileSize)
	headBytes := int64(fileSize)
	buf := &lockedLogBuffer{}
	hf := newHubFixture(t, payload, func(c *HubConfig) {
		c.WarmHeadBytes = headBytes
		c.WarmTailBytes = 0
		c.Logger = newInfoLogger(buf)
	})
	// 假上游对流首请求回一条"起点错位"的 206：Content-Range 声明从 1000 开始，
	// 字节本身与其自洽（body 即 payload[1000:]）。若闸门失效，填充器会丢掉
	// 前导、从块 1 起把 4MiB~8MiB、8MiB~12MiB 收成块 1/块 2。
	hf.up.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Range") != fmt.Sprintf("bytes=0-%d", fileSize-1) {
			return false
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 1000-%d/%d", fileSize-1, fileSize))
		w.Header().Set("Content-Length", strconv.Itoa(fileSize-1000))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[1000:])
		return true
	})

	if rec := hf.warm(&warmRegionsPayload{HeadBytes: headBytes}, nil); rec.Code != http.StatusOK {
		t.Fatalf("/warm 状态 = %d", rec.Code)
	}
	hf.waitRunDone(hf.fileID)

	// 闸门在 Observe 之前生效：总大小未知 → 尾段/续播点不再发；错位流不得落块。
	if reqs := hf.up.snapshot(); len(reqs) != 1 {
		t.Fatalf("错位流弃用后应只发过头段请求，实际 %d 条：%+v", len(reqs), reqs)
	}
	identity := fmt.Sprintf("size:%d", fileSize)
	for _, idx := range []int64{0, 1, 2} {
		if hf.cache.Has(hf.fileID, identity, fileSize, idx) {
			t.Fatalf("错位起点不应落任何块，块 %d 却已落盘", idx)
		}
	}
	waitLogContains(t, buf, "起点不在块边界上")
}

// TestHubWarmRegion401RefreshAndResume 验证区域集流上游 401 的恢复：以 /warm
// 载荷里的 file_token 为键走 master 的 download-link 通道换链，新凭据重试一次，
// 流继续按原区间抓取并落块（与透传路径同一语义）。
func TestHubWarmRegion401RefreshAndResume(t *testing.T) {
	payload := makePayload(2 * blockSize)
	hf := newHubFixture(t, payload, func(c *HubConfig) {
		c.WarmHeadBytes = blockSize
		c.WarmTailBytes = 0
	})
	hf.up.pushStatus(http.StatusUnauthorized) // 第一条上游请求（头段）先吃一个 401

	if rec := hf.warm(nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("/warm 状态 = %d", rec.Code)
	}
	hf.waitRunDone(hf.fileID)

	if calls := hf.masterS.callSnapshot(); len(calls) != 1 || calls[0] != "tok-"+hf.fileID {
		t.Fatalf("master 换链调用 = %v，应以 file_token 为键", calls)
	}
	wantRange := fmt.Sprintf("bytes=0-%d", blockSize-1)
	reqs := hf.up.snapshot()
	if len(reqs) != 2 {
		t.Fatalf("区域集应 401 + 重试 = 2 条请求，实际 %d：%+v", len(reqs), reqs)
	}
	if reqs[0].auth != "Bearer warm-token" || reqs[1].auth != "Bearer refreshed" {
		t.Fatalf("凭据流不符：first=%q retry=%q", reqs[0].auth, reqs[1].auth)
	}
	if reqs[0].rangeHeader != wantRange || reqs[1].rangeHeader != wantRange {
		t.Fatalf("重试应保持原区间 %q：got %q / %q", wantRange, reqs[0].rangeHeader, reqs[1].rangeHeader)
	}
	identity := fmt.Sprintf("size:%d", len(payload))
	if !hf.cache.Has(hf.fileID, identity, int64(len(payload)), 0) {
		t.Fatal("换链重试后头段块 0 未落盘")
	}
	if hf.cache.Has(hf.fileID, identity, int64(len(payload)), 1) {
		t.Fatal("尾段已禁用，块 1 不该落盘")
	}
}

// TestHubWarmWatchdogPausedDuringYield 钉住"让路"与无进展看门狗的边界：客户端
// 在途（播放中）令填充流暂停读取时，看门狗必须停表——否则播放一旦长于
// StallTimeout，恢复读取时读到的是被掐断的流（重发请求 + 误报"读取失败"/
// "提前结束"）。用一条慢速滴流的头段流把时间线拉开：客户端在流中途登记，
// 让路发生在看门狗计时窗口内。
func TestHubWarmWatchdogPausedDuringYield(t *testing.T) {
	payload := makePayload(2 * blockSize)
	buf := &lockedLogBuffer{}
	hf := newHubFixture(t, payload, func(c *HubConfig) {
		c.WarmHeadBytes = blockSize
		c.WarmTailBytes = 0
		c.WarmWait = 5 * time.Second // 播放判据由 clientBegin 直接给出，不受窗口影响
		c.StallTimeout = 1500 * time.Millisecond
		c.Logger = newInfoLogger(buf)
	})

	const chunk = 128 << 10 // 32 段滴完 4MiB 头窗口
	started := make(chan struct{})
	var once sync.Once
	hf.up.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Range") != fmt.Sprintf("bytes=0-%d", blockSize-1) {
			return false // 全量续取的块 1 请求走标准形态
		}
		hdr := w.Header()
		hdr.Set("Content-Type", "video/mp4")
		hdr.Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", blockSize-1, len(payload)))
		hdr.Set("Content-Length", strconv.Itoa(blockSize))
		w.WriteHeader(http.StatusPartialContent)
		for written := 0; written < blockSize; {
			n := chunk
			if blockSize-written < n {
				n = blockSize - written
			}
			if _, err := w.Write(payload[written : written+n]); err != nil {
				return true
			}
			_ = http.NewResponseController(w).Flush()
			written += n
			if written == chunk {
				once.Do(func() { close(started) }) // 流已开始：测试据此登记"播放中"
				time.Sleep(400 * time.Millisecond)
			} else {
				time.Sleep(2 * time.Millisecond)
			}
		}
		return true
	})

	if rec := hf.warm(nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("/warm 状态 = %d", rec.Code)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("慢速滴流头段未在超时内开始")
	}
	// 白盒登记客户端（与 installLink 同一取舍）：播放让路判据只看在途客户端，
	// 走真实 /f/ 请求会额外引入一条透传请求，反而干扰对应关系。
	hf.hub.clientBegin(hf.fileID)
	waitLogContains(t, buf, "hub 预热让路")

	// 播放时长超过 StallTimeout：停表生效时流原样恢复；未停表则被看门狗掐断。
	time.Sleep(2 * hf.hub.cfg.StallTimeout)
	hf.hub.clientEnd(hf.fileID)
	hf.waitRunDone(hf.fileID)

	logs := buf.String()
	if strings.Contains(logs, "上游流读取失败") || strings.Contains(logs, "上游流提前结束") {
		t.Fatalf("让路等待期间看门狗误掐在途流：\n%s", logs)
	}
	// 头段流原样滴完（块 0），全量续取只补块 1——不是被掐断后的整段重发。
	reqs := hf.up.snapshot()
	if len(reqs) != 2 {
		t.Fatalf("上游请求数 = %d，应 2（头段 + 块 1 续取）：%+v", len(reqs), reqs)
	}
	if want := fmt.Sprintf("bytes=0-%d", blockSize-1); reqs[0].rangeHeader != want {
		t.Fatalf("头段区间 = %q，应 %q", reqs[0].rangeHeader, want)
	}
	if want := fmt.Sprintf("bytes=%d-%d", blockSize, 2*blockSize-1); reqs[1].rangeHeader != want {
		t.Fatalf("全量续取区间 = %q，应 %q（从块 1 续）", reqs[1].rangeHeader, want)
	}
	identity := fmt.Sprintf("size:%d", len(payload))
	for _, idx := range []int64{0, 1} {
		if !hf.cache.Has(hf.fileID, identity, int64(len(payload)), idx) {
			t.Fatalf("块 %d 未落盘（头段流应完整续跑）", idx)
		}
	}
}
