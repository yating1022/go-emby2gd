// Package enroll 实现一次性的节点注册（gd-agent enroll）：
// 读机器标识 → POST /api/agent/enroll → 把凭据写入 config.env。
//
// 幂等键是 machine_id（冻结稿 §2.1 / §9-8）：同一台机器重跑安装脚本会**复用**
// 原 agent 记录并轮换 secret/sign_key，不会攒出一堆重复节点。
package enroll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/yating1022/go-emby2gd/agent/internal/api"
	"github.com/yating1022/go-emby2gd/agent/internal/config"
)

// DefaultMachineIDPath 是 Linux 的机器标识文件。
const DefaultMachineIDPath = "/etc/machine-id"

// Options 是 enroll 的输入。
type Options struct {
	MasterURL     string
	Token         string // 注册 Token（本项目：网关配置段 agent-network.enroll-token，见任务 10-08-agent-proxy-network design §2.6）
	PublicBaseURL string // 可空：NAT 后的机器必须显式给（冻结稿 §4.2）
	// Role 是运行角色："node"（缺省/空）或 "hub"（hub 缓存中心）。
	// node 角色的请求与 v0.3.2 逐字节一致（不携带 role 字段）。
	Role          string
	ListenPort    int
	ConfigPath    string // 空则 config.DefaultPath
	Version       string // 二进制版本（构建时注入）
	MachineIDPath string // 空则 /etc/machine-id；测试注入临时文件
	HTTP          *http.Client
	Logger        *slog.Logger
}

// Result 是注册成功后写进配置的内容，便于调用方打日志。
type Result struct {
	Config config.Config
}

type request struct {
	EnrollToken   string  `json:"enroll_token"`
	MachineID     string  `json:"machine_id"`
	Hostname      string  `json:"hostname"`
	Version       string  `json:"version"`
	ListenPort    int     `json:"listen_port"`
	PublicBaseURL *string `json:"public_base_url"`
	// Role 只在 hub 角色时携带（omitempty）：master 侧"缺省 = node"，
	// node 的 enroll 请求因此与 v0.3.2 逐字节一致。
	Role string `json:"role,omitempty"`
}

type response struct {
	AgentID                  string `json:"agent_id"`
	AgentSecret              string `json:"agent_secret"`
	SignKey                  string `json:"sign_key"`
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
}

