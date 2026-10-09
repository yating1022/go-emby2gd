package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- 测试脚手架 -------------------------------------------------------------

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testContent(n int) []byte {
	content := make([]byte, n)
	for i := range content {
		content[i] = byte(i % 251)
	}
	return content
}

// stack 是一套完整的 agent 数据面：fake master + LinkSource + Handler + HTTP 服务。
type stack struct {
	master   *fakeMaster
	source   *LinkSource
	handler  *Handler
	agentURL string
	signKey  []byte
}

// newStack 起一套数据面；linkURL 是 master 下发的直链（通常指向 mock Google）。
func newStack(t *testing.T, linkURL string, mutate func(*Config)) *stack {
	t.Helper()
	signKey := testSignKey(t)
	master := newFakeMaster(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		_, _ = w.Write([]byte(linkJSON(linkURL, time.Hour)))
	})
	source := newTestLinkSource(t, master.srv.URL, nil)
	cfg := Config{
		SignKey:       signKey,
		MaxConcurrent: 8,
		Links:         source,
		Logger:        discardLogger(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	handler := NewHandler(cfg)
	agent := httptest.NewServer(handler)
	t.Cleanup(agent.Close)
	return &stack{master: master, source: source, handler: handler, agentURL: agent.URL, signKey: signKey}
}

// url 生成合法的签名 URL。
func (s *stack) url(fileID string, expires time.Time) string {
	e := strconv.FormatInt(expires.Unix(), 10)
	return fmt.Sprintf("%s/dl/%s?e=%s&s=%s", s.agentURL, fileID, e, Sign(s.signKey, fileID, e))
}

func mustGet(t *testing.T, url string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	for k, values := range header {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体失败：%v", err)
	}
	return body
}

// --- 正常路径 ---------------------------------------------------------------

func TestProxyFullDownload(t *testing.T) {
	content := testContent(64 * 1024)
	var upstreamAuth atomic.Value
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth.Store(r.Header.Get("Authorization"))
		http.ServeContent(w, r, "file.bin", time.Unix(1700000000, 0), bytes.NewReader(content))
	}))
	defer google.Close()

	st := newStack(t, google.URL+"/gdrive/file-1", nil)
	resp := mustGet(t, st.url("file-1", time.Now().Add(time.Hour)), nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", resp.StatusCode)
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("字节数不符：得到 %d，期望 %d", len(body), len(content))
	}
	// 上游必须拿到 master 下发的凭据头（本地 mock 全链路的关键断言）。
	if got := upstreamAuth.Load(); got != "Bearer google-token" {
		t.Fatalf("上游未收到凭据头：%v", got)
	}
	// 客户端响应绝不包含 Google 凭据。
	if raw := strings.Join(resp.Header.Values("Authorization"), ","); raw != "" {
		t.Fatalf("客户端响应不应带 Authorization：%q", raw)
	}
	if got := st.master.requests.Load(); got != 1 {
		t.Fatalf("应只拉一次直链，实际 %d 次", got)
	}
	if st.handler.ActiveStreams() != 0 {
		t.Fatalf("请求结束后活跃流应为 0，实际 %d", st.handler.ActiveStreams())
	}
}

func TestProxyRangePassthrough(t *testing.T) {
	content := testContent(4096)
	var upstreamRange atomic.Value
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRange.Store(r.Header.Get("Range"))
		http.ServeContent(w, r, "file.bin", time.Unix(1700000000, 0), bytes.NewReader(content))
	}))
	defer google.Close()

	st := newStack(t, google.URL+"/gdrive/file-1", nil)
	header := http.Header{"Range": []string{"bytes=100-199"}}
	resp := mustGet(t, st.url("file-1", time.Now().Add(time.Hour)), header)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range 请求应回 206，实际 %d", resp.StatusCode)
	}
	if upstreamRange.Load() != "bytes=100-199" {
		t.Fatalf("Range 未透传给上游：%v", upstreamRange.Load())
	}
	if !bytes.Equal(body, content[100:200]) {
		t.Fatalf("分片字节不符：得到 %d 字节", len(body))
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 100-199/4096" {
		t.Fatalf("Content-Range 未透传：%q", got)
	}
	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges 未透传：%q", got)
	}
	if got := resp.Header.Get("Content-Length"); got != "100" {
		t.Fatalf("Content-Length 未透传：%q", got)
	}
}

