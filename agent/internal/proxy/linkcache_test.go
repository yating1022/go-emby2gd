package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMaster 是一个最小 master：只实现 /api/agent/download-link。
type fakeMaster struct {
	srv      *httptest.Server
	requests atomic.Int64
	lastAuth atomic.Value
	lastID   atomic.Value

	// respond 由测试提供；n 是本次是第几次请求（从 1 开始）。
	respond func(w http.ResponseWriter, r *http.Request, n int64)
}

func newFakeMaster(t *testing.T, respond func(w http.ResponseWriter, r *http.Request, n int64)) *fakeMaster {
	t.Helper()
	m := &fakeMaster{respond: respond}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := m.requests.Add(1)
		m.lastAuth.Store(r.Header.Get("Authorization"))
		m.lastID.Store(r.Header.Get("X-Agent-Id"))
		if r.URL.Path != "/api/agent/download-link" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("file_id"); got == "" {
			t.Errorf("master 收到空 file_id")
		}
		m.respond(w, r, n)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func linkJSON(url string, expiresIn time.Duration) string {
	return fmt.Sprintf(`{"url":%q,"headers":{"Authorization":"Bearer google-token"},"expires_at":%q}`,
		url, time.Now().Add(expiresIn).UTC().Format(time.RFC3339))
}

func newTestLinkSource(t *testing.T, masterURL string, mutate func(*LinkSourceConfig)) *LinkSource {
	t.Helper()
	cfg := LinkSourceConfig{
		MasterURL: masterURL,
		AgentID:   "agent-1",
		Secret:    "secret-1",
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewLinkSource(cfg)
}

func TestLinkCachesByFileID(t *testing.T) {
	master := newFakeMaster(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		_, _ = w.Write([]byte(linkJSON("http://google.example/file1", time.Hour)))
	})
	source := newTestLinkSource(t, master.srv.URL, nil)

	first, err := source.Link(context.Background(), "file-1")
	if err != nil {
		t.Fatalf("第一次 Link 失败：%v", err)
	}
	second, err := source.Link(context.Background(), "file-1")
	if err != nil {
		t.Fatalf("第二次 Link 失败：%v", err)
	}
	if first.URL != second.URL || first.URL != "http://google.example/file1" {
		t.Fatalf("缓存返回不一致：%+v / %+v", first, second)
	}
	if first.Headers["Authorization"] != "Bearer google-token" {
		t.Fatalf("凭据头未缓存：%+v", first.Headers)
	}
	if got := master.requests.Load(); got != 1 {
		t.Fatalf("同一 file_id 应只拉一次直链，实际 %d 次", got)
	}
	if master.lastAuth.Load() != "Bearer secret-1" || master.lastID.Load() != "agent-1" {
		t.Fatalf("agent 凭据头不对：%v / %v", master.lastAuth.Load(), master.lastID.Load())
	}
}

// TestDefaultMarginInSafeChain 钉住余量链：agent 25s < master 30s < 面板 60s
// （父任务 10-08-agent-proxy-network 的 design §2.3）。
//
// 余量 >= master 侧 30s（参考稿取的 5 分钟即如此）时：agent 在 expires_at−余量
// 就把直链视为过期、每次请求都回 master 重拉，而 master 缓存要到 expires_at−30s
// 才失效、原样返回同一份 token → 该窗口内每个 Range 请求都重拉一次
// （病灶同 gdrive-panel.md §3.1）。修任何一侧余量前先读任务 10-08-agent-proxy-network 的 design §2.3。
func TestDefaultMarginInSafeChain(t *testing.T) {
	if defaultMargin <= 0 {
		t.Fatalf("余量必须为正，实际 %v", defaultMargin)
	}
	if defaultMargin >= 30*time.Second {
		t.Fatalf("agent 余量 %v 必须小于 master 侧 30s，否则 expires_at−余量 窗口内每次请求都会重拉同一份 token", defaultMargin)
	}
}

// 有效期落在默认余量（25s）之内：一进缓存就已视为过期，每次请求都应重拉。
// 这条测试钉住余量的**下界**：若余量被改小到明显低于 20s（例如 10s），20s 有效期的
// 直链会被当成新鲜缓存而不重拉，此测试即失败。上界（必须 < master 侧 30s，防止在
// expires_at−余量 窗口内反复重拉同一份 token）由 TestDefaultMarginInSafeChain 钉住
// 常量断言——注意 5 分钟余量在这里的行为与 25s 相同（都是"每次都重拉"），
// 行为测试看不出这个病灶。
func TestLinkRefetchesInsideMargin(t *testing.T) {
	master := newFakeMaster(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		_, _ = w.Write([]byte(linkJSON(fmt.Sprintf("http://google.example/file-%d", n), 20*time.Second)))
	})
	source := newTestLinkSource(t, master.srv.URL, nil)

	if _, err := source.Link(context.Background(), "file-1"); err != nil {
		t.Fatalf("Link 失败：%v", err)
	}
	if _, err := source.Link(context.Background(), "file-1"); err != nil {
		t.Fatalf("Link 失败：%v", err)
	}
	if got := master.requests.Load(); got != 2 {
		t.Fatalf("有效期落在 25s 余量内应重拉，实际 %d 次请求", got)
	}
}

