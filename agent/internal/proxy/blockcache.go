package proxy

import (
	"container/list"
	"strconv"
	"sync"
	"time"
)

// blockSize 是读前缓存的块大小：4MiB。
//
// 块键只记块号，文件偏移 = 块号 × blockSize，因此块内容可以直接按偏移拼接。
// 不足一整块的块只可能是**文件的最后一块**：唯一写缓存的路径是首触预取
// （prefetch.go），它只在响应体恰好到达文件末尾时才落最后一块。
const blockSize = 4 << 20

// maxMetaEntries 是元数据表（fileID → 身份/大小/Content-Type）的容量上限。
// 单条元数据很小，但它同样必须有界：随便删一条只会让那个文件退回"走上游"。
const maxMetaEntries = 4096

// blockKey 是块缓存键：文件 + 内容身份 + 块号。
//
// 内容身份按 ETag → Last-Modified → 总字节数取值（见 fileMeta.identity）。
// 真实 Google 直链响应没有 ETag/Last-Modified（2026-10-09 实测），身份实际由
// **文件总字节数**充当：身份一变，该文件的旧块立即失效（见 Observe）。不与上游
// 内容绑定就无法保证"缓存里的字节 == 上游现在会返回的字节"，而字节一致性是本
// 功能的第一验收项。
type blockKey struct {
	fileID   string
	identity string
	idx      int64
}

// fileMeta 是本地拼装响应头所需的文件元数据（design §4）。
//
// 零值表示"什么都不知道"：size <= 0 视为长度未知。字段合并遵循本项目既有约定
// "非空才覆盖"（见 agent-network.md §3.2）：一次降级的响应（缺 ETag/Content-Type）
// 不得抹掉已知信息。
type fileMeta struct {
	etag         string // 上游 ETag（可能为空：真实 Google 直链就没有）
	lastModified string // 上游 Last-Modified（可能为空；ETag 缺失时充当内容身份）
	size         int64  // 文件总字节数；<= 0 表示未知
	contentType  string
	// acceptRanges 是上游 Accept-Ranges 的原值：本地拼响应头时**照抄观测到的值**，
	// 不凭空合成。上游没给就不给，否则"缓存开/关"在同一请求上的响应头会不一致
	// （字节一致性验收包含白名单响应头）。
	acceptRanges string
}

// identity 是块键使用的"内容身份"：ETag → Last-Modified → 总字节数（size: 前缀）。
//
// 第三级是本轮修订（2026-10-09 真实 Google 直链实测）：响应**没有 ETag、没有
// Last-Modified**，只有 206 的 `Content-Range: bytes a-b/<total>`（或 200 的
// Content-Length）携带文件总字节数。若身份链止步于 Last-Modified，生产环境会
// 一个块都存不下来（零缓存空转）。
//
// 退化路径的已知局限（如实记录，勿当成保证）：总字节数不是内容指纹，只能识破
// "换了大小不同的文件"；**同大小替换骗得过这层身份**——这道残余风险由
// CACHE_MAX_AGE_MINUTES 的年龄兜底（块过期即不服务）。三者全缺才返回空，
// 此时调用方不应缓存（块键为空永远不可命中）。
func (m fileMeta) identity() string {
	if m.etag != "" {
		return m.etag
	}
	if m.lastModified != "" {
		return m.lastModified
	}
	if m.size > 0 {
		return "size:" + strconv.FormatInt(m.size, 10)
	}
	return ""
}

// blockEntry 是 LRU 里的一个块（只增不改：写入后 data 不再变化）。
type blockEntry struct {
	key       blockKey
	data      []byte
	fetchedAt time.Time // 写入时刻：CACHE_MAX_AGE_MINUTES 的年龄检查用它
}

// BlockCache 是纯内存、预算有界的块级 LRU。
//
// 所有方法都可由多个 goroutine 并发调用；Get/Prefix/FullHit 返回的切片由缓存持有，
// **只读**且不得修改（Put 会复制传入的数据，调用方可以安全复用缓冲）。
//
// budget 在构造后不再变化，因此 Enabled 无需加锁。
type BlockCache struct {
	budget int64
	// maxAge 是块的最大可服务年龄（<= 0 = 不做年龄检查）；now 是时钟，生产用
	// time.Now，测试注入（见 setAgeForTest）。两者都只在持锁时读写。
	maxAge time.Duration
	now    func() time.Time

	mu    sync.Mutex
	lru   *list.List // 元素 *blockEntry，队首为最近使用
	index map[blockKey]*list.Element
	metas map[string]fileMeta
	used  int64
}

// NewBlockCache 构造块缓存（不做块年龄检查）；budgetBytes <= 0 表示功能关闭
// （Enabled 恒为 false，此时所有操作都是空操作，数据面行为与不带缓存的版本
// 逐字节一致）。
func NewBlockCache(budgetBytes int64) *BlockCache {
	return NewBlockCacheWithMaxAge(budgetBytes, 0)
}

