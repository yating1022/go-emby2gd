// Package proxy 是 agent 的数据面：校验客户端签名 URL，把请求流式转发给 Google。
//
// 协议与错误语义以冻结稿 §2.4/§2.5 为准：
//
//	GET|HEAD /dl/<file_id>?e=<expiry_unix>&s=<hmac_hex>
//
// 本包不碰 master 的凭据头内容（只在请求间搬运），任何日志都不打印
// Google token / sign_key / agent_secret。
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const (
	dlPrefix = "/dl/"

	// forbiddenText 是 403 的统一文案：缺参 / 签名不符 / 已过期都回它，
	// 不区分失败原因（冻结稿 §2.5）。
	forbiddenText = "链接无效或已过期"

	// busyRetryAfterSeconds 是并发满时给客户端的 Retry-After（秒）。
	busyRetryAfterSeconds = "5"
)

// Config 组装数据面 Handler。
type Config struct {
	// SignKey 是该 agent 专属的签名密钥（由 master 在 enroll 时下发）。
	SignKey []byte
	// MaxConcurrent 是并发流上限；满则立即 503，不排队（冻结稿 §2.4-5）。
	MaxConcurrent int
	// Links 提供 Google 直链（缓存 + 单飞）。
	Links *LinkSource
	// Logger 为空则用 slog.Default()。
	Logger *slog.Logger
	// Client 是上游 HTTP 客户端；为空则用 NewUpstreamClient()（测试注入用）。
	Client *http.Client
	// Now 可注入时钟（测试用），默认 time.Now。
	Now func() time.Time
}

// Handler 实现 http.Handler（挂在 "/dl/" 前缀上）。
type Handler struct {
	cfg    Config
	client *http.Client
	log    *slog.Logger
	now    func() time.Time

	sem    chan struct{}
	active atomic.Int64
}

func NewHandler(cfg Config) *Handler {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 32
	}
	if cfg.Client == nil {
		cfg.Client = NewUpstreamClient()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Links == nil {
		panic("proxy: Config.Links 不能为空")
	}
	return &Handler{
		cfg:    cfg,
		client: cfg.Client,
		log:    cfg.Logger,
		now:    cfg.Now,
		sem:    make(chan struct{}, cfg.MaxConcurrent),
	}
}

