// hub 角色的数据面与控制面（design 10-10-hub-agent-mode §3–§5）。
//
// 数据面 /f/<fileID> 三态服务，语义与 node 的 serveCached 完全一致：
//
//	全命中   → 纯本地 206（不出网、不需要凭据）
//	部分命中 → 前缀先行混合（本地前缀 + 上游续传，校验不过则断连）
//	未命中   → 用 warm 时存的直链回源透传，并同步把字节按块落盘
//
// 没有该文件直链（未 warm）→ 409 {"error":"not_warmed"}。
// 控制面 /warm、/cancel 只允许 HUB_ALLOW_IPS 白名单（空 = 全部拒绝，fail-closed）。
//
// 日志铁律：/warm 载荷含账号级 Google 凭据（auth）与直链，任何路径都不得
// 打印直链/凭据/请求体；只记 file_id、区间、块数与字节数。
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	hubFilePrefix = "/f/"
	hubWarmPath   = "/warm"
	hubCancelPath = "/cancel"

	// warmRequestMaxBytes 限制 /warm 请求体大小（防白名单内的畸形载荷）。
	warmRequestMaxBytes = 1 << 20

	// hubWarmWaitDefault 是"3 分钟规则"的窗口：warm 被接受起计时，
	// 期间出现播放 → 转全量续取；窗口到而无播放 → 停在区域集。
	hubWarmWaitDefault = 3 * time.Minute
	// hubWarmPollDefault 是等待播放的复查间隔。
	hubWarmPollDefault = 250 * time.Millisecond
	// hubRegionTimeout 是一轮"区域集抓取"的总时限；超时即停，未完成部分
	// 交给后续请求（warm 重发/播放触发）续取。
	hubRegionTimeout = 10 * time.Minute
	// hubStallTimeoutDefault 是填充流的无进展看门狗：这么久没读到新字节
	// 即放弃本条流（兜住坏 IP / 静默挂死），已落块保留。
	hubStallTimeoutDefault = 5 * time.Minute
	// hubMaxFullRounds 是全量续取"连续无进展"的轮数上限：超过即停手，
	// 交给下一次播放请求再触发（避免坏直链上无限空转）。
	hubMaxFullRounds = 3
	// hubFillWarnThreshold 是同时在跑填充流的告警阈值（不设硬上限：
	// /warm 契约承诺 200=已接受，拒绝会误导 master 的"已接受"标记）。
	hubFillWarnThreshold = 8
	// hubMaxStates 是 warm 状态表的容量上限（超出先淘汰无在途流的旧条目）。
	hubMaxStates = 4096
	// hubYieldBytes / hubYieldInterval 是 hub 填充流"让路"的检查粒度
	// （与 node 预取同一语义与量级）。
	hubYieldBytes    = 1 << 20
	hubYieldInterval = 250 * time.Millisecond
)

// warmRegionsPayload 是 /warm 载荷里的区域集（原始形态）。
//
// resume_offset_bytes 是 master 现行字段；resume_offset 是任务 design §4 的
// 写法。两者都收、以 *_bytes 优先（兼容而不猜测）。
type warmRegionsPayload struct {
	HeadBytes         int64  `json:"head_bytes"`
	TailBytes         int64  `json:"tail_bytes"`
	ResumeOffset      *int64 `json:"resume_offset,omitempty"`
	ResumeOffsetBytes *int64 `json:"resume_offset_bytes,omitempty"`
}

// warmRequest 是 /warm 的请求体（design §4；字段名与 master 侧冻结一致）。
type warmRequest struct {
	FileID     string              `json:"file_id"`
	FileToken  string              `json:"file_token"`
	DirectLink string              `json:"direct_link"`
	Auth       map[string]string   `json:"auth"`
	Regions    *warmRegionsPayload `json:"regions"`
}

// warmRegionsSpec 是解析后的区域集（0 = 不做该区域）。
type warmRegionsSpec struct {
	headBytes    int64
	tailBytes    int64
	resumeOffset int64
	resumeWindow int64
}

// warmLink 是 hub 持有的一条文件直链（含换链凭据 token）。
type warmLink struct {
	link  Link
	token string // download-link 通道的 file_id（base64url 路径），换新链用
}

// warmState 是一个文件的 warm 状态（字段读写一律在 Hub.mu 下）。
type warmState struct {
	link     warmLink
	spec     warmRegionsSpec
	run      *hubWarmRun
	stopped  bool // /cancel 的粘性停止位；下一次 /warm 清除
	playback bool // 是否出现过"真实播放"（供流即播放判据）
	lastUse  time.Time
}

// ipRule 是白名单的一条规则：单 IP 或 CIDR。
type ipRule struct {
	single netip.Addr
	prefix netip.Prefix
}

// HubConfig 组装 Hub。
type HubConfig struct {
	// AllowIPs 是 HUB_ALLOW_IPS 原文（逗号/空白分隔，支持 CIDR）；空 = 全部拒绝。
	AllowIPs string
	// Cache 是磁盘块存储；必填。
	Cache *DiskCache
	// Links 是直链来源（换链复用同一 download-link 通道）；必填。
	Links  *LinkSource
	Logger *slog.Logger
	// Client 是上游 HTTP 客户端；默认 NewHubUpstreamClient()（多地址拨号）。
	Client *http.Client
	// MaxConcurrent 是 /f/ 并发流上限；满则 503（不排队）。
	MaxConcurrent int
	// Warm* 是 /warm 载荷缺省值（载荷给 0 时回落到这里）。
	WarmHeadBytes         int64
	WarmTailBytes         int64
	WarmResumeWindowBytes int64
	// WarmWait 是 3 分钟窗口（默认 3min）；WarmPollInterval 是等待播放的
	// 复查间隔；StallTimeout 是填充流的无进展看门狗。
	WarmWait         time.Duration
	WarmPollInterval time.Duration
	StallTimeout     time.Duration
	// Now 可注入时钟（测试用），默认 time.Now。
	Now func() time.Time
}

// Hub 实现 hub 角色的 HTTP 处理器（数据面 + 控制面）。
type Hub struct {
	cfg    HubConfig
	log    *slog.Logger
	client *http.Client
	now    func() time.Time
	allow  []ipRule

	cache *DiskCache
	links *LinkSource

	sem    chan struct{}
	active atomic.Int64

	mu         sync.Mutex
	states     map[string]*warmState
	clients    map[string]int // 同文件在途 /f/ 客户端请求数（供流即播放判据）
	activeRuns atomic.Int64
}

