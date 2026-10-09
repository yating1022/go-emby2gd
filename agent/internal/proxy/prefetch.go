package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	// prefetchMaxFiles 是全局同时在跑的首触预取文件数上限。
	// 预取与真实播放共享出口带宽：宁可慢一点，也不要跟起播抢流。
	prefetchMaxFiles = 2

	// prefetchTimeout 是一轮首触预取（头 + 尾）的总时限；超时即放弃，
	// 后续真实请求自然会再次触发。
	prefetchTimeout = 5 * time.Minute
)

// PrefetcherConfig 组装 Prefetcher。
type PrefetcherConfig struct {
	Cache  *BlockCache
	Links  *LinkSource
	Client *http.Client // 与数据面共用的上游客户端；空则新建
	Logger *slog.Logger

	// HeadBytes 是首触预取的头部长度（默认 32MiB）；<= 0 表示不预取。
	HeadBytes int64
	// TailBytes 是首触预取的尾部长度（默认 4MiB）；<= 0 表示不预取尾部。
	TailBytes int64
}

// Prefetcher 实现"首触预取"：某文件第一次被请求时，后台异步把头部与尾部
// 拉进 BlockCache，让随后的起播探测序列变成本地命中。
//
// 它永不阻塞、永不报错：一切失败只记 WARN 后放弃，等待后续请求再次触发；
// 同一文件同时只有一轮预取在跑（单飞）。
type Prefetcher struct {
	cfg PrefetcherConfig

	mu       sync.Mutex
	inflight map[string]struct{}
	sem      chan struct{}
}

// NewPrefetcher 构造预取器。Cache 为空或未开启时它退化为空操作。
func NewPrefetcher(cfg PrefetcherConfig) *Prefetcher {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Client == nil {
		cfg.Client = NewUpstreamClient()
	}
	if cfg.HeadBytes < 0 {
		cfg.HeadBytes = 0
	}
	if cfg.TailBytes < 0 {
		cfg.TailBytes = 0
	}
	return &Prefetcher{
		cfg:      cfg,
		inflight: make(map[string]struct{}),
		sem:      make(chan struct{}, prefetchMaxFiles),
	}
}

// MaybeStart 在"该文件尚无任何缓存块、且没有在途预取"时异步启动首触预取。
//
// 不阻塞、不返回错误；不满足条件（缓存关闭、已有块、并发已满）时直接返回。
func (p *Prefetcher) MaybeStart(fileID string) {
	if p == nil || p.cfg.Cache == nil || !p.cfg.Cache.Enabled() || fileID == "" {
		return
	}
	if p.cfg.Cache.HasFile(fileID) {
		// 已经有块（同一文件只由首触预取写入）：不需要再来一轮。
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.inflight[fileID]; ok {
		return
	}
	select {
	case p.sem <- struct{}{}:
	default:
		// 预取并发已满：本次跳过（不排队、不堆 goroutine），后续请求会再触发。
		return
	}
	p.inflight[fileID] = struct{}{}
	go p.run(fileID)
}

// prefetchRun 是一轮预取的可变状态（只被单个 goroutine 持有）。
type prefetchRun struct {
	p        *Prefetcher
	ctx      context.Context
	fileID   string
	link     Link
	size     int64 // 文件总字节数；<= 0 表示未知
	ident    string
	tailDone bool
	blocks   int
	bytes    int64
}

func (p *Prefetcher) run(fileID string) {
	defer func() {
		<-p.sem
		p.mu.Lock()
		delete(p.inflight, fileID)
		p.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), prefetchTimeout)
	defer cancel()
	started := time.Now()

	link, err := p.cfg.Links.Link(ctx, fileID)
	if err != nil {
		p.cfg.Logger.Warn("首触预取放弃：拿不到直链", "file_id", fileID, "error", err)
		return
	}
	run := &prefetchRun{p: p, ctx: ctx, fileID: fileID, link: link}
	headBlocks := (p.cfg.HeadBytes + blockSize - 1) / blockSize
	p.cfg.Logger.Info("首触预取开始",
		"file_id", fileID, "head_blocks", headBlocks, "tail_bytes", p.cfg.TailBytes)

	for i := int64(0); i < headBlocks; i++ {
		if !run.fetchBlock(i) {
			p.cfg.Logger.Info("首触预取中断",
				"file_id", fileID, "blocks", run.blocks, "bytes", run.bytes)
			return
		}
		if i == 0 {
			// 第一个响应头已解析出文件大小：先补尾巴——起播探测序列里的尾探
			// 通常紧随头部小块之后，尾巴早点就位收益最大。
			if !run.fetchTail() {
				p.cfg.Logger.Info("首触预取中断",
					"file_id", fileID, "blocks", run.blocks, "bytes", run.bytes)
				return
			}
		}
	}
	if !run.tailDone && !run.fetchTail() {
		p.cfg.Logger.Info("首触预取中断",
			"file_id", fileID, "blocks", run.blocks, "bytes", run.bytes)
		return
	}
	p.cfg.Logger.Info("首触预取完成",
		"file_id", fileID,
		"blocks", run.blocks,
		"bytes", run.bytes,
		"duration_ms", time.Since(started).Milliseconds(),
	)
}

// fetchTail 预取文件末尾 [size-TailBytes, size) 覆盖的整块。
// size 未知、尾部开关关闭或已跑过时直接返回 true（不是错误）。
func (r *prefetchRun) fetchTail() bool {
	r.tailDone = true
	if r.p.cfg.TailBytes <= 0 || r.size <= 0 {
		return true
	}
	start := r.size - r.p.cfg.TailBytes
	if start < 0 {
		start = 0
	}
	start = start / blockSize * blockSize
	for idx := start / blockSize; idx*blockSize < r.size; idx++ {
		if !r.fetchBlock(idx) {
			return false
		}
	}
	return true
}

