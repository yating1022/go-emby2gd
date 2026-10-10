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

	// prefetchYieldBytes / prefetchYieldInterval 是"让路"检查的粒度：
	// 每读满 ~1MB 或每 ~250ms 检查一次同文件在途客户端（v0.3.2 design §2.2）。
	prefetchYieldBytes    = 1 << 20
	prefetchYieldInterval = 250 * time.Millisecond

	// prefetchYieldPoll 是让路等待的复查间隔：客户端仍在途就等 500ms 再查，
	// 空闲才继续读。不设硬上限——长播放期间预取静默等待属预期（design §4）。
	prefetchYieldPoll = 500 * time.Millisecond
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
// v0.3.2（2026-10-10 用户实测）起的三个变化：
//   - 头预取是**一条大流**（`Range: bytes=0-(head-1)`）边读边按块切片、凑满即
//     Put——块 0 不再等整段头预取（~10 次小块请求 → 2 条流量级）；
//   - 让路：同文件有客户端请求在途时读取循环暂停（数据面经 ClientBegin/ClientEnd
//     登记），空闲后自动继续；
//   - 断点续取：触发条件是"期望块集（头块 0..H-1 + 尾块）有缺失且无在途预取"，
//     只补缺失段——中断过的文件由后续请求自然接续。
//
// 它永不阻塞数据面、永不报错：一切失败只记日志后放弃，等待后续请求再次触发；
// 同一文件同时只有一轮预取在跑（单飞）。
type Prefetcher struct {
	cfg PrefetcherConfig

	mu       sync.Mutex
	inflight map[string]struct{}
	clients  map[string]int // 同文件在途客户端请求数（让路判据）
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
		clients:  make(map[string]int),
		sem:      make(chan struct{}, prefetchMaxFiles),
	}
}

// ClientBegin 登记一个同文件客户端请求开始（数据面只对 GET 调用）。
//
// 预取的流式读取循环据此"让路"：有客户端在途时暂停读上游，把出口带宽让给
// 播放请求；结束后自动继续。未接线（nil）或缓存关闭时是空操作。
func (p *Prefetcher) ClientBegin(fileID string) {
	if p == nil || p.cfg.Cache == nil || !p.cfg.Cache.Enabled() || fileID == "" {
		return
	}
	p.mu.Lock()
	p.clients[fileID]++
	p.mu.Unlock()
}

// ClientEnd 与 ClientBegin 配对注销；登记数为 0 时删表，避免长驻垃圾键。
func (p *Prefetcher) ClientEnd(fileID string) {
	if p == nil || p.cfg.Cache == nil || !p.cfg.Cache.Enabled() || fileID == "" {
		return
	}
	p.mu.Lock()
	if p.clients[fileID] <= 1 {
		delete(p.clients, fileID)
	} else {
		p.clients[fileID]--
	}
	p.mu.Unlock()
}

// clientActive 报告该文件当前是否有客户端请求在途（让路判据）。
func (p *Prefetcher) clientActive(fileID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clients[fileID] > 0
}

// MaybeStart 在"期望块集（头块 0..H-1 + 尾块）有缺失、且没有在途预取"时
// 异步启动一轮预取（v0.3.2 起：不再以"无任何块"为唯一判据，缺失块由本轮
// 或后续轮次补齐——中断过的文件因此能断点续取）。
//
// 不阻塞、不返回错误；不满足条件（缓存关闭、期块集齐全、并发已满）时直接返回。
func (p *Prefetcher) MaybeStart(fileID string) {
	p.maybeStart(fileID, nil)
}

// MaybeStartWithLink 与 MaybeStart 相同，但本轮预取直接使用给定的上游
// （v2 hub 直连的签名路由），**不向 master 换链**——v2 的契约是"稳态零回访"
// （N3）；本轮上游失效（连接失败 / 409 / 其它非 2xx）时只弃轮，数据面自己
// 会用 stale 提示换链自愈，预取不做任何额外回访。
func (p *Prefetcher) MaybeStartWithLink(fileID string, link Link) {
	p.maybeStart(fileID, &link)
}