// ActiveStreams 是当前活跃流数；心跳用它上报 active_streams（冻结稿 §2.2）。
func (h *Handler) ActiveStreams() int64 { return h.active.Load() }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	start := time.Now()

	fileID := ""
	if strings.HasPrefix(r.URL.Path, dlPrefix) {
		fileID = strings.TrimPrefix(r.URL.Path, dlPrefix)
	}
	status := h.serve(rec, r, fileID)

	// 请求日志：只记 file_id / 状态码 / 耗时 / Range。**不记**签名参数 `s`
	// （签名是"能拉这个文件"的凭据，日志接口可被远程读取），
	// 也绝不记 master 下发的凭据头与 master 返回的 Google token。
	h.log.Info("代理请求",
		"method", r.Method,
		"file_id", fileID,
		"status", status,
		"duration_ms", time.Since(start).Milliseconds(),
		"range", r.Header.Get("Range"),
	)
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, fileID string) int {
	if fileID == "" || strings.Contains(fileID, "/") {
		if !strings.HasPrefix(r.URL.Path, dlPrefix) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "请求的资源不存在", nil)
			return http.StatusNotFound
		}
		writeError(w, http.StatusForbidden, "AGENT_URL_FORBIDDEN", forbiddenText, nil)
		return http.StatusForbidden
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
			"该路径不支持此请求方法", http.Header{"Allow": []string{"GET, HEAD"}})
		return http.StatusMethodNotAllowed
	}
	if !Verify(h.cfg.SignKey, fileID, r.URL.Query().Get("e"), r.URL.Query().Get("s"), h.now()) {
		writeError(w, http.StatusForbidden, "AGENT_URL_FORBIDDEN", forbiddenText, nil)
		return http.StatusForbidden
	}

	// 并发闸门放在验签之后：垃圾请求不能占满闸门把正常客户端挤掉。
	select {
	case h.sem <- struct{}{}:
	default:
		writeError(w, http.StatusServiceUnavailable, "AGENT_BUSY",
			"agent 并发流已满，请稍后重试", http.Header{"Retry-After": []string{busyRetryAfterSeconds}})
		return http.StatusServiceUnavailable
	}
	h.active.Add(1)
	defer func() {
		h.active.Add(-1)
		<-h.sem
	}()

	ctx := r.Context()
	link, err := h.cfg.Links.Link(ctx, fileID)
	if err != nil {
		return h.writeLinkError(w, fileID, err)
	}

	resp, err := h.doUpstream(ctx, r, link)
	if err != nil {
		h.log.Error("连接上游失败", "file_id", fileID, "error", err)
		writeError(w, http.StatusBadGateway, "AGENT_UPSTREAM_ERROR",
			fmt.Sprintf("连接上游失败：%v", err), nil)
		return http.StatusBadGateway
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// 直链可能已过期/被撤销：失效缓存重拉一次再试（冻结稿 §2.3 的单次重试）。
		_ = resp.Body.Close()
		h.log.Warn("上游返回错误，重拉直链后重试一次",
			"file_id", fileID, "upstream_status", resp.StatusCode)
		refreshed, rerr := h.cfg.Links.Refresh(ctx, fileID)
		if rerr != nil {
			return h.writeLinkError(w, fileID, rerr)
		}
		resp, err = h.doUpstream(ctx, r, refreshed)
		if err != nil {
			h.log.Error("连接上游失败", "file_id", fileID, "error", err)
			writeError(w, http.StatusBadGateway, "AGENT_UPSTREAM_ERROR",
				fmt.Sprintf("连接上游失败：%v", err), nil)
			return http.StatusBadGateway
		}
	}
	defer resp.Body.Close()

	// 状态码与白名单响应头原样透传（200/206/416…）。
	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return resp.StatusCode
	}
	buf := make([]byte, copyBufferSize)
	if _, err := io.CopyBuffer(newFlushWriter(w), resp.Body, buf); err != nil {
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			h.log.Info("客户端断开，已取消上游读取", "file_id", fileID)
		} else {
			h.log.Error("转发响应体失败", "file_id", fileID, "error", err)
		}
	}
	return resp.StatusCode
}

// doUpstream 发一次上游请求。method（GET/HEAD）与 Range 都来自客户端。
func (h *Handler) doUpstream(ctx context.Context, r *http.Request, link Link) (*http.Response, error) {
	req, err := newUpstreamRequest(ctx, r.Method, link, r.Header.Get("Range"))
	if err != nil {
		return nil, err
	}
	return h.client.Do(req)
}

// writeLinkError 把"拿不到直链"翻成 502（冻结稿 §2.5：透传中文原因）。
func (h *Handler) writeLinkError(w http.ResponseWriter, fileID string, err error) int {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		h.log.Info("请求在处理前被取消", "file_id", fileID)
		writeError(w, http.StatusBadGateway, "AGENT_LINK_UNAVAILABLE", "请求已取消", nil)
		return http.StatusBadGateway
	}
	h.log.Error("获取直链失败", "file_id", fileID, "error", err)
	code := "AGENT_LINK_UNAVAILABLE"
	var linkErr *LinkError
	if errors.As(err, &linkErr) && linkErr.Code != "" {
		code = linkErr.Code
	}
	writeError(w, http.StatusBadGateway, code, err.Error(), nil)
	return http.StatusBadGateway
}

// errorEnvelope 与 master 的失败响应同形状（backend/app/core/response.py），
// 客户端只需要一种解包逻辑。
type errorEnvelope struct {
	OK    bool `json:"ok"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string, extra http.Header) {
	var body errorEnvelope
	body.OK = false
	body.Error.Code = code
	body.Error.Message = message
	payload, err := json.Marshal(body)
	if err != nil {
		payload = []byte(`{"ok":false,"error":{"code":"INTERNAL_ERROR","message":"服务内部错误"}}`)
	}
	for name, values := range extra {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// statusRecorder 记录实际写出的状态码，供请求日志使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap 让 http.ResponseController 能穿透包装找到原始 ResponseWriter。
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
