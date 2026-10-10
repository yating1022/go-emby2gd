package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/yating1022/go-emby2gd/agent/internal/api"
)

// defaultMargin 是直链"到期前多久视为过期"的默认余量。
//
// 余量链（父任务 10-08-agent-proxy-network 的 design §2.3，实测依据 gdrive-panel.md §3.1）：
//
//	agent 25s  <  master 30s  <  GD 面板 refresh-ahead 60s
//
// 余量必须**严格小于** master 侧 30s。若像参考稿那样取 5 分钟（≥ 30s）：agent 在
// expires_at−5min 就把直链视为过期、每次请求都回 master 重拉，而 master 侧的缓存
// 要到 expires_at−30s 才失效，于是原样返回**同一份** token；agent 拿到后重算余量
// 仍不足，该窗口内每个 Range 请求都会重拉一次（可见病灶与 gdrive-panel.md §3.1
// 描述的 5 分钟余量完全相同）。取 25s 时：agent 在 expires_at−25s 刷新 → master
// 缓存已失效（−30s）→ 真调面板（面板已在 60s 发放窗口内）→ 拿到新令牌。
//
// 修改任何一级余量前先读任务 10-08-agent-proxy-network 的 design §2.3；
// linkcache_test.go 有断言测试钉住此链条。
const defaultMargin = 25 * time.Second

// Link 是 master 签发的一条 Google 直链（冻结稿 §2.3）。
//
// Headers 里是账号级 Google access token——**只能**发往 Google，
// 任何日志、任何客户端响应都不得包含它。
type Link struct {
	URL       string
	Headers   map[string]string
	ExpiresAt time.Time
}

// LinkError 是拿不到直链时的中文原因（冻结稿 §2.5：502 透传中文原因）。
type LinkError struct {
	Message string
	Code    string
}

func (e *LinkError) Error() string { return e.Message }

// LinkSourceConfig 组装 LinkSource。
type LinkSourceConfig struct {
	MasterURL string
	AgentID   string
	Secret    string
	HTTP      *http.Client // 控制面客户端；空则用 api 包默认（30s 超时）
	Logger    *slog.Logger

	// Enabled 报告 master 是否仍允许该 agent 拉新直链（心跳里的 enabled 字段）。
	// 为 false 时：已缓存的直链继续服务（冻结稿 §2.2），但不发新的拉取请求。
	Enabled func() bool

	// Margin 是"到期前多久视为过期"的余量，默认 25 秒。
	// 必须小于 master 侧 30s（链条与病灶说明见 defaultMargin 注释）。
	Margin time.Duration
	// FetchTimeout 是单次拉直链请求的上限，默认 30 秒。
	FetchTimeout time.Duration
	// Now 可注入时钟（测试用），默认 time.Now。
	Now func() time.Time
}

// LinkSource 按 file_id 缓存 Google 直链，并做单飞刷新：
// 一个 agent 对同一文件只持一份缓存，并发请求只触发一次 master 调用。
type LinkSource struct {
	cfg    LinkSourceConfig
	client *api.Client

	mu       sync.Mutex
	entries  map[string]linkEntry
	inflight map[string]*fetchCall
}

type linkEntry struct {
	link      Link
	refreshAt time.Time // 此后的请求视为过期，需要重新拉取
}

// fetchCall 是一次进行中的拉取；等待者通过 done 拿到结果。
type fetchCall struct {
	done chan struct{}
	link Link
	err  error
}

