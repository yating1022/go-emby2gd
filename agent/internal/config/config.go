// Package config 负责 gd-agent 的配置读写（默认 /etc/gd-agent/config.env）。
//
// 文件格式：每行 `KEY=VALUE`，`#` 开头为注释，值可带单/双引号。
// 文件里存着 agent_secret 与 sign_key（节点的注册凭据与 URL 签名密钥），
// 因此**写入权限固定 0600**；读取侧不做权限校验（属主可能是 systemd 的服务用户）。
//
// 同名环境变量优先于文件（测试与调试用）：MASTER_URL / AGENT_ID / AGENT_SECRET /
// SIGN_KEY / LISTEN_PORT / PUBLIC_BASE_URL / MAX_CONCURRENT /
// CACHE_BUDGET_MB / CACHE_MAX_AGE_MINUTES / PREFETCH_HEAD_MB / PREFETCH_TAIL_MB /
// ROLE / HUB_PORT / HUB_ALLOW_IPS / DISK_CACHE_DIR / DISK_BUDGET_GB /
// WARM_HEAD_BYTES / WARM_TAIL_BYTES / WARM_RESUME_WINDOW_BYTES。
//
// 角色（ROLE）缺省为 node：node 模式的读写路径与 v0.3.2 逐字节一致
// （**不写** ROLE 行、不读取任何 hub 专属键）；hub 模式只多读上面那组 hub 键。
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
	// DefaultListenPort 是 node 角色的数据面监听端口（非特权端口，无需 root 运行）。
	DefaultListenPort = 8790
	// DefaultMaxConcurrent 是并发流上限（冻结稿 §2.4 的 AGENT_MAX_CONCURRENT）。
	DefaultMaxConcurrent = 32
	// DefaultCacheBudgetMB 是读前缓存的内存预算（MiB）；0 = 关闭。
	// 单文件首触预取固定约 36MiB（头 32 + 尾 4），256MiB 约容 7 部片的头尾。
	DefaultCacheBudgetMB = 256
	// DefaultCacheMaxAgeMinutes 是 node 缓存块的最大可服务年龄（分钟）；0 = 不做年龄检查。
	// 24 小时：角色是 LRU 之外的第二道兜底——真实 Google 直链没有 ETag/Last-Modified，
	// 内容身份退化到总字节数，识破不了"同大小替换"，靠年龄防止陈旧字节被无限期服务。
	DefaultCacheMaxAgeMinutes = 1440

	// RoleNode / RoleHub 是 ROLE 的两个合法取值（缺省 = node，v0.3.2 行为逐字节一致）。
	RoleNode = "node"
	RoleHub  = "hub"

	// DefaultHubPort 是 hub 角色的监听口（数据面 /f/ 与控制口 /warm、/cancel 共用；
	// master 侧 agent-network.hub-port 默认值必须一致）。
	DefaultHubPort = 8791
	// DefaultDiskCacheDir 是 hub 磁盘块缓存目录（部署约定：250G 数据盘 /home 下）。
	DefaultDiskCacheDir = "/home/ge2o-hub-cache"
	// DefaultDiskBudgetGB 是 hub 磁盘缓存的容量硬上限（GiB，LRU 淘汰到预算内）。
	DefaultDiskBudgetGB = 200
	// DefaultHubCacheMaxAgeMinutes 是 hub 缓存文件的 TTL（分钟）：48 小时。
	// 语义同 node 的块龄：自写入（created_at）起算，超龄一律视为缺失并清扫。
	DefaultHubCacheMaxAgeMinutes = 2880
	// DefaultWarmHeadBytes 是 hub 预热头部区域的默认长度（128MiB，单流切片抓取）。
	DefaultWarmHeadBytes = 128 << 20
	// DefaultWarmTailBytes 是 hub 预热尾部区域的默认长度（4MiB，一条 Range）。
	DefaultWarmTailBytes = 4 << 20
	// DefaultWarmResumeWindowBytes 是续播点区域半径的默认值（4MiB）：
	// 抓取 [resume-window, resume+window]，让"看到哪从哪续播"的第一段探测也命中。
	DefaultWarmResumeWindowBytes = 4 << 20
	// DefaultPrefetchHeadMB 是 node 首触预取的头部长度（MiB）；0 = 不预取。
	DefaultPrefetchHeadMB = 32
	// DefaultPrefetchTailMB 是 node 首触预取的尾部长度（MiB）；0 = 不预取尾部。
	DefaultPrefetchTailMB = 4
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

	// Role 是运行角色："node"（缺省；空字符串与显式 node 等价）或 "hub"。
	// 只有显式的 hub 才切换行为——node 模式的配置读写与 v0.3.2 逐字节一致。
	Role string

	// CacheBudgetMB 是读前缓存的内存预算（MiB）；0 = 关闭，
	// 此时数据面行为与不带缓存的版本逐字节一致。
	CacheBudgetMB int
	// CacheMaxAgeMinutes 是缓存块的最大可服务年龄（分钟）；0 = 不做年龄检查。
	// 超过它的块一律按 miss 处理（缓存开/关仍逐字节一致，只是少了本地命中）。
	// node 默认 24h；hub 上默认 48h（追剧窗口），键名共用。
	CacheMaxAgeMinutes int
	// PrefetchHeadMB / PrefetchTailMB 是首触预取的头/尾长度（MiB）。
	// 头为 0 即不预取（尾预取依赖头响应解析文件长度）。
	PrefetchHeadMB int
	PrefetchTailMB int

	// --- hub 专属（node 模式不读取；设计 10-10-hub-agent-mode §1）------------
	//
	// HubPort 是 hub 的监听口（/f/ 数据面 + /warm、/cancel 控制口共用）。
	HubPort int
	// HubAllowIPs 是拉流口/控制口的来源白名单（逗号/空白分隔的 IP 或 CIDR）。
	// 空 = 全部拒绝（fail-closed）：8791 是公网暴露面，白名单是唯一访问控制。
	HubAllowIPs string
	// DiskCacheDir 是磁盘块缓存目录；必须是绝对路径（部署约定在 250G 数据盘上）。
	DiskCacheDir string
	// DiskBudgetGB 是磁盘缓存容量硬上限（GiB，LRU 按 last_access_at 淘汰整文件）。
	DiskBudgetGB int
	// WarmHeadBytes / WarmTailBytes / WarmResumeWindowBytes 是预热区域集默认值（字节）。
	// master 的 /warm 请求可覆盖（regions 字段）；三个键 hub 专属。
	WarmHeadBytes         int64
	WarmTailBytes         int64
	WarmResumeWindowBytes int64
}