// fetchBlock 拉取并缓存整块 idx（[idx*blockSize, min((idx+1)*blockSize, size)-1]）。
// 返回 false 表示放弃本轮预取。
func (r *prefetchRun) fetchBlock(idx int64) bool {
	wantStart := idx * blockSize
	wantEnd := wantStart + blockSize - 1
	if r.size > 0 && wantEnd > r.size-1 {
		wantEnd = r.size - 1
	}
	if wantStart > wantEnd {
		return true // 文件比预取窗口还小：这一块不存在
	}
	if r.ident != "" {
		if data, ok := r.p.cfg.Cache.Get(r.fileID, r.ident, idx); ok &&
			int64(len(data)) == wantEnd-wantStart+1 {
			return true // 头/尾窗口重叠或上一轮预取已经写好了整块
		}
	}

	resp, err := r.request(wantStart, wantEnd)
	if err != nil {
		r.warn("预取请求失败", "error", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		r.warn("预取：上游状态异常", "upstream_status", resp.StatusCode)
		return false
	}

	meta := responseMeta(resp)
	if meta.size > 0 {
		r.size = meta.size
		if wantEnd > r.size-1 {
			wantEnd = r.size - 1
		}
	}
	gotStart, gotEnd, ok := responseRange(resp)
	if !ok || gotStart != wantStart {
		r.warn("预取：上游返回的区间与请求不符", "want_start", wantStart, "got_start", gotStart)
		return false
	}
	if gotEnd > wantEnd {
		// 上游忽略了 Range（拿 200 整文件回了一条分片请求）：只取要的这一块。
		gotEnd = wantEnd
	}
	body, err := readRangeBody(resp, gotEnd-gotStart+1)
	if err != nil {
		r.warn("预取：上游响应体不完整", "error", err)
		return false
	}
	if int64(len(body)) < blockSize && (r.size <= 0 || gotEnd != r.size-1) {
		// 不足一整块却不在文件末尾：存下去就会成为一个"半块"，后续请求
		// 从它身上取数据会缺字节 —— 宁可整轮放弃。
		r.warn("预取：上游响应体截断（未到文件末尾）",
			"block", idx, "got_bytes", len(body))
		return false
	}

	// 身份链 = ETag → Last-Modified → 总字节数（真实 Google 直链只给最后一项，
	// 见 fileMeta.identity）。合并进元数据表后再取身份：与随后的本地服务看到的
	// 是同一个键。三项全缺只可能是畸形上游——跳过本块即可，绝不写一个空身份
	// 的块进缓存（永远不可命中），也不再整轮放弃（那样生产上会零缓存空转）。
	//
	// 竞态防护：本响应在途期间（读 body 到 Observe 之间）同 id 文件可能恰被替换、
	// 且另一请求已把新身份观测进元数据表。此时直接合并会：ETag 形态下把旧字节写到
	// "新身份"键下；size 形态下把总大小回退成旧值、旧字节落到旧身份键下——与回退
	// 后的 meta 恰好匹配，陈旧字节就有资格被本地服务。所以 Observe 之前先读一次
	// 当前身份，发现「原身份非空且与合并后身份不同」就只弃掉这一块：不整轮放弃，
	// 已回读的字节流也不受影响；弃块是一次性的，后续请求触达上游时会自然重预取自愈。
	prev, _ := r.p.cfg.Cache.Meta(r.fileID)
	merged := r.p.cfg.Cache.Observe(r.fileID, meta)
	identity := merged.identity()
	if identity == "" {
		r.warn("预取：响应缺少任何内容身份（ETag/Last-Modified/总大小），跳过该块")
		return true
	}
	if prevIdent := prev.identity(); prevIdent != "" && prevIdent != identity {
		r.warn("预取：响应在途期间内容身份已变化，弃用该块",
			"block", idx, "prev_identity", prevIdent, "identity", identity)
		return true
	}
	r.ident = identity
	r.p.cfg.Cache.Put(r.fileID, identity, idx, body)
	r.blocks++
	r.bytes += int64(len(body))
	return true
}

// request 发一次上游 Range 请求；401/403 时重拉直链重试一次（与数据面同一语义），
// 成功后本轮后续块都用新直链接续，避免每块都撞一次 401。
func (r *prefetchRun) request(start, end int64) (*http.Response, error) {
	header := fmt.Sprintf("bytes=%d-%d", start, end)
	resp, err := r.do(header)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return resp, nil
	}
	_ = resp.Body.Close()
	r.warn("预取：上游返回错误，重拉直链后重试一次", "upstream_status", resp.StatusCode)
	refreshed, rerr := r.p.cfg.Links.Refresh(r.ctx, r.fileID)
	if rerr != nil {
		return nil, rerr
	}
	r.link = refreshed
	return r.do(header)
}

func (r *prefetchRun) do(rangeHeader string) (*http.Response, error) {
	req, err := newUpstreamRequest(r.ctx, http.MethodGet, r.link, rangeHeader)
	if err != nil {
		return nil, err
	}
	return r.p.cfg.Client.Do(req)
}

func (r *prefetchRun) warn(msg string, args ...any) {
	r.p.cfg.Logger.Warn(msg, append([]any{"file_id", r.fileID}, args...)...)
}

// readRangeBody 读满期望长度的响应体；多一个字节即判为异常（防止把整文件读进内存）。
func readRangeBody(resp *http.Response, want int64) ([]byte, error) {
	if want <= 0 {
		return nil, fmt.Errorf("期望长度不合法：%d", want)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, want+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != want {
		return nil, fmt.Errorf("上游返回 %d 字节，期望 %d（截断或超长）", len(body), want)
	}
	return body, nil
}
