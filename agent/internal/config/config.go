// Package config 负责 gd-agent 的配置读写（默认 /etc/gd-agent/config.env）。
//
// 文件格式：每行 `KEY=VALUE`，`#` 开头为注释，值可带单/双引号。
// 文件里存着 agent_secret 与 sign_key（节点的注册凭据与 URL 签名密钥），
// 因此**写入权限固定 0600**；读取侧不做权限校验（属主可能是 systemd 的服务用户）。
//
// 同名环境变量优先于文件（测试与调试用）：MASTER_URL / AGENT_ID / AGENT_SECRET /
// SIGN_KEY / LISTEN_PORT / PUBLIC_BASE_URL / MAX_CONCURRENT。
//
// `MAX_CONCURRENT` 兼容冻结稿 的称呼 `AGENT_MAX_CONCURRENT`——**文件与
// 环境变量两种位置都认**（只认环境变量会让"按冻结稿 写进配置文件"变成一个
// 静默无效的设置，这类无声的配置失配最难查）。
package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// DefaultPath 是 systemd 服务使用的配置路径（安装脚本 enroll 时写入）。
	DefaultPath = "/etc/gd-agent/config.env"
	// DefaultListenPort 是数据面监听端口（非特权端口，无需 root 运行）。
	DefaultListenPort = 8790
	// DefaultMaxConcurrent 是并发流上限（冻结稿 §2.4 的 AGENT_MAX_CONCURRENT）。
	DefaultMaxConcurrent = 32
)

// Config 是 serve 运行所需的全部配置。
type Config struct {
	MasterURL     string
	AgentID       string
	AgentSecret   string
	SignKey       string // 64 位 hex 字符串（32 字节），客户端 URL 签名密钥
	ListenPort    int
	PublicBaseURL string // 可空：为空时 master 按源 IP 推导（冻结稿 §4.2）
	MaxConcurrent int
}

// Load 读取配置文件并叠加同名环境变量。
//
// 文件不存在不算致命：只要环境变量把必填项补齐即可（本地联调/测试用）。
// 必填项缺失时返回中文错误，调用方（serve）应以非零码退出。
func Load(path string) (Config, error) {
	if path == "" {
		path = DefaultPath
	}
	cfg := Config{
		ListenPort:    DefaultListenPort,
		MaxConcurrent: DefaultMaxConcurrent,
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		values, perr := Parse(data)
		if perr != nil {
			return Config{}, fmt.Errorf("配置文件 %s 解析失败：%w", path, perr)
		}
		applyValues(&cfg, values)
	case os.IsNotExist(err):
		// 允许纯环境变量运行（测试/调试）；下面由 Validate 兜底。
	case os.IsPermission(err):
		// 最常见的现场：安装脚本以 root 跑 enroll（文件 0600 root:root），
		// 而 systemd 服务以专用用户运行 → EACCES。给中文提示，别只留英文 errno。
		return Config{}, fmt.Errorf(
			"读取配置文件 %s 失败：%w（权限不足：安装脚本 enroll 后需把配置文件交给服务用户，"+
				"如 `chown gd-agent:gd-agent %s`）", path, err, path)
	default:
		return Config{}, fmt.Errorf("读取配置文件 %s 失败：%w", path, err)
	}
	applyEnv(&cfg)
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("配置不完整（文件 %s，可用环境变量覆盖）：%w", path, err)
	}
	return cfg, nil
}

// Validate 校验 serve 必需的字段。
func (c Config) Validate() error {
	var missing []string
	if c.MasterURL == "" {
		missing = append(missing, "MASTER_URL")
	}
	if c.AgentID == "" {
		missing = append(missing, "AGENT_ID")
	}
	if c.AgentSecret == "" {
		missing = append(missing, "AGENT_SECRET")
	}
	if c.SignKey == "" {
		missing = append(missing, "SIGN_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("缺少必填项：%s（请先运行 gd-agent enroll）", strings.Join(missing, "、"))
	}
	if c.ListenPort <= 0 || c.ListenPort > 65535 {
		return fmt.Errorf("LISTEN_PORT 取值不合法：%d", c.ListenPort)
	}
	if c.MaxConcurrent <= 0 {
		return fmt.Errorf("MAX_CONCURRENT 取值不合法：%d", c.MaxConcurrent)
	}
	return nil
}

// SignKeyBytes 把 hex 字符串的 SIGN_KEY 解码为字节。
// 解码唯一入口——不要让各调用点各自 hex.DecodeString。
func (c Config) SignKeyBytes() ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(c.SignKey))
	if err != nil {
		return nil, fmt.Errorf("SIGN_KEY 不是合法的十六进制字符串：%w", err)
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("SIGN_KEY 为空")
	}
	return key, nil
}

