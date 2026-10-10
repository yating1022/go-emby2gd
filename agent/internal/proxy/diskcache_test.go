package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 磁盘块存储的单测（design §7）：布局/temp+rename、身份作废、TTL、LRU、
// 崩溃复用（重新打开目录）、atime 节流、并发。

// testDiskCache 构造一个注入时钟的磁盘缓存。
func testDiskCache(t *testing.T, budget int64, maxAge time.Duration, now func() time.Time) *DiskCache {
	t.Helper()
	c, err := NewDiskCache(DiskCacheConfig{
		Dir:         t.TempDir(),
		BudgetBytes: budget,
		MaxAge:      maxAge,
		Now:         now,
		Logger:      discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	return c
}

// makeBlock 造一段可辨认的块数据（首字节 = 块号，便于逐块校验）。
func makeBlock(idx int64, length int) []byte {
	data := make([]byte, length)
	for i := range data {
		data[i] = byte(int(idx) + i%251)
	}
	return data
}

func TestDiskCachePutGetLayout(t *testing.T) {
	c := testDiskCache(t, 1<<30, 0, time.Now)
	fileID := "file-1"
	meta := fileMeta{etag: "etag-1", size: int64(blockSize) + 100, contentType: "video/mp4"}
	if got := c.Observe(fileID, meta); got.identity() != "etag-1" {
		t.Fatalf("Observe identity = %q", got.identity())
	}

	block0 := makeBlock(0, blockSize)
	if !c.Put(fileID, "etag-1", 0, block0) {
		t.Fatal("Put block0 失败")
	}
	last := makeBlock(1, 100)
	if !c.Put(fileID, "etag-1", 1, last) {
		t.Fatal("Put last block 失败")
	}

	// 目录布局：<dir>/<hash[:2]>/<hash>/{meta.json, <idx>.blk}。
	dir := c.fileDir(fileID)
	if _, err := os.Stat(filepath.Join(dir, diskMetaName)); err != nil {
		t.Fatalf("meta.json 不存在：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "0"+diskBlockSuffix)); err != nil {
		t.Fatalf("0.blk 不存在：%v", err)
	}
	// temp+rename：目录里不得有残留的临时文件。
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if len(e.Name()) > 4 && e.Name()[len(e.Name())-4:] == ".tmp" {
			t.Fatalf("发现残留临时文件：%s", e.Name())
		}
	}

	got, ok := c.Get(fileID, "etag-1", 0)
	if !ok || !bytes.Equal(got, block0) {
		t.Fatalf("Get block0 ok=%v len=%d", ok, len(got))
	}
	if !c.Has(fileID, "etag-1", int64(blockSize)+100, 1) {
		t.Fatal("Has last block = false")
	}
	// 身份不符/长度不符一律 miss。
	if _, ok := c.Get(fileID, "other-etag", 0); ok {
		t.Fatal("身份不符的 Get 不该命中")
	}
	m, ok := c.Meta(fileID)
	if !ok || m.size != int64(blockSize)+100 || m.identity() != "etag-1" {
		t.Fatalf("Meta = %+v ok=%v", m, ok)
	}
	if c.UsedBytes() != int64(blockSize)+100 {
		t.Fatalf("UsedBytes = %d", c.UsedBytes())
	}
}

func TestDiskCacheIdentityChangeWipesBlocks(t *testing.T) {
	c := testDiskCache(t, 1<<30, 0, time.Now)
	fileID := "file-2"
	c.Observe(fileID, fileMeta{etag: "old", size: int64(blockSize)})
	c.Put(fileID, "old", 0, makeBlock(0, blockSize))
	if _, ok := c.Get(fileID, "old", 0); !ok {
		t.Fatal("预置块 Get 失败")
	}

	c.Observe(fileID, fileMeta{etag: "new", size: int64(blockSize)})
	if _, ok := c.Get(fileID, "new", 0); ok {
		t.Fatal("身份变化后旧块仍可命中")
	}
	if c.UsedBytes() != 0 {
		t.Fatalf("身份变化后 UsedBytes = %d，应为 0", c.UsedBytes())
	}
	if _, err := os.Stat(filepath.Join(c.fileDir(fileID), "0"+diskBlockSuffix)); !os.IsNotExist(err) {
		t.Fatalf("旧块文件未删除：err=%v", err)
	}
}

func TestDiskCacheTTLLazyAndSweep(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	c := testDiskCache(t, 1<<30, time.Hour, clock)
	fileID := "file-ttl"
	c.Observe(fileID, fileMeta{etag: "e", size: 10})
	c.Put(fileID, "e", 0, makeBlock(0, blockSize))

	now = now.Add(2 * time.Hour)
	// 惰性判定：Meta/Get/Has 全部按 miss 处理。
	if _, ok := c.Meta(fileID); ok {
		t.Fatal("超龄文件 Meta 仍命中")
	}
	if _, ok := c.Get(fileID, "e", 0); ok {
		t.Fatal("超龄文件 Get 仍命中")
	}
	if c.Has(fileID, "e", 0, 0) {
		t.Fatal("超龄文件 Has 仍为真")
	}
	c.Sweep()
	if _, err := os.Stat(c.fileDir(fileID)); !os.IsNotExist(err) {
		t.Fatalf("清扫未删除超龄目录：%v", err)
	}
	if c.UsedBytes() != 0 || c.FileCount() != 0 {
		t.Fatalf("清扫后 used=%d files=%d", c.UsedBytes(), c.FileCount())
	}
}

func TestDiskCacheLRUBudgetEvictsOldest(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	// 预算只够一个块：写第二个文件后必须淘汰更老的第一个。
	c := testDiskCache(t, blockSize, 0, clock)
	fileA, fileB := "file-a", "file-b"
	c.Observe(fileA, fileMeta{etag: "a", size: int64(blockSize)})
	c.Put(fileA, "a", 0, makeBlock(0, blockSize))
	now = now.Add(time.Minute)
	c.Observe(fileB, fileMeta{etag: "b", size: int64(blockSize)})
	c.Put(fileB, "b", 0, makeBlock(0, blockSize))

	if evicted := c.EnforceBudget(); evicted != 1 {
		t.Fatalf("EnforceBudget 淘汰数 = %d，应为 1", evicted)
	}
	if _, ok := c.Meta(fileA); ok {
		t.Fatal("更老的文件 A 未被淘汰")
	}
	if _, ok := c.Meta(fileB); !ok {
		t.Fatal("较新的文件 B 被误淘汰")
	}
	if _, err := os.Stat(c.fileDir(fileA)); !os.IsNotExist(err) {
		t.Fatalf("A 的目录未删除：%v", err)
	}
}

func TestDiskCacheReloadReusesBlocks(t *testing.T) {
	dir := t.TempDir()
	cfg := DiskCacheConfig{Dir: dir, BudgetBytes: 1 << 30, Logger: discardLogger()}
	first, err := NewDiskCache(cfg)
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	fileID := "file-crash"
	first.Observe(fileID, fileMeta{etag: "e-crash", size: int64(blockSize) + 5, contentType: "video/mp4"})
	first.Put(fileID, "e-crash", 0, makeBlock(0, blockSize))
	first.Put(fileID, "e-crash", 1, makeBlock(1, 5))

	// "重启"：同一个目录重新打开，已 rename 的块必须可复用（懒装载）。
	second, err := NewDiskCache(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := second.Get(fileID, "e-crash", 0)
	if !ok || !bytes.Equal(got, makeBlock(0, blockSize)) {
		t.Fatalf("重启后 block0 ok=%v len=%d", ok, len(got))
	}
	if !second.Has(fileID, "e-crash", int64(blockSize)+5, 1) {
		t.Fatal("重启后尾块 Has = false")
	}
	m, ok := second.Meta(fileID)
	if !ok || m.identity() != "e-crash" || m.size != int64(blockSize)+5 {
		t.Fatalf("重启后 Meta = %+v ok=%v", m, ok)
	}
	// 全量清扫（启动恢复路径）也不得丢块。
	second.Sweep()
	if _, ok := second.Get(fileID, "e-crash", 0); !ok {
		t.Fatal("Sweep 后块丢失")
	}
}

func TestDiskCacheCoverageAndStream(t *testing.T) {
	c := testDiskCache(t, 1<<30, 0, time.Now)
	fileID := "file-stream"
	size := int64(blockSize) + 100
	c.Observe(fileID, fileMeta{etag: "es", size: size})
	c.Put(fileID, "es", 0, makeBlock(0, blockSize))
	c.Put(fileID, "es", 1, makeBlock(1, 100))

	avail, full := c.Coverage(fileID, "es", 0, size-1)
	if !full || avail != size {
		t.Fatalf("Coverage 全域 = (%d,%v)", avail, full)
	}
	// 起点在块中间：连续覆盖从 start 算起。
	avail, full = c.Coverage(fileID, "es", blockSize-50, size-1)
	if !full || avail != 150 {
		t.Fatalf("Coverage 跨界 = (%d,%v)，应 (150,true)", avail, full)
	}

	var buf bytes.Buffer
	if err := c.Stream(&buf, fileID, "es", blockSize-50, 100); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	want := append(append([]byte{}, makeBlock(0, blockSize)[blockSize-50:]...), makeBlock(1, 100)[:50]...)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatal("Stream 内容与磁盘块不一致")
	}

	// 缺失块：Coverage 停在缺口处，Stream 报错（调用方据此断连）。
	c2 := testDiskCache(t, 1<<30, 0, time.Now)
	c2.Observe(fileID, fileMeta{etag: "es", size: size})
	c2.Put(fileID, "es", 1, makeBlock(1, 100))
	if avail, full := c2.Coverage(fileID, "es", 0, size-1); full || avail != 0 {
		t.Fatalf("缺块 Coverage = (%d,%v)，应 (0,false)", avail, full)
	}
	if err := c2.Stream(&bytes.Buffer{}, fileID, "es", 0, 10); err == nil {
		t.Fatal("缺块 Stream 应报错")
	}
}

func TestDiskCacheAccessThrottleAndPin(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	c := testDiskCache(t, 1<<30, time.Hour, clock)
	fileID := "file-pin"
	c.Observe(fileID, fileMeta{etag: "ep", size: 10})
	c.Put(fileID, "ep", 0, makeBlock(0, blockSize))

	// atime 节流：30s 内的访问只更新内存，不落盘。
	now = now.Add(5 * time.Second)
	c.Get(fileID, "ep", 0)
	assertMetaAtime(t, c, fileID, false)

	// 超过节流窗口：下一次访问把 last_access_at 落盘。
	now = now.Add(diskAccessFlushThrottle + time.Second)
	c.Get(fileID, "ep", 0)
	assertMetaAtime(t, c, fileID, true)

	// Pin：供流中的文件跳过 TTL 清扫；释放后下一轮清理。
	// 注意 Meta 对超龄恒 miss（惰性判定与 Pin 无关），不能拿它当"目录还在"的
	// 证据——直接查内存索引与块文件。
	release := c.Pin(fileID)
	now = now.Add(2 * time.Hour)
	c.Sweep()
	if c.FileCount() != 1 || c.UsedBytes() != int64(blockSize) {
		t.Fatalf("Pin 中的文件被 TTL 清扫：files=%d used=%d", c.FileCount(), c.UsedBytes())
	}
	if _, err := os.Stat(filepath.Join(c.fileDir(fileID), "0"+diskBlockSuffix)); err != nil {
		t.Fatalf("Pin 中的文件被 TTL 清扫：%v", err)
	}
	release()
	c.Sweep()
	if _, err := os.Stat(c.fileDir(fileID)); !os.IsNotExist(err) {
		t.Fatalf("释放后超龄目录未被清理：%v", err)
	}
}

// assertMetaAtime 检查磁盘上的 meta.json 的 last_access_at 是否已更新到"现在"。
func assertMetaAtime(t *testing.T, c *DiskCache, fileID string, flushed bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(c.fileDir(fileID), diskMetaName))
	if err != nil {
		t.Fatalf("读 meta.json: %v", err)
	}
	var meta diskMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("解析 meta.json: %v", err)
	}
	same := meta.LastAccessAt.Equal(c.now())
	if same != flushed {
		t.Fatalf("atime 落盘状态 = %v，应 %v（disk=%v now=%v）",
			same, flushed, meta.LastAccessAt, c.now())
	}
}

