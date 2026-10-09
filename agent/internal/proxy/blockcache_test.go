package proxy

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

// 块缓存的单测都用"短块"（长度远小于 blockSize）来验证预算/LRU/拼接逻辑：
// Put 不要求长度等于 blockSize，只有"块内容连续到文件末尾"的语义与块号有关。

func TestBlockCacheDisabledIsNoop(t *testing.T) {
	c := NewBlockCache(0)
	if c.Enabled() {
		t.Fatal("预算 0 应视为关闭")
	}
	c.Put("f", "e", 0, []byte("data"))
	if _, ok := c.Get("f", "e", 0); ok {
		t.Fatal("关闭时不应有命中")
	}
	if c.HasFile("f") {
		t.Fatal("关闭时不应报告已有块")
	}
	if _, ok := c.Meta("f"); ok {
		t.Fatal("关闭时不应记录元数据")
	}
	if got, n := c.Prefix("f", "e", 0, 100); n != 0 || got != nil {
		t.Fatalf("关闭时 Prefix 应为空，实际 n=%d", n)
	}
	c.SetEtag("f", "v1") // 不应 panic
	// nil 缓存同样退化为关闭。
	var nilCache *BlockCache
	if nilCache.Enabled() {
		t.Fatal("nil 缓存应视为关闭")
	}
	nilCache.Put("f", "e", 0, []byte("x"))
	if _, ok := nilCache.Get("f", "e", 0); ok {
		t.Fatal("nil 缓存不应有命中")
	}
}

func TestBlockCachePutGet(t *testing.T) {
	c := NewBlockCache(1 << 20)
	want := []byte("0123456789")
	c.Put("f", "e", 0, want)

	got, ok := c.Get("f", "e", 0)
	if !ok {
		t.Fatal("刚写入的块应能命中")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("块内容不符：%q", got)
	}
	if _, ok := c.Get("f", "e", 1); ok {
		t.Fatal("不同块号不应命中")
	}
	if _, ok := c.Get("f", "other", 0); ok {
		t.Fatal("不同身份（ETag）不应命中")
	}
	if _, ok := c.Get("other", "e", 0); ok {
		t.Fatal("不同文件不应命中")
	}
}

// Put 复制的语义：调用方的缓冲被复用/改写后，缓存里的内容不能跟着变。
func TestBlockCachePutCopiesData(t *testing.T) {
	c := NewBlockCache(1 << 20)
	buf := []byte("original")
	c.Put("f", "e", 0, buf)
	copy(buf, "hijacked")

	got, ok := c.Get("f", "e", 0)
	if !ok || string(got) != "original" {
		t.Fatalf("Put 应复制数据，实际 %q", got)
	}
}

func TestBlockCachePutKeepsFirstWriter(t *testing.T) {
	c := NewBlockCache(1 << 20)
	c.Put("f", "e", 0, []byte("first"))
	c.Put("f", "e", 0, []byte("second"))

	got, _ := c.Get("f", "e", 0)
	if string(got) != "first" {
		t.Fatalf("键已存在应以先到为准，实际 %q", got)
	}
}

func TestBlockCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := NewBlockCache(100)
	blk0 := bytes.Repeat([]byte{0}, 60)
	blk1 := bytes.Repeat([]byte{1}, 30)
	blk2 := bytes.Repeat([]byte{2}, 30)
	c.Put("f", "e", 0, blk0)
	c.Put("f", "e", 1, blk1)
	if c.used != 90 {
		t.Fatalf("预算占用应为 90，实际 %d", c.used)
	}
	// 命中一次块 0：它变成最近使用，接下来该淘汰块 1。
	if _, ok := c.Get("f", "e", 0); !ok {
		t.Fatal("块 0 应命中")
	}
	c.Put("f", "e", 2, blk2)
	if c.used > c.budget {
		t.Fatalf("淘汰后不应超预算：used=%d budget=%d", c.used, c.budget)
	}
	if _, ok := c.Get("f", "e", 0); !ok {
		t.Fatal("最近使用的块 0 不应被淘汰")
	}
	if _, ok := c.Get("f", "e", 1); ok {
		t.Fatal("最久未用的块 1 应被淘汰")
	}
	if _, ok := c.Get("f", "e", 2); !ok {
		t.Fatal("新写入的块 2 应在缓存里")
	}
}

