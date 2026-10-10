package proxy

// F1（v0.4.1）：上游"连接级失败"（dial / connection refused / 网络不可达等
// **无任何 HTTP 响应**的错误）→ 复用既有 Refresh 语义换链一次 + 重试一次；
// 超时不触发（避免双倍等待）；日志不打印完整上游 URL（签名/密钥不进日志）。
//
// 调用链证据取"每个地址实际发生的拨号次数"：旧链地址恰好 1 次（绝不原样重试
// 死地址）、新链地址恰好 1 次（唯一一次重试），master 恰好 2 次（拉链 + 换链）。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// --- 分类：什么算"连接级失败" ------------------------------------------------

// timeoutNetErr 是 Timeout()=true 的最小 net.Error（模拟 ResponseHeaderTimeout）。
type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "i/o timeout" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return true }

func TestIsConnectionError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"空错误", nil, false},
		{"拨号被拒（完整包裹链）", &net.OpError{Op: "dial", Net: "tcp",
			Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, true},
		{"网络不可达", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ENETUNREACH}, true},
		{"主机不可达", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EHOSTUNREACH}, true},
		{"DNS 解析失败（拨号阶段）", &net.OpError{Op: "dial", Net: "tcp",
			Err: &net.DNSError{Err: "no such host", Name: "hub.invalid"}}, true},
		{"报文往返前连接被重置", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, true},
		{"响应头超时（net.Error.Timeout）", &net.OpError{Op: "read", Net: "tcp", Err: timeoutNetErr{}}, false},
		{"拨号超时", &net.OpError{Op: "dial", Net: "tcp", Err: timeoutNetErr{}}, false},
		{"客户端取消", context.Canceled, false},
		{"上下文超时", context.DeadlineExceeded, false},
		{"普通错误", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isConnectionError(tc.err); got != tc.want {
				t.Errorf("isConnectionError(%v) = %v，期望 %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestUpstreamErrorTextDropsURL(t *testing.T) {
	err := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	wrapped := &url.Error{Op: "Get", URL: "http://hub.example:8791/f/secret-token?x=1", Err: err}
	got := upstreamErrorText(wrapped)
	if strings.Contains(got, "http://") || strings.Contains(got, "secret-token") {
		t.Fatalf("日志文本不应包含完整 URL：%q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Fatalf("底层原因应保留：%q", got)
	}
}

// --- 脚手架 -------------------------------------------------------------------

// countingDialer 统计每个地址实际发生的拨号次数（F1 的请求计数证据）。
type countingDialer struct {
	mu     sync.Mutex
	counts map[string]int
}

func newCountingDialer() *countingDialer { return &countingDialer{counts: map[string]int{}} }

func (d *countingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.counts[addr]++
	d.mu.Unlock()
	return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
}

func (d *countingDialer) count(addr string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.counts[addr]
}

func (d *countingDialer) total() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	total := 0
	for _, n := range d.counts {
		total += n
	}
	return total
}

func addrOf(rawURL string) string { return strings.TrimPrefix(rawURL, "http://") }

// closedPortURL 返回一个"已关闭端口"的 URL：连它必然 connection refused
// （模拟 hub 停机后 master 先前下发、还留在节点链接缓存里的旧链）。
func closedPortURL(t *testing.T) string {
	t.Helper()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	return deadURL
}

func signedURLFor(signKey []byte, agentURL, fileID string) string {
	e := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	return fmt.Sprintf("%s/dl/%s?e=%s&s=%s", agentURL, fileID, e, Sign(signKey, fileID, e))
}

// --- 透传路径 ------------------------------------------------------------------

// TestProxyRetriesOnceOnConnectionRefused：旧链指向死地址 → 换链一次 → 重试一次
// 成功；客户端拿到完整响应；每个地址的拨号计数与 master 请求数精确。
func TestProxyRetriesOnceOnConnectionRefused(t *testing.T) {
	content := testContent(64 * 1024)
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Unix(1700000000, 0), bytes.NewReader(content))
	}))
	defer google.Close()

	deadURL := closedPortURL(t)
	deadAddr := addrOf(deadURL)

	master := newFakeMaster(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		if n == 1 {
			// 第一次：坏链（死 hub）——模拟停机后缓存的 hub URL。
			_, _ = w.Write([]byte(linkJSON(deadURL+"/f/file-1", time.Hour)))
			return
		}
		// 换链：master 回退到 Google 直链。
		_, _ = w.Write([]byte(linkJSON(google.URL+"/gdrive/file-1", time.Hour)))
	})

	logs := &lockedLogBuffer{}
	dial := newCountingDialer()
	signKey := testSignKey(t)
	handler := NewHandler(Config{
		SignKey:       signKey,
		MaxConcurrent: 8,
		Links:         newTestLinkSource(t, master.srv.URL, nil),
		Logger:        newWarnLogger(logs),
		Client:        &http.Client{Transport: upstreamTransport(dial.DialContext)},
	})
	agent := httptest.NewServer(handler)
	defer agent.Close()

	resp := mustGet(t, signedURLFor(signKey, agent.URL, "file-1"), nil)
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("换链重试后应 200，实际 %d", resp.StatusCode)
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("换链重试后字节不符：%d != %d", len(body), len(content))
	}
	if got := dial.count(deadAddr); got != 1 {
		t.Fatalf("旧链地址应恰好拨号 1 次（绝不原样重试死地址），实际 %d", got)
	}
	if got := dial.count(addrOf(google.URL)); got != 1 {
		t.Fatalf("换链后的重试应恰好 1 次，实际 %d", got)
	}
	if got := dial.total(); got != 2 {
		t.Fatalf("总上游请求应恰好 2 次（旧链 1 + 新链 1），实际 %d", got)
	}
	if got := master.requests.Load(); got != 2 {
		t.Fatalf("应恰好换链一次（1 拉链 + 1 换链），实际 %d", got)
	}
	waitLogContains(t, logs, "连接上游失败，重拉直链后重试一次")
	// 日志铁律：完整上游 URL（可能带签名参数）不进日志。
	if s := logs.String(); strings.Contains(s, "http://") {
		t.Fatalf("日志不应包含完整 URL：\n%s", s)
	}
}

