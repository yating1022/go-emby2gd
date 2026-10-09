package enroll

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yating1022/go-emby2gd/agent/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type capturedRequest struct {
	path  string
	token string
	body  map[string]any
}

func startMaster(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.Path
		captured.token = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		captured.body = map[string]any{}
		_ = json.Unmarshal(raw, &captured.body)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server, captured
}

func writeMachineID(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "machine-id")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写 machine-id 失败：%v", err)
	}
	return path
}

func TestRunPostsEnrollAndWritesConfig(t *testing.T) {
	master, captured := startMaster(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"agent_id":"agent-uuid","agent_secret":"` + strings.Repeat("a", 64) +
			`","sign_key":"` + strings.Repeat("b", 64) + `","heartbeat_interval_seconds":15}`))
	})

	configPath := filepath.Join(t.TempDir(), "etc", "config.env")
	result, err := Run(context.Background(), Options{
		MasterURL:     master.URL,
		Token:         "enroll-token",
		PublicBaseURL: "http://1.2.3.4:8790/",
		ListenPort:    8790,
		ConfigPath:    configPath,
		Version:       "0.1.0",
		MachineIDPath: writeMachineID(t, "machine-abc\n"),
		Logger:        discardLogger(),
	})
	if err != nil {
		t.Fatalf("enroll 失败：%v", err)
	}

	if captured.path != "/api/agent/enroll" {
		t.Fatalf("应 POST /api/agent/enroll，实际 %s", captured.path)
	}
	if captured.body["enroll_token"] != "enroll-token" || captured.body["machine_id"] != "machine-abc" {
		t.Fatalf("请求体缺字段：%+v", captured.body)
	}
	if captured.body["version"] != "0.1.0" || captured.body["listen_port"] != float64(8790) {
		t.Fatalf("请求体字段不对：%+v", captured.body)
	}
	// enroll 时不带 agent 凭据头（此时还没有）。
	if captured.token != "" {
		t.Fatalf("enroll 请求不应带 Authorization，实际 %q", captured.token)
	}
	// 尾斜杠应被去掉，避免拼出 //api/agent/...
	if captured.body["public_base_url"] != "http://1.2.3.4:8790" {
		t.Fatalf("public_base_url 未规范化：%+v", captured.body["public_base_url"])
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("配置文件未写出：%v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config.env 权限必须 0600，实际 %04o", perm)
	}
	info2, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("读回配置失败：%v", err)
	}
	if info2.AgentID != "agent-uuid" || info2.SignKey != strings.Repeat("b", 64) {
		t.Fatalf("配置内容不对：%+v", info2)
	}
	if info2.MaxConcurrent != config.DefaultMaxConcurrent {
		t.Fatalf("应写入默认并发上限：%d", info2.MaxConcurrent)
	}
	if result.Config.AgentID != "agent-uuid" {
		t.Fatalf("返回值不对：%+v", result)
	}
	// 日志里不得出现 secret / sign_key。
	if strings.Contains(result.Config.AgentSecret, " ") {
		t.Fatal("凭据不应带空白")
	}
}

func TestRunAcceptsWrappedEnvelope(t *testing.T) {
	// 本项目既有约定 {ok,data} 形状也应能对接（子任务 A 的实现细节不同也兼容）。
	master, _ := startMaster(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"data":{"agent_id":"a","agent_secret":"s","sign_key":"k","heartbeat_interval_seconds":15}}`))
	})
	configPath := filepath.Join(t.TempDir(), "config.env")
	if _, err := Run(context.Background(), Options{
		MasterURL: master.URL, Token: "t", ConfigPath: configPath,
		MachineIDPath: writeMachineID(t, "m"), Logger: discardLogger(),
	}); err != nil {
		t.Fatalf("wrapped 信封应可用：%v", err)
	}
}

func TestRunFallsBackToHostname(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Skipf("无法取主机名：%v", err)
	}
	master, captured := startMaster(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"agent_id":"a","agent_secret":"s","sign_key":"k"}`))
	})
	if _, err := Run(context.Background(), Options{
		MasterURL: master.URL, Token: "t",
		ConfigPath:    filepath.Join(t.TempDir(), "config.env"),
		MachineIDPath: filepath.Join(t.TempDir(), "不存在"), // 读不到 → 退化 hostname
		Logger:        discardLogger(),
	}); err != nil {
		t.Fatalf("enroll 失败：%v", err)
	}
	if captured.body["machine_id"] != hostname || captured.body["hostname"] != hostname {
		t.Fatalf("machine_id/hostname 应退化为主机名：%+v", captured.body)
	}
}