func TestBlockCacheSkipsOversizedBlock(t *testing.T) {
	c := NewBlockCache(100)
	c.Put("f", "e", 0, make([]byte, 200))
	if _, ok := c.Get("f", "e", 0); ok {
		t.Fatal("单块超预算不应入库（否则会把自己立刻淘汰掉）")
	}
	if c.used != 0 {
		t.Fatalf("不应占用预算：%d", c.used)
	}
}

func TestBlockCacheSetEtagInvalidatesBlocks(t *testing.T) {
	c := NewBlockCache(1 << 20)
	c.SetEtag("f", "v1")
	c.Put("f", "v1", 0, []byte("old-bytes"))
	if _, ok := c.Get("f", "v1", 0); !ok {
		t.Fatal("v1 的块应命中")
	}

	c.SetEtag("f", "v2") // 身份变化 → 旧块立即失效
	if _, ok := c.Get("f", "v1", 0); ok {
		t.Fatal("ETag 变化后旧块必须不可再命中")
	}
	c.Put("f", "v2", 0, []byte("new-bytes"))
	if _, ok := c.Get("f", "v2", 0); !ok {
		t.Fatal("新身份的块应命中")
	}

	c.SetEtag("f", "v2") // 同值：不能误伤
	if _, ok := c.Get("f", "v2", 0); !ok {
		t.Fatal("ETag 未变时不应清除块")
	}
}

// 空 ETag 代表"这次响应没告诉我们内容身份"：不能拿它做失配判断，
// 否则一次降级响应就会把好块全清掉（下一次真 ETag 又要重建）。
func TestBlockCacheSetEtagEmptyIsNoop(t *testing.T) {
	c := NewBlockCache(1 << 20)
	c.SetEtag("f", "v1")
	c.Put("f", "v1", 0, []byte("data"))

	c.SetEtag("f", "")
	if _, ok := c.Get("f", "v1", 0); !ok {
		t.Fatal("空 ETag 不应清除旧块")
	}
	if m, _ := c.Meta("f"); m.etag != "v1" {
		t.Fatalf("空 ETag 不应覆盖已记录的 ETag：%q", m.etag)
	}
}

// 身份链：ETag → Last-Modified → 总字节数（R1 修订：真实 Google 直链没有前两者，
// 身份只能落到总大小）。切换/变化都必须让旧块不可命中。
func TestBlockCacheIdentityChain(t *testing.T) {
	c := NewBlockCache(1 << 20)
	m := c.Observe("f", fileMeta{lastModified: "LM-1", size: 100, contentType: "video/x-matroska"})
	if m.identity() != "LM-1" {
		t.Fatalf("无 ETag 时身份应退化为 Last-Modified，实际 %q", m.identity())
	}
	c.Put("f", m.identity(), 0, []byte("data"))
	if _, ok := c.Get("f", "LM-1", 0); !ok {
		t.Fatal("按 Last-Modified 身份应能命中")
	}

	// 之后响应带上了 ETag：身份切换，旧块失效。
	m2 := c.Observe("f", fileMeta{etag: "E-1"})
	if m2.identity() != "E-1" {
		t.Fatalf("身份应切换为 ETag，实际 %q", m2.identity())
	}
	if _, ok := c.Get("f", "LM-1", 0); ok {
		t.Fatal("身份切换后旧块必须不可再命中")
	}

	// 尺寸路径（真实 Google 形态）：无 ETag/Last-Modified 时身份 = 总字节数。
	m3 := c.Observe("h", fileMeta{size: 4096})
	if m3.identity() != "size:4096" {
		t.Fatalf("身份应退化为总字节数，实际 %q", m3.identity())
	}
	c.Put("h", m3.identity(), 0, []byte("data"))
	if _, ok := c.Get("h", "size:4096", 0); !ok {
		t.Fatal("按总大小身份应能命中")
	}

	// 同 id 换文件、大小不同 → 身份变化，旧块立即不可命中（透传 Observe 的落点）。
	if m4 := c.Observe("h", fileMeta{size: 8192}); m4.identity() != "size:8192" {
		t.Fatalf("大小变化后身份应更新，实际 %q", m4.identity())
	}
	if _, ok := c.Get("h", "size:4096", 0); ok {
		t.Fatal("总大小变化后旧块必须不可再命中")
	}
	if _, ok := c.Get("h", "size:8192", 0); ok {
		t.Fatal("新身份下还没有块，不应命中")
	}

	// 三者全缺：identity 为空，调用方据此不缓存（块键为空永远不可命中）。
	if empty := c.Observe("g", fileMeta{}); empty.identity() != "" {
		t.Fatalf("没有任何身份字段时 identity 应为空，实际 %q", empty.identity())
	}
}