// maybeStart 是两种入口的公共实现；upstream 非空表示本轮使用它作为上游。
func (p *Prefetcher) maybeStart(fileID string, upstream *Link) {
	if p == nil || p.cfg.Cache == nil || !p.cfg.Cache.Enabled() || fileID == "" {
		return
	}
	if !p.needsPrefetch(fileID) {
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
	go p.run(fileID, upstream)
}

// needsPrefetch 报告该文件的期望块集是否仍有缺失。
//
// 期望块集由 meta.size + 配置**确定性推出**（头块 0..H-1 + 尾块），不引入新的
// 持久状态（design §2.3）：文件比窗口小就只剩覆盖到的块；总大小未知时按头窗口
// 上限推断。过期块在 Get 里随手回收——回收即"缺失"，自然触发重取。
func (p *Prefetcher) needsPrefetch(fileID string) bool {
	if p.cfg.HeadBytes <= 0 && p.cfg.TailBytes <= 0 {
		return false
	}
	meta, _ := p.cfg.Cache.Meta(fileID)
	for _, idx := range expectedBlocks(p.cfg.HeadBytes, p.cfg.TailBytes, meta.size) {
		if !cacheBlockPresent(p.cfg.Cache, fileID, meta.identity(), meta.size, idx) {
			return true
		}
	}
	return false
}

// expectedBlocks 推出期望块集（升序）：头窗口 [0, min(headBytes,size)) 覆盖的块
// + 尾窗口 [size-tailBytes, size) 覆盖的块；size <= 0（未知）时只有头块。
func expectedBlocks(headBytes, tailBytes, size int64) []int64 {
	var out []int64
	headEnd := headBytes
	if size > 0 && headEnd > size {
		headEnd = size
	}
	if headEnd > 0 {
		n := (headEnd + blockSize - 1) / blockSize
		for idx := int64(0); idx < n; idx++ {
			out = append(out, idx)
		}
	}
	if tailBytes > 0 && size > 0 {
		start := size - tailBytes
		if start < 0 {
			start = 0
		}
		for idx := start / blockSize; idx <= (size-1)/blockSize; idx++ {
			if len(out) > 0 && idx <= out[len(out)-1] {
				continue // 小文件上头尾窗口重叠：去重
			}
			out = append(out, idx)
		}
	}
	return out
}

// cacheBlockPresent 报告块 idx 是否以**完整长度**在缓存里。
//
// 长度判据与写入方（首触预取）一致：整块 = blockSize；不足一整块只可能是文件
// 最后一块（长度 = size - idx*blockSize）。身份为空时永远不命中（那些块进不了
// 缓存），按缺失处理。
func cacheBlockPresent(c *BlockCache, fileID, identity string, size, idx int64) bool {
	if identity == "" {
		return false
	}
	want := int64(blockSize)
	if size > 0 && (idx+1)*blockSize > size {
		want = size - idx*blockSize
	}
	if want <= 0 {
		return true // 该块在文件里不存在
	}
	data, ok := c.Get(fileID, identity, idx)
	return ok && int64(len(data)) == want
}

// prefetchRun 是一轮预取的可变状态（只被单个 goroutine 持有）。
type prefetchRun struct {
	p      *Prefetcher
	ctx    context.Context
	fileID string
	link   Link
	// hubDirect 表示本轮上游来自 v2 签名 URL（hub 直连）：上游非 2xx 时不做
	// master 换链（N3），直接交回调用方按"状态异常"弃轮。
	hubDirect bool
	size      int64 // 文件总字节数；<= 0 表示未知
	ident     string
	started   time.Time
	blocks    int   // 本轮已入缓存的块数
	bytes     int64 // 本轮已入缓存的字节数
	requests  int   // 本轮已打开的上游流数
	firstPut  bool  // 是否已记过"首块就绪"
}

// run 跑一轮预取。upstream 非空表示本轮使用该上游（v2 hub 直连），不向 master 换链。
func (p *Prefetcher) run(fileID string, upstream *Link) {
	defer func() {
		<-p.sem
		p.mu.Lock()
		delete(p.inflight, fileID)
		p.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), prefetchTimeout)
	defer cancel()

	var link Link
	if upstream != nil {
		link = *upstream
	} else {
		got, err := p.cfg.Links.Link(ctx, fileID)
		if err != nil {
			p.cfg.Logger.Warn("首触预取放弃：拿不到直链", "file_id", fileID, "error", err)
			return
		}
		link = got
	}
	run := &prefetchRun{p: p, ctx: ctx, fileID: fileID, link: link, hubDirect: upstream != nil, started: time.Now()}
	// 续取判据的种子：身份与总大小取自当前元数据（可能已被首个请求观测进缓存）。
	// 没有它，缺失扫描会因身份为空把"缓存里已有块"误判成缺失，把续取退化成
	// 从块 0 重拉（design §2.3：只抓缺失）。
	if meta, ok := p.cfg.Cache.Meta(fileID); ok {
		run.size, run.ident = meta.size, meta.identity()
	}
	headBlocks := (p.cfg.HeadBytes + blockSize - 1) / blockSize
	p.cfg.Logger.Info("首触预取开始",
		"file_id", fileID, "head_blocks", headBlocks, "tail_bytes", p.cfg.TailBytes)

	// 头段：从第一条缺失块起开一条大流（首触时身份/大小未知 → 从块 0 起）。
	if start, ok := run.firstMissingHead(); ok {
		if !run.fetchStream(start*blockSize, run.headEnd()) {
			run.logInterrupted()
			return
		}
	}
	// 尾段：头段完成后再补（缺失的尾块单独一条流，design §2.3）。
	if start, ok := run.firstMissingTail(); ok {
		if !run.fetchStream(start*blockSize, run.size-1) {
			run.logInterrupted()
			return
		}
	}

	if missing := run.missingCount(); missing > 0 {
		p.cfg.Logger.Info("预取未完成：仍有缺失块，等待后续请求续取",
			"file_id", fileID, "missing", missing,
			"blocks", run.blocks, "bytes", run.bytes, "requests", run.requests,
			"duration_ms", time.Since(run.started).Milliseconds())
		return
	}
	p.cfg.Logger.Info("首触预取完成",
		"file_id", fileID,
		"blocks", run.blocks,
		"bytes", run.bytes,
		"requests", run.requests,
		"duration_ms", time.Since(run.started).Milliseconds(),
	)
}