// NewBlockCacheWithMaxAge 同 NewBlockCache，另设块的最大可服务年龄
// （maxAge <= 0 = 不做年龄检查）。
//
// 年龄是 LRU 之外的第二道兜底：身份链退化到总字节数时，"同大小替换"骗得过
// 内容身份，但骗不过时间——超过 maxAge 的块一律不服务（见 Get/walk/HasFile）。
func NewBlockCacheWithMaxAge(budgetBytes int64, maxAge time.Duration) *BlockCache {
	if budgetBytes < 0 {
		budgetBytes = 0
	}
	return &BlockCache{
		budget: budgetBytes,
		maxAge: maxAge,
		now:    time.Now,
		lru:    list.New(),
		index:  make(map[blockKey]*list.Element),
		metas:  make(map[string]fileMeta),
	}
}

// setAgeForTest 注入最大可服务年龄与时钟（仅测试使用，供 TTL 用例推进时间）。
// 所有读写都在 c.mu 下，与在途操作并发调用也是安全的。
func (c *BlockCache) setAgeForTest(maxAge time.Duration, now func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxAge, c.now = maxAge, now
}

// Enabled 报告缓存是否开启（预算 > 0）。
func (c *BlockCache) Enabled() bool { return c != nil && c.budget > 0 }

// expiredLocked 报告该块的块龄是否超过 maxAge；调用方须持有 c.mu。
func (c *BlockCache) expiredLocked(el *list.Element) bool {
	if c.maxAge <= 0 {
		return false
	}
	return c.now().Sub(el.Value.(*blockEntry).fetchedAt) > c.maxAge
}

