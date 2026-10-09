// 端到端链路测试（全部 httptest，不经真实网络）：
//
//	enroll（写 config.env）→ 心跳 → 向 master 拉直链 → Range 代理 → 206 + 正确字节
//
// 这里刻意从外部（package proxy_test）按 main.go 的接线方式组装，
// 保证"实际启动的组件组合"被这套测试覆盖。
package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yating1022/go-emby2gd/agent/internal/config"
	"github.com/yating1022/go-emby2gd/agent/internal/enroll"
	"github.com/yating1022/go-emby2gd/agent/internal/heartbeat"
	"github.com/yating1022/go-emby2gd/agent/internal/proxy"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func e2eContent(n int) []byte {
	content := make([]byte, n)
	for i := range content {
		content[i] = byte((i * 7) % 251)
	}
	return content
}

func TestEndToEndEnrollHeartbeatLinkAndRangeProxy(t *testing.T) {
	content := e2eContent(128 * 1024)
	const (
		agentID    = "agent-uuid-e2e"
		agentToken = "dev-token"
	)

	// --- mock Google：支持 Range/HEAD 的真实语义 ---
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Unix(1700000000, 0), bytes.NewReader(content))
	}))
	defer google.Close()

	// --- mock master：三个 agent 接口 ---
	var (
		heartbeats    atomic.Int64
		linkRequests  atomic.Int64
		lastHeartbeat atomic.Value
	)
	signKeyHex := strings.Repeat("c0", 32)
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agent/enroll":
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			if body["enroll_token"] != agentToken {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"ENROLL_TOKEN_INVALID","message":"注册 Token 无效"}}`))
				return
			}
			// 用 {ok,data} 信封（本项目既有约定）返回，顺带覆盖信封兼容。
			_, _ = fmt.Fprintf(w, `{"ok":true,"data":{"agent_id":%q,"agent_secret":"%s","sign_key":%q,"heartbeat_interval_seconds":15}}`,
				agentID, strings.Repeat("ab", 32), signKeyHex)
		case "/api/agent/heartbeat":
			if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("ab", 32) || r.Header.Get("X-Agent-Id") != agentID {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"UNAUTHORIZED","message":"凭据无效"}}`))
				return
			}
			raw, _ := io.ReadAll(r.Body)
			lastHeartbeat.Store(string(raw))
			heartbeats.Add(1)
			_, _ = w.Write([]byte(`{"ok":true,"enabled":true,"heartbeat_interval_seconds":1}`))
		case "/api/agent/download-link":
			fileID := r.URL.Query().Get("file_id")
			if fileID == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"VALIDATION_ERROR","message":"缺少 file_id"}}`))
				return
			}
			linkRequests.Add(1)
			// 冻结稿的裸对象形状（agent 两种信封都要能解）。
			_, _ = fmt.Fprintf(w, `{"url":%q,"headers":{"Authorization":"Bearer google-access-token"},"expires_at":%q}`,
				google.URL+"/gdrive/"+fileID, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		default:
			http.NotFound(w, r)
		}
	}))
	defer master.Close()

	// --- ① enroll：真实写 config.env ---
	configPath := filepath.Join(t.TempDir(), "gd-agent", "config.env")
	machineID := filepath.Join(t.TempDir(), "machine-id")
	if err := os.WriteFile(machineID, []byte("machine-e2e\n"), 0o644); err != nil {
		t.Fatalf("准备 machine-id 失败：%v", err)
	}
	if _, err := enroll.Run(context.Background(), enroll.Options{
		MasterURL:     master.URL,
		Token:         agentToken,
		ListenPort:    8790,
		ConfigPath:    configPath,
		Version:       "e2e",
		MachineIDPath: machineID,
		Logger:        quietLogger(),
	}); err != nil {
		t.Fatalf("enroll 失败：%v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("读配置失败：%v", err)
	}
	signKey, err := cfg.SignKeyBytes()
	if err != nil {
		t.Fatalf("SIGN_KEY 解码失败：%v", err)
	}

	// --- ② 数据面接线（与 main.go 的 runServe 一致，含读前缓存与首触预取） ---
	var enabled atomic.Bool
	enabled.Store(true)
	links := proxy.NewLinkSource(proxy.LinkSourceConfig{
		MasterURL: cfg.MasterURL,
		AgentID:   cfg.AgentID,
		Secret:    cfg.AgentSecret,
		Logger:    quietLogger(),
		Enabled:   enabled.Load,
	})
	client := proxy.NewUpstreamClient()
	cache := proxy.NewBlockCacheWithMaxAge(
		int64(cfg.CacheBudgetMB)<<20, time.Duration(cfg.CacheMaxAgeMinutes)*time.Minute)
	prefetcher := proxy.NewPrefetcher(proxy.PrefetcherConfig{
		Cache:     cache,
		Links:     links,
		Client:    client,
		Logger:    quietLogger(),
		HeadBytes: int64(cfg.PrefetchHeadMB) << 20,
		TailBytes: int64(cfg.PrefetchTailMB) << 20,
	})
	handler := proxy.NewHandler(proxy.Config{
		SignKey:       signKey,
		MaxConcurrent: 4,
		Links:         links,
		Logger:        quietLogger(),
		Client:        client,
		Cache:         cache,
		Prefetch:      prefetcher,
	})
	agent := httptest.NewServer(handler)
	defer agent.Close()

	// --- ③ 心跳循环 ---
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	beat := heartbeat.New(heartbeat.Options{
		MasterURL:     cfg.MasterURL,
		AgentID:       cfg.AgentID,
		Secret:        cfg.AgentSecret,
		Version:       "e2e",
		ListenPort:    cfg.ListenPort,
		ActiveStreams: handler.ActiveStreams,
		Interval:      20 * time.Millisecond,
		Logger:        quietLogger(),
	})
	go beat.Run(ctx, &enabled)

	deadline := time.Now().Add(3 * time.Second)
	for heartbeats.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if heartbeats.Load() == 0 {
		t.Fatal("心跳未到达 mock master")
	}
	if body := fmt.Sprint(lastHeartbeat.Load()); !strings.Contains(body, `"active_streams"`) {
		t.Fatalf("心跳体应带 active_streams：%s", body)
	}

	// --- ④ 客户端拿签名 URL，做 Range 拉流 ---
	const fileID = "demo-file-id"
	expiry := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	url := fmt.Sprintf("%s/dl/%s?e=%s&s=%s", agent.URL, fileID, expiry, proxy.Sign(signKey, fileID, expiry))

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	req.Header.Set("Range", "bytes=100-199")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Range 请求失败：%v", err)
	}
	chunk, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("读响应体失败：%v", err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range 请求应回 206，实际 %d", resp.StatusCode)
	}
	if !bytes.Equal(chunk, content[100:200]) {
		t.Fatalf("分片字节不符：得到 %d 字节", len(chunk))
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 100-199/131072" {
		t.Fatalf("Content-Range 不对：%q", got)
	}

	// --- ⑤ 全量下载：直链走缓存，不再打扰 master ---
	resp2, err := http.Get(url)
	if err != nil {
		t.Fatalf("全量请求失败：%v", err)
	}
	full, err := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if err != nil {
		t.Fatalf("读响应体失败：%v", err)
	}
	if resp2.StatusCode != http.StatusOK || !bytes.Equal(full, content) {
		t.Fatalf("全量下载不符：status=%d bytes=%d", resp2.StatusCode, len(full))
	}
	if got := linkRequests.Load(); got != 1 {
		t.Fatalf("两次下载应只向 master 拉一次直链，实际 %d 次", got)
	}
}