// NewHub 构造 hub 处理器。
func NewHub(cfg HubConfig) *Hub {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 32
	}
	if cfg.Client == nil {
		cfg.Client = NewHubUpstreamClient()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.WarmWait <= 0 {
		cfg.WarmWait = hubWarmWaitDefault
	}
	if cfg.WarmPollInterval <= 0 {
		cfg.WarmPollInterval = hubWarmPollDefault
	}
	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = hubStallTimeoutDefault
	}
	if cfg.Cache == nil {
		panic("proxy: HubConfig.Cache 不能为空")
	}
	if cfg.Links == nil {
		panic("proxy: HubConfig.Links 不能为空")
	}
	h := &Hub{
		cfg:     cfg,
		log:     cfg.Logger,
		client:  cfg.Client,
		now:     cfg.Now,
		cache:   cfg.Cache,
		links:   cfg.Links,
		allow:   parseAllowIPs(cfg.AllowIPs, cfg.Logger),
		sem:     make(chan struct{}, cfg.MaxConcurrent),
		states:  make(map[string]*warmState),
		clients: make(map[string]int),
	}
	if len(h.allow) == 0 {
		h.log.Warn("HUB_ALLOW_IPS 白名单为空：/f/ 与 /warm、/cancel 全部拒绝（fail-closed）")
	}
	return h
}

// ActiveStreams 是当前活跃的 /f/ 流数；心跳用它上报 active_streams。
func (h *Hub) ActiveStreams() int64 { return h.active.Load() }