// --- TTL（CACHE_MAX_AGE_MINUTES）：过期块不服务，但仍可被 LRU 回收 --------------

// assertCacheInvariants 断言缓存内部不变量：LRU 与索引一致、预算记账与实际数据量
// 一致、不超预算。过期回收（Get/Prefix/FullHit/HasFile 里的 removeLocked）与
// LRU 淘汰走同一条回收路径，调用后必须仍然自洽。
func assertCacheInvariants(t *testing.T, c *BlockCache) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var sum int64
	for _, el := range c.index {
		sum += int64(len(el.Value.(*blockEntry).data))
	}
	if sum != c.used {
		t.Fatalf("预算记账与实际数据量不一致：used=%d 实际=%d", c.used, sum)
	}
	if c.lru.Len() != len(c.index) {
		t.Fatalf("LRU 与索引不一致：lru=%d index=%d", c.lru.Len(), len(c.index))
	}
	if c.used > c.budget {
		t.Fatalf("超出预算：used=%d budget=%d", c.used, c.budget)
	}
}

// 块龄超过最大年龄一律按 miss 处理（Get/Prefix/FullHit/HasFile 一致），并顺手
// 回收该块（否则同键的旧块会挡住重新预取）；按"当前"时钟写入的块照常可服务。
func TestBlockCacheExpiredBlocksAreNotServed(t *testing.T) {
	base := time.Unix(1700000000, 0)
	c := NewBlockCacheWithMaxAge(1<<20, 24*time.Hour)
	c.setAgeForTest(24*time.Hour, func() time.Time { return base })

	for _, file := range []string{"f", "g", "h", "i"} {
		c.Put(file, "e", 0, []byte("old-"+file))
	}
	if _, ok := c.Get("f", "e", 0); !ok {
		t.Fatal("未过期时块应命中")
	}

	// 时钟推进到 25h（超过 24h 上限）：所有块过期。
	c.setAgeForTest(24*time.Hour, func() time.Time { return base.Add(25 * time.Hour) })
	if _, ok := c.Get("f", "e", 0); ok {
		t.Fatal("过期块不得被 Get 命中")
	}
	if got, n := c.Prefix("g", "e", 0, 100); n != 0 || got != nil {
		t.Fatalf("过期块不得被 Prefix 命中：n=%d", n)
	}
	if _, ok := c.FullHit("h", "e", 0, 0); ok {
		t.Fatal("过期块不得被 FullHit 命中")
	}
	if c.HasFile("i") {
		t.Fatal("只剩过期块时不应报告已有块（否则该文件永远不再预取）")
	}
	// 四个块都被顺带回收：索引与预算记账必须一起清干净（removeLocked 的不变量）。
	c.mu.Lock()
	left, used := len(c.index), c.used
	c.mu.Unlock()
	if left != 0 || used != 0 {
		t.Fatalf("过期块应被顺带回收：剩余块=%d used=%d", left, used)
	}
	assertCacheInvariants(t, c)

	// 过期块被顺带回收后同键可写新块（Put 先到为准），新块按当前时钟计龄、可服务。
	c.Put("f", "e", 0, []byte("fresh"))
	if got, ok := c.Get("f", "e", 0); !ok || string(got) != "fresh" {
		t.Fatalf("回收后应能写入并命中新块：%q ok=%v", got, ok)
	}
	assertCacheInvariants(t, c)
}

