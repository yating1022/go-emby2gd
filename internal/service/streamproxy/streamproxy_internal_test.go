package streamproxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs"
)

// captureLogger 测试用日志捕获器
type captureLogger struct {
	mu   sync.Mutex
	msgs []string
}

// Log 实现 logs.Logger
func (c *captureLogger) Log(content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, content)
}

// contains 判断捕获到的日志中是否存在包含指定片段的行
func (c *captureLogger) contains(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, msg := range c.msgs {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}

// all 返回捕获到的全部日志 (副本)
func (c *captureLogger) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.msgs...)
}

// captureLogs 注册一个日志捕获器, 测试结束时自动移除
func captureLogs(t *testing.T) *captureLogger {
	t.Helper()

	logger := new(captureLogger)
	id, ok := logs.RegisterLogger(logger)
	if !ok {
		t.Fatal("注册日志捕获器失败")
	}
	t.Cleanup(func() { logs.RemoveLogger(id) })
	return logger
}

// resetSlotsForTest 重置并发信号量, 便于测试注入确定的并发上限
//
// 走 export_test.go 提供的测试专用入口, 不再修改配置对象
func resetSlotsForTest(limit int) {
	ResetSlotsForTest(limit)
}

// blockingUpstream 会阻塞直到被放行的假上游, 用于观测并发规模
type blockingUpstream struct {
	server  *httptest.Server
	mu      sync.Mutex
	current int
	peak    int
	total   int
	release chan struct{}
	once    sync.Once
}

// newBlockingUpstream 启动一个阻塞式假上游
func newBlockingUpstream(t *testing.T) *blockingUpstream {
	t.Helper()

	up := &blockingUpstream{release: make(chan struct{})}
	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.mu.Lock()
		up.current++
		up.total++
		if up.current > up.peak {
			up.peak = up.current
		}
		up.mu.Unlock()

		<-up.release

		up.mu.Lock()
		up.current--
		up.mu.Unlock()
		w.Write([]byte("ok"))
	}))

	t.Cleanup(func() {
		up.unblock()
		up.server.CloseClientConnections()
		up.server.Close()
	})
	return up
}

// unblock 放行所有被阻塞的上游请求 (可重复调用)
func (u *blockingUpstream) unblock() {
	u.once.Do(func() { close(u.release) })
}

// stats 读取当前并发数、并发峰值与累计请求数
func (u *blockingUpstream) stats() (current, peak, total int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.current, u.peak, u.total
}

// waitFor 等待条件成立, 超时则判定测试失败
func waitFor(t *testing.T, desc string, cond func() bool, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时", desc)
}

// runProxyAsync 在独立 goroutine 中调用 Proxy
func runProxyAsync(ctx context.Context, target string) (wait func() (bool, error)) {
	done := make(chan struct{})
	var written bool
	var err error

	go func() {
		defer close(done)
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, "http://project.local/stream", nil)
		if reqErr != nil {
			err = reqErr
			return
		}
		written, err = Proxy(httptest.NewRecorder(), req, target)
	}()

	return func() (bool, error) {
		<-done
		return written, err
	}
}

// proxyConfigWithLimit 注入一份并发上限为 limit 的配置
//
// 上限通过测试专用入口注入信号量, 不再改配置对象:
// 运行期改配置会与 initSlots 的读取形成数据竞争
func proxyConfigWithLimit(t *testing.T, domains []string, limit int) {
	t.Helper()

	internalProxyConfig(t, func(p *config.StrmProxy) { p.Domains = domains })
	resetSlotsForTest(limit)
}

func TestProxy_ConcurrencyLimit(t *testing.T) {
	up := newBlockingUpstream(t)
	proxyConfigWithLimit(t, []string{up.server.URL}, 2)
	target := up.server.URL + "/media.mp4"

	const concurrent = 5
	waiters := make([]func() (bool, error), 0, concurrent)
	for i := 0; i < concurrent; i++ {
		waiters = append(waiters, runProxyAsync(context.Background(), target))
	}

	waitFor(t, "两路传输进入上游", func() bool {
		current, _, _ := up.stats()
		return current == 2
	}, 5*time.Second)

	// 再观察一段时间, 确认并发数没有被突破
	time.Sleep(200 * time.Millisecond)
	current, peak, _ := up.stats()
	if current != 2 {
		t.Errorf("同时进入上游的传输数 = %d, want 2", current)
	}
	if peak > 2 {
		t.Errorf("并发峰值 = %d, 不应超过上限 2", peak)
	}

	up.unblock()
	for i, wait := range waiters {
		written, err := wait()
		if err != nil {
			t.Errorf("第 %d 个请求返回错误: %v", i, err)
		}
		if !written {
			t.Errorf("第 %d 个请求 written = false, 期望 true", i)
		}
	}

	if _, peak, total := up.stats(); peak > 2 || total != concurrent {
		t.Errorf("并发峰值 = %d (want <= 2), 上游请求数 = %d (want %d)", peak, total, concurrent)
	}
}