// logInterrupted 记一条"本轮中断"日志（失败 / 弃流路径；未完成部分交给后续
// 请求续取，见 MaybeStart 的续取判据）。
func (r *prefetchRun) logInterrupted() {
	r.p.cfg.Logger.Info("首触预取中断",
		"file_id", r.fileID, "blocks", r.blocks, "bytes", r.bytes,
		"requests", r.requests, "duration_ms", time.Since(r.started).Milliseconds())
}

// headEnd 是头窗口的最后一个字节偏移（含）。
func (r *prefetchRun) headEnd() int64 {
	end := r.p.cfg.HeadBytes
	if r.size > 0 && end > r.size {
		end = r.size
	}
	return end - 1
}

// headBlockCount 是头窗口覆盖的块数（0..H-1）。
func (r *prefetchRun) headBlockCount() int64 {
	end := r.p.cfg.HeadBytes
	if end <= 0 {
		return 0
	}
	if r.size > 0 && end > r.size {
		end = r.size
	}
	return (end + blockSize - 1) / blockSize
}

// firstMissingHead 返回头段第一条缺失块的下标；ok=false 表示头段齐全或未启用。
func (r *prefetchRun) firstMissingHead() (int64, bool) {
	n := r.headBlockCount()
	for idx := int64(0); idx < n; idx++ {
		if !cacheBlockPresent(r.p.cfg.Cache, r.fileID, r.ident, r.size, idx) {
			return idx, true
		}
	}
	return 0, false
}

// firstMissingTail 返回尾段第一条缺失块的下标；ok=false 表示尾段齐全、未启用
// 或总大小未知（尾窗口无从推断）。
func (r *prefetchRun) firstMissingTail() (int64, bool) {
	if r.p.cfg.TailBytes <= 0 || r.size <= 0 {
		return 0, false
	}
	start := r.size - r.p.cfg.TailBytes
	if start < 0 {
		start = 0
	}
	for idx := start / blockSize; idx <= (r.size-1)/blockSize; idx++ {
		if !cacheBlockPresent(r.p.cfg.Cache, r.fileID, r.ident, r.size, idx) {
			return idx, true
		}
	}
	return 0, false
}

// missingCount 统计期望块集里仍缺失的块数（收尾日志用）。
func (r *prefetchRun) missingCount() int {
	missing := 0
	for _, idx := range expectedBlocks(r.p.cfg.HeadBytes, r.p.cfg.TailBytes, r.size) {
		if !cacheBlockPresent(r.p.cfg.Cache, r.fileID, r.ident, r.size, idx) {
			missing++
		}
	}
	return missing
}

// blockLen 是块 idx 的期望完整长度：整块 = blockSize；文件最后一块按剩余字节数
// （不足一整块的块只可能是文件最后一块）。
func (r *prefetchRun) blockLen(idx int64) int64 {
	if r.size > 0 && (idx+1)*blockSize > r.size {
		if n := r.size - idx*blockSize; n > 0 {
			return n
		}
	}
	return blockSize
}

