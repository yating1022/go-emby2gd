package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ROLE 与 hub 专属键的测试（任务 10-10-hub-agent-mode S3）。
// 核心契约：
//   - 缺省 ROLE = node：Load→Write 往返与 v0.3.2 逐字节一致（不写 ROLE 行、不写 hub 键）；
//   - node 模式**不读取**任何 hub 专属键（残留行忽略，Config 字段保持零值）；
//   - hub 模式读全部 hub 键并套默认值；hub 专属键只在 hub 角色下校验。

// clearHubEnv 把本组测试关心的环境变量清成"未设置"语义（空值），
// 避免开发机/CI 里残留的 ROLE、HUB_PORT 之类同名变量影响断言。
func clearHubEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"ROLE", "HUB_PORT", "HUB_ALLOW_IPS", "DISK_CACHE_DIR", "DISK_BUDGET_GB",
		"WARM_HEAD_BYTES", "WARM_TAIL_BYTES", "WARM_RESUME_WINDOW_BYTES",
		"CACHE_MAX_AGE_MINUTES",
	} {
		t.Setenv(key, "")
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("准备配置失败：%v", err)
	}
	return path
}

// node 配置文件（无 ROLE）写回来必须与 v0.3.2 的 marshal 输出逐字节一致。
func TestNodeMarshalByteIdentical(t *testing.T) {
	clearHubEnv(t)
	path := filepath.Join(t.TempDir(), "config.env")
	cfg := Config{
		MasterURL: "http://m", AgentID: "a", AgentSecret: "s", SignKey: "k",
		ListenPort: DefaultListenPort, MaxConcurrent: DefaultMaxConcurrent,
		CacheBudgetMB: DefaultCacheBudgetMB, CacheMaxAgeMinutes: DefaultCacheMaxAgeMinutes,
		PrefetchHeadMB: DefaultPrefetchHeadMB, PrefetchTailMB: DefaultPrefetchTailMB,
	}
	if cfg.IsHub() {
		t.Fatal("零值角色不应是 hub")
	}
	if err := Write(path, cfg); err != nil {
		t.Fatalf("Write 失败：%v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读配置失败：%v", err)
	}
	expected := "# gd-agent 配置（由 gd-agent enroll 生成；重跑 enroll 会整体覆盖）\n" +
		"MASTER_URL=http://m\n" +
		"AGENT_ID=a\n" +
		"AGENT_SECRET=s\n" +
		"SIGN_KEY=k\n" +
		"LISTEN_PORT=8790\n" +
		"PUBLIC_BASE_URL=\n" +
		"MAX_CONCURRENT=32\n" +
		"CACHE_BUDGET_MB=256\n" +
		"CACHE_MAX_AGE_MINUTES=1440\n" +
		"PREFETCH_HEAD_MB=32\n" +
		"PREFETCH_TAIL_MB=4\n"
	if string(raw) != expected {
		t.Fatalf("node 模式配置内容与 v0.3.2 不再逐字节一致：\n--- got ---\n%s\n--- want ---\n%s", raw, expected)
	}
	for _, key := range []string{"ROLE", "HUB_PORT", "HUB_ALLOW_IPS", "DISK_CACHE_DIR", "DISK_BUDGET_GB", "WARM_"} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("node 模式不应写出 hub 键 %s：\n%s", key, raw)
		}
	}
}