// maxAge <= 0 = 不做年龄检查（0 是合法配置值）：时钟怎么走都不影响服务，
// 与不带年龄上限的版本行为一致。
func TestBlockCacheNoAgeCheckWhenMaxAgeNotSet(t *testing.T) {
	base := time.Unix(1700000000, 0)
	c := NewBlockCache(1 << 20)
	c.setAgeForTest(0, func() time.Time { return base })
	c.Put("f", "e", 0, []byte("data"))

	c.setAgeForTest(0, func() time.Time { return base.Add(10 * 365 * 24 * time.Hour) })
	if _, ok := c.Get("f", "e", 0); !ok {
		t.Fatal("maxAge<=0 时不做年龄检查，块必须照常命中")
	}
	if !c.HasFile("f") {
		t.Fatal("maxAge<=0 时 HasFile 不应受年龄影响")
	}
	assertCacheInvariants(t, c)
}

// 过期块只是"不服务"，不是"被立刻清扫"：没被访问到时它留在 LRU 里，
// 预算压力下照常被淘汰（回收路径不依赖过期检测）。
func TestBlockCacheExpiredBlocksAreEvictable(t *testing.T) {
	base := time.Unix(1700000000, 0)
	c := NewBlockCacheWithMaxAge(200, 24*time.Hour)
	c.setAgeForTest(24*time.Hour, func() time.Time { return base })

	c.Put("old", "e", 0, bytes.Repeat([]byte{1}, 100)) // 最旧 + 即将过期
	c.setAgeForTest(24*time.Hour, func() time.Time { return base.Add(25 * time.Hour) })
	c.Put("a", "e", 0, bytes.Repeat([]byte{2}, 100))
	c.Put("b", "e", 0, bytes.Repeat([]byte{3}, 100)) // 超预算：淘汰最久未用的

	c.mu.Lock()
	_, alive := c.index[blockKey{fileID: "old", identity: "e", idx: 0}]
	used := c.used
	c.mu.Unlock()
	if alive {
		t.Fatal("过期块在预算压力下应被 LRU 回收")
	}
	if used != 200 {
		t.Fatalf("淘汰后预算占用应为 200，实际 %d", used)
	}
	if _, ok := c.Get("a", "e", 0); !ok {
		t.Fatal("较新的块不应被淘汰")
	}
	assertCacheInvariants(t, c)
}

// 元数据合并不是"整体覆盖"：降级响应（缺字段）不得抹掉已知信息。
func TestBlockCacheMetaMergeKeepsKnownFields(t *testing.T) {
	c := NewBlockCache(1 << 20)
	c.Observe("f", fileMeta{etag: "v1", size: 4096, contentType: "video/mp4", lastModified: "LM-1"})

	m := c.Observe("f", fileMeta{}) // 空观测：原样返回
	if m.size != 4096 || m.contentType != "video/mp4" || m.etag != "v1" || m.lastModified != "LM-1" {
		t.Fatalf("空观测不应改动元数据：%+v", m)
	}

	m = c.Observe("f", fileMeta{etag: "v1"}) // 只带 ETag 的降级响应
	if m.size != 4096 || m.contentType != "video/mp4" {
		t.Fatalf("缺字段不应被清零：%+v", m)
	}
	if _, ok := c.Meta("f"); !ok {
		t.Fatal("Meta 应能取到记录")
	}
	if m2, ok := c.Meta("f2"); ok {
		t.Fatalf("未观测过的文件不应有元数据：%+v", m2)
	}
}

func TestBlockCachePrefixStopsAtShortBlock(t *testing.T) {
	c := NewBlockCache(1 << 20)
	data := []byte("0123456789abcdefghij") // 20 字节 = 文件的最后一块（短块）
	c.Put("f", "e", 0, data)
	c.Put("f", "e", 1, []byte("NEXT"))

	got, n := c.Prefix("f", "e", 5, 6)
	if n != 6 || !bytes.Equal(got, data[5:11]) {
		t.Fatalf("块内偏移 + limit 截断不对：n=%d got=%q", n, got)
	}
	got, n = c.Prefix("f", "e", 5, 1000)
	if n != 15 || !bytes.Equal(got, data[5:]) {
		t.Fatalf("应返回块内偏移之后的全部数据：n=%d got=%q", n, got)
	}
	got, n = c.Prefix("f", "e", 0, 1000)
	if n != 20 || !bytes.Equal(got, data) {
		t.Fatalf("短块是文件最后一块，不得续接下一块：n=%d got=%q", n, got)
	}
	if _, n := c.Prefix("f", "e", 25, 10); n != 0 {
		t.Fatalf("越界起点应返回空：n=%d", n)
	}
	if _, n := c.Prefix("f", "e", 0, 0); n != 0 {
		t.Fatalf("limit 0 应返回空：n=%d", n)
	}
}

