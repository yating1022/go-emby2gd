package enroll

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yating1022/go-emby2gd/agent/internal/config"
)

func readFileString(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
}

// ROLE 在 enroll 环节的契约（任务 10-10-hub-agent-mode S3）：
//   - node（缺省）请求体不带 role 字段、配置不写 ROLE 行——与 v0.3.2 逐字节一致；
//   - hub 请求体带 "role":"hub"，配置写 ROLE 行与全部 hub 键；
//   - 非法角色在发请求前就被拒绝。

// clearEnrollEnv 清掉会干扰断言的环境变量（Load 会读它们）。
func clearEnrollEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"MASTER_URL", "AGENT_ID", "AGENT_SECRET", "SIGN_KEY", "ROLE",
		"HUB_PORT", "HUB_ALLOW_IPS", "DISK_CACHE_DIR", "DISK_BUDGET_GB",
		"WARM_HEAD_BYTES", "WARM_TAIL_BYTES", "WARM_RESUME_WINDOW_BYTES",
		"LISTEN_PORT", "CACHE_MAX_AGE_MINUTES",
	} {
		t.Setenv(key, "")
	}
}

func TestRunNodeOmitsRoleField(t *testing.T) {
	clearEnrollEnv(t)
	master, captured := startMaster(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"agent_id":"a","agent_secret":"s","sign_key":"k"}`))
	})
	configPath := filepath.Join(t.TempDir(), "config.env")
	if _, err := Run(context.Background(), Options{
		MasterURL: master.URL, Token: "t", ConfigPath: configPath,
		MachineIDPath: writeMachineID(t, "m"), Logger: discardLogger(),
	}); err != nil {
		t.Fatalf("node enroll 失败：%v", err)
	}
	if _, present := captured.body["role"]; present {
		t.Fatalf("node 的 enroll 请求不应带 role 字段：%+v", captured.body)
	}
	raw, err := readFileString(configPath)
	if err != nil {
		t.Fatalf("读配置失败：%v", err)
	}
	if strings.Contains(raw, "ROLE") || strings.Contains(raw, "HUB_") || strings.Contains(raw, "DISK_") {
		t.Fatalf("node 配置不应写 hub 键：\n%s", raw)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("读回配置失败：%v", err)
	}
	if cfg.IsHub() {
		t.Fatalf("node enroll 后不应是 hub：%+v", cfg)
	}
	// 显式 --role node 与缺省同效。
	if _, err := Run(context.Background(), Options{
		MasterURL: master.URL, Token: "t", Role: "node", ConfigPath: configPath,
		MachineIDPath: writeMachineID(t, "m"), Logger: discardLogger(),
	}); err != nil {
		t.Fatalf("显式 node enroll 失败：%v", err)
	}
	if _, present := captured.body["role"]; present {
		t.Fatalf("显式 --role node 的请求也不应带 role 字段：%+v", captured.body)
	}
}

func TestRunHubCarriesRoleAndDefaults(t *testing.T) {
	clearEnrollEnv(t)
	master, captured := startMaster(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"agent_id":"hub-uuid","agent_secret":"s","sign_key":"k"}`))
	})
	configPath := filepath.Join(t.TempDir(), "config.env")
	if _, err := Run(context.Background(), Options{
		MasterURL: master.URL, Token: "t", Role: "  HuB ", ConfigPath: configPath,
		MachineIDPath: writeMachineID(t, "hub-machine"), Logger: discardLogger(),
	}); err != nil {
		t.Fatalf("hub enroll 失败：%v", err)
	}
	if captured.body["role"] != "hub" {
		t.Fatalf("hub 的 enroll 请求应带 role=hub：%+v", captured.body)
	}
	if captured.body["listen_port"] != float64(config.DefaultHubPort) {
		t.Fatalf("hub 缺省端口应为 %d，实际 %v", config.DefaultHubPort, captured.body["listen_port"])
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("读回配置失败：%v", err)
	}
	if !cfg.IsHub() || cfg.Role != config.RoleHub {
		t.Fatalf("hub enroll 后 Load 应为 hub：%+v", cfg)
	}
	if cfg.HubPort != config.DefaultHubPort || cfg.ListenPort != config.DefaultHubPort {
		t.Fatalf("HUB_PORT 与 LISTEN_PORT 应同值（%d）：%+v", config.DefaultHubPort, cfg)
	}
	if cfg.CacheMaxAgeMinutes != config.DefaultHubCacheMaxAgeMinutes {
		t.Fatalf("hub 的 CACHE_MAX_AGE_MINUTES 应为 %d，实际 %d",
			config.DefaultHubCacheMaxAgeMinutes, cfg.CacheMaxAgeMinutes)
	}
	if cfg.DiskCacheDir != config.DefaultDiskCacheDir || cfg.DiskBudgetGB != config.DefaultDiskBudgetGB ||
		cfg.WarmHeadBytes != config.DefaultWarmHeadBytes {
		t.Fatalf("hub 默认值未写入：%+v", cfg)
	}
	// 白名单刻意留空（fail-closed，等管理员填）；但键必须出现在文件里。
	raw, err := readFileString(configPath)
	if err != nil {
		t.Fatalf("读配置失败：%v", err)
	}
	if !strings.Contains(raw, "HUB_ALLOW_IPS=\n") {
		t.Fatalf("hub 配置应留出 HUB_ALLOW_IPS 空行给管理员：\n%s", raw)
	}
	if cfg.HubAllowIPs != "" {
		t.Fatalf("HUB_ALLOW_IPS 缺省应为空（fail-closed），实际 %q", cfg.HubAllowIPs)
	}
}

// 显式 --port 优先于角色缺省（hub 下 8791 只是缺省）。
func TestRunHubExplicitPortWins(t *testing.T) {
	clearEnrollEnv(t)
	master, captured := startMaster(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"agent_id":"a","agent_secret":"s","sign_key":"k"}`))
	})
	configPath := filepath.Join(t.TempDir(), "config.env")
	if _, err := Run(context.Background(), Options{
		MasterURL: master.URL, Token: "t", Role: "hub", ListenPort: 9000,
		ConfigPath: configPath, MachineIDPath: writeMachineID(t, "m"), Logger: discardLogger(),
	}); err != nil {
		t.Fatalf("hub enroll 失败：%v", err)
	}
	if captured.body["listen_port"] != float64(9000) {
		t.Fatalf("显式端口应优先：%+v", captured.body)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("读回配置失败：%v", err)
	}
	if cfg.HubPort != 9000 || cfg.ListenPort != 9000 {
		t.Fatalf("配置里 HUB_PORT/LISTEN_PORT 都应为 9000：%+v", cfg)
	}
}

// 非法角色必须在发请求之前被拒绝（不能先注册出半截节点再报错）。
func TestRunRejectsInvalidRoleBeforePosting(t *testing.T) {
	clearEnrollEnv(t)
	var hits atomic.Int64
	master, _ := startMaster(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"agent_id":"a","agent_secret":"s","sign_key":"k"}`))
	})
	_, err := Run(context.Background(), Options{
		MasterURL: master.URL, Token: "t", Role: "worker",
		ConfigPath:    filepath.Join(t.TempDir(), "config.env"),
		MachineIDPath: writeMachineID(t, "m"), Logger: discardLogger(),
	})
	if err == nil {
		t.Fatal("非法 --role 应被拒绝")
	}
	if !strings.Contains(err.Error(), "role") || !strings.Contains(err.Error(), "hub") {
		t.Fatalf("错误信息应点名 --role 与合法值，实际：%v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("非法角色不应向 master 发请求，实际 %d 次", hits.Load())
	}
}