// node 模式不读取 hub 专属键：残留行忽略，字段保持零值，也不影响启动校验。
func TestNodeIgnoresHubKeys(t *testing.T) {
	clearHubEnv(t)
	path := writeConfig(t, "MASTER_URL=http://m\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\n"+
		"HUB_PORT=9999\nHUB_ALLOW_IPS=10.0.0.0/8\nDISK_CACHE_DIR=/data/x\nDISK_BUDGET_GB=1\n"+
		"WARM_HEAD_BYTES=1\nWARM_TAIL_BYTES=2\nWARM_RESUME_WINDOW_BYTES=3\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if cfg.Role != "" || cfg.IsHub() {
		t.Fatalf("无 ROLE 时角色应为缺省 node，实际 %q", cfg.Role)
	}
	if cfg.HubPort != 0 || cfg.HubAllowIPs != "" || cfg.DiskCacheDir != "" ||
		cfg.DiskBudgetGB != 0 || cfg.WarmHeadBytes != 0 || cfg.WarmTailBytes != 0 || cfg.WarmResumeWindowBytes != 0 {
		t.Fatalf("node 模式不应读取 hub 专属键：%+v", cfg)
	}
	if port := cfg.ServePort(); port != cfg.ListenPort {
		t.Fatalf("node 的 ServePort 应为 LISTEN_PORT，实际 %d", port)
	}
}

// ROLE 大小写不敏感；环境变量优先于文件（与其它键同一优先级）。
func TestRoleNormalizationAndEnvPrecedence(t *testing.T) {
	clearHubEnv(t)
	path := writeConfig(t, "MASTER_URL=http://m\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\nROLE=  HuB \n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if !cfg.IsHub() || cfg.Role != RoleHub {
		t.Fatalf("ROLE 应大小写不敏感并归一化为 hub，实际 %q", cfg.Role)
	}

	// 文件 hub + env node → node（env 优先）。
	t.Setenv("ROLE", "node")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if cfg.IsHub() {
		t.Fatalf("env ROLE=node 应压过文件的 hub，实际 %q", cfg.Role)
	}

	// 文件 node + env hub → hub。
	t.Setenv("ROLE", "HUB")
	path = writeConfig(t, "MASTER_URL=http://m\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\nROLE=node\n")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if !cfg.IsHub() {
		t.Fatalf("env ROLE=HUB 应压过文件的 node，实际 %q", cfg.Role)
	}
}

// hub 缺省的默认值必须与 master 侧约定一致（新增键全在这里钉住）。
func TestHubDefaults(t *testing.T) {
	clearHubEnv(t)
	path := writeConfig(t, "MASTER_URL=http://m\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\nROLE=hub\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if cfg.HubPort != 8791 {
		t.Fatalf("HUB_PORT 默认应为 8791（master 侧 hub-port 约定），实际 %d", cfg.HubPort)
	}
	if cfg.DiskCacheDir != "/home/ge2o-hub-cache" {
		t.Fatalf("DISK_CACHE_DIR 默认不符：%q", cfg.DiskCacheDir)
	}
	if cfg.DiskBudgetGB != 200 {
		t.Fatalf("DISK_BUDGET_GB 默认应为 200，实际 %d", cfg.DiskBudgetGB)
	}
	if cfg.CacheMaxAgeMinutes != 2880 {
		t.Fatalf("hub 的 CACHE_MAX_AGE_MINUTES 默认应为 2880（48h），实际 %d", cfg.CacheMaxAgeMinutes)
	}
	if cfg.WarmHeadBytes != 128<<20 || cfg.WarmTailBytes != 4<<20 || cfg.WarmResumeWindowBytes != 4<<20 {
		t.Fatalf("预热区域默认不符：head=%d tail=%d resume=%d",
			cfg.WarmHeadBytes, cfg.WarmTailBytes, cfg.WarmResumeWindowBytes)
	}
	if port := cfg.ServePort(); port != 8791 {
		t.Fatalf("hub 的 ServePort 应为 HUB_PORT，实际 %d", port)
	}
}

