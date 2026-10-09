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
	"strconv"
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
	// Cache 是读前缓存；为空或预算 0 时数据面行为与不带缓存的版本逐字节一致。
	Cache *BlockCache
	// Prefetch 是首触预取器；为空时不预取（测试与关闭场景）。
	Prefetch *Prefetcher
}

// Handler 实现 http.Handler（挂在 "/dl/" 前缀上）。
type Handler struct {
	cfg    Config
	client *http.Client
	log    *slog.Logger
	now    func() time.Time

	cache    *BlockCache
	prefetch *Prefetcher

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
	if cfg.Cache == nil {
		cfg.Cache = NewBlockCache(0) // 未接线 = 功能关闭
	}
	return &Handler{
		cfg:      cfg,
		client:   cfg.Client,
		log:      cfg.Logger,
		now:      cfg.Now,
		cache:    cfg.Cache,
		prefetch: cfg.Prefetch,
		sem:      make(chan struct{}, cfg.MaxConcurrent),
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

	// 首触预取：该文件第一次被请求时异步把头部/尾部拉进缓存（内部判重与开关）。
	// 异步且不返回错误——绝不影响本次请求的响应与耗时。
	h.prefetch.MaybeStart(fileID)

	ctx := r.Context()
	link, err := h.cfg.Links.Link(ctx, fileID)
	if err != nil {
		return h.writeLinkError(w, fileID, err)
	}

	// 读前缓存：只在能完整解析出单区间 `Range: bytes=<start>-<end?>` 的 GET 上启用；
	// 其它形态（无 Range、后缀区间、多区间、HEAD）一律走现状。
	if r.Method == http.MethodGet && h.cache.Enabled() {
		if rng, ok := parseByteRange(r.Header.Get("Range")); ok {
			if status, served := h.serveCached(w, r, fileID, rng, link); served {
				return status
			}
		}
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

	// 状态 3（未命中/无需本地服务）：现状**纯透传**——不缓存任何路过的字节。
	// 只观察响应头里的元数据（内容身份 + 长度/类型）：身份变了说明文件已换版本，
	// 旧块必须立刻作废（随后自然触发下一轮首触预取）。
	//
	// 这里必须观察**完整**元数据而不是只观察 ETag：ETag 缺失时身份退化为
	// Last-Modified（见 fileMeta.identity），而"只观察 ETag"在缺 ETag 的响应上是
	// 空操作——那会让旧身份的块被本地服务一直用下去（陈旧字节），连混合服务的
	// 身份不符回退也救不回来（回退后的透传观测同样是空操作）。
	if h.cache.Enabled() && isCacheableStatus(resp.StatusCode) {
		h.cache.Observe(fileID, responseMeta(resp))
	}

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

// --- 读前缓存的服务三态（design §4） -----------------------------------------

// maxCachePrefixBytes 限制"混合服务"一次最多用多少本地前缀。
// 前缀要拼成一段连续内存，超出部分交给上游——协议语义不变，只是少用一点本地数据；
// 这道上限让"极端大的请求 + 极端大的预算"也不会造成一次巨型拼接。
const maxCachePrefixBytes = 32 << 20

// serveCached 尝试用本地块服务该 Range：
//
//	全命中 → 纯本地 206（**不触上游**）
//	部分命中 → 混合：先打开上游 [start+c, end] 请求，**成功后才写第一个字节**
//	未命中/不可用 → 返回 served=false，调用方走现状**纯透传**（不缓存任何字节）
//
// 只有"能完整解析出 bytes=<start>-<end?>、长度已知且身份可判"的请求才会走到这里。
func (h *Handler) serveCached(w http.ResponseWriter, r *http.Request, fileID string, rng byteRange, link Link) (int, bool) {
	meta, ok := h.cache.Meta(fileID)
	if !ok || meta.size <= 0 {
		return 0, false
	}
	identity := meta.identity()
	if identity == "" {
		return 0, false
	}

	start, end := rng.start, rng.end
	if rng.openEnded() {
		end = meta.size - 1
	}
	// 越界/贴边的请求交给上游：Range 的 clamp / 416 语义以 Google 的响应为准，
	// 本地拼一个不同的结果就会破坏"缓存开/关逐字节一致"。
	if start > end || start >= meta.size || end >= meta.size {
		return 0, false
	}

	if parts, hit := h.cache.FullHit(fileID, identity, start, end); hit {
		writeCachedHeaders(w.Header(), meta, start, end)
		w.WriteHeader(http.StatusPartialContent)
		for _, part := range parts {
			if _, err := w.Write(part); err != nil {
				h.log.Info("客户端断开，缓存响应未写完", "file_id", fileID)
				return http.StatusPartialContent, true
			}
		}
		h.log.Info("缓存命中，本地服务",
			"file_id", fileID, "start", start, "end", end, "bytes", end-start+1)
		return http.StatusPartialContent, true
	}

	limit := end - start + 1
	if limit > maxCachePrefixBytes {
		limit = maxCachePrefixBytes
	}
	prefix, n := h.cache.Prefix(fileID, identity, start, limit)
	if n == 0 {
		return 0, false
	}

	resp, err := h.doUpstreamRange(r.Context(), link, start+n, end)
	if err != nil {
		h.log.Warn("混合服务：上游请求失败，整体回退透传", "file_id", fileID, "error", err)
		return 0, false
	}
	if !isCacheableStatus(resp.StatusCode) {
		_ = resp.Body.Close()
		h.log.Warn("混合服务：上游状态异常，整体回退透传",
			"file_id", fileID, "upstream_status", resp.StatusCode)
		return 0, false
	}
	// 余段响应必须**确实是** [start+n, end]：上游可能忽略 Range 直接回 200 整文件
	// （或回报别的区间）。照抄这种响应体就会把"本地前缀 + 错位数据"拼给客户端——
	// 而且声明了 Content-Length，客户端拿到的是静默损坏的字节。宁可整体回退透传：
	// 透传把上游行为原样转给客户端，也就与"缓存关"逐字节一致（预取路径对同一
	// 情况也是同样的处理：区间不符即放弃，见 prefetch.go fetchBlock）。
	if resp.StatusCode != http.StatusPartialContent {
		_ = resp.Body.Close()
		h.log.Warn("混合服务：上游未按 Range 回 206，整体回退透传",
			"file_id", fileID, "upstream_status", resp.StatusCode)
		return 0, false
	}
	if gotStart, gotEnd, ok := responseRange(resp); !ok || gotStart != start+n || gotEnd != end {
		_ = resp.Body.Close()
		h.log.Warn("混合服务：上游响应区间与请求不符，整体回退透传",
			"file_id", fileID, "want_start", start+n, "want_end", end,
			"got_start", gotStart, "got_end", gotEnd)
		return 0, false
	}
	// 上游余段的身份必须与本地前缀一致：不一致等于把两个版本的数据拼给客户端。
	if got := responseMeta(resp).identity(); got != identity {
		_ = resp.Body.Close()
		h.log.Warn("混合服务：上游内容身份与本地前缀不符，整体回退透传", "file_id", fileID)
		return 0, false
	}
	defer resp.Body.Close()

	writeCachedHeaders(w.Header(), meta, start, end)
	w.WriteHeader(http.StatusPartialContent)
	flush := newFlushWriter(w)
	if _, err := flush.Write(prefix); err != nil {
		h.log.Info("客户端断开，混合响应未写完", "file_id", fileID)
		return http.StatusPartialContent, true
	}
	if _, err := io.CopyBuffer(flush, resp.Body, make([]byte, copyBufferSize)); err != nil {
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			h.log.Info("客户端断开，已取消上游读取", "file_id", fileID)
		} else {
			h.log.Error("混合服务：转发上游余段失败", "file_id", fileID, "error", err)
		}
	}
	h.log.Info("混合服务：本地前缀 + 上游续传",
		"file_id", fileID, "prefix_bytes", n, "start", start, "end", end)
	return http.StatusPartialContent, true
}

// doUpstreamRange 只取文件区间 [start, end] 的上游请求（混合服务用）。
func (h *Handler) doUpstreamRange(ctx context.Context, link Link, start, end int64) (*http.Response, error) {
	req, err := newUpstreamRequest(ctx, http.MethodGet, link, fmt.Sprintf("bytes=%d-%d", start, end))
	if err != nil {
		return nil, err
	}
	return h.client.Do(req)
}

// --- Range 与响应头解析 -------------------------------------------------------

// byteRange 是能完整解析出来的单区间 Range。
type byteRange struct {
	start int64
	end   int64 // < 0 表示开区间（bytes=<start>-）
}

func (r byteRange) openEnded() bool { return r.end < 0 }

// parseByteRange 只接受单区间的 `bytes=<start>-<end?>`；其余形态（无 Range、
// 后缀区间 bytes=-N、多区间、非法值）返回 ok=false，由调用方走现状透传。
func parseByteRange(header string) (byteRange, bool) {
	if header == "" {
		return byteRange{}, false
	}
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok {
		return byteRange{}, false
	}
	spec = strings.TrimSpace(spec)
	if strings.Contains(spec, ",") {
		return byteRange{}, false
	}
	startStr, endStr, ok := strings.Cut(spec, "-")
	if !ok || strings.TrimSpace(startStr) == "" {
		return byteRange{}, false
	}
	start, err := strconv.ParseInt(strings.TrimSpace(startStr), 10, 64)
	if err != nil || start < 0 {
		return byteRange{}, false
	}
	end := int64(-1)
	if endStr = strings.TrimSpace(endStr); endStr != "" {
		parsed, perr := strconv.ParseInt(endStr, 10, 64)
		if perr != nil || parsed < start {
			return byteRange{}, false
		}
		end = parsed
	}
	return byteRange{start: start, end: end}, true
}

// responseMeta 解析上游响应里与缓存有关的元数据（身份链、总长、拼头字段）。
//
// 注意：真实 Google 直链的 206 **没有 ETag、没有 Last-Modified**（2026-10-09
// 实测），身份实际来自 responseTotalSize 解析出的总字节数——所以这里必须把
// size 一并带回去，只取 ETag/Last-Modified 会让身份链落空。
func responseMeta(resp *http.Response) fileMeta {
	return fileMeta{
		etag:         resp.Header.Get("ETag"),
		lastModified: resp.Header.Get("Last-Modified"),
		contentType:  resp.Header.Get("Content-Type"),
		acceptRanges: resp.Header.Get("Accept-Ranges"),
		size:         responseTotalSize(resp),
	}
}

// responseTotalSize 解析文件总字节数：206 取 Content-Range 的总长，
// 200 取 Content-Length；未知返回 0。
func responseTotalSize(resp *http.Response) int64 {
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		// 形如 "bytes 0-99/12345"；"bytes */12345"（416）没有可用的区间信息。
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			if n, err := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64); err == nil && n > 0 {
				return n
			}
		}
		return 0
	}
	if resp.StatusCode == http.StatusOK {
		if n, err := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("Content-Length")), 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// responseRange 解析响应体对应的文件区间 [start, end]（含两端）。