// fetchStream 打开一条 [start, end] 的上游大流（沿用既有 request()：206/起点
// 校验与 401/403→Refresh 重试语义不变），边读边按块切片、**凑满一块立即 Put**
// ——块 0 在收到第一个 blockSize 时就绪。
//
// 返回 false 表示本轮应停下（失败、弃流或上下文取消）；已收字节里凑满的块
// 保留，不完整的块绝不写入，未完成部分交给后续请求续取（design §2.1/§2.3）。
func (r *prefetchRun) fetchStream(start, end int64) bool {
	if end < start {
		return true
	}
	// 让路：开流前先确认没有同文件客户端在途——起播探测优先用带宽。
	if !r.yieldToClients() {
		return false
	}

	resp, err := r.request(start, end)
	r.requests++
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
	}
	gotStart, gotEnd, ok := responseRange(resp)
	if !ok {
		r.warn("预取：无法解析上游返回的区间")
		return false
	}
	if gotStart%blockSize != 0 {
		// 响应的实际起点不在块边界上：按块号取整会把错位字节挂到相邻块上
		// （本地服务的字节一致性优先）。正常的 200 整文件（起点 0）与 206
		// 精确区间（起点 = 块边界）都不会走到这里；弃掉本条流，后续请求再试。
		r.warn("预取：上游响应的起点不在块边界上，弃用本条流",
			"want_start", start, "got_start", gotStart)
		return false
	}
	if gotStart != start {
		// 上游忽略了 Range（拿 200 整文件回了一条分片请求）或回报了别的区间：
		// 按响应的**实际起点**尽力填块（块号 = 文件偏移 / blockSize），不完整
		// 块照旧不写。这是"尽力而为"路径，不放弃整轮（design §2.1）。
		r.warn("预取：上游未按 Range 返回，按实际起点尽力填块",
			"want_start", start, "got_start", gotStart)
	}
	stopAt := gotEnd
	if stopAt > end {
		stopAt = end
	}
	if stopAt < gotStart {
		r.warn("预取：上游响应区间与请求无交集",
			"want_start", start, "want_end", end, "got_start", gotStart, "got_end", gotEnd)
		return false
	}

	// 身份链 = ETag → Last-Modified → 总字节数（真实 Google 直链只给最后一项，
	// 见 fileMeta.identity）。流首观察一次并取合并后的身份；三项全缺只可能是
	// 畸形上游——弃掉本条流（这条流的每个块都会缺身份，读下去只是白费带宽），
	// 后续请求会再试。
	//
	// 竞态防护（v0.3.1 语义推广到流）：本响应在途期间同 id 文件可能恰被替换、
	// 且另一请求已把新身份观测进元数据表。此时直接 Put 会把旧版本的字节写成
	// "新身份键下的陈旧数据"（ETag 形态）或把总大小回退成旧值（size 形态）。
	// 所以 Observe 之前先读一次当前身份，发现「原身份非空且与合并后身份不同」
	// 就弃掉整条流；流**进行中**再变化由 putBlock 的逐块新鲜度检查兜底。
	prev, _ := r.p.cfg.Cache.Meta(r.fileID)
	merged := r.p.cfg.Cache.Observe(r.fileID, meta)
	identity := merged.identity()
	if identity == "" {
		r.warn("预取：响应缺少任何内容身份（ETag/Last-Modified/总大小），放弃本条流")
		return false
	}
	if prevIdent := prev.identity(); prevIdent != "" && prevIdent != identity {
		r.warn("预取：响应在途期间内容身份已变化，弃用本条流",
			"prev_identity", prevIdent, "identity", identity)
		return false
	}
	r.ident = identity

	// 流式读取：边读边切片，凑满一块**立即** Put。
	buf := make([]byte, copyBufferSize)
	pos := gotStart
	curIdx := pos / blockSize
	cur := make([]byte, 0, blockSize)
	lastCheck := time.Now()
	var sinceCheck int64
	for pos <= stopAt {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			pos += int64(n)
			if !r.feed(&cur, &curIdx, buf[:n]) {
				return false
			}
			sinceCheck += int64(n)
		}
		if rerr != nil {
			if rerr == io.EOF {
				break // 正常读完或上游短读，由下方判定
			}
			r.warn("预取：上游流读取失败", "error", rerr)
			return false
		}
		if sinceCheck >= prefetchYieldBytes || time.Since(lastCheck) >= prefetchYieldInterval {
			lastCheck = time.Now()
			sinceCheck = 0
			if !r.yieldToClients() {
				return false
			}
		}
	}
	if pos <= stopAt {
		// 流提前结束（短读/EOF/被掐断）：不完整的块绝不写入缓存，
		// 已齐的块保留，剩余部分交给后续请求续取。
		r.warn("预取：上游流提前结束，未完成部分交后续请求续取",
			"got_bytes", pos-gotStart, "want_bytes", stopAt-gotStart+1)
		return false
	}
	return true
}