func NewLinkSource(cfg LinkSourceConfig) *LinkSource {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Margin <= 0 {
		cfg.Margin = defaultMargin
	}
	if cfg.FetchTimeout <= 0 {
		cfg.FetchTimeout = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &LinkSource{
		cfg: cfg,
		client: &api.Client{
			BaseURL: cfg.MasterURL,
			AgentID: cfg.AgentID,
			Secret:  cfg.Secret,
			HTTP:    cfg.HTTP,
		},
		entries:  make(map[string]linkEntry),
		inflight: make(map[string]*fetchCall),
	}
}

// Link 返回可用直链：命中新鲜缓存直接返回，否则（单飞地）向 master 拉取。
func (s *LinkSource) Link(ctx context.Context, fileID string) (Link, error) {
	if link, ok := s.cached(fileID); ok {
		return link, nil
	}
	if err := s.checkEnabled(); err != nil {
		return Link{}, err
	}

	s.mu.Lock()
	// double-check：等锁期间可能已有人填好了缓存。
	if link, ok := s.freshLocked(fileID); ok {
		s.mu.Unlock()
		return link, nil
	}
	if call, ok := s.inflight[fileID]; ok {
		s.mu.Unlock()
		select {
		case <-call.done:
			if call.err != nil {
				return Link{}, call.err
			}
			return call.link, nil
		case <-ctx.Done():
			return Link{}, ctx.Err()
		}
	}
	call := &fetchCall{done: make(chan struct{})}
	s.inflight[fileID] = call
	s.mu.Unlock()

	link, err := s.fetch(ctx, fileID, false)

	s.mu.Lock()
	delete(s.inflight, fileID)
	if err == nil {
		s.entries[fileID] = s.newEntry(link)
	}
	call.link, call.err = link, err
	s.mu.Unlock()
	close(call.done)

	return link, err
}

// Refresh 无条件重新拉取并覆盖缓存（上游 401/403 后的单次重试用，冻结稿 §2.3）。
func (s *LinkSource) Refresh(ctx context.Context, fileID string) (Link, error) {
	return s.refresh(ctx, fileID, false)
}

// RefreshStale 与 Refresh 相同，但请求里带 stale=1："hub 侧的状态可能已陈旧，
// 请先重新确认再应答"（v0.4.2 N4 的换链提示，hub 直连失败时使用）。
//
// 语义边界：提示由 master 解释（收到它先同步重发一次 /warm 再应答）；请求与响应
// 形状与普通换链完全一致，因此对不认识该参数的 master 天然兼容（多余参数被忽略）。
func (s *LinkSource) RefreshStale(ctx context.Context, fileID string) (Link, error) {
	return s.refresh(ctx, fileID, true)
}

// refresh 换链的公共实现。
func (s *LinkSource) refresh(ctx context.Context, fileID string, stale bool) (Link, error) {
	if err := s.checkEnabled(); err != nil {
		return Link{}, err
	}
	link, err := s.fetch(ctx, fileID, stale)
	if err != nil {
		return Link{}, err
	}
	s.mu.Lock()
	s.entries[fileID] = s.newEntry(link)
	s.mu.Unlock()
	return link, nil
}

func (s *LinkSource) cached(fileID string) (Link, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.freshLocked(fileID)
}

func (s *LinkSource) freshLocked(fileID string) (Link, bool) {
	entry, ok := s.entries[fileID]
	if !ok || !s.cfg.Now().Before(entry.refreshAt) {
		return Link{}, false
	}
	return entry.link, true
}

func (s *LinkSource) newEntry(link Link) linkEntry {
	refreshAt := link.ExpiresAt.Add(-s.cfg.Margin)
	if !refreshAt.After(s.cfg.Now()) {
		// 余量不足（或有效期无法解析）：本次照用，但下一次请求就重拉。
		refreshAt = s.cfg.Now()
	}
	return linkEntry{link: link, refreshAt: refreshAt}
}

func (s *LinkSource) checkEnabled() error {
	if s.cfg.Enabled != nil && !s.cfg.Enabled() {
		return &LinkError{
			Message: "agent 已被 master 禁用，暂不获取新直链（已缓存的直链仍可继续服务）",
			Code:    "AGENT_DISABLED",
		}
	}
	return nil
}

type downloadLinkResponse struct {
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt string            `json:"expires_at"`
}

// fetch 向 master 拉一条直链。
//
// 请求用脱离调用方取消的 ctx：拉直链是单飞的共享动作，第一个客户端断开
// 不应该把其他等待者一起打断；仍然受 FetchTimeout 约束。
//
// stale=true 时附加 stale=1：换链方的 hub 直连刚失败（连接级失败或 409），
// 提示 master 先重新确认 hub 状态再应答（v0.4.2 N4）。
func (s *LinkSource) fetch(ctx context.Context, fileID string, stale bool) (Link, error) {
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.FetchTimeout)
	defer cancel()

	var resp downloadLinkResponse
	query := url.Values{"file_id": {fileID}}
	if stale {
		query.Set("stale", "1")
	}
	if err := s.client.GetJSON(fetchCtx, "/api/agent/download-link", query, &resp); err != nil {
		if ctx.Err() != nil {
			return Link{}, ctx.Err() // 调用方（客户端）已断开
		}
		if fetchCtx.Err() != nil {
			return Link{}, &LinkError{Message: "向 master 拉取直链超时", Code: "AGENT_LINK_TIMEOUT"}
		}
		return Link{}, &LinkError{Message: err.Error(), Code: errorCode(err)}
	}
	if resp.URL == "" {
		return Link{}, &LinkError{Message: "master 未返回直链地址", Code: "AGENT_LINK_EMPTY"}
	}
	expiresAt, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	if err != nil {
		// 不因解析失败拒绝服务：按"立即过期"处理（下次请求重拉），并记一条告警。
		s.cfg.Logger.Warn("master 返回的 expires_at 无法解析，该直链不做缓存",
			"file_id", fileID, "expires_at", resp.ExpiresAt, "error", err)
		expiresAt = s.cfg.Now()
	}
	if resp.Headers == nil {
		resp.Headers = map[string]string{}
	}
	s.cfg.Logger.Info("已获取直链", "file_id", fileID, "expires_at", expiresAt.Format(time.RFC3339))
	return Link{URL: resp.URL, Headers: resp.Headers, ExpiresAt: expiresAt}, nil
}

func errorCode(err error) string {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}