// 206 读 Content-Range，200 视为整文件（0 到 Content-Length-1）；解析不出返回 ok=false。
func responseRange(resp *http.Response) (start, end int64, ok bool) {
	cr := strings.TrimSpace(resp.Header.Get("Content-Range"))
	if resp.StatusCode == http.StatusOK && cr == "" {
		n := responseTotalSize(resp)
		if n <= 0 {
			return 0, 0, false
		}
		return 0, n - 1, true
	}
	spec, found := strings.CutPrefix(cr, "bytes ")
	if !found {
		return 0, 0, false
	}
	rangePart, _, _ := strings.Cut(spec, "/")
	startStr, endStr, found := strings.Cut(rangePart, "-")
	if !found {
		return 0, 0, false
	}
	parsedStart, err1 := strconv.ParseInt(strings.TrimSpace(startStr), 10, 64)
	parsedEnd, err2 := strconv.ParseInt(strings.TrimSpace(endStr), 10, 64)
	if err1 != nil || err2 != nil || parsedStart < 0 || parsedEnd < parsedStart {
		return 0, 0, false
	}
	return parsedStart, parsedEnd, true
}

func isCacheableStatus(code int) bool {
	return code == http.StatusOK || code == http.StatusPartialContent
}

// writeCachedHeaders 为本地/混合的 206 拼白名单响应头（与透传同一套字段）。
//
// Accept-Ranges 只照抄上游观测到的值，不凭空合成：上游没给（真实 Google 直链
// 就没给）而本地硬写一行，会让"缓存开/关"的同一请求响应头不一致——字节一致性
// 验收包含白名单响应头。
func writeCachedHeaders(dst http.Header, meta fileMeta, start, end int64) {
	if meta.contentType != "" {
		dst.Set("Content-Type", meta.contentType)
	}
	dst.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, meta.size))
	dst.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if meta.acceptRanges != "" {
		dst.Set("Accept-Ranges", meta.acceptRanges)
	}
	if meta.etag != "" {
		dst.Set("ETag", meta.etag)
	}
	if meta.lastModified != "" {
		dst.Set("Last-Modified", meta.lastModified)
	}
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