// master 用 `core/timefmt.to_utc_iso()` 序列化 expires_at：**保留微秒**，形如
// `2026-10-09T12:00:00.123456Z`。必须能解析并进缓存——解析失败虽不报错，
// 但每个客户端请求都会回 master 重拉（"缓存永远不生效"且现场无明显症状）。
func TestLinkAcceptsMasterMicrosecondExpiresAt(t *testing.T) {
	master := newFakeMaster(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		_, _ = fmt.Fprintf(w, `{"url":"http://google.example/file1","headers":{},"expires_at":%q}`,
			time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05.000000Z"))
	})
	source := newTestLinkSource(t, master.srv.URL, nil)

	for i := 0; i < 3; i++ {
		if _, err := source.Link(context.Background(), "file-1"); err != nil {
			t.Fatalf("第 %d 次 Link 失败：%v", i+1, err)
		}
	}
	if got := master.requests.Load(); got != 1 {
		t.Fatalf("带微秒的 expires_at 应能解析并进缓存，实际拉了 %d 次直链", got)
	}
}

func TestLinkSingleFlight(t *testing.T) {
	release := make(chan struct{})
	master := newFakeMaster(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		<-release // 拖住所有并发请求，制造单飞窗口
		_, _ = w.Write([]byte(linkJSON("http://google.example/slow", time.Hour)))
	})
	source := newTestLinkSource(t, master.srv.URL, nil)

	const goroutines = 20
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			link, err := source.Link(context.Background(), "file-same")
			if err != nil {
				errs <- err
				return
			}
			if link.URL != "http://google.example/slow" {
				errs <- fmt.Errorf("URL 不符：%s", link.URL)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond) // 让所有 goroutine 都进入等待
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发 Link 失败：%v", err)
	}
	if got := master.requests.Load(); got != 1 {
		t.Fatalf("单飞应只产生一次 master 请求，实际 %d 次", got)
	}
}

func TestLinkDisabledUsesCacheButNoNewFetches(t *testing.T) {
	var enabled atomic.Bool
	enabled.Store(true)
	master := newFakeMaster(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		_, _ = w.Write([]byte(linkJSON("http://google.example/file1", time.Hour)))
	})
	source := newTestLinkSource(t, master.srv.URL, func(cfg *LinkSourceConfig) {
		cfg.Enabled = enabled.Load
	})

	if _, err := source.Link(context.Background(), "file-1"); err != nil {
		t.Fatalf("Link 失败：%v", err)
	}
	enabled.Store(false)

	// 已缓存的直链继续服务（冻结稿 §2.2：不打断已发出的 URL）。
	if _, err := source.Link(context.Background(), "file-1"); err != nil {
		t.Fatalf("禁用后仍应能用缓存：%v", err)
	}
	if got := master.requests.Load(); got != 1 {
		t.Fatalf("禁用后不应再拉直链，实际 %d 次请求", got)
	}

	// 没有缓存的文件：报中文原因，且不发请求。
	_, err := source.Link(context.Background(), "file-2")
	var linkErr *LinkError
	if err == nil || !asLinkError(err, &linkErr) {
		t.Fatalf("禁用且无缓存应报 *LinkError，实际 %v", err)
	}
	if !strings.Contains(linkErr.Message, "禁用") {
		t.Fatalf("错误文案应为中文提示禁用：%s", linkErr.Message)
	}
	if got := master.requests.Load(); got != 1 {
		t.Fatalf("禁用后不应发起新请求，实际 %d 次", got)
	}
}