// TestProxyConnectionRefusedRetriesOnceThen502：换链后仍是死地址 → 恰好再试一次
// 即按现状 502（不循环换链、不原样重试多次）。
func TestProxyConnectionRefusedRetriesOnceThen502(t *testing.T) {
	deadURL := closedPortURL(t)
	deadAddr := addrOf(deadURL)

	master := newFakeMaster(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		// 永远发死链：换链也救不回来。
		_, _ = w.Write([]byte(linkJSON(deadURL+"/f/file-1", time.Hour)))
	})

	logs := &lockedLogBuffer{}
	dial := newCountingDialer()
	signKey := testSignKey(t)
	handler := NewHandler(Config{
		SignKey:       signKey,
		MaxConcurrent: 8,
		Links:         newTestLinkSource(t, master.srv.URL, nil),
		Logger:        newWarnLogger(logs),
		Client:        &http.Client{Transport: upstreamTransport(dial.DialContext)},
	})
	agent := httptest.NewServer(handler)
	defer agent.Close()

	resp := mustGet(t, signedURLFor(signKey, agent.URL, "file-1"), nil)
	body := string(readBody(t, resp))

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("换链后仍失败应 502，实际 %d", resp.StatusCode)
	}
	if !strings.Contains(body, "连接上游失败") {
		t.Fatalf("应是中文原因：%s", body)
	}
	if got := dial.count(deadAddr); got != 2 {
		t.Fatalf("应恰好 2 次拨号（首次 + 唯一一次重试），实际 %d", got)
	}
	if got := dial.total(); got != 2 {
		t.Fatalf("总上游请求应恰好 2 次，实际 %d", got)
	}
	if got := master.requests.Load(); got != 2 {
		t.Fatalf("应恰好换链一次后停手（不循环），实际 %d", got)
	}
	waitLogContains(t, logs, "连接上游失败，重拉直链后重试一次")
}

// --- 混合态（本地前缀 + 上游余段） ---------------------------------------------

// newConnRetryStack 组装一套"节点读前缓存 + 死链首发"的数据面：
// master 第一次发 deadURL，其后发 newURL；缓存已预置块 0。
func newConnRetryStack(t *testing.T, content []byte, deadURL, newURL string, logs *lockedLogBuffer) (*httptest.Server, *countingDialer, *fakeMaster, *BlockCache) {
	t.Helper()
	master := newFakeMaster(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		if n == 1 {
			_, _ = w.Write([]byte(linkJSON(deadURL+"/f/file-1", time.Hour)))
			return
		}
		_, _ = w.Write([]byte(linkJSON(newURL, time.Hour)))
	})

	cache := NewBlockCache(64 << 20)
	identity := fmt.Sprintf("size:%d", len(content))
	cache.Observe("file-1", fileMeta{size: int64(len(content))})
	cache.Put("file-1", identity, 0, content[:blockSize])

	dial := newCountingDialer()
	handler := NewHandler(Config{
		SignKey:       testSignKey(t),
		MaxConcurrent: 8,
		Links:         newTestLinkSource(t, master.srv.URL, nil),
		Logger:        newWarnLogger(logs),
		Client:        &http.Client{Transport: upstreamTransport(dial.DialContext)},
		Cache:         cache, // Prefetch 不接：不起异步预取，请求计数保持确定
	})
	agent := httptest.NewServer(handler)
	t.Cleanup(agent.Close)
	return agent, dial, master, cache
}