// CACHE_MAX_AGE_MINUTES：文件显式值 > hub 缺省；env 显式值 > 文件。
func TestHubCacheMaxAgeExplicitWins(t *testing.T) {
	clearHubEnv(t)
	path := writeConfig(t, "MASTER_URL=http://m\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\n"+
		"ROLE=hub\nCACHE_MAX_AGE_MINUTES=60\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if cfg.CacheMaxAgeMinutes != 60 {
		t.Fatalf("文件显式值应优先于 hub 缺省，实际 %d", cfg.CacheMaxAgeMinutes)
	}
	t.Setenv("CACHE_MAX_AGE_MINUTES", "30")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if cfg.CacheMaxAgeMinutes != 30 {
		t.Fatalf("env 显式值应优先，实际 %d", cfg.CacheMaxAgeMinutes)
	}
}

// hub 键覆盖：文件与环境变量都要生效（含 int64 键）。
func TestHubKeysFromFileAndEnv(t *testing.T) {
	clearHubEnv(t)
	path := writeConfig(t, "MASTER_URL=http://m\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\n"+
		"ROLE=hub\nHUB_PORT=9000\nHUB_ALLOW_IPS=10.0.0.1, 192.168.0.0/16\n"+
		"DISK_CACHE_DIR=/data/hub-cache\nDISK_BUDGET_GB=500\n"+
		"WARM_HEAD_BYTES=268435456\nWARM_TAIL_BYTES=8388608\nWARM_RESUME_WINDOW_BYTES=16777216\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if cfg.HubPort != 9000 || cfg.HubAllowIPs != "10.0.0.1, 192.168.0.0/16" ||
		cfg.DiskCacheDir != "/data/hub-cache" || cfg.DiskBudgetGB != 500 ||
		cfg.WarmHeadBytes != 268435456 || cfg.WarmTailBytes != 8388608 || cfg.WarmResumeWindowBytes != 16777216 {
		t.Fatalf("文件里的 hub 键未全部生效：%+v", cfg)
	}

	t.Setenv("HUB_PORT", "9001")
	t.Setenv("HUB_ALLOW_IPS", "127.0.0.1")
	t.Setenv("DISK_CACHE_DIR", "/data/other")
	t.Setenv("DISK_BUDGET_GB", "10")
	t.Setenv("WARM_HEAD_BYTES", "1")
	t.Setenv("WARM_TAIL_BYTES", "0")
	t.Setenv("WARM_RESUME_WINDOW_BYTES", "0")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if cfg.HubPort != 9001 || cfg.HubAllowIPs != "127.0.0.1" || cfg.DiskCacheDir != "/data/other" ||
		cfg.DiskBudgetGB != 10 || cfg.WarmHeadBytes != 1 || cfg.WarmTailBytes != 0 || cfg.WarmResumeWindowBytes != 0 {
		t.Fatalf("环境变量未覆盖 hub 键：%+v", cfg)
	}
}

// 非法 ROLE：Load 与 Validate 都要给中文错误（点名 ROLE 与合法值）。
func TestInvalidRoleRejected(t *testing.T) {
	clearHubEnv(t)
	path := writeConfig(t, "MASTER_URL=http://m\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\nROLE=worker\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("非法 ROLE 应被拒绝")
	}
	if !strings.Contains(err.Error(), "ROLE") || !strings.Contains(err.Error(), "node") || !strings.Contains(err.Error(), "hub") {
		t.Fatalf("错误信息应点名 ROLE 与合法值，实际：%v", err)
	}

	cfg := Config{MasterURL: "m", AgentID: "a", AgentSecret: "s", SignKey: "k", ListenPort: 8790, MaxConcurrent: 1, Role: "Worker"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate 应拒绝非法角色取值")
	}
	if !ValidRole("") || !ValidRole("node") || !ValidRole("HUB") || ValidRole("worker") {
		t.Fatal("ValidRole 判定不符：空/node/HUB 合法，worker 非法")
	}
}

// hub 专属键只在 hub 角色下校验：node 里的残留值不会让服务起不来（已由
// TestNodeIgnoresHubKeys 覆盖读取侧）；hub 下的非法值必须被拒绝。
func TestHubValidation(t *testing.T) {
	clearHubEnv(t)
	base := "MASTER_URL=http://m\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\nROLE=hub\n"
	cases := []struct {
		extra string
		want  string
	}{
		{"HUB_PORT=0\n", "HUB_PORT"},
		{"HUB_PORT=70000\n", "HUB_PORT"},
		{"DISK_BUDGET_GB=0\n", "DISK_BUDGET_GB"},
		{"DISK_CACHE_DIR=relative/cache\n", "DISK_CACHE_DIR"},
		{"WARM_HEAD_BYTES=-1\n", "WARM_HEAD_BYTES"},
		{"WARM_TAIL_BYTES=-1\n", "WARM_TAIL_BYTES"},
		{"WARM_RESUME_WINDOW_BYTES=-1\n", "WARM_RESUME_WINDOW_BYTES"},
	}
	for _, tc := range cases {
		path := writeConfig(t, base+tc.extra)
		_, err := Load(path)
		if err == nil {
			t.Fatalf("%s 应被拒绝", tc.extra)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("错误信息应点名 %s，实际：%v", tc.want, err)
		}
	}
}

// hub 配置全键往返：Write 写出 ROLE 行与全部 hub 键，Load 后与写入值全等。
func TestHubRoundTrip(t *testing.T) {
	clearHubEnv(t)
	path := filepath.Join(t.TempDir(), "config.env")
	cfg := Config{
		MasterURL: "http://m", AgentID: "a", AgentSecret: "s", SignKey: "k",
		ListenPort: DefaultListenPort, MaxConcurrent: DefaultMaxConcurrent,
		Role:                  RoleHub,
		CacheBudgetMB:         DefaultCacheBudgetMB,
		CacheMaxAgeMinutes:    DefaultHubCacheMaxAgeMinutes,
		PrefetchHeadMB:        DefaultPrefetchHeadMB,
		PrefetchTailMB:        DefaultPrefetchTailMB,
		HubPort:               DefaultHubPort,
		HubAllowIPs:           "10.0.0.1, 192.168.1.0/24",
		DiskCacheDir:          DefaultDiskCacheDir,
		DiskBudgetGB:          DefaultDiskBudgetGB,
		WarmHeadBytes:         DefaultWarmHeadBytes,
		WarmTailBytes:         DefaultWarmTailBytes,
		WarmResumeWindowBytes: DefaultWarmResumeWindowBytes,
	}
	if err := Write(path, cfg); err != nil {
		t.Fatalf("Write 失败：%v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读配置失败：%v", err)
	}
	for _, want := range []string{
		"ROLE=hub", "HUB_PORT=" + strconv.Itoa(DefaultHubPort), "HUB_ALLOW_IPS=10.0.0.1, 192.168.1.0/24",
		"DISK_CACHE_DIR=" + DefaultDiskCacheDir, "DISK_BUDGET_GB=" + strconv.Itoa(DefaultDiskBudgetGB),
		"WARM_HEAD_BYTES=" + strconv.Itoa(DefaultWarmHeadBytes),
		"CACHE_MAX_AGE_MINUTES=" + strconv.Itoa(DefaultHubCacheMaxAgeMinutes),
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("hub 配置应写出 %s：\n%s", want, raw)
		}
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if got != cfg {
		t.Fatalf("hub 往返不一致：\n得到 %+v\n期望 %+v", got, cfg)
	}
}

// hub 的 HUB_ALLOW_IPS 默认留空（fail-closed：空 = 全部拒绝），
// enroll 写出的配置文件里该行应为空值等管理员填写。
func TestHubAllowIPsDefaultsEmpty(t *testing.T) {
	clearHubEnv(t)
	path := writeConfig(t, "MASTER_URL=http://m\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\nROLE=hub\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if cfg.HubAllowIPs != "" {
		t.Fatalf("HUB_ALLOW_IPS 缺省应为空（fail-closed），实际 %q", cfg.HubAllowIPs)
	}
}
