package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteThenLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gd-agent", "config.env")
	cfg := Config{
		MasterURL:     "http://master.example:8000",
		AgentID:       "agent-uuid-1",
		AgentSecret:   strings.Repeat("a", 64),
		SignKey:       strings.Repeat("b", 64),
		ListenPort:    8790,
		PublicBaseURL: "http://1.2.3.4:8790",
		MaxConcurrent: 16,
	}
	if err := Write(path, cfg); err != nil {
		t.Fatalf("Write 失败：%v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat 失败：%v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("配置权限应为 0600，实际 %04o", perm)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if got != cfg {
		t.Fatalf("往返不一致：\n得到 %+v\n期望 %+v", got, cfg)
	}
}

func TestWriteIsIdempotentAndOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.env")
	first := Config{MasterURL: "http://a", AgentID: "1", AgentSecret: "s", SignKey: "k", ListenPort: 8790, MaxConcurrent: 32}
	if err := Write(path, first); err != nil {
		t.Fatalf("第一次 Write 失败：%v", err)
	}
	second := Config{MasterURL: "http://b", AgentID: "2", AgentSecret: "s2", SignKey: "k2", ListenPort: 9000, MaxConcurrent: 4}
	if err := Write(path, second); err != nil {
		t.Fatalf("第二次 Write 失败：%v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile 失败：%v", err)
	}
	if strings.Contains(string(raw), "s2") && strings.Contains(string(raw), "s") && strings.Count(string(raw), "AGENT_SECRET") != 1 {
		t.Fatalf("重复写入不应残留旧凭据：\n%s", raw)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if got.AgentID != "2" || got.MasterURL != "http://b" {
		t.Fatalf("旧值未整体覆盖：%+v", got)
	}
}

func TestWriteTightensExistingPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.env")
	if err := os.WriteFile(path, []byte("MASTER_URL=http://old\n"), 0o644); err != nil {
		t.Fatalf("准备文件失败：%v", err)
	}
	if err := Write(path, Config{MasterURL: "http://new"}); err != nil {
		t.Fatalf("Write 失败：%v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat 失败：%v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("已存在文件应被收紧到 0600，实际 %04o", perm)
	}
}

func TestLoadMissingFileWithoutEnvFails(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.env"))
	if err == nil {
		t.Fatal("缺文件且无环境变量时应报错")
	}
	msg := err.Error()
	for _, want := range []string{"MASTER_URL", "AGENT_ID", "AGENT_SECRET", "SIGN_KEY", "enroll"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息应包含 %q，实际：%s", want, msg)
		}
	}
}

func TestLoadAllowsEnvOnly(t *testing.T) {
	t.Setenv("MASTER_URL", "http://env-master")
	t.Setenv("AGENT_ID", "env-agent")
	t.Setenv("AGENT_SECRET", "env-secret")
	t.Setenv("SIGN_KEY", "env-key")
	t.Setenv("LISTEN_PORT", "9100")
	t.Setenv("MAX_CONCURRENT", "8")

	cfg, err := Load(filepath.Join(t.TempDir(), "missing.env"))
	if err != nil {
		t.Fatalf("纯环境变量应可用：%v", err)
	}
	if cfg.MasterURL != "http://env-master" || cfg.AgentID != "env-agent" || cfg.ListenPort != 9100 || cfg.MaxConcurrent != 8 {
		t.Fatalf("环境变量覆盖未生效：%+v", cfg)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.env")
	if err := Write(path, Config{
		MasterURL: "http://file", AgentID: "file-agent", AgentSecret: "s", SignKey: "k",
		ListenPort: 8790, MaxConcurrent: 32,
	}); err != nil {
		t.Fatalf("Write 失败：%v", err)
	}
	t.Setenv("MASTER_URL", "http://env")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if cfg.MasterURL != "http://env" {
		t.Fatalf("环境变量应覆盖文件，实际 %q", cfg.MasterURL)
	}
	if cfg.AgentID != "file-agent" {
		t.Fatalf("未覆盖的字段应保留文件值，实际 %q", cfg.AgentID)
	}
}

func TestEnvMaxConcurrentAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.env")
	if err := Write(path, Config{MasterURL: "http://m", AgentID: "a", AgentSecret: "s", SignKey: "k", ListenPort: 8790, MaxConcurrent: 32}); err != nil {
		t.Fatalf("Write 失败：%v", err)
	}
	t.Setenv("AGENT_MAX_CONCURRENT", "5")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if cfg.MaxConcurrent != 5 {
		t.Fatalf("AGENT_MAX_CONCURRENT 别名未生效，实际 %d", cfg.MaxConcurrent)
	}
}

func TestEmptyEnvValueDoesNotClearFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.env")
	if err := Write(path, Config{MasterURL: "http://file", AgentID: "a", AgentSecret: "s", SignKey: "k", ListenPort: 8790, MaxConcurrent: 32}); err != nil {
		t.Fatalf("Write 失败：%v", err)
	}
	t.Setenv("MASTER_URL", "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if cfg.MasterURL != "http://file" {
		t.Fatalf("空环境变量不应清空文件值，实际 %q", cfg.MasterURL)
	}
}

// 冻结稿 把该开关称作 AGENT_MAX_CONCURRENT：写进配置文件也必须生效，
// 否则会是一个"设置了却静默无效"的失配。
func TestFileMaxConcurrentAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.env")
	body := "MASTER_URL=http://m\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\n" +
		"LISTEN_PORT=8790\nAGENT_MAX_CONCURRENT=7\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("准备配置失败：%v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if cfg.MaxConcurrent != 7 {
		t.Fatalf("AGENT_MAX_CONCURRENT 写进文件应生效，实际 %d", cfg.MaxConcurrent)
	}
}

// 两个键同时出现时，配置文件的规范名 MAX_CONCURRENT 优先（与 applyEnv 同一规则）。
func TestFileMaxConcurrentCanonicalWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.env")
	body := "MASTER_URL=http://m\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\n" +
		"MAX_CONCURRENT=5\nAGENT_MAX_CONCURRENT=7\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("准备配置失败：%v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if cfg.MaxConcurrent != 5 {
		t.Fatalf("MAX_CONCURRENT 应优先，实际 %d", cfg.MaxConcurrent)
	}
}

// 权限不足是最常见的现场（enroll 以 root 写 0600，服务以专用用户运行）：
// 错误必须是中文并给出 chown 指引，而不是只留一个英文 errno。
func TestLoadPermissionDeniedGivesChineseHint(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 运行时权限位不生效")
	}
	path := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(path, []byte("MASTER_URL=http://m\n"), 0o000); err != nil {
		t.Fatalf("准备配置失败：%v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("无读权限应报错")
	}
	if !strings.Contains(err.Error(), "chown") || !strings.Contains(err.Error(), "权限不足") {
		t.Fatalf("权限错误应给中文提示（含 chown 指引），实际：%v", err)
	}
}

func TestParseHandlesCommentsQuotesAndCRLF(t *testing.T) {
	values, err := Parse([]byte("# 注释\r\nMASTER_URL=\"http://m\"\r\n\r\nLISTEN_PORT='9000'\r\n"))
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if values["MASTER_URL"] != "http://m" || values["LISTEN_PORT"] != "9000" {
		t.Fatalf("解析结果不对：%+v", values)
	}
}

func TestParseRejectsBadLine(t *testing.T) {
	if _, err := Parse([]byte("MASTER_URL=http://m\n这不是一行配置\n")); err == nil {
		t.Fatal("缺 = 的行应报错")
	}
}

func TestSignKeyBytes(t *testing.T) {
	cfg := Config{SignKey: strings.Repeat("0a", 32)}
	key, err := cfg.SignKeyBytes()
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	if len(key) != 32 {
		t.Fatalf("应为 32 字节，实际 %d", len(key))
	}
	if _, err := (Config{SignKey: "zz"}).SignKeyBytes(); err == nil {
		t.Fatal("非法 hex 应报错")
	}
	if _, err := (Config{}).SignKeyBytes(); err == nil {
		t.Fatal("空 SIGN_KEY 应报错")
	}
}

// --- 读前缓存三键 -------------------------------------------------------------

func TestCacheKeysDefaultWhenAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.env")
	body := "MASTER_URL=http://m\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("准备配置失败：%v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if cfg.CacheBudgetMB != DefaultCacheBudgetMB || cfg.CacheMaxAgeMinutes != DefaultCacheMaxAgeMinutes ||
		cfg.PrefetchHeadMB != DefaultPrefetchHeadMB || cfg.PrefetchTailMB != DefaultPrefetchTailMB {
		t.Fatalf("缺省时应回落默认值，实际 %+v", cfg)
	}
	if DefaultCacheBudgetMB != 256 || DefaultCacheMaxAgeMinutes != 1440 ||
		DefaultPrefetchHeadMB != 32 || DefaultPrefetchTailMB != 4 {
		t.Fatalf("默认值不符合设计：%d/%d/%d/%d",
			DefaultCacheBudgetMB, DefaultCacheMaxAgeMinutes, DefaultPrefetchHeadMB, DefaultPrefetchTailMB)
	}
}

func TestCacheKeysFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.env")
	body := "MASTER_URL=http://m\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\n" +
		"CACHE_BUDGET_MB=512\nCACHE_MAX_AGE_MINUTES=60\nPREFETCH_HEAD_MB=64\nPREFETCH_TAIL_MB=8\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("准备配置失败：%v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if cfg.CacheBudgetMB != 512 || cfg.CacheMaxAgeMinutes != 60 || cfg.PrefetchHeadMB != 64 || cfg.PrefetchTailMB != 8 {
		t.Fatalf("文件里的四键未生效：%+v", cfg)
	}
}

func TestCacheKeysFromEnvOverrideFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.env")
	body := "MASTER_URL=http://m\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\n" +
		"CACHE_BUDGET_MB=512\nCACHE_MAX_AGE_MINUTES=60\nPREFETCH_HEAD_MB=64\nPREFETCH_TAIL_MB=8\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("准备配置失败：%v", err)
	}
	t.Setenv("CACHE_BUDGET_MB", "0")
	t.Setenv("CACHE_MAX_AGE_MINUTES", "30")
	t.Setenv("PREFETCH_HEAD_MB", "16")
	t.Setenv("PREFETCH_TAIL_MB", "0")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	// 0 是合法取值（关闭），必须能与"未设置"区分开。
	if cfg.CacheBudgetMB != 0 || cfg.CacheMaxAgeMinutes != 30 || cfg.PrefetchHeadMB != 16 || cfg.PrefetchTailMB != 0 {
		t.Fatalf("环境变量应覆盖文件：%+v", cfg)
	}
}

func TestCacheKeysRejectNegative(t *testing.T) {
	for _, tc := range []struct {
		key string
	}{
		{"CACHE_BUDGET_MB"},
		{"CACHE_MAX_AGE_MINUTES"},
		{"PREFETCH_HEAD_MB"},
		{"PREFETCH_TAIL_MB"},
	} {
		path := filepath.Join(t.TempDir(), "config.env")
		body := "MASTER_URL=http://m\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\n" + tc.key + "=-1\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("准备配置失败：%v", err)
		}
		_, err := Load(path)
		if err == nil {
			t.Fatalf("%s=-1 应被拒绝", tc.key)
		}
		if !strings.Contains(err.Error(), tc.key) {
			t.Fatalf("错误信息应点名 %s，实际：%v", tc.key, err)
		}
	}
}

// 非整数取值（如手写成 "512M"）：与既有键（LISTEN_PORT / MAX_CONCURRENT）的既定
// 语义一致——忽略该行、保留默认，而不是静默取一个别的值或让 serve 起不来。
// 能解析但非法的负值则必须拒绝（见 TestCacheKeysRejectNegative）。
func TestCacheKeysIgnoreNonNumericValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.env")
	body := "MASTER_URL=http://m\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\n" +
		"CACHE_BUDGET_MB=512M\nCACHE_MAX_AGE_MINUTES=1h\nPREFETCH_HEAD_MB=abc\nPREFETCH_TAIL_MB=4 \n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("准备配置失败：%v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if cfg.CacheBudgetMB != DefaultCacheBudgetMB || cfg.CacheMaxAgeMinutes != DefaultCacheMaxAgeMinutes ||
		cfg.PrefetchHeadMB != DefaultPrefetchHeadMB {
		t.Fatalf("非法取值应回落默认：%+v", cfg)
	}
	if cfg.PrefetchTailMB != 4 {
		t.Fatalf("合法取值（含尾随空白）应生效：%+v", cfg)
	}
}

func TestWriteIncludesCacheKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.env")
	cfg := Config{
		MasterURL: "http://m", AgentID: "a", AgentSecret: "s", SignKey: "k",
		ListenPort: 8790, MaxConcurrent: 32,
		CacheBudgetMB: DefaultCacheBudgetMB, CacheMaxAgeMinutes: DefaultCacheMaxAgeMinutes,
		PrefetchHeadMB: DefaultPrefetchHeadMB, PrefetchTailMB: DefaultPrefetchTailMB,
	}
	if err := Write(path, cfg); err != nil {
		t.Fatalf("Write 失败：%v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读配置失败：%v", err)
	}
	for _, want := range []string{
		"CACHE_BUDGET_MB=256", "CACHE_MAX_AGE_MINUTES=1440",
		"PREFETCH_HEAD_MB=32", "PREFETCH_TAIL_MB=4",
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("config.env 应写出 %s（管理员照着改才不用翻文档）：\n%s", want, raw)
		}
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if got != cfg {
		t.Fatalf("含缓存键的往返不一致：\n得到 %+v\n期望 %+v", got, cfg)
	}
}