// Write 把配置写入 path（权限固定 0600，父目录按需创建）。
//
// 写入是幂等的：重复 enroll（重跑安装脚本）会整体覆盖，
// 旧 secret/sign_key 不残留。
func Write(path string, c Config) error {
	if path == "" {
		path = DefaultPath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建配置目录失败：%w", err)
	}
	data := []byte("# gd-agent 配置（由 gd-agent enroll 生成；重跑 enroll 会整体覆盖）\n" + c.marshal())
	// 先写临时文件再改名：避免写一半被 serve 读到残缺配置。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入配置失败：%w", err)
	}
	// 显式 chmod：文件已存在（旧版本可能权限更宽）时 WriteFile 不会收紧权限。
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("设置配置权限失败：%w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("保存配置失败：%w", err)
	}
	return nil
}

func (c Config) marshal() string {
	var b strings.Builder
	writeKV := func(k, v string) {
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	writeKV("MASTER_URL", c.MasterURL)
	writeKV("AGENT_ID", c.AgentID)
	writeKV("AGENT_SECRET", c.AgentSecret)
	writeKV("SIGN_KEY", c.SignKey)
	writeKV("LISTEN_PORT", strconv.Itoa(c.ListenPort))
	writeKV("PUBLIC_BASE_URL", c.PublicBaseURL)
	writeKV("MAX_CONCURRENT", strconv.Itoa(c.MaxConcurrent))
	return b.String()
}

// Parse 解析 config.env 内容（KEY=VALUE，`#` 注释，值可带引号）。
func Parse(data []byte) (map[string]string, error) {
	values := make(map[string]string)
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("第 %d 行不是 KEY=VALUE：%q", i+1, line)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("第 %d 行缺少键名：%q", i+1, line)
		}
		values[key] = unquote(strings.TrimSpace(value))
	}
	return values, nil
}

func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

func applyValues(cfg *Config, values map[string]string) {
	set := func(key string, dst *string) {
		if v, ok := values[key]; ok {
			*dst = v
		}
	}
	set("MASTER_URL", &cfg.MasterURL)
	set("AGENT_ID", &cfg.AgentID)
	set("AGENT_SECRET", &cfg.AgentSecret)
	set("SIGN_KEY", &cfg.SignKey)
	set("PUBLIC_BASE_URL", &cfg.PublicBaseURL)
	if v, ok := values["LISTEN_PORT"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ListenPort = n
		}
	}
	// MAX_CONCURRENT 为准；AGENT_MAX_CONCURRENT 是冻结稿 的称呼（与 applyEnv
	// 同一套优先级，避免"文件里写了却静默忽略"）。
	for _, key := range []string{"MAX_CONCURRENT", "AGENT_MAX_CONCURRENT"} {
		if v, ok := values[key]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				cfg.MaxConcurrent = n
			}
			break
		}
	}
}

// applyEnv 叠加同名环境变量；空值视为未设置（否则 systemd 的
// `Environment=MASTER_URL=` 会静默清空配置）。
func applyEnv(cfg *Config) {
	override := func(key string, dst *string) {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			*dst = v
		}
	}
	override("MASTER_URL", &cfg.MasterURL)
	override("AGENT_ID", &cfg.AgentID)
	override("AGENT_SECRET", &cfg.AgentSecret)
	override("SIGN_KEY", &cfg.SignKey)
	override("PUBLIC_BASE_URL", &cfg.PublicBaseURL)
	if v := strings.TrimSpace(os.Getenv("LISTEN_PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ListenPort = n
		}
	}
	// MAX_CONCURRENT 优先；兼容冻结稿 的 AGENT_MAX_CONCURRENT 称呼。
	for _, key := range []string{"MAX_CONCURRENT", "AGENT_MAX_CONCURRENT"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				cfg.MaxConcurrent = n
			}
			break
		}
	}
}