// ServeHTTP 按路径路由；访问控制统一在最前（白名单外一律 403）。
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	start := time.Now()
	status := h.route(rec, r)
	// 请求日志只记方法/路径/远端/状态/耗时；/warm 的请求体绝不进日志。
	h.log.Info("hub 请求",
		"method", r.Method,
		"path", r.URL.Path,
		"remote", remoteIP(r.RemoteAddr),
		"status", status,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

func (h *Hub) route(w http.ResponseWriter, r *http.Request) int {
	if !h.allowed(r.RemoteAddr) {
		writeHubError(w, http.StatusForbidden, "forbidden")
		return http.StatusForbidden
	}
	switch {
	case strings.HasPrefix(r.URL.Path, hubFilePrefix):
		return h.serveFile(w, r, hubFileID(r))
	case r.URL.Path == hubWarmPath:
		return h.handleWarm(w, r)
	case r.URL.Path == hubCancelPath:
		return h.handleCancel(w, r)
	default:
		writeHubError(w, http.StatusNotFound, "not_found")
		return http.StatusNotFound
	}
}

// hubFileID 从 /f/<fileID> 里取出文件 id。master 用 url.PathEscape 编码，
// 文件 id 自身含 "/" 时会以 %2F 出现（r.URL.Path 已被解码成 "/"），
// 这时从 EscapedPath 还原。
func hubFileID(r *http.Request) string {
	fileID := strings.TrimPrefix(r.URL.Path, hubFilePrefix)
	if !strings.Contains(fileID, "/") {
		return fileID
	}
	if esc := r.URL.EscapedPath(); strings.HasPrefix(esc, hubFilePrefix) {
		if decoded, err := url.PathUnescape(strings.TrimPrefix(esc, hubFilePrefix)); err == nil {
			return decoded
		}
	}
	return fileID
}

// parseAllowIPs 解析白名单原文：逗号或空白分隔，元素为单 IP 或 CIDR；
// 解析不了的条目跳过并 WARN（宁缺毋滥：坏条目不能变成"放行所有"）。
func parseAllowIPs(raw string, logger *slog.Logger) []ipRule {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	rules := make([]ipRule, 0, len(fields))
	for _, token := range fields {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(token); err == nil {
			addr := prefix.Addr()
			if addr.Is4In6() {
				if prefix.Bits() < 96 {
					logger.Warn("HUB_ALLOW_IPS 条目无法识别，已跳过", "entry", token)
					continue
				}
				prefix = netip.PrefixFrom(addr.Unmap(), prefix.Bits()-96)
			}
			rules = append(rules, ipRule{prefix: prefix.Masked()})
			continue
		}
		if addr, err := netip.ParseAddr(token); err == nil {
			rules = append(rules, ipRule{single: addr.Unmap()})
			continue
		}
		logger.Warn("HUB_ALLOW_IPS 条目无法识别，已跳过", "entry", token)
	}
	return rules
}

// allowed 报告远端地址是否在白名单内（白名单为空恒为 false）。
func (h *Hub) allowed(remoteAddr string) bool {
	addr, err := netip.ParseAddr(remoteIP(remoteAddr))
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, rule := range h.allow {
		if rule.single.IsValid() {
			if rule.single == addr {
				return true
			}
			continue
		}
		if rule.prefix.IsValid() && rule.prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// remoteIP 取 "host:port" 里的 host（已是裸 IP；解析失败原样返回交给白名单拒绝）。
func remoteIP(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// hubStatusBody 是控制面的简单响应体。
type hubStatusBody struct {
	Status string `json:"status"`
}

// writeHubJSON 写一个 JSON 响应。
func writeHubJSON(w http.ResponseWriter, status int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		payload = []byte(`{"error":"internal_error"}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// writeHubError 写 hub 数据面/控制面的统一错误体：{"error":"<code>"}
// （design §3 冻结的 not_warmed 形状；与 master 的项目信封刻意不同——
// 这个接口只面向 master 与内网节点，契约以 design 为准）。
func writeHubError(w http.ResponseWriter, status int, code string) {
	payload, _ := json.Marshal(map[string]string{"error": code})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// --- 数据面：/f/<fileID> 三态服务 ----------------------------------------------

// serveFile 服务一次 /f/ 请求。三态与 node 的 serveCached 完全一致，差别只在
// 缓存是磁盘的、直链来自 /warm（或换链）。
func (h *Hub) serveFile(w http.ResponseWriter, r *http.Request, fileID string) int {
	if fileID == "" {
		writeHubError(w, http.StatusNotFound, "not_found")
		return http.StatusNotFound
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeHubError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return http.StatusMethodNotAllowed
	}

	select {
	case h.sem <- struct{}{}:
	default:
		w.Header().Set("Retry-After", busyRetryAfterSeconds)
		writeHubError(w, http.StatusServiceUnavailable, "busy")
		return http.StatusServiceUnavailable
	}
	h.active.Add(1)
	defer func() {
		h.active.Add(-1)
		<-h.sem
	}()

	link, hasLink := h.stateLink(fileID)
	if r.Method == http.MethodGet {
		// 供流即"真实播放"判据（design §3）：登记在途客户端并顺带触发续取。
		h.clientBegin(fileID)
		defer h.clientEnd(fileID)
		h.maybeContinue(fileID)
	}

	// 读前缓存三态只对 GET 启用（与 node 一致：HEAD 一律走现状）。
	if r.Method == http.MethodGet && h.cache.Enabled() {
		if status, served := h.serveCachedDisk(w, r, fileID, hasLink, link); served {
			return status
		}
	}

	// 未命中：需要 warm 时存的直链回源。
	if !hasLink {
		writeHubError(w, http.StatusConflict, "not_warmed")
		return http.StatusConflict
	}
	return h.servePassthrough(w, r, fileID, link)
}

// serveCachedDisk 尝试用磁盘块服务该 Range（三态分发）。
//
//	全命中   → serveLocal（纯本地 206）
//	部分命中 → serveMixed（本地前缀 + 上游续传；无直链则 409）
//	空覆盖   → served=false，调用方走透传
func (h *Hub) serveCachedDisk(w http.ResponseWriter, r *http.Request, fileID string, hasLink bool, link Link) (int, bool) {
	meta, ok := h.cache.Meta(fileID)
	if !ok || meta.size <= 0 {
		return 0, false
	}
	identity := meta.identity()
	if identity == "" {
		return 0, false
	}
	// 后缀区间 bytes=-N（v0.4.1 F2）：size 已知时收窄为 [size-N, size-1]（N>=size →
	// 整个文件；N=0 → 解析失败，原样透传交上游按其 416 语义处理），此后与常规
	// 区间一样走三态服务——缓存命中即可本地供流。size 未知的路径根本走不到这里
	// （上面 meta.size<=0 已返回），维持"透传上游"的现状。
	rng, ok := parseByteRangeSized(r.Header.Get("Range"), meta.size)
	if !ok {
		return 0, false
	}
	start, end := rng.start, rng.end
	if rng.openEnded() {
		end = meta.size - 1
	}
	// 越界/贴边交给上游：clamp/416 语义以 Google 的响应为准（与 node 同一理由）。
	if start > end || start >= meta.size || end >= meta.size {
		return 0, false
	}

	// Pin 先于 Coverage：供流期间文件不参与 TTL/LRU（读到一半被淘汰就是坏字节）。
	release := h.cache.Pin(fileID)
	avail, full := h.cache.Coverage(fileID, identity, start, end)
	switch {
	case full:
		return h.serveLocal(w, r, fileID, meta, start, end, release), true
	case avail > 0 && hasLink:
		return h.serveMixed(w, r, fileID, meta, link, identity, start, end, avail, release), true
	case avail > 0:
		// 有本地前缀但没直链：约等于 all-or-nothing——宁可 409 让 master 重 warm，
		// 也不给节点拼一个"半本地半无源"的响应。
		release()
		writeHubError(w, http.StatusConflict, "not_warmed")
		return http.StatusConflict, true
	default:
		release()
		return 0, false
	}
}

// serveLocal 纯本地 206（不出网）。读盘失败只能断连（Content-Length 已声明）。
func (h *Hub) serveLocal(w http.ResponseWriter, r *http.Request, fileID string, meta fileMeta, start, end int64, release func()) int {
	defer release()
	writeCachedHeaders(w.Header(), meta, start, end)
	w.WriteHeader(http.StatusPartialContent)
	if err := h.cache.Stream(newFlushWriter(w), fileID, meta.identity(), start, end-start+1); err != nil {
		if r.Context().Err() != nil {
			h.log.Info("客户端断开，缓存响应未写完", "file_id", fileID)
			return http.StatusPartialContent
		}
		h.log.Warn("hub 缓存命中：本地读取失败，断开连接", "file_id", fileID, "error", err)
		panic(http.ErrAbortHandler)
	}
	h.log.Info("hub 缓存命中，本地服务",
		"file_id", fileID, "start", start, "end", end, "bytes", end-start+1)
	return http.StatusPartialContent
}

// serveMixed 部分命中：先写 206 头 + 本地前缀并 Flush，再从上游续传
// [start+avail, end]。校验（206 + 精确区间 + 身份一致）不过只能断连
// （头与前缀已写出，无法整体回退；断连等价网络中断，客户端自愈重试）。
func (h *Hub) serveMixed(w http.ResponseWriter, r *http.Request, fileID string, meta fileMeta, link Link, identity string, start, end, avail int64, release func()) int {
	defer release()
	writeCachedHeaders(w.Header(), meta, start, end)
	w.WriteHeader(http.StatusPartialContent)
	flush := newFlushWriter(w)
	if err := h.cache.Stream(flush, fileID, identity, start, avail); err != nil {
		if r.Context().Err() != nil {
			h.log.Info("客户端断开，混合响应未写完", "file_id", fileID)
			return http.StatusPartialContent
		}
		return h.abortMixed(fileID, "hub 混合服务：本地前缀读取失败，断开连接（前缀已写出，无法回退）", "error", err)
	}
	resume := start + avail
	if resume > end {
		// 本地前缀已覆盖整个请求（与 Coverage 之间被并发填充）：没有余段要续传。
		h.log.Info("hub 混合服务：本地前缀 + 上游续传",
			"file_id", fileID, "prefix_bytes", avail, "start", start, "end", end)
		return http.StatusPartialContent
	}

	resp, err := h.upstreamRangeWithRefresh(r.Context(), fileID, link, resume, end)
	if err != nil {
		return h.abortMixed(fileID, "hub 混合服务：上游请求失败，断开连接（前缀已写出，无法回退）", "error", err)
	}
	if !isCacheableStatus(resp.StatusCode) {
		_ = resp.Body.Close()
		return h.abortMixed(fileID, "hub 混合服务：上游状态异常，断开连接（前缀已写出，无法回退）",
			"upstream_status", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusPartialContent {
		_ = resp.Body.Close()
		return h.abortMixed(fileID, "hub 混合服务：上游未按 Range 回 206，断开连接（前缀已写出，无法回退）",
			"upstream_status", resp.StatusCode)
	}
	if gotStart, gotEnd, ok := responseRange(resp); !ok || gotStart != resume || gotEnd != end {
		_ = resp.Body.Close()
		return h.abortMixed(fileID, "hub 混合服务：上游响应区间与请求不符，断开连接（前缀已写出，无法回退）",
			"want_start", resume, "want_end", end, "got_start", gotStart, "got_end", gotEnd)
	}
	if got := responseMeta(resp); got.identity() != identity {
		h.cache.Observe(fileID, got)
		_ = resp.Body.Close()
		return h.abortMixed(fileID, "hub 混合服务：上游内容身份与本地前缀不符，断开连接（前缀已写出，无法回退）")
	}
	defer resp.Body.Close()

	if _, err := io.CopyBuffer(flush, resp.Body, make([]byte, copyBufferSize)); err != nil {
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			h.log.Info("客户端断开，已取消上游读取", "file_id", fileID)
		} else {
			h.log.Error("hub 混合服务：转发上游余段失败", "file_id", fileID, "error", err)
		}
	}
	h.log.Info("hub 混合服务：本地前缀 + 上游续传",
		"file_id", fileID, "prefix_bytes", avail, "start", start, "end", end)
	return http.StatusPartialContent
}

// abortMixed 记 WARN 后立刻断开连接（http.ErrAbortHandler 哨兵，不打栈）。
// 返回值只为让调用点写成 `return h.abortMixed(...)`；本函数不会真正返回。
func (h *Hub) abortMixed(fileID, msg string, args ...any) int {
	h.log.Warn(msg, append([]any{"file_id", fileID}, args...)...)
	panic(http.ErrAbortHandler)
}

// servePassthrough 未命中：用 warm 存的直链回源透传；可缓存的响应边透传边
// 按块**同步**落盘（design §3 的 miss 路径；写盘失败只跳过该块，不中断供流）。
func (h *Hub) servePassthrough(w http.ResponseWriter, r *http.Request, fileID string, link Link) int {
	resp, err := h.doUpstream(r.Context(), r.Method, link, r.Header.Get("Range"))
	if err != nil {
		h.log.Error("hub 连接上游失败", "file_id", fileID, "error", err)
		writeError(w, http.StatusBadGateway, "AGENT_UPSTREAM_ERROR",
			fmt.Sprintf("连接上游失败：%v", err), nil)
		return http.StatusBadGateway
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// 直链已过期/被撤销（master 下发的直链约 1h 有效）：换新链重试一次，
		// 与 node/预取同一语义（冻结稿 §2.3）。
		_ = resp.Body.Close()
		h.log.Warn("hub 上游返回错误，重拉直链后重试一次",
			"file_id", fileID, "upstream_status", resp.StatusCode)
		refreshed, rerr := h.refreshWarmLink(r.Context(), fileID)
		if rerr != nil {
			h.log.Error("hub 获取直链失败", "file_id", fileID, "error", rerr)
			writeError(w, http.StatusBadGateway, "AGENT_LINK_UNAVAILABLE", rerr.Error(), nil)
			return http.StatusBadGateway
		}
		resp, err = h.doUpstream(r.Context(), r.Method, refreshed, r.Header.Get("Range"))
		if err != nil {
			h.log.Error("hub 连接上游失败", "file_id", fileID, "error", err)
			writeError(w, http.StatusBadGateway, "AGENT_UPSTREAM_ERROR",
				fmt.Sprintf("连接上游失败：%v", err), nil)
			return http.StatusBadGateway
		}
	}
	defer resp.Body.Close()

	merged := fileMeta{}
	cacheable := h.cache.Enabled() && isCacheableStatus(resp.StatusCode)
	if cacheable {
		merged = h.cache.Observe(fileID, responseMeta(resp))
	}
	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return resp.StatusCode
	}

	dst := newFlushWriter(w)
	var tee *hubFiller
	if cacheable {
		if identity := merged.identity(); identity != "" {
			if gotStart, _, ok := responseRange(resp); ok {
				tee = newHubFiller(h.cache, h.log, fileID, identity, merged.size, gotStart)
				dst = io.MultiWriter(dst, tee)
			}
		}
	}
	buf := make([]byte, copyBufferSize)
	if _, err := io.CopyBuffer(dst, resp.Body, buf); err != nil {
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			h.log.Info("客户端断开，已取消上游读取", "file_id", fileID)
		} else {
			h.log.Error("hub 转发响应体失败", "file_id", fileID, "error", err)
		}
	}
	if tee != nil {
		tee.finish()
	}
	return resp.StatusCode
}

// doUpstream 发一次上游请求（method 与 Range 都来自调用方）。
func (h *Hub) doUpstream(ctx context.Context, method string, link Link, rangeHeader string) (*http.Response, error) {
	req, err := newUpstreamRequest(ctx, method, link, rangeHeader)
	if err != nil {
		return nil, err
	}
	return h.client.Do(req)
}

// upstreamRangeWithRefresh 取 [start, end]；401/403 时换新链且只重试一次。
func (h *Hub) upstreamRangeWithRefresh(ctx context.Context, fileID string, link Link, start, end int64) (*http.Response, error) {
	header := fmt.Sprintf("bytes=%d-%d", start, end)
	resp, err := h.doUpstream(ctx, http.MethodGet, link, header)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return resp, nil
	}
	_ = resp.Body.Close()
	h.log.Warn("hub 上游返回错误，重拉直链后重试一次",
		"file_id", fileID, "upstream_status", resp.StatusCode)
	refreshed, rerr := h.refreshWarmLink(ctx, fileID)
	if rerr != nil {
		return nil, rerr
	}
	return h.doUpstream(ctx, http.MethodGet, refreshed, header)
}

// --- 透传同步落盘（hubFiller） -------------------------------------------------

// hubFiller 是"路过的字节按块落盘"的 io.Writer 实现：
//   - Write 永不报错（透传是主线，落盘是尽力而为；写盘失败只跳过该块）；
//   - 只有从**块边界**开始的完整块才 Put（凑满即落盘，与 v0.3.2 预取同一不变式）；
//   - 起点不在块边界（客户端 Range 任意起点）时丢掉块内前缀字节，从下一块起收集；
//   - 落块前做身份新鲜度检查（流在途期间文件被替换 → 停手，不写陈旧字节）。
type hubFiller struct {
	cache    *DiskCache
	log      *slog.Logger
	fileID   string
	identity string
	size     int64

	idx      int64 // 下一个待落盘块号
	pos      int64 // 已消费的文件偏移（日志用）
	skip     int64 // 待丢弃的前导字节（到下一个块边界为止）
	buf      []byte
	blocks   int
	bytes    int64
	failed   bool
	firstPut bool
	started  time.Time
}

// newHubFiller 构造一个从文件偏移 start 开始收集的填充器。
func newHubFiller(cache *DiskCache, log *slog.Logger, fileID, identity string, size, start int64) *hubFiller {
	f := &hubFiller{
		cache:    cache,
		log:      log,
		fileID:   fileID,
		identity: identity,
		size:     size,
		pos:      start,
		started:  time.Now(),
	}
	if rem := start % blockSize; rem != 0 {
		// 起点不在块边界：本块缺头部字节，凑不齐，丢到下一块边界再收。
		f.skip = blockSize - rem
		f.idx = start/blockSize + 1
	} else {
		f.idx = start / blockSize
	}
	f.buf = make([]byte, 0, blockSize)
	return f
}

// blockLen 是块 idx 的期望完整长度（与预取/磁盘写侧同一判据）。
func (f *hubFiller) blockLen(idx int64) int64 {
	if f.size > 0 && (idx+1)*blockSize > f.size {
		if n := f.size - idx*blockSize; n > 0 {
			return n
		}
	}
	return blockSize
}

// Write 实现 io.Writer；永不返回错误。
func (f *hubFiller) Write(p []byte) (int, error) {
	n := len(p)
	if f.failed || f.identity == "" || len(p) == 0 {
		return n, nil
	}
	data := p
	if f.skip > 0 {
		if int64(len(data)) <= f.skip {
			f.skip -= int64(len(data))
			f.pos += int64(len(data))
			return n, nil
		}
		data = data[f.skip:]
		f.pos += f.skip
		f.skip = 0
	}
	f.buf = append(f.buf, data...)
	f.pos += int64(len(data))
	for {
		need := f.blockLen(f.idx)
		if need <= 0 || int64(len(f.buf)) < need {
			return n, nil
		}
		if !f.put(f.idx, f.buf[:need]) {
			f.failed = true
			return n, nil
		}
		f.buf = append(f.buf[:0], f.buf[need:]...)
		f.idx++
	}
}

// put 落一个完整块；身份在流进行中变化（文件被替换）返回 false 让调用方停手。
func (f *hubFiller) put(idx int64, data []byte) bool {
	if meta, ok := f.cache.Meta(f.fileID); ok {
		if id := meta.identity(); id != "" && id != f.identity {
			f.log.Warn("hub 落盘：流进行中内容身份已变化，停止收集",
				"file_id", f.fileID, "block", idx, "prev_identity", f.identity, "identity", id)
			return false
		}
	}
	if f.cache.Put(f.fileID, f.identity, idx, data) {
		f.blocks++
		f.bytes += int64(len(data))
		if !f.firstPut {
			f.firstPut = true
			f.log.Info("hub 预热：首块就绪",
				"file_id", f.fileID, "block", idx, "bytes", len(data),
				"duration_ms", time.Since(f.started).Milliseconds())
		}
	}
	return true
}

// finish 收尾：有落块时记一条汇总日志，并在超预算时触发一轮 LRU。
func (f *hubFiller) finish() {
	if f.blocks == 0 {
		return
	}
	f.cache.EnforceBudget()
	f.log.Info("hub 透传同步落盘",
		"file_id", f.fileID, "blocks", f.blocks, "bytes", f.bytes,
		"duration_ms", time.Since(f.started).Milliseconds())
}

// --- 控制面：/warm 与 /cancel --------------------------------------------------

// hubWarmRun 是一轮预热/续取的可变状态（只被单个 goroutine 持有，链接从
// Hub.states 现读，避免多副本竞态）。
type hubWarmRun struct {
	h          *Hub
	ctx        context.Context
	cancel     context.CancelFunc
	fileID     string
	spec       warmRegionsSpec
	acceptedAt time.Time
	started    time.Time
	requests   int
	blocks     int
	bytes      int64
}

// handleWarm 处理一条 /warm：建立/刷新状态，必要时启动预热。
//
// 语义（design §4 + master 对齐）：
//   - 幂等：同文件已有在途流（无论直链是否相同）→ 200 并采纳新直链/token；
//     区域集已齐且无续传 → 200 无动作；
//   - 接受即 200 {"status":"warming"}（master 依赖 200 = 已接受）；
//   - 未知字段容忍（不开 DisallowUnknownFields，契约允许未来加字段）；
//   - file_token 缓存起来供直链过期后走 download-link 通道换链。
func (h *Hub) handleWarm(w http.ResponseWriter, r *http.Request) int {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeHubError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return http.StatusMethodNotAllowed
	}
	body := http.MaxBytesReader(w, r.Body, warmRequestMaxBytes)
	var req warmRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeHubError(w, http.StatusBadRequest, "bad_request")
		return http.StatusBadRequest
	}
	fileID := strings.TrimSpace(req.FileID)
	if fileID == "" || strings.Contains(fileID, "/") {
		writeHubError(w, http.StatusBadRequest, "bad_request")
		return http.StatusBadRequest
	}
	if strings.TrimSpace(req.DirectLink) == "" {
		writeHubError(w, http.StatusBadRequest, "bad_request")
		return http.StatusBadRequest
	}
	spec := h.resolveRegions(req.Regions)
	link := warmLink{
		link:  Link{URL: req.DirectLink, Headers: req.Auth},
		token: strings.TrimSpace(req.FileToken),
	}
	now := h.now()

	h.mu.Lock()
	st := h.states[fileID]
	if st == nil {
		h.evictStatesLocked()
		st = &warmState{}
		h.states[fileID] = st
	}
	st.lastUse = now
	st.stopped = false
	st.spec = spec
	st.link = link
	st.playback = h.clients[fileID] > 0
	repeat := st.run != nil
	h.mu.Unlock()

	if repeat {
		// 重复 warm：在途流不动，直链/token 已被采纳供后续请求与换链使用。
		h.log.Info("hub 预热：重复 warm，已采纳新直链", "file_id", fileID)
		writeHubJSON(w, http.StatusOK, hubStatusBody{Status: "warming"})
		return http.StatusOK
	}
	if h.regionsComplete(fileID, spec) {
		// 幂等：区域集已齐且无续传 → 200 无动作。
		writeHubJSON(w, http.StatusOK, hubStatusBody{Status: "warming"})
		return http.StatusOK
	}
	h.startRun(fileID)
	h.log.Info("hub 预热已接受",
		"file_id", fileID, "head_bytes", spec.headBytes,
		"tail_bytes", spec.tailBytes, "resume_offset", spec.resumeOffset)
	writeHubJSON(w, http.StatusOK, hubStatusBody{Status: "warming"})
	return http.StatusOK
}

// handleCancel 处理 /cancel：立即停止续传（已落块保留、粘性停止到下一次 /warm）。
func (h *Hub) handleCancel(w http.ResponseWriter, r *http.Request) int {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeHubError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return http.StatusMethodNotAllowed
	}
	body := http.MaxBytesReader(w, r.Body, warmRequestMaxBytes)
	var req struct {
		FileID string `json:"file_id"`
	}
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeHubError(w, http.StatusBadRequest, "bad_request")
		return http.StatusBadRequest
	}
	fileID := strings.TrimSpace(req.FileID)
	if fileID == "" {
		writeHubError(w, http.StatusBadRequest, "bad_request")
		return http.StatusBadRequest
	}
	h.mu.Lock()
	var run *hubWarmRun
	if st := h.states[fileID]; st != nil {
		st.stopped = true
		st.lastUse = h.now()
		run = st.run
	}
	h.mu.Unlock()
	if run != nil {
		run.cancel()
	}
	h.log.Info("hub 预热已取消", "file_id", fileID, "had_run", run != nil)
	writeHubJSON(w, http.StatusOK, hubStatusBody{Status: "stopped"})
	return http.StatusOK
}

// resolveRegions 解析区域集：载荷给 0 的项回落到配置缺省值。
func (h *Hub) resolveRegions(p *warmRegionsPayload) warmRegionsSpec {
	spec := h.defaultSpec()
	if p == nil {
		return spec
	}
	if p.HeadBytes > 0 {
		spec.headBytes = p.HeadBytes
	}
	if p.TailBytes > 0 {
		spec.tailBytes = p.TailBytes
	}
	// resume_offset_bytes 是 master 现行字段（冻结名）；resume_offset 是任务
	// design §4 的写法。两者都收、以 *_bytes 优先（master 目前恒不发）。
	switch {
	case p.ResumeOffsetBytes != nil && *p.ResumeOffsetBytes > 0:
		spec.resumeOffset = *p.ResumeOffsetBytes
	case p.ResumeOffset != nil && *p.ResumeOffset > 0:
		spec.resumeOffset = *p.ResumeOffset
	}
	return spec
}

func (h *Hub) defaultSpec() warmRegionsSpec {
	return warmRegionsSpec{
		headBytes:    h.cfg.WarmHeadBytes,
		tailBytes:    h.cfg.WarmTailBytes,
		resumeWindow: h.cfg.WarmResumeWindowBytes,
	}
}

// startRun 启动一轮预热（区域集 + 3 分钟窗口 + 全量续取）；已有在途流/已停止/
// 没直链时是空操作。调用方无需持锁。
func (h *Hub) startRun(fileID string) {
	h.mu.Lock()
	st := h.states[fileID]
	if st == nil || st.stopped || st.run != nil || st.link.link.URL == "" {
		h.mu.Unlock()
		return
	}
	spec := st.spec
	if spec.headBytes <= 0 && spec.tailBytes <= 0 && spec.resumeOffset <= 0 {
		spec = h.defaultSpec()
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &hubWarmRun{
		h: h, ctx: ctx, cancel: cancel, fileID: fileID,
		spec: spec, acceptedAt: h.now(), started: time.Now(),
	}
	st.run = run
	n := h.activeRuns.Add(1)
	h.mu.Unlock()
	if n > hubFillWarnThreshold {
		h.log.Warn("hub 同时在跑的填充流偏多", "active_fills", n)
	}
	go run.execute()
}

// regionsComplete 报告区域集（头段 + 尾段 + 续播点窗口）在本地是否已齐。
func (h *Hub) regionsComplete(fileID string, spec warmRegionsSpec) bool {
	meta, ok := h.cache.Meta(fileID)
	if !ok || meta.size <= 0 || meta.identity() == "" {
		return false
	}
	last := (meta.size - 1) / blockSize
	if spec.headBytes > 0 {
		lastHead := (spec.headBytes - 1) / blockSize
		if lastHead > last {
			lastHead = last
		}
		if _, missing := h.cache.firstMissingBlock(fileID, meta.identity(), meta.size, 0, lastHead); missing {
			return false
		}
	}
	if spec.tailBytes > 0 {
		from := meta.size - spec.tailBytes
		if from < 0 {
			from = 0
		}
		if _, missing := h.cache.firstMissingBlock(fileID, meta.identity(), meta.size, from/blockSize, last); missing {
			return false
		}
	}
	if spec.resumeOffset > 0 && spec.resumeWindow > 0 {
		if start, end, ok := resumeRange(meta, spec); ok {
			if _, missing := h.cache.firstMissingBlock(fileID, meta.identity(), meta.size, start/blockSize, end/blockSize); missing {
				return false
			}
		}
	}
	return true
}

// resumeRange 把续播点窗口 clamp 进文件并**对齐到块边界**（整块才可落盘）。
func resumeRange(meta fileMeta, spec warmRegionsSpec) (start, end int64, ok bool) {
	start = spec.resumeOffset - spec.resumeWindow
	if start < 0 {
		start = 0
	}
	end = spec.resumeOffset + spec.resumeWindow - 1
	if end > meta.size-1 {
		end = meta.size - 1
	}
	if start > end {
		return 0, 0, false
	}
	start = (start / blockSize) * blockSize
	return start, end, true
}

// evictStatesLocked 状态表满时淘汰最老的"无在途流"条目；调用方须持有 h.mu。
func (h *Hub) evictStatesLocked() {
	for len(h.states) >= hubMaxStates {
		var oldestID string
		var oldestAt time.Time
		for id, st := range h.states {
			if st.run != nil {
				continue
			}
			if oldestID == "" || st.lastUse.Before(oldestAt) {
				oldestID, oldestAt = id, st.lastUse
			}
		}
		if oldestID == "" {
			return
		}
		delete(h.states, oldestID)
	}
}

// maybeContinue 在播放请求到来时（供流即播放）触发全量续取：有状态、未停止、
// 没在途流、有直链且有缺失块才启动。空操作没有任何副作用。
func (h *Hub) maybeContinue(fileID string) {
	h.mu.Lock()
	st := h.states[fileID]
	if st == nil || st.stopped || st.run != nil || st.link.link.URL == "" {
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	meta, ok := h.cache.Meta(fileID)
	if !ok || meta.size <= 0 || meta.identity() == "" {
		return
	}
	last := (meta.size - 1) / blockSize
	if _, missing := h.cache.firstMissingBlock(fileID, meta.identity(), meta.size, 0, last); !missing {
		return
	}
	h.startRun(fileID)
}

// --- 预热状态机 ----------------------------------------------------------------

// execute 是一轮预热的生命周期：区域集 → 3 分钟规则 →（有播放才）全量续取。
func (r *hubWarmRun) execute() {
	defer func() {
		r.cancel()
		r.h.mu.Lock()
		if st := r.h.states[r.fileID]; st != nil && st.run == r {
			st.run = nil
		}
		r.h.mu.Unlock()
		r.h.activeRuns.Add(-1)
	}()
	r.h.log.Info("hub 预热开始",
		"file_id", r.fileID, "head_bytes", r.spec.headBytes,
		"tail_bytes", r.spec.tailBytes, "resume_offset", r.spec.resumeOffset)

	regionCtx, regionCancel := context.WithTimeout(r.ctx, hubRegionTimeout)
	r.fetchRegions(regionCtx)
	regionCancel()

	// 3 分钟规则：warm 接受起计时；期间出现播放 → 转全量续取；到点无播放 → 停在区域集。
	if !r.playbackSeen() {
		if !r.waitPlayback() {
			r.h.log.Info("hub 预热：窗口内无播放，停在区域集",
				"file_id", r.fileID, "blocks", r.blocks, "bytes", r.bytes, "requests", r.requests)
			return
		}
	}
	r.h.log.Info("hub 预热：检测到播放，转全量续取", "file_id", r.fileID)
	r.fetchFull()
}

// fetchRegions 抓区域集：①头段单流切片（v0.3.2 语义）；②尾段；③续播点窗口。
// ②③各一条小请求（design §4）。
func (r *hubWarmRun) fetchRegions(ctx context.Context) {
	meta, _ := r.h.cache.Meta(r.fileID)
	// ① 头段：单流大流，从第一条缺失块起（断点续取）。
	if start, ok := r.firstMissingHead(meta); ok {
		r.fetchRegion(ctx, start*blockSize, r.headEnd(meta))
	}
	if ctx.Err() != nil {
		return
	}
	meta, ok := r.h.cache.Meta(r.fileID)
	if !ok || meta.size <= 0 {
		return // 头段没能带回总大小：②③无从推断（后续请求/播放会再来）
	}
	// ② 尾段。
	if start, ok := r.firstMissingTail(meta); ok {
		r.fetchRegion(ctx, start*blockSize, meta.size-1)
	}
	if ctx.Err() != nil {
		return
	}
	// ③ 续播点窗口（clamp + 块对齐；只在确有缺失时发）。
	if r.spec.resumeOffset > 0 {
		if start, end, ok := resumeRange(meta, r.spec); ok {
			if _, missing := r.h.cache.firstMissingBlock(r.fileID, meta.identity(), meta.size, start/blockSize, end/blockSize); missing {
				r.fetchRegion(ctx, start, end)
			}
		}
	}
}

// firstMissingHead 返回头段第一条缺失块；ok=false 表示头段齐全或未启用。
func (r *hubWarmRun) firstMissingHead(meta fileMeta) (int64, bool) {
	if r.spec.headBytes <= 0 {
		return 0, false
	}
	last := (r.spec.headBytes - 1) / blockSize
	if meta.size > 0 {
		if lb := (meta.size - 1) / blockSize; lb < last {
			last = lb
		}
	}
	if last < 0 {
		return 0, false
	}
	return r.h.cache.firstMissingBlock(r.fileID, meta.identity(), meta.size, 0, last)
}

// headEnd 是头窗口的最后一个字节偏移（含），按已知总大小收窄。
func (r *hubWarmRun) headEnd(meta fileMeta) int64 {
	end := r.spec.headBytes - 1
	if meta.size > 0 && end > meta.size-1 {
		end = meta.size - 1
	}
	return end
}

// firstMissingTail 返回尾段第一条缺失块；ok=false 表示尾段齐全/未启用/大小未知。
func (r *hubWarmRun) firstMissingTail(meta fileMeta) (int64, bool) {
	if r.spec.tailBytes <= 0 || meta.size <= 0 {
		return 0, false
	}
	from := meta.size - r.spec.tailBytes
	if from < 0 {
		from = 0
	}
	last := (meta.size - 1) / blockSize
	return r.h.cache.firstMissingBlock(r.fileID, meta.identity(), meta.size, from/blockSize, last)
}

// fetchRegion 打开一条 [start, end] 上游流（end < 0 = 开区间到文件尾），
// 边读边凑满即落块。返回 true 表示流完整到达 stopAt；false 表示应停
// （失败/取消/让路超时/短读），未完成部分交给后续请求续取。
func (r *hubWarmRun) fetchRegion(ctx context.Context, start, end int64) bool {
	if start < 0 {
		start = 0
	}
	if end >= 0 && end < start {
		return true
	}
	if !r.yieldToClients(ctx) {
		return false
	}

	// 无进展看门狗：这么久没读到新字节即掐掉本条流（兜坏 IP / 静默挂死）。
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	watchdog := time.AfterFunc(r.h.cfg.StallTimeout, streamCancel)
	defer watchdog.Stop()

	resp, err := r.requestRange(streamCtx, start, end)
	r.requests++
	if err != nil {
		r.warn("hub 预热请求失败", "error", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		r.warn("hub 预热：上游状态异常", "upstream_status", resp.StatusCode)
		return false
	}

	meta := responseMeta(resp)
	gotStart, gotEnd, ok := responseRange(resp)
	if !ok {
		r.warn("hub 预热：无法解析上游返回的区间")
		return false
	}
	if gotStart%blockSize != 0 {
		// 实际起点不在块边界上：按块号取整会把错位字节挂到相邻块上（字节一致性
		// 优先）。正常 200 整文件（0）与块对齐的 206 不会走到这里。
		r.warn("hub 预热：上游响应的起点不在块边界上，弃用本条流",
			"want_start", start, "got_start", gotStart)
		return false
	}
	if gotStart != start {
		// 上游忽略了 Range（拿 200 整文件回了一条分片请求）：按实际起点尽力填块。
		r.warn("hub 预热：上游未按 Range 返回，按实际起点尽力填块",
			"want_start", start, "got_start", gotStart)
	}
	stopAt := gotEnd
	if end >= 0 && stopAt > end {
		stopAt = end
	}
	if stopAt < gotStart {
		r.warn("hub 预热：上游响应区间与请求无交集",
			"want_start", start, "want_end", end, "got_start", gotStart, "got_end", gotEnd)
		return false
	}

	// 身份链 = ETag → Last-Modified → 总字节数（fileMeta.identity）。竞态防护与
	// 预取同一套：Observe 前先读，在途变化即弃流；流进行中再变化由填充器逐块兜底。
	prev, _ := r.h.cache.Meta(r.fileID)
	merged := r.h.cache.Observe(r.fileID, meta)
	identity := merged.identity()
	if identity == "" {
		r.warn("hub 预热：响应缺少任何内容身份（ETag/Last-Modified/总大小），放弃本条流")
		return false
	}
	if prevIdent := prev.identity(); prevIdent != "" && prevIdent != identity {
		r.warn("hub 预热：响应在途期间内容身份已变化，弃用本条流",
			"prev_identity", prevIdent, "identity", identity)
		return false
	}
	size := merged.size
	if size <= 0 {
		size = meta.size
	}

	filler := newHubFiller(r.h.cache, r.h.log, r.fileID, identity, size, gotStart)
	buf := make([]byte, copyBufferSize)
	pos := gotStart
	sinceCheck := int64(0)
	lastCheck := time.Now()
	for pos <= stopAt {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			pos += int64(n)
			_, _ = filler.Write(buf[:n])
			watchdog.Reset(r.h.cfg.StallTimeout)
			sinceCheck += int64(n)
		}
		if rerr != nil {
			if rerr == io.EOF {
				break // 正常读完或上游短读，由下方判定
			}
			r.warn("hub 预热：上游流读取失败", "error", rerr)
			return false
		}
		if sinceCheck >= hubYieldBytes || time.Since(lastCheck) >= hubYieldInterval {
			sinceCheck = 0
			// 让路等待期间停表：播放可能持续数十分钟，无进展看门狗在等待期内触发
			// 会把在途流误掐（恢复后立刻读失败重发请求 + 误导性告警）。
			watchdog.Stop()
			if !r.yieldToClients(ctx) {
				return false
			}
			watchdog.Reset(r.h.cfg.StallTimeout)
			lastCheck = time.Now()
		}
	}
	r.blocks += filler.blocks
	r.bytes += filler.bytes
	if pos <= stopAt {
		r.warn("hub 预热：上游流提前结束，未完成部分交后续请求续取",
			"got_bytes", pos-gotStart, "want_bytes", stopAt-gotStart+1)
		return false
	}
	return true
}

// fetchFull 全量续取：循环"从第一条缺失块起到文件尾"；每次流都让路、断点续取。
// 连续 hubMaxFullRounds 条流毫无进展即停手（交给下一次播放请求再触发）。
func (r *hubWarmRun) fetchFull() {
	rounds := 0
	for {
		if r.ctx.Err() != nil {
			return
		}
		meta, ok := r.h.cache.Meta(r.fileID)
		if !ok || meta.size <= 0 || meta.identity() == "" {
			r.warn("hub 预热：缺少元数据，无法全量续取")
			return
		}
		last := (meta.size - 1) / blockSize
		start, missing := r.h.cache.firstMissingBlock(r.fileID, meta.identity(), meta.size, 0, last)
		if !missing {
			r.h.log.Info("hub 全量缓存完成",
				"file_id", r.fileID, "blocks", r.blocks, "bytes", r.bytes,
				"requests", r.requests, "duration_ms", time.Since(r.started).Milliseconds())
			r.h.cache.EnforceBudget()
			return
		}
		if r.fetchRegion(r.ctx, start*blockSize, meta.size-1) {
			rounds = 0
			continue
		}
		rounds++
		if rounds >= hubMaxFullRounds {
			r.warn("hub 预热：全量续取连续无进展，停在已落块", "rounds", rounds)
			return
		}
	}
}

// requestRange 发一次上游 Range 请求；401/403 时走 master 的 download-link
// 通道换新链（file_token 为键）并重试一次。
func (r *hubWarmRun) requestRange(ctx context.Context, start, end int64) (*http.Response, error) {
	header := fmt.Sprintf("bytes=%d-", start)
	if end >= 0 {
		header = fmt.Sprintf("bytes=%d-%d", start, end)
	}
	current, ok := r.h.currentWarmLink(r.fileID)
	if !ok {
		return nil, fmt.Errorf("hub 预热：该文件的直链已被清除")
	}
	resp, err := r.h.doUpstream(ctx, http.MethodGet, current.link, header)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return resp, nil
	}
	_ = resp.Body.Close()
	r.warn("hub 预热：上游返回错误，重拉直链后重试一次", "upstream_status", resp.StatusCode)
	refreshed, rerr := r.h.refreshWarmLink(ctx, r.fileID)
	if rerr != nil {
		return nil, rerr
	}
	return r.h.doUpstream(ctx, http.MethodGet, refreshed, header)
}

// refreshWarmLink 走 master 的 download-link 通道换一条新直链（以 /warm 载荷
// 里缓存的 file_token 为键；没有 token 时退回用 fileID）。新链写回状态，
// 供后续请求与在途流共用。
func (h *Hub) refreshWarmLink(ctx context.Context, fileID string) (Link, error) {
	h.mu.Lock()
	key := fileID
	if st := h.states[fileID]; st != nil && st.link.token != "" {
		key = st.link.token
	}
	h.mu.Unlock()

	link, err := h.links.Refresh(ctx, key)
	if err != nil {
		return Link{}, err
	}
	h.mu.Lock()
	if st := h.states[fileID]; st != nil {
		st.link.link = link
	}
	h.mu.Unlock()
	return link, nil
}

// currentWarmLink 现读该文件的直链（在途流每次请求都取最新，避免多副本竞态）。
func (h *Hub) currentWarmLink(fileID string) (warmLink, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.states[fileID]
	if st == nil || st.link.link.URL == "" {
		return warmLink{}, false
	}
	return st.link, true
}

// playbackSeen 报告是否出现过"真实播放"（design §3：供流即播放判据）。
func (r *hubWarmRun) playbackSeen() bool {
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	if r.h.clients[r.fileID] > 0 {
		return true
	}
	st := r.h.states[r.fileID]
	return st != nil && st.playback
}

// waitPlayback 在 3 分钟窗口内等待播放；到点无播放或上下文取消返回 false。
func (r *hubWarmRun) waitPlayback() bool {
	deadline := r.acceptedAt.Add(r.h.cfg.WarmWait)
	for {
		if r.playbackSeen() {
			return true
		}
		if !r.h.now().Before(deadline) {
			return false
		}
		select {
		case <-r.ctx.Done():
			return false
		case <-time.After(r.h.cfg.WarmPollInterval):
		}
	}
}

// yieldToClients 实现让路：同文件有客户端在途时暂停读取（每 500ms 复查），
// 空闲后自动继续；上下文取消返回 false。
func (r *hubWarmRun) yieldToClients(ctx context.Context) bool {
	if !r.h.clientActive(r.fileID) {
		return true
	}
	r.h.log.Info("hub 预热让路：同文件有客户端在途，暂停读取", "file_id", r.fileID)
	for r.h.clientActive(r.fileID) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(prefetchYieldPoll):
		}
	}
	r.h.log.Info("hub 预热恢复：客户端已结束，继续读取", "file_id", r.fileID)
	return true
}

func (r *hubWarmRun) warn(msg string, args ...any) {
	r.h.log.Warn(msg, append([]any{"file_id", r.fileID}, args...)...)
}

// --- 客户端登记与状态查询 ------------------------------------------------------

// clientBegin 登记一个 /f/ GET 开始：既是让路判据，也是"真实播放"证据。
func (h *Hub) clientBegin(fileID string) {
	h.mu.Lock()
	h.clients[fileID]++
	if st := h.states[fileID]; st != nil {
		st.playback = true
		st.lastUse = h.now()
	}
	h.mu.Unlock()
}

// clientEnd 与 clientBegin 配对；计数归零时删表，避免长驻垃圾键。
func (h *Hub) clientEnd(fileID string) {
	h.mu.Lock()
	if h.clients[fileID] <= 1 {
		delete(h.clients, fileID)
	} else {
		h.clients[fileID]--
	}
	h.mu.Unlock()
}

// clientActive 报告该文件当前是否有客户端请求在途（让路判据）。
func (h *Hub) clientActive(fileID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clients[fileID] > 0
}

// stateLink 返回该文件当前持有的直链。
func (h *Hub) stateLink(fileID string) (Link, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.states[fileID]
	if st == nil || st.link.link.URL == "" {
		return Link{}, false
	}
	return st.link.link, true
}