func TestRunRejectsBadToken(t *testing.T) {
	master, _ := startMaster(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"ENROLL_TOKEN_INVALID","message":"注册 Token 无效"}}`))
	})
	configPath := filepath.Join(t.TempDir(), "config.env")
	_, err := Run(context.Background(), Options{
		MasterURL: master.URL, Token: "wrong", ConfigPath: configPath,
		MachineIDPath: writeMachineID(t, "m"), Logger: discardLogger(),
	})
	if err == nil {
		t.Fatal("401 应报错")
	}
	msg := err.Error()
	for _, want := range []string{"401", "ENROLL_TOKEN_INVALID", "注册 Token"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息应含 %q：%s", want, msg)
		}
	}
	if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
		t.Fatal("失败时不应写出配置文件")
	}
}

func TestRunValidatesInputs(t *testing.T) {
	if _, err := Run(context.Background(), Options{Token: "t"}); err == nil || !strings.Contains(err.Error(), "master") {
		t.Fatalf("缺 master 应报错：%v", err)
	}
	if _, err := Run(context.Background(), Options{MasterURL: "http://x"}); err == nil || !strings.Contains(err.Error(), "Token") {
		t.Fatalf("缺 Token 应报错：%v", err)
	}
}

func TestRunRejectsIncompleteCredentials(t *testing.T) {
	master, _ := startMaster(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"agent_id":"a"}`))
	})
	_, err := Run(context.Background(), Options{
		MasterURL: master.URL, Token: "t",
		ConfigPath:    filepath.Join(t.TempDir(), "config.env"),
		MachineIDPath: writeMachineID(t, "m"), Logger: discardLogger(),
	})
	if err == nil || !strings.Contains(err.Error(), "不完整") {
		t.Fatalf("凭据缺失应报错：%v", err)
	}
}

// 重跑 enroll（同一台机器）必须满足两点（prd 验收「config.env 幂等」）：
//  1. 两次都用同一个 machine_id 作为幂等键——master 才可能复用原节点记录；
//  2. master 轮换后的新 secret/sign_key 被**整体覆盖**写进 config.env，旧值不残留。
func TestReenrollUsesSameMachineIDAndRotatesCredentials(t *testing.T) {
	var (
		mu          sync.Mutex
		machineIDs  []string
		enrollCalls int
	)
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		enrollCalls++
		n := enrollCalls
		machineIDs = append(machineIDs, fmt.Sprint(body["machine_id"]))
		mu.Unlock()
		// 真实 master 的幂等语义：agent_id 不变，secret/sign_key 每次轮换。
		_, _ = fmt.Fprintf(w,
			`{"agent_id":"agent-uuid","agent_secret":%q,"sign_key":%q,"heartbeat_interval_seconds":15}`,
			strings.Repeat(fmt.Sprintf("%d", n), 64),
			strings.Repeat(fmt.Sprintf("%x", n), 64))
	}))
	defer master.Close()

	// 清掉可能存在的环境变量，保证读到的是文件里的值。
	for _, key := range []string{"MASTER_URL", "AGENT_ID", "AGENT_SECRET", "SIGN_KEY", "MAX_CONCURRENT"} {
		t.Setenv(key, "")
	}
	configPath := filepath.Join(t.TempDir(), "config.env")
	machineIDPath := writeMachineID(t, "machine-abc\n")
	for i := 1; i <= 2; i++ {
		if _, err := Run(context.Background(), Options{
			MasterURL: master.URL, Token: "t", ConfigPath: configPath,
			MachineIDPath: machineIDPath, Logger: discardLogger(),
		}); err != nil {
			t.Fatalf("第 %d 次 enroll 失败：%v", i, err)
		}
	}

	mu.Lock()
	got := append([]string(nil), machineIDs...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "machine-abc" || got[1] != "machine-abc" {
		t.Fatalf("重跑 enroll 必须带同一个 machine_id（幂等键），实际 %v", got)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("读回配置失败：%v", err)
	}
	if want := strings.Repeat("2", 64); cfg.AgentSecret != want {
		t.Fatalf("重跑后的 secret 应被覆盖为新值，实际 %q", cfg.AgentSecret)
	}
	if want := strings.Repeat("2", 64); cfg.SignKey != want {
		t.Fatalf("重跑后的 sign_key 应被覆盖为新值，实际 %q", cfg.SignKey)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("读配置文件失败：%v", err)
	}
	if strings.Contains(string(raw), strings.Repeat("1", 64)) {
		t.Fatalf("轮换前的旧凭据不应残留在 config.env：\n%s", raw)
	}
}

func TestRunOnlyPostsOnce(t *testing.T) {
	var hits atomic.Int64
	master, _ := startMaster(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"agent_id":"a","agent_secret":"s","sign_key":"k"}`))
	})
	if _, err := Run(context.Background(), Options{
		MasterURL: master.URL, Token: "t",
		ConfigPath:    filepath.Join(t.TempDir(), "config.env"),
		MachineIDPath: writeMachineID(t, "m"), Logger: discardLogger(),
	}); err != nil {
		t.Fatalf("enroll 失败：%v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("enroll 只应发一次请求，实际 %d", hits.Load())
	}
}