// feed 把刚读到的数据续接进当前块缓冲，凑满一块立即 Put。
// 返回 false 表示身份在流进行中变了，调用方应停下整条流。
func (r *prefetchRun) feed(cur *[]byte, curIdx *int64, data []byte) bool {
	for len(data) > 0 {
		need := r.blockLen(*curIdx) - int64(len(*cur))
		if need <= 0 {
			// 退化情形（块长判据在流内变化）：先把已满的缓冲落掉再续，
			// 避免原地打转；错长的块会在读取侧按"不完整"被拒，自愈。
			if !r.putBlock(*curIdx, *cur) {
				return false
			}
			*cur = make([]byte, 0, blockSize)
			*curIdx++
			continue
		}
		take := int64(len(data))
		if take > need {
			take = need
		}
		*cur = append(*cur, data[:take]...)
		data = data[take:]
		if int64(len(*cur)) == r.blockLen(*curIdx) {
			if !r.putBlock(*curIdx, *cur) {
				return false
			}
			*cur = make([]byte, 0, blockSize)
			*curIdx++
		}
	}
	return true
}

// putBlock 写入一个凑满的块。流可能很长，期间同 id 文件若被替换、其它请求已
// 观测到新身份，继续写就是把旧字节挂到"新身份"键下——弃用本条流（一次性；
// 已写块/已发字节不受影响，后续请求自然重预取自愈）。
func (r *prefetchRun) putBlock(idx int64, data []byte) bool {
	if cur, ok := r.p.cfg.Cache.Meta(r.fileID); ok {
		if id := cur.identity(); id != "" && id != r.ident {
			r.warn("预取：流进行中内容身份已变化，弃用本条流",
				"block", idx, "prev_identity", r.ident, "identity", id)
			return false
		}
	}
	// Put 是"先到为准"：已存在的键（包括尚未被回收的过期条目）会让新块被静默跳过。
	// 旧实现的逐块路径每次都先 Get（顺带回收过期块）再请求/Put；流式路径补上这步：
	// Get 命中且长度一致 = 块已新鲜就绪，直接算完成；未命中时 Get 已顺手回收过期
	// 条目，随后的 Put 才真正落块（CACHE_MAX_AGE 过期后重预取的场景，见 Get 注释）。
	if got, ok := r.p.cfg.Cache.Get(r.fileID, r.ident, idx); ok && int64(len(got)) == int64(len(data)) {
		return true
	}
	r.p.cfg.Cache.Put(r.fileID, r.ident, idx, data)
	r.blocks++
	r.bytes += int64(len(data))
	if !r.firstPut {
		r.firstPut = true
		r.p.cfg.Logger.Info("预取: 首块就绪",
			"file_id", r.fileID, "block", idx, "bytes", len(data),
			"duration_ms", time.Since(r.started).Milliseconds())
	}
	return true
}

// yieldToClients 实现"让路"（design §2.2）：同文件有客户端请求在途时暂停读取，
// 每 500ms 复查，空闲后自动继续。返回 false 表示等待期间上下文被取消。
func (r *prefetchRun) yieldToClients() bool {
	if !r.p.clientActive(r.fileID) {
		return true
	}
	r.p.cfg.Logger.Info("预取让路：同文件有客户端在途，暂停读取", "file_id", r.fileID)
	for r.p.clientActive(r.fileID) {
		select {
		case <-r.ctx.Done():
			return false
		case <-time.After(prefetchYieldPoll):
		}
	}
	r.p.cfg.Logger.Info("预取恢复：客户端已结束，继续读取", "file_id", r.fileID)
	return true
}

// request 发一次上游 Range 请求；401/403 时重拉直链重试一次（与数据面同一语义），
// 成功后本轮后续流都用新直链接续，避免每次都撞一次 401。
//
// hubDirect（v2 hub 直连）时不做 master 换链：非 2xx（含 409 not_warmed）原样交回，
// 由 fetchStream 按"上游状态异常"弃轮——数据面自己会用 stale 提示换链自愈（N3/N4）。
func (r *prefetchRun) request(start, end int64) (*http.Response, error) {
	header := fmt.Sprintf("bytes=%d-%d", start, end)
	resp, err := r.do(header)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return resp, nil
	}
	if r.hubDirect {
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