func TestProxyRangeNotSatisfiablePassthrough(t *testing.T) {
	content := testContent(1024)
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Unix(1700000000, 0), bytes.NewReader(content))
	}))
	defer google.Close()

	st := newStack(t, google.URL+"/gdrive/file-1", nil)
	resp := mustGet(t, st.url("file-1", time.Now().Add(time.Hour)), http.Header{"Range": []string{"bytes=99999-"}})
	_ = readBody(t, resp)
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("越界 Range 应透传 416，实际 %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Range") == "" {
		t.Fatal("416 应带 Content-Range 说明文件长度")
	}
}

func TestProxyHead(t *testing.T) {
	content := testContent(2048)
	var upstreamMethod atomic.Value
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamMethod.Store(r.Method)
		http.ServeContent(w, r, "file.bin", time.Unix(1700000000, 0), bytes.NewReader(content))
	}))
	defer google.Close()

	st := newStack(t, google.URL+"/gdrive/file-1", nil)
	req, err := http.NewRequest(http.MethodHead, st.url("file-1", time.Now().Add(time.Hour)), nil)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD 应回 200，实际 %d", resp.StatusCode)
	}
	if upstreamMethod.Load() != http.MethodHead {
		t.Fatalf("上游应收到 HEAD，实际 %v", upstreamMethod.Load())
	}
	if len(body) != 0 {
		t.Fatalf("HEAD 不应有响应体，实际 %d 字节", len(body))
	}
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(content)) {
		t.Fatalf("HEAD 应透传 Content-Length，实际 %q", got)
	}
}

// --- 防开放代理 -------------------------------------------------------------

func TestProxyRejectsBadSignature(t *testing.T) {
	var upstreamHits atomic.Int64
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		_, _ = w.Write([]byte("secret-bytes"))
	}))
	defer google.Close()
	st := newStack(t, google.URL+"/gdrive/file-1", nil)

	valid := st.url("file-1", time.Now().Add(time.Hour))
	expired := st.url("file-1", time.Now().Add(-time.Minute))
	tampered := strings.TrimSuffix(valid, valid[len(valid)-1:]) + "0"
	if tampered == valid {
		tampered = valid[:len(valid)-1] + "1"
	}
	caseList := []struct {
		name string
		url  string
	}{
		{"缺 s", st.agentURL + "/dl/file-1?e=" + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)},
		{"缺 e", strings.Split(valid, "?")[0]},
		{"已过期", expired},
		{"签名被篡改", tampered},
	}

	var bodies []string
	for _, tc := range caseList {
		resp := mustGet(t, tc.url, nil)
		body := string(readBody(t, resp))
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s：应回 403，实际 %d", tc.name, resp.StatusCode)
		}
		bodies = append(bodies, body)
	}
	// 统一文案：不泄露失败原因（冻结稿 §2.5）。
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Fatalf("403 文案必须逐字一致：\n%s\nvs\n%s", bodies[0], bodies[i])
		}
	}
	if !strings.Contains(bodies[0], "链接无效或已过期") {
		t.Fatalf("403 文案应为中文统一提示：%s", bodies[0])
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("签名失败的请求不得打到上游，实际 %d 次", upstreamHits.Load())
	}
	if st.master.requests.Load() != 0 {
		t.Fatalf("签名失败的请求不得向 master 拉直链，实际 %d 次", st.master.requests.Load())
	}
}

func TestProxyRejectsOtherMethodsAndPaths(t *testing.T) {
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer google.Close()
	st := newStack(t, google.URL+"/gdrive/file-1", nil)

	req, err := http.NewRequest(http.MethodPost, st.url("file-1", time.Now().Add(time.Hour)), nil)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	_ = readBody(t, resp)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST 应回 405，实际 %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Allow"), "GET") || !strings.Contains(resp.Header.Get("Allow"), "HEAD") {
		t.Fatalf("405 应带 Allow 头，实际 %q", resp.Header.Get("Allow"))
	}

	notFound := mustGet(t, st.agentURL+"/other/path", nil)
	_ = readBody(t, notFound)
	if notFound.StatusCode != http.StatusNotFound {
		t.Fatalf("未知路径应回 404，实际 %d", notFound.StatusCode)
	}

	empty := mustGet(t, st.agentURL+"/dl/", nil)
	_ = readBody(t, empty)
	if empty.StatusCode != http.StatusForbidden {
		t.Fatalf("缺 file_id 应回 403，实际 %d", empty.StatusCode)
	}
}

// --- 并发闸门 ---------------------------------------------------------------

