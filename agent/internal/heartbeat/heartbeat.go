// Package heartbeat 维护 agent → master 的心跳循环（冻结稿 §2.2）。
//
// 语义要点：
//   - 间隔 15s（以 master 返回的 heartbeat_interval_seconds 为准，可被服务端调整）；
//   - 鉴权失败（401）**不退出**：指数退避重试并打 ERROR 日志，
//     由人工重跑安装脚本重新注册；
//   - master 回 `enabled:false`（管理员禁用）时只是把开关置 false——
//     agent 停止向 master 要新直链，但继续心跳（这样解禁后自动恢复）。
package heartbeat

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/yating1022/go-emby2gd/agent/internal/api"
)

const (
	// DefaultInterval 是冻结稿 §2.2 约定的心跳间隔。
	DefaultInterval = 15 * time.Second
	// DefaultMaxBackoff 是退避上限：5 分钟。
	DefaultMaxBackoff = 5 * time.Minute
	// beatTimeout 是单次心跳请求的上限（不拖住循环）。
	beatTimeout = 15 * time.Second
)

// Options 组装心跳客户端。
type Options struct {
	MasterURL     string
	AgentID       string
	Secret        string
	Version       string
	ListenPort    int
	PublicBaseURL string

	// Interval 是初始心跳间隔（默认 15s）。测试里用毫秒级值。
	Interval time.Duration
	// MaxBackoff 是退避上限（默认 5min）。
	MaxBackoff time.Duration
	// ActiveStreams 返回当前活跃流数（接数据面的计数）；可为 nil。
	ActiveStreams func() int64

	HTTP   *http.Client
	Logger *slog.Logger
}

// Client 是心跳循环。
type Client struct {
	opts    Options
	client  *api.Client
	log     *slog.Logger
	started time.Time
}

func New(opts Options) *Client {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = DefaultMaxBackoff
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Client{
		opts: opts,
		client: &api.Client{
			BaseURL: opts.MasterURL,
			AgentID: opts.AgentID,
			Secret:  opts.Secret,
			HTTP:    opts.HTTP,
		},
		log:     opts.Logger,
		started: time.Now(),
	}
}

// request 是心跳请求体（字段名与冻结稿 §2.2 逐字一致）。
type request struct {
	ActiveStreams int64   `json:"active_streams"`
	Version       string  `json:"version"`
	UptimeSeconds int64   `json:"uptime_seconds"`
	ListenPort    int     `json:"listen_port"`
	PublicBaseURL *string `json:"public_base_url"`
}

type response struct {
	OK                       bool  `json:"ok"`
	Enabled                  *bool `json:"enabled"`
	HeartbeatIntervalSeconds int   `json:"heartbeat_interval_seconds"`
}

// Run 阻塞地跑心跳，直到 ctx 取消。
//
// enabled 由 master 的响应驱动（缺字段时保持原值，避免把网络抖动当成"被禁用"）。
func (c *Client) Run(ctx context.Context, enabled *atomic.Bool) {
	interval := c.opts.Interval
	var (
		failures  int
		delay     time.Duration
		connected bool
	)
	for {
		if delay > 0 && !sleep(ctx, delay) {
			return
		}
		resp, err := c.beat(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
			delay = backoffDelay(interval, c.opts.MaxBackoff, failures)
			c.logFailure(err, failures, delay)
			continue
		}
		if !connected {
			c.log.Info("心跳已联通", "interval", interval)
			connected = true
		} else if failures > 0 {
			c.log.Info("心跳已恢复", "failures", failures)
		}
		failures = 0
		if resp.HeartbeatIntervalSeconds > 0 {
			interval = time.Duration(resp.HeartbeatIntervalSeconds) * time.Second
		}
		delay = interval
		if resp.Enabled != nil && enabled != nil {
			if prev := enabled.Swap(*resp.Enabled); prev != *resp.Enabled {
				if *resp.Enabled {
					c.log.Info("master 已启用该节点，恢复获取新直链")
				} else {
					c.log.Warn("master 已禁用该节点：不再获取新直链（已在途的流不打断）")
				}
			}
		}
	}
}

func (c *Client) beat(ctx context.Context) (response, error) {
	var active int64
	if c.opts.ActiveStreams != nil {
		active = c.opts.ActiveStreams()
	}
	body := request{
		ActiveStreams: active,
		Version:       c.opts.Version,
		UptimeSeconds: int64(time.Since(c.started).Seconds()),
		ListenPort:    c.opts.ListenPort,
		PublicBaseURL: optionalString(c.opts.PublicBaseURL),
	}
	callCtx, cancel := context.WithTimeout(ctx, beatTimeout)
	defer cancel()

	var resp response
	if err := c.client.PostJSON(callCtx, "/api/agent/heartbeat", body, &resp); err != nil {
		return response{}, err
	}
	return resp, nil
}

func (c *Client) logFailure(err error, failures int, retryIn time.Duration) {
	var apiErr *api.Error
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
		// 凭据失效是"需要人工介入"的状态：明确提示重跑安装脚本。
		c.log.Error("心跳鉴权失败（HTTP 401）：凭据已失效或节点已被 master 删除；agent 不退出，将退避重试。请重跑安装脚本重新注册",
			"attempt", failures, "retry_in", retryIn.String(), "code", apiErr.Code)
		return
	}
	c.log.Warn("心跳失败，将退避重试",
		"attempt", failures, "retry_in", retryIn.String(), "error", err)
}

// backoffDelay 计算第 failures 次失败后的等待：base、base*2、base*4… 封顶 max。
func backoffDelay(base, max time.Duration, failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	delay := base
	for i := 1; i < failures; i++ {
		if delay >= max {
			return max
		}
		delay *= 2
	}
	if delay > max {
		return max
	}
	return delay
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