// IsHub 报告是否以 hub 角色运行（缺省与显式 node 都是 false）。
func (c Config) IsHub() bool { return c.Role == RoleHub }

// ServePort 是本实例实际监听的端口：hub 用 HUB_PORT，node 用 LISTEN_PORT。
func (c Config) ServePort() int {
	if c.IsHub() {
		return c.HubPort
	}
	return c.ListenPort
}

// Load 读取配置文件并叠加同名环境变量。
//
// 文件不存在不算致命：只要环境变量把必填项补齐即可（本地联调/测试用）。
// 必填项缺失时返回中文错误，调用方（serve）应以非零码退出。
func Load(path string) (Config, error) {
	if path == "" {
		path = DefaultPath
	}
	data, err := os.ReadFile(path)
	var values map[string]string
	switch {
	case err == nil:
		var perr error
		values, perr = Parse(data)
		if perr != nil {
			return Config{}, fmt.Errorf("配置文件 %s 解析失败：%w", path, perr)
		}
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

	// 角色要先知道再套默认值：CACHE_MAX_AGE_MINUTES 的缺省在 hub 上是 48h
	// （追剧窗口），node 上仍是 24h。显式写了该键则显式值优先。
	//
	// hub 专属键的默认值与读取同样只发生在 hub 角色：node 模式的 Config 对这些
	// 字段一无所知（node 配置里残留的 hub 键不参与运行、也读不进 Config），
	// Load→Write 往返与 v0.3.2 逐字节一致。
	role := roleFrom(values)
	cfg := Config{
		ListenPort:         DefaultListenPort,
		MaxConcurrent:      DefaultMaxConcurrent,
		CacheBudgetMB:      DefaultCacheBudgetMB,
		CacheMaxAgeMinutes: DefaultCacheMaxAgeMinutes,
		PrefetchHeadMB:     DefaultPrefetchHeadMB,
		PrefetchTailMB:     DefaultPrefetchTailMB,
		Role:               role,
	}
	if role == RoleHub {
		cfg.HubPort = DefaultHubPort
		cfg.DiskCacheDir = DefaultDiskCacheDir
		cfg.DiskBudgetGB = DefaultDiskBudgetGB
		cfg.WarmHeadBytes = DefaultWarmHeadBytes
		cfg.WarmTailBytes = DefaultWarmTailBytes
		cfg.WarmResumeWindowBytes = DefaultWarmResumeWindowBytes
		if !keyPresent("CACHE_MAX_AGE_MINUTES", values) {
			cfg.CacheMaxAgeMinutes = DefaultHubCacheMaxAgeMinutes
		}
	}
	applyValues(&cfg, values)
	applyEnv(&cfg)
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("配置不完整（文件 %s，可用环境变量覆盖）：%w", path, err)
	}
	return cfg, nil
}

