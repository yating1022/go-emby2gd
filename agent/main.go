// gd-agent：GD 代理网络的 agent 节点（数据面出口，零第三方依赖）。
//
// 子命令：
//
//	enroll  一次性注册：向 master 换取凭据并写入 /etc/gd-agent/config.env（0600）
//	serve   常驻服务：心跳 + 数据面代理（GET/HEAD /dl/<file_id>?e&s）
//	version 打印版本（构建时用 -ldflags -X main.version=... 注入）
//
// 日志一律中文、走 stdout（由 systemd/journald 收集）；任何路径都不打印
// agent_secret / sign_key / Google token。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yating1022/go-emby2gd/agent/internal/config"
	"github.com/yating1022/go-emby2gd/agent/internal/enroll"
	"github.com/yating1022/go-emby2gd/agent/internal/heartbeat"
	"github.com/yating1022/go-emby2gd/agent/internal/proxy"
)

// version 由发布流水线注入：-ldflags "-X main.version=<tag>"。
var version = "dev"

// signKeyBytes 是协议约定的 SIGN_KEY 长度：master 用 secrets.token_hex(32)
// 生成 64 个 hex 字符（冻结稿 §2.1），解码后应为 32 字节。
const signKeyBytes = 32

const usageText = `gd-agent —— GD 代理网络节点

用法：
  gd-agent enroll --master <master地址> --token <注册Token> [--role node|hub] [--public-url <对外地址>] [--port <监听端口>] [--config <路径>]
  gd-agent serve  [--config <路径>]
  gd-agent version

说明：
  enroll   一次性注册（幂等：同一台机器重跑会轮换凭据并复用原节点记录）
  serve    常驻服务：心跳 + 数据面；默认读 /etc/gd-agent/config.env。
           角色由配置里的 ROLE 决定：缺省 node（客户端拉流节点，端口 LISTEN_PORT=8790）；
           hub = 磁盘缓存中心（端口 HUB_PORT=8791，"内网口"同时承载 /f/<fileID> 数据面
           与 /warm、/cancel 控制面，访问控制为 HUB_ALLOW_IPS 白名单）。
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 是 main 的可测试入口：返回进程退出码（0 成功、1 运行失败、2 用法错误）。
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	switch args[0] {
	case "enroll":
		return runEnroll(args[1:], stdout, stderr)
	case "serve":
		return runServe(args[1:], stdout, stderr)
	case "version", "-v", "--version":
		fmt.Fprintf(stdout, "gd-agent %s\n", version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return 0
	default:
		fmt.Fprintf(stderr, "未知子命令：%s\n\n%s", args[0], usageText)
		return 2
	}
}

func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func runEnroll(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	fs.SetOutput(stderr)
	master := fs.String("master", "", "master 地址（如 http://1.2.3.4:8000）")
	token := fs.String("token", "", "注册 Token（master 网页「节点」页复制）")
	role := fs.String("role", "", "运行角色：node（缺省）| hub（磁盘缓存中心）")
	publicURL := fs.String("public-url", "", "本机对外地址（NAT 后必填，如 http://1.2.3.4:8790）")
	port := fs.Int("port", 0, "监听端口（缺省 node=8790、hub=8791）")
	configPath := fs.String("config", config.DefaultPath, "配置文件写入路径")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !config.ValidRole(*role) {
		fmt.Fprintf(stderr, "--role 取值不合法：%q（合法值：node、hub）\n\n%s", *role, usageText)
		return 2
	}

	logger := newLogger(stdout)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	_, err := enroll.Run(ctx, enroll.Options{
		MasterURL:     *master,
		Token:         *token,
		Role:          *role,
		PublicBaseURL: *publicURL,
		ListenPort:    *port,
		ConfigPath:    *configPath,
		Version:       version,
		Logger:        logger,
	})
	if err != nil {
		fmt.Fprintf(stderr, "gd-agent enroll 失败：%v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "注册完成。启动服务：systemctl enable --now gd-agent\n")
	return 0
}

func runServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", config.DefaultPath, "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	logger := newLogger(stdout)
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "gd-agent serve 启动失败：%v\n", err)
		return 1
	}
	// hub 角色走独立的服务装配（磁盘缓存 + /f/ 数据面 + /warm、/cancel 控制面）；
	// node 角色的代码路径与 v0.3.2 完全一致（下面一字未动）。
	if cfg.IsHub() {
		return runServeHub(cfg, logger, stderr)
	}
	signKey, err := cfg.SignKeyBytes()
	if err != nil {
		fmt.Fprintf(stderr, "gd-agent serve 启动失败：%v\n", err)
		return 1
	}
	// 协议约定 sign_key 是 secrets.token_hex(32)（64 hex → 32 字节）。长度不对
	// 只可能来自被截断/人工改库：签名仍能自洽（master 用同一把钥匙签），
	// 但强度被削弱，且现场表现为"一切正常"——所以这里响亮提醒，不静默接受。
	if len(signKey) != signKeyBytes {
		logger.Warn("SIGN_KEY 长度与协议约定不符，签名强度可能被削弱；请重跑安装脚本重新注册",
			"expected_bytes", signKeyBytes, "actual_bytes", len(signKey))
	}

	// enabled 由心跳驱动（master 可在网页禁用节点）。
	var enabled atomic.Bool
	enabled.Store(true)

	links := proxy.NewLinkSource(proxy.LinkSourceConfig{
		MasterURL: cfg.MasterURL,
		AgentID:   cfg.AgentID,
		Secret:    cfg.AgentSecret,
		Logger:    logger,
		Enabled:   enabled.Load,
	})
	// 读前缓存：预算 0 = 关闭（数据面行为与不带缓存的版本逐字节一致）；
	// 块龄超过 CACHE_MAX_AGE_MINUTES 一律按 miss 处理（同大小替换的兜底）。
	// 上游客户端由数据面与预取共用，避免两套连接池互相抢配额。
	client := proxy.NewUpstreamClient()
	cache := proxy.NewBlockCacheWithMaxAge(
		int64(cfg.CacheBudgetMB)<<20, time.Duration(cfg.CacheMaxAgeMinutes)*time.Minute)
	prefetcher := proxy.NewPrefetcher(proxy.PrefetcherConfig{
		Cache:     cache,
		Links:     links,
		Client:    client,
		Logger:    logger,
		HeadBytes: int64(cfg.PrefetchHeadMB) << 20,
		TailBytes: int64(cfg.PrefetchTailMB) << 20,
	})
	handler := proxy.NewHandler(proxy.Config{
		SignKey:       signKey,
		MaxConcurrent: cfg.MaxConcurrent,
		Links:         links,
		Logger:        logger,
		Client:        client,
		Cache:         cache,
		Prefetch:      prefetcher,
	})
	beat := heartbeat.New(heartbeat.Options{
		MasterURL:     cfg.MasterURL,
		AgentID:       cfg.AgentID,
		Secret:        cfg.AgentSecret,
		Version:       version,
		ListenPort:    cfg.ListenPort,
		PublicBaseURL: cfg.PublicBaseURL,
		ActiveStreams: handler.ActiveStreams,
		Logger:        logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.ListenPort),
		Handler: handler,
		// 只防慢速请求头（数据面只有 GET/HEAD，没有请求体可读）。
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0,
		// WriteTimeout 必须为 0：响应体是大文件流，整体写超时会把下载砍断。
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	go beat.Run(ctx, &enabled)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("gd-agent 启动",
			"version", version,
			"listen_port", cfg.ListenPort,
			"master_url", cfg.MasterURL,
			"max_concurrent", cfg.MaxConcurrent,
			"public_base_url", cfg.PublicBaseURL,
			"cache_budget_mb", cfg.CacheBudgetMB,
			"cache_max_age_minutes", cfg.CacheMaxAgeMinutes,
			"prefetch_head_mb", cfg.PrefetchHeadMB,
			"prefetch_tail_mb", cfg.PrefetchTailMB,
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	code := 0
	select {
	case <-ctx.Done():
		logger.Info("收到退出信号，开始优雅停机")
	case err := <-errCh:
		logger.Error("HTTP 服务异常退出", "error", err)
		code = 1
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("优雅停机超时，强制关闭", "error", err)
		_ = server.Close()
	}
	logger.Info("gd-agent 已退出")
	return code
}

// runServeHub 是 hub 角色的常驻服务（design 10-10-hub-agent-mode §1–§5）。
//
// 与 node 角色的差别：
//   - 不挂客户端拉流路由（没有 /dl/，签名密钥不参与）：数据面是 /f/<fileID>，
//     访问控制为 HUB_ALLOW_IPS 白名单（空 = 全部拒绝，fail-closed）；
//   - 上游出口用多地址快速失败拨号（Twon→googleapis 坏 IP 防雷，design §5）；
//   - 磁盘块缓存 + 周期清扫（TTL 48h + LRU 200G）。
func runServeHub(cfg config.Config, logger *slog.Logger, stderr io.Writer) int {
	cache, err := proxy.NewDiskCache(proxy.DiskCacheConfig{
		Dir:         cfg.DiskCacheDir,
		BudgetBytes: int64(cfg.DiskBudgetGB) << 30,
		MaxAge:      time.Duration(cfg.CacheMaxAgeMinutes) * time.Minute,
		Logger:      logger,
	})
	if err != nil {
		fmt.Fprintf(stderr, "gd-agent serve 启动失败：%v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 启动即扫一轮（重建磁盘索引 + 让容量计账立刻准确），随后每 10 分钟一轮。
	cache.StartSweeper(ctx)

	// enabled 由心跳驱动（master 可在网页禁用节点；hub 被禁用后不再拉新直链，
	// 已缓存的字节与 warm 存的直链仍继续服务）。
	var enabled atomic.Bool
	enabled.Store(true)

	links := proxy.NewLinkSource(proxy.LinkSourceConfig{
		MasterURL: cfg.MasterURL,
		AgentID:   cfg.AgentID,
		Secret:    cfg.AgentSecret,
		Logger:    logger,
		Enabled:   enabled.Load,
	})
	hub := proxy.NewHub(proxy.HubConfig{
		AllowIPs:              cfg.HubAllowIPs,
		Cache:                 cache,
		Links:                 links,
		Logger:                logger,
		MaxConcurrent:         cfg.MaxConcurrent,
		WarmHeadBytes:         cfg.WarmHeadBytes,
		WarmTailBytes:         cfg.WarmTailBytes,
		WarmResumeWindowBytes: cfg.WarmResumeWindowBytes,
	})
	beat := heartbeat.New(heartbeat.Options{
		MasterURL:     cfg.MasterURL,
		AgentID:       cfg.AgentID,
		Secret:        cfg.AgentSecret,
		Version:       version,
		ListenPort:    cfg.HubPort,
		PublicBaseURL: cfg.PublicBaseURL,
		ActiveStreams: hub.ActiveStreams,
		Logger:        logger,
	})

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.HubPort),
		Handler: hub,
		// 与 node 同款超时取舍：只防慢速请求头；响应体是文件流，WriteTimeout 必须为 0。
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}

	go beat.Run(ctx, &enabled)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("gd-agent 启动（hub）",
			"version", version,
			"hub_port", cfg.HubPort,
			"master_url", cfg.MasterURL,
			"max_concurrent", cfg.MaxConcurrent,
			"disk_cache_dir", cfg.DiskCacheDir,
			"disk_budget_gb", cfg.DiskBudgetGB,
			"cache_max_age_minutes", cfg.CacheMaxAgeMinutes,
			"warm_head_bytes", cfg.WarmHeadBytes,
			"warm_tail_bytes", cfg.WarmTailBytes,
			"allow_ips", cfg.HubAllowIPs,
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	code := 0
	select {
	case <-ctx.Done():
		logger.Info("收到退出信号，开始优雅停机")
	case err := <-errCh:
		logger.Error("HTTP 服务异常退出", "error", err)
		code = 1
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("优雅停机超时，强制关闭", "error", err)
		_ = server.Close()
	}
	logger.Info("gd-agent 已退出")
	return code
}