func TestDiskCacheConcurrentAccess(t *testing.T) {
	c := testDiskCache(t, 1<<30, 0, time.Now)
	fileID := "file-conc"
	c.Observe(fileID, fileMeta{etag: "ec", size: int64(blockSize) * 4})
	done := make(chan struct{})
	for w := 0; w < 4; w++ {
		go func(w int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 20; i++ {
				idx := int64((w + i) % 4)
				c.Put(fileID, "ec", idx, makeBlock(idx, blockSize))
				c.Get(fileID, "ec", idx)
				c.Has(fileID, "ec", int64(blockSize)*4, idx)
				c.Coverage(fileID, "ec", idx*blockSize, idx*blockSize+1023)
				_ = c.Stream(&bytes.Buffer{}, fileID, "ec", idx*blockSize, 16)
			}
		}(w)
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	// 定序：所有块最终都在。
	for idx := int64(0); idx < 4; idx++ {
		if !c.Has(fileID, "ec", int64(blockSize)*4, idx) {
			t.Fatalf("并发结束后块 %d 缺失", idx)
		}
	}
}

func TestDiskCacheSweeperContextCancel(t *testing.T) {
	c := testDiskCache(t, 1<<30, 0, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	c.StartSweeper(ctx)
	cancel()
	// 幂等：重复启动不 panic。
	c.StartSweeper(context.Background())
	// 让首轮清扫（goroutine）有机会跑完，确认无 panic 即可。
	time.Sleep(10 * time.Millisecond)
	if c.FileCount() != 0 {
		t.Fatalf("空缓存 FileCount = %d", c.FileCount())
	}
}