func TestProxyConcurrencyGateReturns503(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		_, _ = w.Write([]byte("late-bytes"))
	}))
	defer google.Close()

	st := newStack(t, google.URL+"/gdrive/file-1", func(cfg *Config) { cfg.MaxConcurrent = 1 })
	url := st.url("file-1", time.Now().Add(time.Hour))

	firstDone := make(chan int, 1)
	go func() {
		resp := mustGet(t, url, nil)
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		firstDone <- resp.StatusCode
	}()
	<-entered

	second := mustGet(t, url, nil)
	secondBody := readBody(t, second)
	if second.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("并发满应回 503，实际 %d", second.StatusCode)
	}
	if got := second.Header.Get("Retry-After"); got != "5" {
		t.Fatalf("503 应带 Retry-After: 5，实际 %q", got)
	}
	if !strings.Contains(string(secondBody), "并发") {
		t.Fatalf("503 文案应为中文提示：%s", secondBody)
	}
	if st.handler.ActiveStreams() != 1 {
		t.Fatalf("第一个请求仍应在途，活跃流应为 1，实际 %d", st.handler.ActiveStreams())
	}

	close(release)
	if status := <-firstDone; status != http.StatusOK {
		t.Fatalf("第一个请求应正常完成，实际 %d", status)
	}
	// 活跃流计数最终归零（心跳要用它上报 active_streams）。
	deadline := time.Now().Add(2 * time.Second)
	for st.handler.ActiveStreams() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if st.handler.ActiveStreams() != 0 {
		t.Fatalf("活跃流计数未归零：%d", st.handler.ActiveStreams())
	}
}

// --- 直链获取失败与上游错误 --------------------------------------------------

func TestProxyLinkUnavailableIs502WithChineseReason(t *testing.T) {
	var upstreamHits atomic.Int64
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
	}))
	defer google.Close()

	signKey := testSignKey(t)
	master := newFakeMaster(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"GD_QUOTA_EXCEEDED","message":"Google 配额已用尽，请稍后重试"}}`))
	})
	handler := NewHandler(Config{
		SignKey:       signKey,
		MaxConcurrent: 4,
		Links:         newTestLinkSource(t, master.srv.URL, nil),
		Logger:        discardLogger(),
	})
	agent := httptest.NewServer(handler)
	defer agent.Close()

	e := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	resp := mustGet(t, fmt.Sprintf("%s/dl/file-1?e=%s&s=%s", agent.URL, e, Sign(signKey, "file-1", e)), nil)
	body := string(readBody(t, resp))

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("拿不到直链应回 502，实际 %d", resp.StatusCode)
	}
	if !strings.Contains(body, "Google 配额已用尽") {
		t.Fatalf("应透传 master 的中文原因：%s", body)
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("没有直链时不应请求上游")
	}
}

func TestProxyUpstreamErrorIsPassthroughAndRefetchesOnce(t *testing.T) {
	var googleHits atomic.Int64
	content := testContent(128)
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 前两次（首次 + 重试）回 403 模拟直链失效；第三次起正常工作。
		if googleHits.Add(1) <= 2 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"fileNotDownloadable"}}`))
			return
		}
		http.ServeContent(w, r, "file.bin", time.Unix(1700000000, 0), bytes.NewReader(content))
	}))
	defer google.Close()

	st := newStack(t, google.URL+"/gdrive/file-1", nil)
	resp := mustGet(t, st.url("file-1", time.Now().Add(time.Hour)), nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Google 的错误应原样透传（403），实际 %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "fileNotDownloadable") {
		t.Fatalf("响应体应原样透传：%s", body)
	}
	if googleHits.Load() != 2 {
		t.Fatalf("401/403 后应重拉直链并重试一次，上游应被命中 2 次，实际 %d", googleHits.Load())
	}
	if got := st.master.requests.Load(); got != 2 {
		t.Fatalf("重试前应重新拉一次直链，实际 %d 次", got)
	}

	// 第二次客户端请求：刷新过的直链已在缓存，不应再拉直链，且这次成功。
	resp2 := mustGet(t, st.url("file-1", time.Now().Add(time.Hour)), nil)
	body2 := readBody(t, resp2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("第二次请求应成功，实际 %d", resp2.StatusCode)
	}
	if !bytes.Equal(body2, content) {
		t.Fatal("第二次请求字节不符")
	}
	if got := st.master.requests.Load(); got != 2 {
		t.Fatalf("刷新结果应进缓存，实际累计 %d 次拉取", got)
	}
}