func TestRefreshReplacesCache(t *testing.T) {
	master := newFakeMaster(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		_, _ = w.Write([]byte(linkJSON(fmt.Sprintf("http://google.example/file-%d", n), time.Hour)))
	})
	source := newTestLinkSource(t, master.srv.URL, nil)

	first, err := source.Link(context.Background(), "file-1")
	if err != nil {
		t.Fatalf("Link 失败：%v", err)
	}
	refreshed, err := source.Refresh(context.Background(), "file-1")
	if err != nil {
		t.Fatalf("Refresh 失败：%v", err)
	}
	if refreshed.URL == first.URL {
		t.Fatalf("Refresh 应拿到新直链：%s", refreshed.URL)
	}
	again, err := source.Link(context.Background(), "file-1")
	if err != nil {
		t.Fatalf("Link 失败：%v", err)
	}
	if again.URL != refreshed.URL {
		t.Fatalf("Refresh 应覆盖缓存：%s != %s", again.URL, refreshed.URL)
	}
	if got := master.requests.Load(); got != 2 {
		t.Fatalf("应为 2 次请求，实际 %d", got)
	}
}

func TestLinkErrorPassesThroughChineseMessage(t *testing.T) {
	master := newFakeMaster(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"GD_QUOTA_EXCEEDED","message":"Google 配额已用尽"}}`))
	})
	source := newTestLinkSource(t, master.srv.URL, nil)

	_, err := source.Link(context.Background(), "file-1")
	var linkErr *LinkError
	if err == nil || !asLinkError(err, &linkErr) {
		t.Fatalf("应报 *LinkError，实际 %v", err)
	}
	if !strings.Contains(linkErr.Message, "Google 配额已用尽") {
		t.Fatalf("应透传 master 的中文原因：%s", linkErr.Message)
	}
	if linkErr.Code != "GD_QUOTA_EXCEEDED" {
		t.Fatalf("应透传 master 的错误码：%s", linkErr.Code)
	}
}

func TestLinkUnparsableExpiresAtIsNotCached(t *testing.T) {
	master := newFakeMaster(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		_, _ = w.Write([]byte(`{"url":"http://google.example/x","headers":{},"expires_at":"not-a-time"}`))
	})
	source := newTestLinkSource(t, master.srv.URL, nil)

	for i := 0; i < 2; i++ {
		if _, err := source.Link(context.Background(), "file-1"); err != nil {
			t.Fatalf("Link 失败：%v", err)
		}
	}
	if got := master.requests.Load(); got != 2 {
		t.Fatalf("有效期无法解析时不应缓存，实际 %d 次请求", got)
	}
}

func TestLinkEmptyURLIsError(t *testing.T) {
	master := newFakeMaster(t, func(w http.ResponseWriter, r *http.Request, n int64) {
		_, _ = w.Write([]byte(`{"url":"","headers":{},"expires_at":"2030-01-01T00:00:00Z"}`))
	})
	source := newTestLinkSource(t, master.srv.URL, nil)
	if _, err := source.Link(context.Background(), "file-1"); err == nil {
		t.Fatal("空 URL 应报错")
	}
}

// asLinkError 是 errors.As 的本地封装，避免测试文件为一个断言引入 errors 别名。
func asLinkError(err error, target **LinkError) bool {
	if e, ok := err.(*LinkError); ok {
		*target = e
		return true
	}
	return false
}