// roleFrom 取出生效角色：环境变量 ROLE 优先于文件（与 applyEnv 同一优先级），
// 两边都空 = node。非法值不在这里报错（Validate 统一给中文提示）。
func roleFrom(values map[string]string) string {
	if v := strings.TrimSpace(os.Getenv("ROLE")); v != "" {
		return normalizeRole(v)
	}
	if v, ok := values["ROLE"]; ok {
		return normalizeRole(v)
	}
	return ""
}

// normalizeRole 归一化角色写法（大小写不敏感）；空串保持空串
// ——node 的"缺省"与"显式 node"等价，但不给配置文件凭空添行。
func normalizeRole(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// NormalizeRole 是 normalizeRole 的导出形态：enroll 与 serve 共用同一套归一化，
// 避免出现"enroll 写了 hub、serve 却认不得"的失配。
func NormalizeRole(v string) string { return normalizeRole(v) }

// ValidRole 报告角色取值是否合法：空串（缺省 = node）、node、hub 都合法。
func ValidRole(v string) bool {
	switch normalizeRole(v) {
	case "", RoleNode, RoleHub:
		return true
	}
	return false
}

// keyPresent 报告某键是否在文件或环境变量里显式出现过（非空）。
func keyPresent(key string, values map[string]string) bool {
	if v, ok := values[key]; ok && strings.TrimSpace(v) != "" {
		return true
	}
	return strings.TrimSpace(os.Getenv(key)) != ""
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
	switch normalizeRole(c.Role) {
	case "", RoleNode, RoleHub:
	default:
		return fmt.Errorf("ROLE 取值不合法：%q（合法值：node、hub）", c.Role)
	}
	if c.ListenPort <= 0 || c.ListenPort > 65535 {
		return fmt.Errorf("LISTEN_PORT 取值不合法：%d", c.ListenPort)
	}
	if c.MaxConcurrent <= 0 {
		return fmt.Errorf("MAX_CONCURRENT 取值不合法：%d", c.MaxConcurrent)
	}
	// 0 是合法且常用的取值（关闭缓存 / 不预取）；负数只可能是手抖或改错了键。
	if c.CacheBudgetMB < 0 {
		return fmt.Errorf("CACHE_BUDGET_MB 取值不合法：%d（0 = 关闭读前缓存）", c.CacheBudgetMB)
	}
	if c.CacheMaxAgeMinutes < 0 {
		return fmt.Errorf("CACHE_MAX_AGE_MINUTES 取值不合法：%d（0 = 不做年龄检查）", c.CacheMaxAgeMinutes)
	}
	if c.PrefetchHeadMB < 0 {
		return fmt.Errorf("PREFETCH_HEAD_MB 取值不合法：%d（0 = 不预取头部）", c.PrefetchHeadMB)
	}
	if c.PrefetchTailMB < 0 {
		return fmt.Errorf("PREFETCH_TAIL_MB 取值不合法：%d（0 = 不预取尾部）", c.PrefetchTailMB)
	}
	// hub 专属键只在 hub 角色下校验：node 配置里残留的 hub 键不参与运行，
	// 也不该让 node 起不来（"白名单为 hub 专属配置项，node 模式不读取"）。
	if c.IsHub() {
		if c.HubPort <= 0 || c.HubPort > 65535 {
			return fmt.Errorf("HUB_PORT 取值不合法：%d", c.HubPort)
		}
		if c.DiskBudgetGB < 1 {
			return fmt.Errorf("DISK_BUDGET_GB 取值不合法：%d（hub 磁盘缓存必须大于 0）", c.DiskBudgetGB)
		}
		if !filepath.IsAbs(c.DiskCacheDir) {
			return fmt.Errorf("DISK_CACHE_DIR 必须是绝对路径：%q", c.DiskCacheDir)
		}
		if c.WarmHeadBytes < 0 {
			return fmt.Errorf("WARM_HEAD_BYTES 取值不合法：%d（0 = 不抓头部区域）", c.WarmHeadBytes)
		}
		if c.WarmTailBytes < 0 {
			return fmt.Errorf("WARM_TAIL_BYTES 取值不合法：%d（0 = 不抓尾部区域）", c.WarmTailBytes)
		}
		if c.WarmResumeWindowBytes < 0 {
			return fmt.Errorf("WARM_RESUME_WINDOW_BYTES 取值不合法：%d（0 = 不抓续播点区域）", c.WarmResumeWindowBytes)
		}
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
	// 读前缓存四键：enroll 会写进默认值（管理员照着头改即可），删掉某行则回落默认。
	writeKV("CACHE_BUDGET_MB", strconv.Itoa(c.CacheBudgetMB))
	writeKV("CACHE_MAX_AGE_MINUTES", strconv.Itoa(c.CacheMaxAgeMinutes))
	writeKV("PREFETCH_HEAD_MB", strconv.Itoa(c.PrefetchHeadMB))
	writeKV("PREFETCH_TAIL_MB", strconv.Itoa(c.PrefetchTailMB))
	// node 角色不写 ROLE 行、不写 hub 键：配置文件内容与 v0.3.2 逐字节一致。
	// hub 角色写全，白名单留空提示管理员填写（空 = 全部拒绝，fail-closed）。
	if c.IsHub() {
		writeKV("ROLE", RoleHub)
		writeKV("HUB_PORT", strconv.Itoa(c.HubPort))
		writeKV("HUB_ALLOW_IPS", c.HubAllowIPs)
		writeKV("DISK_CACHE_DIR", c.DiskCacheDir)
		writeKV("DISK_BUDGET_GB", strconv.Itoa(c.DiskBudgetGB))
		writeKV("WARM_HEAD_BYTES", strconv.FormatInt(c.WarmHeadBytes, 10))
		writeKV("WARM_TAIL_BYTES", strconv.FormatInt(c.WarmTailBytes, 10))
		writeKV("WARM_RESUME_WINDOW_BYTES", strconv.FormatInt(c.WarmResumeWindowBytes, 10))
	}
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
	setInt := func(key string, dst *int) {
		if v, ok := values[key]; ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				*dst = n
			}
		}
	}
	setInt("LISTEN_PORT", &cfg.ListenPort)
	setInt("CACHE_BUDGET_MB", &cfg.CacheBudgetMB)
	setInt("CACHE_MAX_AGE_MINUTES", &cfg.CacheMaxAgeMinutes)
	setInt("PREFETCH_HEAD_MB", &cfg.PrefetchHeadMB)
	setInt("PREFETCH_TAIL_MB", &cfg.PrefetchTailMB)
	// hub 专属键：node 模式一律不读取（残留的行直接忽略）。
	if cfg.IsHub() {
		set("HUB_ALLOW_IPS", &cfg.HubAllowIPs)
		set("DISK_CACHE_DIR", &cfg.DiskCacheDir)
		setInt("HUB_PORT", &cfg.HubPort)
		setInt("DISK_BUDGET_GB", &cfg.DiskBudgetGB)
		setInt64 := func(key string, dst *int64) {
			if v, ok := values[key]; ok {
				if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
					*dst = n
				}
			}
		}
		setInt64("WARM_HEAD_BYTES", &cfg.WarmHeadBytes)
		setInt64("WARM_TAIL_BYTES", &cfg.WarmTailBytes)
		setInt64("WARM_RESUME_WINDOW_BYTES", &cfg.WarmResumeWindowBytes)
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
	override("ROLE", &cfg.Role)
	cfg.Role = normalizeRole(cfg.Role)
	overrideInt := func(key string, dst *int) {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	overrideInt("LISTEN_PORT", &cfg.ListenPort)
	overrideInt("CACHE_BUDGET_MB", &cfg.CacheBudgetMB)
	overrideInt("CACHE_MAX_AGE_MINUTES", &cfg.CacheMaxAgeMinutes)
	overrideInt("PREFETCH_HEAD_MB", &cfg.PrefetchHeadMB)
	overrideInt("PREFETCH_TAIL_MB", &cfg.PrefetchTailMB)
	// hub 专属键：只有 hub 角色读取（node 环境里残留的同名变量不参与）。
	if cfg.IsHub() {
		override("HUB_ALLOW_IPS", &cfg.HubAllowIPs)
		override("DISK_CACHE_DIR", &cfg.DiskCacheDir)
		overrideInt("HUB_PORT", &cfg.HubPort)
		overrideInt("DISK_BUDGET_GB", &cfg.DiskBudgetGB)
		overrideInt64 := func(key string, dst *int64) {
			if v := strings.TrimSpace(os.Getenv(key)); v != "" {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					*dst = n
				}
			}
		}
		overrideInt64("WARM_HEAD_BYTES", &cfg.WarmHeadBytes)
		overrideInt64("WARM_TAIL_BYTES", &cfg.WarmTailBytes)
		overrideInt64("WARM_RESUME_WINDOW_BYTES", &cfg.WarmResumeWindowBytes)
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
