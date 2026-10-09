package heartbeat

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type recorder struct {
	mu       sync.Mutex
	bodies   []map[string]any
	authHdr  []string
	agentHdr []string
}

func (r *recorder) add(rq *http.Request) {
	raw, _ := io.ReadAll(rq.Body)
	body := map[string]any{}
	_ = json.Unmarshal(raw, &body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, body)
	r.authHdr = append(r.authHdr, rq.Header.Get("Authorization"))
	r.agentHdr = append(r.agentHdr, rq.Header.Get("X-Agent-Id"))
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func (r *recorder) first() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bodies) == 0 {
		return nil
	}
	return r.bodies[0]
}

func startMaster(t *testing.T, handler func(n int64, w http.ResponseWriter)) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/heartbeat" {
			http.NotFound(w, r)
			return
		}
		n := hits.Add(1)
		rec.add(r)
		handler(n, w)
	}))
	t.Cleanup(server.Close)
	return server, rec
}

func newClient(masterURL string, mutate func(*Options)) *Client {
	opts := Options{
		MasterURL:  masterURL,
		AgentID:    "agent-1",
		Secret:     "secret-1",
		Version:    "0.1.0",
		ListenPort: 8790,
		Interval:   20 * time.Millisecond,
		MaxBackoff: 60 * time.Millisecond,
		Logger:     discardLogger(),
	}
	if mutate != nil {
		mutate(&opts)
	}
	return New(opts)
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

func TestHeartbeatReportsFieldsAndAuth(t *testing.T) {
	master, rec := startMaster(t, func(n int64, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"ok":true,"enabled":true}`))
	})
	client := newClient(master.URL, func(opts *Options) {
		opts.ActiveStreams = func() int64 { return 7 }
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var enabled atomic.Bool
	enabled.Store(true)
	go client.Run(ctx, &enabled)

	waitFor(t, 2*time.Second, func() bool { return rec.count() >= 2 }, "至少两次心跳")
	body := rec.first()
	if body["active_streams"] != float64(7) {
		t.Fatalf("active_streams 应为回调值：%+v", body)
	}
	if body["version"] != "0.1.0" || body["listen_port"] != float64(8790) {
		t.Fatalf("心跳字段不对：%+v", body)
	}
	if uptime, ok := body["uptime_seconds"].(float64); !ok || uptime < 0 {
		t.Fatalf("uptime_seconds 应为非负数：%+v", body["uptime_seconds"])
	}
	if v, present := body["public_base_url"]; !present || v != nil {
		t.Fatalf("未配置 public_base_url 时应上报 null：%+v", body)
	}
	if rec.authHdr[0] != "Bearer secret-1" || rec.agentHdr[0] != "agent-1" {
		t.Fatalf("心跳凭据头不对：%q / %q", rec.authHdr[0], rec.agentHdr[0])
	}
}

func TestHeartbeatAdoptsServerInterval(t *testing.T) {
	master, rec := startMaster(t, func(n int64, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"ok":true,"enabled":true,"heartbeat_interval_seconds":1}`))
	})
	client := newClient(master.URL, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var enabled atomic.Bool
	enabled.Store(true)
	go client.Run(ctx, &enabled)

	time.Sleep(1200 * time.Millisecond)
	// 初始间隔 20ms，若未采纳服务端的 1s，这段时间会有几十次心跳。
	if got := rec.count(); got > 3 {
		t.Fatalf("应采纳服务端下发的 1s 间隔，实际 1.2s 内 %d 次", got)
	}
	if got := rec.count(); got < 2 {
		t.Fatalf("1.2s 内至少应有 2 次心跳，实际 %d", got)
	}
}

func TestHeartbeat401RetriesWithBackoffAndNeverExits(t *testing.T) {
	master, rec := startMaster(t, func(n int64, w http.ResponseWriter) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"AGENT_UNAUTHORIZED","message":"agent 凭据无效"}}`))
	})
	client := newClient(master.URL, nil)

	ctx, cancel := context.WithCancel(context.Background())
	var enabled atomic.Bool
	enabled.Store(true)
	done := make(chan struct{})
	go func() {
		client.Run(ctx, &enabled)
		close(done)
	}()

	time.Sleep(300 * time.Millisecond)
	if got := rec.count(); got < 3 {
		t.Fatalf("401 应退避重试（不退出），实际 %d 次", got)
	}
	select {
	case <-done:
		t.Fatal("401 不应让心跳循环退出")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消后心跳循环应退出")
	}
}

func TestHeartbeatEnabledFlagFollowsMaster(t *testing.T) {
	var disable atomic.Bool
	disable.Store(true)
	master, _ := startMaster(t, func(n int64, w http.ResponseWriter) {
		if disable.Load() {
			_, _ = w.Write([]byte(`{"ok":true,"enabled":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"enabled":true}`))
	})
	client := newClient(master.URL, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var enabled atomic.Bool
	enabled.Store(true)
	go client.Run(ctx, &enabled)

	waitFor(t, 2*time.Second, func() bool { return !enabled.Load() }, "master 禁用后置 false")
	disable.Store(false)
	waitFor(t, 2*time.Second, func() bool { return enabled.Load() }, "master 解禁后自动恢复")
}

func TestHeartbeatKeepsFlagWhenFieldAbsent(t *testing.T) {
	master, _ := startMaster(t, func(n int64, w http.ResponseWriter) {
		// 老/新 master 都可能不回 enabled：不得把它当成"被禁用"。
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	client := newClient(master.URL, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var enabled atomic.Bool
	enabled.Store(true)
	go client.Run(ctx, &enabled)

	time.Sleep(120 * time.Millisecond)
	if !enabled.Load() {
		t.Fatal("响应缺 enabled 字段时应保持原状态")
	}
}

func TestBackoffDelay(t *testing.T) {
	base := 15 * time.Second
	max := 5 * time.Minute
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, base},
		{1, base},
		{2, 30 * time.Second},
		{3, 60 * time.Second},
		{4, 2 * time.Minute},
		{5, 4 * time.Minute},
		{6, max},
		{50, max},
	}
	for _, tc := range cases {
		if got := backoffDelay(base, max, tc.failures); got != tc.want {
			t.Fatalf("第 %d 次失败：得到 %s，期望 %s", tc.failures, got, tc.want)
		}
	}
}