// --- 302 跟随与转发语义 ------------------------------------------------------

// hostMappedClient 让假域名（gdrive.test / cdn.test）指向本地 httptest 服务，
// 这样才能真正验证"跨域重定向会剥离 Authorization"（同域不会剥离）。
func hostMappedClient(t *testing.T, mapping map[string]string) *http.Client {
	t.Helper()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				target, ok := mapping[host]
				if !ok {
					return nil, fmt.Errorf("测试未映射的主机：%s", host)
				}
				return dialer.DialContext(ctx, network, target)
			},
		},
	}
}

func TestProxyFollowsCrossDomainRedirectWithoutAuthorization(t *testing.T) {
	payload := []byte("bytes-from-the-signed-redirect-target")
	var secondHopAuth atomic.Value
	secondHopAuth.Store("<未到达>")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHopAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload)
	}))
	defer cdn.Close()

	var firstHopAuth atomic.Value
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHopAuth.Store(r.Header.Get("Authorization"))
		http.Redirect(w, r, "http://cdn.test/blob?signature=xyz", http.StatusFound)
	}))
	defer google.Close()

	client := hostMappedClient(t, map[string]string{
		"gdrive.test": strings.TrimPrefix(google.URL, "http://"),
		"cdn.test":    strings.TrimPrefix(cdn.URL, "http://"),
	})
	st := newStack(t, "http://gdrive.test/drive/v3/files/file-1?alt=media", func(cfg *Config) {
		cfg.Client = client
	})

	resp := mustGet(t, st.url("file-1", time.Now().Add(time.Hour)), nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应跟随 302 并回 200，实际 %d", resp.StatusCode)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("重定向目标的字节应透传：%q", body)
	}
	if got := firstHopAuth.Load(); got != "Bearer google-token" {
		t.Fatalf("第一跳应带凭据头（否则 Google 不会给 302）：%v", got)
	}
	if got := secondHopAuth.Load(); got != "" {
		t.Fatalf("跨域重定向不得携带 Authorization（Go 默认行为，不许手动加回），实际 %q", got)
	}
}

func TestProxyClientDisconnectCancelsUpstream(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("测试服务器不支持 Flush")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("0123456789"))
		flusher.Flush()
		select {
		case <-r.Context().Done():
			close(upstreamCanceled)
		case <-time.After(10 * time.Second):
			t.Error("上游等待超时：客户端断开后没有取消上游请求")
		}
	}))
	defer google.Close()

	st := newStack(t, google.URL+"/gdrive/file-1", nil)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, st.url("file-1", time.Now().Add(time.Hour)), nil)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	buf := make([]byte, 10)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("读取流前段失败：%v", err)
	}
	cancel()
	_ = resp.Body.Close()

	select {
	case <-upstreamCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("客户端断开后上游请求未被取消")
	}
}

// --- 链接缓存与 master 交互 ---------------------------------------------------

func TestProxyCachesLinkAcrossRequests(t *testing.T) {
	content := testContent(256)
	var googleHits atomic.Int64
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		googleHits.Add(1)
		http.ServeContent(w, r, "file.bin", time.Unix(1700000000, 0), bytes.NewReader(content))
	}))
	defer google.Close()

	st := newStack(t, google.URL+"/gdrive/file-1", nil)
	url := st.url("file-1", time.Now().Add(time.Hour))
	for i := 0; i < 3; i++ {
		resp := mustGet(t, url, nil)
		_ = readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 次请求状态码 %d", i+1, resp.StatusCode)
		}
	}
	if got := st.master.requests.Load(); got != 1 {
		t.Fatalf("三次下载应只拉一次直链，实际 %d 次", got)
	}
	if got := googleHits.Load(); got != 3 {
		t.Fatalf("上游应被命中 3 次，实际 %d", got)
	}
}

func TestProxyUpstreamTransportFailureIs502(t *testing.T) {
	// 指向一个已关闭的端口：模拟 Google 侧连不上。
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	st := newStack(t, deadURL+"/gdrive/file-1", nil)
	resp := mustGet(t, st.url("file-1", time.Now().Add(time.Hour)), nil)
	body := string(readBody(t, resp))

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("上游连不上应回 502，实际 %d", resp.StatusCode)
	}
	if !strings.Contains(body, "连接上游失败") {
		t.Fatalf("应是中文原因：%s", body)
	}
}