// Run 执行注册并写配置。失败返回中文错误（含 HTTP 状态与响应 code）。
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.MasterURL == "" {
		return Result{}, fmt.Errorf("缺少 master 地址（--master）")
	}
	if opts.Token == "" {
		return Result{}, fmt.Errorf("缺少注册 Token（--token）：请在 master 网页「节点」页复制安装命令")
	}
	role := config.NormalizeRole(opts.Role)
	if !config.ValidRole(role) {
		return Result{}, fmt.Errorf("--role 取值不合法：%q（合法值：node、hub）", opts.Role)
	}
	if role == "" {
		role = config.RoleNode
	}
	// 端口缺省按角色取：node 数据面 8790、hub 内网口 8791（与 master 的
	// agent-network.hub-port 默认值一致）。显式 --port 一律优先。
	if opts.ListenPort == 0 {
		if role == config.RoleHub {
			opts.ListenPort = config.DefaultHubPort
		} else {
			opts.ListenPort = config.DefaultListenPort
		}
	}
	if opts.ConfigPath == "" {
		opts.ConfigPath = config.DefaultPath
	}
	if opts.MachineIDPath == "" {
		opts.MachineIDPath = DefaultMachineIDPath
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		hostname = "unknown-host"
	}
	machineID := readMachineID(opts.MachineIDPath, hostname, logger)

	var publicBaseURL *string
	if strings.TrimSpace(opts.PublicBaseURL) != "" {
		value := strings.TrimRight(strings.TrimSpace(opts.PublicBaseURL), "/")
		publicBaseURL = &value
	}

	client := &api.Client{BaseURL: opts.MasterURL, HTTP: opts.HTTP}
	var resp response
	payload := request{
		EnrollToken:   opts.Token,
		MachineID:     machineID,
		Hostname:      hostname,
		Version:       opts.Version,
		ListenPort:    opts.ListenPort,
		PublicBaseURL: publicBaseURL,
	}
	if role == config.RoleHub {
		payload.Role = config.RoleHub
	}
	var apiErr *api.Error
	if err := client.PostJSON(ctx, "/api/agent/enroll", payload, &resp); err != nil {
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
			return Result{}, fmt.Errorf("注册被拒绝：注册 Token 无效或已轮换（HTTP 401，code=%s）；请在 master 网页重新复制安装命令", apiErr.Code)
		}
		return Result{}, fmt.Errorf("向 master 注册失败：%w", err)
	}
	if resp.AgentID == "" || resp.AgentSecret == "" || resp.SignKey == "" {
		return Result{}, fmt.Errorf("master 返回的注册凭据不完整（agent_id/agent_secret/sign_key 均为必填）")
	}

	cfg := config.Config{
		MasterURL:     strings.TrimRight(strings.TrimSpace(opts.MasterURL), "/"),
		AgentID:       resp.AgentID,
		AgentSecret:   resp.AgentSecret,
		SignKey:       resp.SignKey,
		ListenPort:    opts.ListenPort,
		PublicBaseURL: strings.TrimRight(strings.TrimSpace(opts.PublicBaseURL), "/"),
		MaxConcurrent: config.DefaultMaxConcurrent,
		// 读前缓存四键写默认值（而不是留空）：配置文件里看得见，改起来有据可依；
		// 删掉某一行则回落同名默认值（见 config.Load）。
		CacheBudgetMB:      config.DefaultCacheBudgetMB,
		CacheMaxAgeMinutes: config.DefaultCacheMaxAgeMinutes,
		PrefetchHeadMB:     config.DefaultPrefetchHeadMB,
		PrefetchTailMB:     config.DefaultPrefetchTailMB,
	}
	if role == config.RoleHub {
		// hub：写 ROLE 与 hub 专属键（config.marshal 只在 IsHub 时输出）。
		// 端口复用 --port：hub 的监听口即 HUB_PORT，写进配置后 serve 按它监听；
		// LISTEN_PORT 同值，避免配置文件里出现两个互不相干的端口。
		cfg.Role = config.RoleHub
		cfg.HubPort = opts.ListenPort
		cfg.CacheMaxAgeMinutes = config.DefaultHubCacheMaxAgeMinutes
		cfg.DiskCacheDir = config.DefaultDiskCacheDir
		cfg.DiskBudgetGB = config.DefaultDiskBudgetGB
		cfg.WarmHeadBytes = config.DefaultWarmHeadBytes
		cfg.WarmTailBytes = config.DefaultWarmTailBytes
		cfg.WarmResumeWindowBytes = config.DefaultWarmResumeWindowBytes
		// HUB_ALLOW_IPS 刻意留空：8791 是公网暴露面，白名单是唯一访问控制，
		// 空 = 全部拒绝（fail-closed）。管理员必须显式填 master 与各节点的 IP。
	}
	if err := config.Write(opts.ConfigPath, cfg); err != nil {
		return Result{}, err
	}
	if role == config.RoleHub {
		logger.Info("注册成功，配置已写入",
			"agent_id", cfg.AgentID, "config", opts.ConfigPath, "listen_port", cfg.ListenPort,
			"role", config.RoleHub)
	} else {
		// node：日志与 v0.3.2 逐字一致（不凭空多一个 role 字段）。
		logger.Info("注册成功，配置已写入",
			"agent_id", cfg.AgentID, "config", opts.ConfigPath, "listen_port", cfg.ListenPort)
	}
	return Result{Config: cfg}, nil
}

// readMachineID 读 /etc/machine-id；读不到就退化为主机名（冻结稿 §2.1）。
func readMachineID(path, hostname string, logger *slog.Logger) string {
	data, err := os.ReadFile(path)
	if err != nil {
		logger.Warn("读取 machine-id 失败，改用主机名作为 machine_id", "path", path, "error", err)
		return hostname
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		logger.Warn("machine-id 为空，改用主机名作为 machine_id", "path", path)
		return hostname
	}
	return id
}