// 跨块拼接：前一块必须是完整块（长度 == blockSize），否则按"文件最后一块"处理。
func TestBlockCachePrefixAndFullHitAcrossBlocks(t *testing.T) {
	c := NewBlockCache(int64(2 * blockSize))
	first := make([]byte, blockSize)
	for i := range first {
		first[i] = byte(i)
	}
	tail := []byte("TAIL")
	c.Put("f", "e", 0, first)
	c.Put("f", "e", 1, tail)

	// 跨块起点：块 0 的倒数第 2 个字节开始，拿满块 0 尾部 + 块 1。
	got, n := c.Prefix("f", "e", blockSize-2, blockSize)
	want := append(append([]byte(nil), first[blockSize-2:]...), tail...)
	if n != int64(len(want)) || !bytes.Equal(got, want) {
		t.Fatalf("跨块前缀不对：n=%d 期望 %d", n, len(want))
	}

	if _, ok := c.FullHit("f", "e", blockSize-2, blockSize+int64(len(tail))-1); !ok {
		t.Fatal("覆盖块 0 尾部 + 短块 1 的区间应算全命中")
	}
	if _, ok := c.FullHit("f", "e", blockSize-2, blockSize+int64(len(tail))); ok {
		t.Fatal("超出文件末尾的区间不算全命中")
	}
	if _, ok := c.FullHit("f", "e", 0, 0); !ok {
		t.Fatal("单字节区间应命中")
	}
	if _, ok := c.FullHit("f", "e", 0, blockSize); !ok {
		t.Fatal("块 0 是完整块，越界的 1 字节应由块 1 补齐")
	}
	if _, ok := c.FullHit("f", "e", 0, blockSize+int64(len(tail))+10); ok {
		t.Fatal("超出文件末尾的区间不算全命中")
	}
}

func TestBlockCacheHasFile(t *testing.T) {
	c := NewBlockCache(1 << 20)
	if c.HasFile("f") {
		t.Fatal("空缓存不应报告已有块")
	}
	c.Put("f", "e", 0, []byte("x"))
	if !c.HasFile("f") {
		t.Fatal("写入后应报告已有块")
	}
	if c.HasFile("g") {
		t.Fatal("没有块的文件不应被报告")
	}
}

// 并发读写：预取写入（Put/Observe）与命中读取（Get/Prefix/FullHit）同时发生，
// 且预算很小、持续淘汰。-race 下运行；收尾校验内部不变量（LRU 与索引一致、
// 预算记账与实际数据量一致、不超预算）——Get 返回的只读切片在淘汰后也必须仍然有效。
func TestBlockCacheConcurrentAccess(t *testing.T) {
	const blockLen = 64 << 10
	c := NewBlockCache(3 * blockLen) // 只装得下 3 块：每次都淘汰

	blocks := make([][]byte, 8)
	for i := range blocks {
		blocks[i] = bytes.Repeat([]byte{byte(i + 1)}, blockLen)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				idx := int64((g + i) % len(blocks))
				c.Put("f", "e1", idx, blocks[idx])
				if data, ok := c.Get("f", "e1", idx); ok && len(data) == 0 {
					t.Errorf("命中却拿到空块：idx=%d", idx)
				}
				if _, n := c.Prefix("f", "e1", idx*blockLen, blockLen); n < 0 || n > blockLen {
					t.Errorf("Prefix 返回长度异常：%d", n)
				}
				if parts, ok := c.FullHit("f", "e1", 0, blockLen-1); ok && len(parts) == 0 {
					t.Errorf("FullHit 命中却没有数据")
				}
				c.Observe("f", fileMeta{etag: "e1", size: int64(len(blocks)) * blockLen})
				_ = c.HasFile("f")
			}
		}(g)
	}
	wg.Wait()
	assertCacheInvariants(t, c)
}