func TestProxy_WaitReleasedOnClientCancel(t *testing.T) {
	up := newBlockingUpstream(t)
	proxyConfigWithLimit(t, []string{up.server.URL}, 1)
	target := up.server.URL + "/media.mp4"

	// 1 第一个请求占住唯一的槽位
	first := runProxyAsync(context.Background(), target)
	waitFor(t, "第一路传输进入上游", func() bool {
		current, _, _ := up.stats()
		return current == 1
	}, 5*time.Second)

	// 2 第二个请求在等待槽位期间被客户端取消
	ctx, cancel := context.WithCancel(context.Background())
	second := runProxyAsync(ctx, target)
	time.Sleep(100 * time.Millisecond)
	cancel()

	done := make(chan struct{})
	var written bool
	var err error
	go func() {
		defer close(done)
		written, err = second()
	}()

	select {
	case <-done:
		if err == nil {
			t.Error("等待槽位期间客户端取消应返回错误")
		}
		if written {
			t.Error("尚未写出响应, written 应为 false")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("客户端取消后等待槽位应立即结束")
	}

	if _, _, total := up.stats(); total != 1 {
		t.Errorf("上游请求数 = %d, want 1 (等待中的请求不应发起上游请求)", total)
	}

	up.unblock()
	if _, err := first(); err != nil {
		t.Errorf("第一个请求返回错误: %v", err)
	}
}

func TestProxy_SlotNotLeaked(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	proxyConfigWithLimit(t, []string{upstream.URL}, 1)
	target := upstream.URL + "/media.mp4"

	for i := 0; i < 5; i++ {
		wait := runProxyAsync(context.Background(), target)

		done := make(chan struct{})
		var written bool
		var err error
		go func() {
			defer close(done)
			written, err = wait()
		}()

		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("第 %d 个串行请求被永久阻塞, 槽位可能泄漏", i)
		}

		if err != nil {
			t.Errorf("第 %d 个请求返回错误: %v", i, err)
		}
		if !written {
			t.Errorf("第 %d 个请求 written = false, 期望 true", i)
		}
	}
}

func TestProxy_UnlimitedSkipsWaiting(t *testing.T) {
	logger := captureLogs(t)

	up := newBlockingUpstream(t)
	proxyConfigWithLimit(t, []string{up.server.URL}, 0)
	if !ProxyEnabled() {
		t.Fatal("测试配置应处于开启状态")
	}
	target := up.server.URL + "/media.mp4"

	const concurrent = 4
	waiters := make([]func() (bool, error), 0, concurrent)
	for i := 0; i < concurrent; i++ {
		waiters = append(waiters, runProxyAsync(context.Background(), target))
	}

	waitFor(t, "四路传输全部进入上游", func() bool {
		current, _, _ := up.stats()
		return current == concurrent
	}, 5*time.Second)

	if _, peak, _ := up.stats(); peak != concurrent {
		t.Errorf("不做限制时并发峰值 = %d, want %d", peak, concurrent)
	}
	if logger.contains("并发传输已达上限") {
		t.Error("不做限制时不应输出等待槽位的日志 (L13)")
	}

	up.unblock()
	for i, wait := range waiters {
		if _, err := wait(); err != nil {
			t.Errorf("第 %d 个请求返回错误: %v", i, err)
		}
	}
}

func TestProxy_ClientDisconnect(t *testing.T) {
	logger := captureLogs(t)

	upstreamGone := make(chan struct{})
	var once sync.Once
	signalGone := func() { once.Do(func() { close(upstreamGone) }) }

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		chunk := bytes.Repeat([]byte{7}, 4096)
		for i := 0; i < 500; i++ {
			select {
			case <-r.Context().Done():
				signalGone()
				return
			default:
			}
			if _, err := w.Write(chunk); err != nil {
				signalGone()
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	internalProxyConfig(t, func(proxy *config.StrmProxy) { proxy.Domains = []string{upstream.URL} })

	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Proxy(w, r, upstream.URL+"/media.mp4")
	}))
	defer proxySrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxySrv.URL, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("请求代理失败: %v", err)
	}

	if _, err := io.ReadFull(resp.Body, make([]byte, 64)); err != nil {
		t.Fatalf("读取首段响应失败: %v", err)
	}

	// 客户端断开
	cancel()
	resp.Body.Close()

	select {
	case <-upstreamGone:
	case <-time.After(5 * time.Second):
		t.Fatal("客户端断开后上游连接未在预期时间内释放")
	}

	deadline := time.Now().Add(3 * time.Second)
	for !logger.contains("传输中断") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !logger.contains("传输中断") {
		t.Error("客户端断开时应输出传输中断日志 (L9')")
	}
}

func TestProxy_LogPrefixOnFailure(t *testing.T) {
	logger := captureLogs(t)

	// 1 上游建连失败 → L12
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := closed.URL
	closed.Close()

	internalProxyConfig(t, func(proxy *config.StrmProxy) { proxy.Domains = []string{deadURL} })
	if written, err := Proxy(httptest.NewRecorder(), mustClientRequest(t), deadURL+"/media.mp4"); err == nil || written {
		t.Fatalf("上游建连失败时应返回错误且未写出响应, 实际 written=%v, err=%v", written, err)
	}
	if !logger.contains("[直链代理] 请求上游失败") {
		t.Errorf("建连失败时应输出带前缀的 L12 日志, 已捕获: %v", logger.all())
	}

	// 2 重定向超过上限 → L11
	loop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer loop.Close()

	internalProxyConfig(t, func(proxy *config.StrmProxy) {
		proxy.Domains = []string{loop.URL}
		proxy.MaxRedirectDepth = 1
	})
	if written, err := Proxy(httptest.NewRecorder(), mustClientRequest(t), loop.URL+"/loop"); err == nil || written {
		t.Fatalf("跳数超限时应返回错误且未写出响应, 实际 written=%v, err=%v", written, err)
	}
	if !logger.contains("[直链代理] 代理失败") {
		t.Errorf("代理失败时应输出带前缀的 L11 日志, 已捕获: %v", logger.all())
	}
}

// mustClientRequest 构造一个模拟客户端发往本项目的请求
func mustClientRequest(t *testing.T) *http.Request {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, "http://project.local/stream", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	return req
}