// Get 取一个块，命中会提升其 LRU 位置。返回的切片只读。
//
// **块龄超过 maxAge 一律按 miss 处理**，并顺手回收该块（过期块永远不会再被
// 服务，回收后该文件就能重新预取——Put 对同键是先到为准，留着旧块会挡住新块）。
func (c *BlockCache) Get(fileID, identity string, idx int64) ([]byte, bool) {
	if !c.Enabled() {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[blockKey{fileID: fileID, identity: identity, idx: idx}]
	if !ok {
		return nil, false
	}
	if c.expiredLocked(el) {
		c.removeLocked(el)
		return nil, false
	}
	c.lru.MoveToFront(el)
	return el.Value.(*blockEntry).data, true
}

// Put 写入一个块（数据会复制一份，调用方的缓冲可安全复用）。
//
// 唯一调用方是首触预取；键已存在则跳过——先到为准（头预取与尾预取在小文件上
// 会覆盖同一块，重复写只会白费一次拷贝）。identity 为空说明调用方没拿到任何
// 内容身份（理论上不可达，见 fileMeta.identity）：这种块永远不可命中，不写。
func (c *BlockCache) Put(fileID, identity string, idx int64, data []byte) {
	if !c.Enabled() || fileID == "" || identity == "" || len(data) == 0 {
		return
	}
	size := int64(len(data))
	if size > c.budget {
		// 单块就超预算：存进去会立刻把自己（或别人）全淘汰掉，直接不存。
		return
	}
	copied := make([]byte, size)
	copy(copied, data)
	key := blockKey{fileID: fileID, identity: identity, idx: idx}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.index[key]; ok {
		return
	}
	c.index[key] = c.lru.PushFront(&blockEntry{key: key, data: copied, fetchedAt: c.now()})
	c.used += size
	for c.used > c.budget {
		back := c.lru.Back()
		if back == nil {
			break
		}
		c.removeLocked(back)
	}
}

// Prefix 返回从 start 起**连续命中**的块数据（最多 limit 字节），供混合服务
// 写出本地前缀。返回的 data 长度恒等于 n；n == 0 表示 start 处没有命中。
// 块龄超过 maxAge 的块视作缺块（同 Get）。
func (c *BlockCache) Prefix(fileID, identity string, start, limit int64) ([]byte, int64) {
	parts, n := c.walk(fileID, identity, start, limit)
	switch len(parts) {
	case 0:
		return nil, 0
	case 1:
		return parts[0], n
	}
	out := make([]byte, 0, n)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out, n
}

// FullHit 报告 [start, end] 是否完整落在缓存里有**效**（未过期）的连续块里；
// 命中时逐块返回数据（不拼接，供大 Range 的纯本地服务流式写出，避免一次性复制整段）。
//
// 返回的切片只读且由缓存持有。
func (c *BlockCache) FullHit(fileID, identity string, start, end int64) ([][]byte, bool) {
	if end < start {
		return nil, false
	}
	parts, n := c.walk(fileID, identity, start, end-start+1)
	if n != end-start+1 {
		return nil, false
	}
	return parts, true
}

// walk 从 start 起逐块收集连续命中的有效块，直到缺块、块过期、块内偏移越界或
// 收集够 limit 字节。命中的块会提升 LRU 位置；返回的切片只读。
func (c *BlockCache) walk(fileID, identity string, start, limit int64) ([][]byte, int64) {
	if !c.Enabled() || limit <= 0 || start < 0 {
		return nil, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	idx := start / blockSize
	off := start % blockSize
	var parts [][]byte
	var total int64
	for total < limit {
		el, ok := c.index[blockKey{fileID: fileID, identity: identity, idx: idx}]
		if !ok {
			break
		}
		if c.expiredLocked(el) {
			// 过期块视作缺块：连续前缀到此断开；顺手回收（见 Get 的注释）。
			c.removeLocked(el)
			break
		}
		c.lru.MoveToFront(el)
		data := el.Value.(*blockEntry).data
		if off >= int64(len(data)) {
			// 块内偏移越界（短块）：这里接不上，停。
			break
		}
		part := data[off:]
		if room := limit - total; int64(len(part)) > room {
			part = part[:room]
		}
		parts = append(parts, part)
		total += int64(len(part))
		if int64(len(data)) < blockSize {
			// 不足一整块的块 = 文件最后一块：后面没有可续接的数据。
			break
		}
		idx++
		off = 0
	}
	return parts, total
}

// SetEtag 记录该文件最近一次上游响应的 ETag；与已记录身份不同则清除该文件全部旧块。
//
// 这是"身份变化即整文件失效"的便捷入口。空 ETag 是空操作：空值代表"这次响应没有
// 告诉我们内容身份"，拿它做失配判断会把好块全清掉（下一个真身份又要重建），
// 而降级响应不该有这种破坏力（同 §3.2 的非空才覆盖语义）。
func (c *BlockCache) SetEtag(fileID, etag string) {
	c.Observe(fileID, fileMeta{etag: etag})
}

// Observe 合并一次上游响应的元数据，返回合并后的结果。
//
// 语义：
//   - 只覆盖非空/已知字段（"非空才覆盖"），降级响应不会抹掉已知信息；
//   - 内容身份（ETag → Last-Modified → 总字节数）一旦变化，立即清除该文件的
//     全部旧块——身份变了，旧块与新块拼在一起就是错字节；
//   - 身份链退化到总字节数时，这是"同 id 换文件、大小不同"的识破点；同大小替换
//     由 CACHE_MAX_AGE_MINUTES 兜底（见 fileMeta.identity 的局限说明）。
//
// 传零值 fileMeta 是合法的无操作。
func (c *BlockCache) Observe(fileID string, m fileMeta) fileMeta {
	if !c.Enabled() || fileID == "" {
		return m
	}
	if m == (fileMeta{}) {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.metas[fileID]
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.metas[fileID]
	prev := cur.identity()
	if m.etag != "" {
		cur.etag = m.etag
	}
	if m.lastModified != "" {
		cur.lastModified = m.lastModified
	}
	if m.size > 0 {
		cur.size = m.size
	}
	if m.contentType != "" {
		cur.contentType = m.contentType
	}
	if m.acceptRanges != "" {
		cur.acceptRanges = m.acceptRanges
	}
	if identity := cur.identity(); identity != "" && identity != prev {
		for key, el := range c.index {
			if key.fileID == fileID && key.identity != identity {
				c.removeLocked(el)
			}
		}
	}
	if _, ok := c.metas[fileID]; !ok && len(c.metas) >= maxMetaEntries {
		// 表满：随机让出一条（元数据丢失只影响该文件能否走本地服务）。
		for key := range c.metas {
			delete(c.metas, key)
			break
		}
	}
	c.metas[fileID] = cur
	return cur
}

// Meta 返回该文件当前生效的元数据；从未观测到过上游响应时 ok 为 false。
func (c *BlockCache) Meta(fileID string) (fileMeta, bool) {
	if !c.Enabled() {
		return fileMeta{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.metas[fileID]
	return m, ok
}

// HasFile 报告该文件是否还有**有效**（未过期）的缓存块（首触预取的触发判据）。
//
// 顺手回收过期块：过期块既不可服务，也不该让"已有块"成立——否则该文件永远不再
// 预取（Put 对同键是先到为准，旧块会挡住新块），缓存就成了纯负担。
func (c *BlockCache) HasFile(fileID string) bool {
	if !c.Enabled() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	found := false
	for key, el := range c.index {
		if key.fileID != fileID {
			continue
		}
		if c.expiredLocked(el) {
			c.removeLocked(el)
			continue
		}
		found = true
	}
	return found
}

// removeLocked 摘掉一个块并回退预算占用；调用方须持有 c.mu。
func (c *BlockCache) removeLocked(el *list.Element) {
	entry := el.Value.(*blockEntry)
	c.lru.Remove(el)
	delete(c.index, entry.key)
	c.used -= int64(len(entry.data))
}