// TestCacheMixedRetriesOnceOnConnectionRefused：前缀命中块 0 后，余段请求打到
// 死 hub → 换链一次 + 重试一次 → 客户端拿到完整正确的响应（不断连）。
func TestCacheMixedRetriesOnceOnConnectionRefused(t *testing.T) {
	content := randomContent(3*blockSize+555, 63)
	g := newGoogleFile(t, content)
	deadURL := closedPortURL(t)

	logs := &lockedLogBuffer{}
	agent, dial, master, _ := newConnRetryStack(t, content, deadURL, g.linkURL(), logs)
	signKey := testSignKey(t)

	// 第一次请求（完全落在块 0）：本地全命中，顺带把"死链"拉进链接缓存。
	resp1 := mustGet(t, signedURLFor(signKey, agent.URL, "file-1"), rangeHeader("bytes=0-999"))
	body1 := readBody(t, resp1)
	if resp1.StatusCode != http.StatusPartialContent || !bytes.Equal(body1, content[:1000]) {
		t.Fatalf("块 0 本地全命中不符：status=%d bytes=%d", resp1.StatusCode, len(body1))
	}
	if dial.total() != 0 {
		t.Fatalf("全命中不该拨上游，实际 %d 次", dial.total())
	}

	// 第二次：前缀命中块 0 → 余段请求打到死地址 → 换链 + 重试一次补齐整段。
	resp2 := mustGet(t, signedURLFor(signKey, agent.URL, "file-1"), rangeHeader("bytes=0-"))
	body2 := readBody(t, resp2)
	if resp2.StatusCode != http.StatusPartialContent {
		t.Fatalf("混合态应回 206，实际 %d", resp2.StatusCode)
	}
	if !bytes.Equal(body2, content) {
		t.Fatalf("换链重试后应给出完整正确的整段：len=%d，期望 %d", len(body2), len(content))
	}
	if got := dial.count(addrOf(deadURL)); got != 1 {
		t.Fatalf("旧链地址应恰好拨号 1 次，实际 %d", got)
	}
	if got := dial.count(addrOf(g.srv.URL)); got != 1 {
		t.Fatalf("换链后的余段重试应恰好 1 次，实际 %d", got)
	}
	if got := master.requests.Load(); got != 2 {
		t.Fatalf("应恰好换链一次（1 拉链 + 1 换链），实际 %d", got)
	}
	waitLogContains(t, logs, "混合服务：连接上游失败，重拉直链后重试一次")
	if s := logs.String(); strings.Contains(s, "断开连接") {
		t.Fatalf("重试成功不应断连：\n%s", s)
	}
	if s := logs.String(); strings.Contains(s, "http://") {
		t.Fatalf("日志不应包含完整 URL：\n%s", s)
	}
}

// TestCacheMixedAbortsWhenRetryAlsoRefused：换链后仍是死地址 → 只重试一次即断连
// （前缀已写出无法回退）；客户端拿到完整前缀 + 断连，不循环换链。
func TestCacheMixedAbortsWhenRetryAlsoRefused(t *testing.T) {
	content := randomContent(3*blockSize+666, 64)
	deadURL := closedPortURL(t)

	logs := &lockedLogBuffer{}
	agent, dial, master, _ := newConnRetryStack(t, content, deadURL, deadURL+"/f/file-1", logs)
	signKey := testSignKey(t)

	// 首次请求拿死链并本地全命中块 0。
	resp := mustGet(t, signedURLFor(signKey, agent.URL, "file-1"), rangeHeader("bytes=0-999"))
	_ = readBody(t, resp)
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("块 0 本地全命中不符：status=%d", resp.StatusCode)
	}

	resp = mustGet(t, signedURLFor(signKey, agent.URL, "file-1"), rangeHeader("bytes=0-"))
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("前缀先行应先写出 206 头，实际 %d", resp.StatusCode)
	}
	body := readBodyExpectClosed(t, resp)
	if len(body) != blockSize || !bytes.Equal(body, content[:blockSize]) {
		t.Fatalf("断连前应恰好拿到完整前缀：len=%d", len(body))
	}
	if got := dial.count(addrOf(deadURL)); got != 2 {
		t.Fatalf("应恰好 2 次拨号（首次 + 唯一一次重试），实际 %d", got)
	}
	if got := master.requests.Load(); got != 2 {
		t.Fatalf("应恰好换链一次（不循环），实际 %d", got)
	}
	waitLogContains(t, logs, "断开连接")
}
