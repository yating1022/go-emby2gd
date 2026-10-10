package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hub 磁盘块存储（design 10-10-hub-agent-mode §2）。
//
// 目录布局：<dir>/<sha256(fileID) 前 2 位>/<sha256(fileID)>/{meta.json, <idx>.blk}。
// 文件名即块号，偏移 = 块号 × blockSize（4MiB）；**只有凑满的块**才会落盘
// （唯一例外是文件的最后一块，长度 = size − idx×blockSize）——与 node 内存版
// 的写入不变式相同，本地拼响应时的判据因此与 node 的 walk/cacheBlockPresent 一致。
//
// 与内存版 BlockCache 的三处刻意差异：
//   - identity 不匹配 = "整目录作废重抓"（磁盘布局冻结为每文件一个目录，
//     没法像内存那样让多个身份的三元组键共存）；
//   - TTL/LRU 以**文件**为单位（48h 自 created_at 起；超预算按 last_access_at 淘汰整文件）；
//   - Get/Stream 真的读盘，失败一律按 miss 处理，绝不把半截数据拼进响应。
//
// 并发约定（避免死锁，请勿颠倒）：分片锁 → c.mu；任何文件系统 I/O 都在 c.mu 之外。
// 供流期间 Stream 会整段持有分片锁，因此并发的淘汰/作废会排队等它读完——
// 这正是"读到一半被淘汰就是坏字节"的护栏；Pin 则让整个播放会话内文件不被淘汰。
const (
	// diskMetaName 是每个文件目录里的元数据文件名（原子写：temp+rename）。
	diskMetaName = "meta.json"
	// diskBlockSuffix 是块文件后缀；文件名去掉后缀即块号。
	diskBlockSuffix = ".blk"
	// diskTempPattern 是块文件临时名模式（崩溃残留由装载/清扫顺手回收）。
	diskTempPattern = "block-*.tmp"
	// diskSweepInterval 是周期清扫间隔（design §2 的"如 10min 一轮"）。
	diskSweepInterval = 10 * time.Minute
	// diskAccessFlushThrottle 是 last_access_at 落盘的节流窗口（design §2 的 ≥30s）。
	diskAccessFlushThrottle = 30 * time.Second
	// diskStaleTempAge 是判定"崩溃残留"的临时文件年龄：比它老才清。
	// 年轻的不动——那可能是另一个 goroutine 正在写的临时文件。
	diskStaleTempAge = time.Hour
	// diskShardCount 是按 fileID 分片锁的分片数（design §2 的"按 fileID 分片锁"）。
	diskShardCount = 64
)

// diskMeta 是 meta.json 的内容（design §2：file_id/identity/size/created_at/last_access_at；
// 另存拼响应头所需的 ETag/Last-Modified/Content-Type/Accept-Ranges——与 node 的
// fileMeta 一一对应，本地 206 的白名单响应头因此与上游观测值一致）。
type diskMeta struct {
	FileID       string    `json:"file_id"`
	Identity     string    `json:"identity"`
	Size         int64     `json:"size"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	ContentType  string    `json:"content_type,omitempty"`
	AcceptRanges string    `json:"accept_ranges,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	LastAccessAt time.Time `json:"last_access_at"`
}

// fileMeta 还原成与 node 缓存同一套的元数据视图。
func (m diskMeta) fileMeta() fileMeta {
	return fileMeta{
		etag:         m.ETag,
		lastModified: m.LastModified,
		size:         m.Size,
		contentType:  m.ContentType,
		acceptRanges: m.AcceptRanges,
	}
}

// diskFile 是一个文件目录的内存索引（字段读写一律在 DiskCache.mu 下）。
type diskFile struct {
	fileID string
	dir    string
	meta   diskMeta
	// blocks 是磁盘上存在的块：块号 → 字节数（懒装载时来自目录扫描，
	// 因此"崩溃后已 rename 的块"重启后立即可复用）。
	blocks map[int64]int64
	used   int64 // blocks 的字节和
	// refs 是"供流中"引用计数：> 0 的文件不参与 TTL 清扫与 LRU 淘汰。
	refs int
	// lastFlush 是 meta.json 上次落盘时刻（last_access_at 节流用）。
	lastFlush time.Time
}

// DiskCacheConfig 组装 DiskCache。
type DiskCacheConfig struct {
	Dir         string
	BudgetBytes int64
	// MaxAge 是文件自 created_at 起的最大可服务年龄（<= 0 = 不做年龄检查）。
	// hub 上默认 48h（CACHE_MAX_AGE_MINUTES=2880）。
	MaxAge time.Duration
	Logger *slog.Logger
	// Now 可注入时钟（测试用），默认 time.Now。
	Now func() time.Time
	// SweepInterval 是周期清扫间隔，默认 10min（测试可缩短）。
	SweepInterval time.Duration
}

// DiskCache 是 hub 的磁盘块存储（temp+rename 原子落盘；并发安全）。
type DiskCache struct {
	cfg DiskCacheConfig
	log *slog.Logger
	now func() time.Time

	// shards 是按 fileID 的分片锁：目录级操作（装载/写块/作废/淘汰/供流）互斥到单个文件。
	shards [diskShardCount]sync.Mutex

	mu    sync.Mutex
	files map[string]*diskFile
	used  int64 // 全部已登记文件的块字节和（首轮全量扫描后与磁盘一致）

	sweepOnce sync.Once
}

// NewDiskCache 打开/创建磁盘缓存目录。目录不可用时返回中文错误（调用方应拒绝启动：
// hub 没有缓存目录就只剩透传，运维上属于配置错误）。
func NewDiskCache(cfg DiskCacheConfig) (*DiskCache, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("磁盘缓存目录（DISK_CACHE_DIR）不能为空")
	}
	if cfg.BudgetBytes <= 0 {
		return nil, fmt.Errorf("磁盘缓存预算（DISK_BUDGET_GB）必须大于 0")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = diskSweepInterval
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建磁盘缓存目录失败：%w", err)
	}
	// 写一次探针文件验证可写：磁盘满/只读挂载属于启动就该发现的问题。
	probe := filepath.Join(cfg.Dir, ".write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return nil, fmt.Errorf("磁盘缓存目录不可写：%w", err)
	}
	_ = os.Remove(probe)

	c := &DiskCache{
		cfg:   cfg,
		log:   cfg.Logger,
		now:   cfg.Now,
		files: make(map[string]*diskFile),
	}
	c.log.Info("hub 磁盘缓存就绪", "dir", cfg.Dir, "budget_bytes", cfg.BudgetBytes)
	return c, nil
}

// Enabled 报告缓存是否开启。
func (c *DiskCache) Enabled() bool {
	return c != nil && c.cfg.Dir != "" && c.cfg.BudgetBytes > 0
}

// StartSweeper 启动周期清扫：**立即**跑一轮（重建崩溃前的磁盘索引、让容量计账准确），
// 之后每 SweepInterval 一轮。同一个 DiskCache 只会启动一个清扫协程。
func (c *DiskCache) StartSweeper(ctx context.Context) {
	if !c.Enabled() {
		return
	}
	c.sweepOnce.Do(func() {
		go func() {
			c.Sweep()
			t := time.NewTicker(c.cfg.SweepInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					c.Sweep()
				}
			}
		}()
	})
}

// shard 返回该 fileID 的分片锁。
func (c *DiskCache) shard(fileID string) *sync.Mutex {
	sum := sha256.Sum256([]byte(fileID))
	return &c.shards[int(sum[0])%diskShardCount]
}

// fileDir 返回该文件的目录：<Dir>/<hash 前 2 位>/<hash>（design §2 的布局）。
func (c *DiskCache) fileDir(fileID string) string {
	sum := sha256.Sum256([]byte(fileID))
	h := hex.EncodeToString(sum[:])
	return filepath.Join(c.cfg.Dir, h[:2], h)
}

func (c *DiskCache) blockPath(dir string, idx int64) string {
	return filepath.Join(dir, strconv.FormatInt(idx, 10)+diskBlockSuffix)
}

// expiredLocked 报告文件是否超龄；调用方须持有 c.mu。
func (c *DiskCache) expiredLocked(f *diskFile) bool {
	if c.cfg.MaxAge <= 0 || f.meta.CreatedAt.IsZero() {
		return false
	}
	return c.now().Sub(f.meta.CreatedAt) > c.cfg.MaxAge
}

// observeLocked 合并一次元数据（非空才覆盖，与 BlockCache.Observe 同一语义）；
// 调用方须持有 c.mu。identity 变化时返回 changed=true，由调用方清块。
func observeLocked(f *diskFile, m fileMeta, now time.Time) (changed bool) {
	prev := f.meta.Identity
	if m.etag != "" {
		f.meta.ETag = m.etag
	}
	if m.lastModified != "" {
		f.meta.LastModified = m.lastModified
	}
	if m.size > 0 {
		f.meta.Size = m.size
	}
	if m.contentType != "" {
		f.meta.ContentType = m.contentType
	}
	if m.acceptRanges != "" {
		f.meta.AcceptRanges = m.acceptRanges
	}
	if f.meta.CreatedAt.IsZero() {
		f.meta.CreatedAt = now
	}
	next := f.meta.fileMeta().identity()
	if next != "" && next != prev {
		f.meta.CreatedAt = now // 新内容从此刻重新计时（TTL 是身份链退化后的第二道防线）
		changed = true
	}
	if next != "" {
		f.meta.Identity = next
	}
	f.meta.LastAccessAt = now
	return changed
}

// loadDir 读取一个文件目录（meta.json + 块目录扫描），返回内存索引；目录或
// meta.json 不存在时 ok=false。这就是"启动恢复/懒扫描"的装载原语：崩溃后已
// rename 的块在重启后由目录扫描直接复用（原子性由 temp+rename 保证）。
func (c *DiskCache) loadDir(fileID, dir string) (*diskFile, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, diskMetaName))
	if err != nil {
		return nil, false
	}
	var meta diskMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		c.log.Warn("hub 磁盘缓存：meta.json 无法解析，目录按空处理", "file_id", fileID, "error", err)
		return nil, false
	}
	if meta.FileID == "" {
		meta.FileID = fileID
	}
	if meta.FileID != fileID {
		// 哈希撞目录（理论上不可能）或人工搬动：不认，避免把别人的字节当成本文件的。
		c.log.Warn("hub 磁盘缓存：目录里的 file_id 与请求不符，忽略该目录",
			"want_file_id", fileID, "got_file_id", meta.FileID)
		return nil, false
	}
	// identity 是派生值：字段能算出就重算（修正历史/手改的失配），
	// 算不出（防御性路径建的文件）则保留存盘值。
	if recomputed := meta.fileMeta().identity(); recomputed != "" {
		meta.Identity = recomputed
	}

	f := &diskFile{fileID: fileID, dir: dir, meta: meta, blocks: make(map[int64]int64)}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false
		}
		c.log.Warn("hub 磁盘缓存：目录扫描失败，按空处理", "file_id", fileID, "error", err)
		return nil, false
	}
	now := c.now()
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, diskBlockSuffix) {
			idx, err := strconv.ParseInt(strings.TrimSuffix(name, diskBlockSuffix), 10, 64)
			if err != nil || idx < 0 {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			f.blocks[idx] = info.Size()
			f.used += info.Size()
			continue
		}
		// 崩溃残留的临时文件：只清足够老的（年轻的可能正被别的 goroutine 写着）。
		if strings.HasPrefix(name, "block-") && strings.HasSuffix(name, ".tmp") {
			if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > diskStaleTempAge {
				_ = os.Remove(filepath.Join(dir, name))
			}
		}
	}
	return f, true
}

// lockedEntry 返回该文件的内存索引（装载 + 登记）；调用方须持有分片锁。
func (c *DiskCache) lockedEntry(fileID string) (*diskFile, bool) {
	c.mu.Lock()
	f, ok := c.files[fileID]
	c.mu.Unlock()
	if ok {
		return f, true
	}
	f, ok = c.loadDir(fileID, c.fileDir(fileID))
	if !ok {
		return nil, false
	}
	c.register(f)
	return f, true
}

// register 把装载好的文件放进索引（重复注册以先到为准）。
func (c *DiskCache) register(f *diskFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.files[f.fileID]; ok {
		return
	}
	c.files[f.fileID] = f
	c.used += f.used
}

// fileFor 返回该文件的内存索引；首访从磁盘惰性装载。目录不存在返回 ok=false
// （不做"负缓存"：每次访问一次 stat 的成本远低于维护失效的复杂度）。
func (c *DiskCache) fileFor(fileID string) (*diskFile, bool) {
	c.mu.Lock()
	f, ok := c.files[fileID]
	c.mu.Unlock()
	if ok {
		return f, true
	}
	shard := c.shard(fileID)
	shard.Lock()
	defer shard.Unlock()
	// 双检：等分片锁期间可能已被别的 goroutine 装载。
	return c.lockedEntry(fileID)
}

// writeMeta 把 meta.json 原子写回（temp+rename）。元数据快照在 mu 下取，
// 写盘在锁外做。
func (c *DiskCache) writeMeta(f *diskFile) {
	c.mu.Lock()
	snapshot := f.meta
	c.mu.Unlock()
	payload, err := json.Marshal(snapshot)
	if err != nil {
		c.log.Warn("hub 磁盘缓存：meta.json 序列化失败", "file_id", f.fileID, "error", err)
		return
	}
	tmp := filepath.Join(f.dir, diskMetaName+".tmp")
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		c.log.Warn("hub 磁盘缓存：meta.json 写入失败", "file_id", f.fileID, "error", err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(f.dir, diskMetaName)); err != nil {
		c.log.Warn("hub 磁盘缓存：meta.json 落盘失败", "file_id", f.fileID, "error", err)
		_ = os.Remove(tmp)
	}
}

// Meta 返回该文件当前生效的元数据（与 node 的 BlockCache.Meta 同语义）。
func (c *DiskCache) Meta(fileID string) (fileMeta, bool) {
	if !c.Enabled() || fileID == "" {
		return fileMeta{}, false
	}
	f, ok := c.fileFor(fileID)
	if !ok {
		return fileMeta{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.expiredLocked(f) {
		return fileMeta{}, false
	}
	return f.meta.fileMeta(), true
}

// Observe 合并一次上游响应的元数据并返回合并结果（与 BlockCache.Observe 同语义：
// 非空才覆盖；identity 变化 = 整目录作废重抓）。零值 fileMeta 是"只读当前值"。
func (c *DiskCache) Observe(fileID string, m fileMeta) fileMeta {
	if !c.Enabled() || fileID == "" {
		return m
	}
	shard := c.shard(fileID)
	shard.Lock()
	defer shard.Unlock()

	f, ok := c.lockedEntry(fileID)
	created := false
	if !ok {
		if m == (fileMeta{}) {
			return fileMeta{}
		}
		// 还没有目录：建一个（identity 由字段算出；三项全缺时不建——
		// 没有身份的文件永远不会被服务，建目录只是垃圾）。
		if m.identity() == "" {
			return m
		}
		newFile, err := c.createFile(fileID, m.identity())
		if err != nil {
			c.log.Warn("hub 磁盘缓存：创建目录失败", "file_id", fileID, "error", err)
			return m
		}
		c.register(newFile)
		f = newFile
		created = true
	}

	now := c.now()
	c.mu.Lock()
	if m == (fileMeta{}) {
		out := f.meta.fileMeta()
		c.mu.Unlock()
		return out
	}
	changed := observeLocked(f, m, now)
	out := f.meta.fileMeta()
	var wipe []string
	if changed {
		wipe = make([]string, 0, len(f.blocks))
		for idx := range f.blocks {
			wipe = append(wipe, c.blockPath(f.dir, idx))
		}
		c.used -= f.used
		f.used = 0
		f.blocks = make(map[int64]int64)
	}
	flushed := now.Sub(f.lastFlush) >= diskAccessFlushThrottle
	if flushed {
		f.lastFlush = now
	}
	// created 时必须重写：createFile 已先落过一份"只有身份、size=0"的 meta，
	// 本次合并出的 size/Content-Type 不重写就会在重启后丢失。
	fresh := created || f.meta.CreatedAt.Equal(now)
	c.mu.Unlock()

	for _, path := range wipe {
		_ = os.Remove(path)
	}
	if changed || flushed || fresh {
		c.writeMeta(f)
	}
	return out
}

// Get 读一个块（返回新切片，调用方可安全持有）。任何一步不满足（未开启、identity
// 不符、超龄、缺块、读盘失败/长度不符）都返回 miss——绝不返回半截数据。
func (c *DiskCache) Get(fileID, identity string, idx int64) ([]byte, bool) {
	if !c.Enabled() || fileID == "" || identity == "" {
		return nil, false
	}
	shard := c.shard(fileID)
	shard.Lock()
	defer shard.Unlock()

	f, ok := c.lockedEntry(fileID)
	if !ok {
		return nil, false
	}
	c.mu.Lock()
	if f.meta.Identity != identity || c.expiredLocked(f) {
		c.mu.Unlock()
		return nil, false
	}
	length, present := f.blocks[idx]
	path := c.blockPath(f.dir, idx)
	now := c.now()
	f.meta.LastAccessAt = now
	flushed := now.Sub(f.lastFlush) >= diskAccessFlushThrottle
	if flushed {
		f.lastFlush = now
	}
	c.mu.Unlock()

	if !present {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil || int64(len(data)) != length {
		return nil, false
	}
	if flushed {
		c.writeMeta(f)
	}
	return data, true
}

// Has 报告块 idx 是否以**完整期望长度**在缓存里（不读块数据）。
// 长度判据与写入侧一致：整块 = blockSize；文件最后一块 = size − idx×blockSize。
func (c *DiskCache) Has(fileID, identity string, size, idx int64) bool {
	if !c.Enabled() || fileID == "" || identity == "" || idx < 0 {
		return false
	}
	f, ok := c.fileFor(fileID)
	if !ok {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if f.meta.Identity != identity || c.expiredLocked(f) {
		return false
	}
	length, ok := f.blocks[idx]
	if !ok {
		return false
	}
	want := int64(blockSize)
	if size > 0 && (idx+1)*blockSize > size {
		want = size - idx*blockSize
	}
	if want <= 0 {
		return true // 该块在文件里不存在
	}
	return length == want
}

// Put 写入一个**完整**块（temp+rename 原子落盘）。返回 false 表示跳过：
// 缓存关闭 / 缺 fileID 或 identity / identity 与已登记的不符（旧身份在途流，
// 由 Observe 负责作废重建）/ 建目录或写盘失败（只记 WARN，不中断供流）。
func (c *DiskCache) Put(fileID, identity string, idx int64, data []byte) bool {
	if !c.Enabled() || fileID == "" || identity == "" || len(data) == 0 || idx < 0 {
		return false
	}
	shard := c.shard(fileID)
	shard.Lock()
	defer shard.Unlock()

	f, ok := c.lockedEntry(fileID)
	if !ok {
		var err error
		f, err = c.createFile(fileID, identity)
		if err != nil {
			c.log.Warn("hub 磁盘缓存：建目录失败，跳过该块", "file_id", fileID, "error", err)
			return false
		}
		c.register(f)
	}

	c.mu.Lock()
	if f.meta.Identity != identity {
		c.mu.Unlock()
		return false
	}
	if _, exists := f.blocks[idx]; exists {
		c.mu.Unlock()
		return true // 先到为准（与内存版 Put 同语义）
	}
	c.mu.Unlock()

	tmp, err := os.CreateTemp(f.dir, diskTempPattern)
	if err != nil {
		c.log.Warn("hub 磁盘缓存：创建临时块文件失败，跳过该块", "file_id", fileID, "error", err)
		return false
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		c.log.Warn("hub 磁盘缓存：写块失败，跳过该块", "file_id", fileID, "block", idx, "error", err)
		return false
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		c.log.Warn("hub 磁盘缓存：关闭临时块文件失败，跳过该块", "file_id", fileID, "block", idx, "error", err)
		return false
	}
	if err := os.Rename(tmpName, c.blockPath(f.dir, idx)); err != nil {
		_ = os.Remove(tmpName)
		c.log.Warn("hub 磁盘缓存：块落盘失败，跳过该块", "file_id", fileID, "block", idx, "error", err)
		return false
	}

	now := c.now()
	c.mu.Lock()
	f.blocks[idx] = int64(len(data))
	f.used += int64(len(data))
	c.used += int64(len(data))
	f.meta.LastAccessAt = now
	flushed := now.Sub(f.lastFlush) >= diskAccessFlushThrottle
	if flushed {
		f.lastFlush = now
	}
	c.mu.Unlock()
	if flushed {
		c.writeMeta(f)
	}
	return true
}

// createFile 建立新文件目录与 meta.json（调用方须持有分片锁）。
func (c *DiskCache) createFile(fileID, identity string) (*diskFile, error) {
	dir := c.fileDir(fileID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	now := c.now()
	meta := diskMeta{FileID: fileID, Identity: identity, CreatedAt: now, LastAccessAt: now}
	// identity 形如 "size:<n>" 时把总大小还原进元数据（createFile 的防御性调用方
	// 还没 Observe 过），让"identity 由字段重算"的不变式成立。
	if strings.HasPrefix(identity, "size:") {
		if n, err := strconv.ParseInt(strings.TrimPrefix(identity, "size:"), 10, 64); err == nil {
			meta.Size = n
		}
	} else if meta.fileMeta().identity() == "" {
		// 其它形态（ETag/Last-Modified）无从区分：记进 ETag 字段保住身份字符串。
		// hub 的正常路径（先 Observe 再 Put）不会走到这里。
		meta.ETag = identity
	}
	f := &diskFile{fileID: fileID, dir: dir, meta: meta, blocks: make(map[int64]int64), lastFlush: now}
	c.writeMeta(f)
	return f, nil
}

// Pin 给文件加一个"供流中"引用；返回释放函数（幂等）。被引用的文件不参与
// TTL 清扫与 LRU 淘汰——长供流读到一半被淘汰就是坏字节。文件不在缓存里是空操作。
func (c *DiskCache) Pin(fileID string) func() {
	noop := func() {}
	if !c.Enabled() || fileID == "" {
		return noop
	}
	f, ok := c.fileFor(fileID)
	if !ok {
		return noop
	}
	c.mu.Lock()
	f.refs++
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			if f.refs > 0 {
				f.refs--
			}
			c.mu.Unlock()
		})
	}
}

// Coverage 报告 [start, end] 的本地覆盖：avail = 自 start 起的**连续**可用字节数
// （不越过 end），full = 整个区间都被覆盖。判定不出网、不读块数据。
func (c *DiskCache) Coverage(fileID, identity string, start, end int64) (avail int64, full bool) {
	if !c.Enabled() || fileID == "" || identity == "" || start < 0 || end < start {
		return 0, false
	}
	f, ok := c.fileFor(fileID)
	if !ok {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if f.meta.Identity != identity || c.expiredLocked(f) {
		return 0, false
	}
	limit := end - start + 1
	idx := start / blockSize
	off := start % blockSize
	var total int64
	for total < limit {
		length, ok := f.blocks[idx]
		if !ok || off >= length {
			break
		}
		n := length - off
		if room := limit - total; n > room {
			n = room
		}
		total += n
		if length < blockSize {
			break // 短块 = 文件最后一块，后面没有可续接的数据（同 walk）
		}
		idx++
		off = 0
	}
	return total, total == limit
}

// Stream 把 [start, start+n) 的本地块按序写进 w：一次最多读一块（4MiB），
// 不做"大内存聚合"（Twon 只有 2G 内存）。调用方应先用 Coverage 确认覆盖并
// 用 Pin 钉住文件；块缺失/读盘失败返回错误（响应已声明 Content-Length，
// 调用方只能断连，不能拼一个自相矛盾的响应）。
func (c *DiskCache) Stream(w io.Writer, fileID, identity string, start, n int64) error {
	if n <= 0 {
		return nil
	}
	if !c.Enabled() {
		return fmt.Errorf("hub 磁盘缓存未开启")
	}
	shard := c.shard(fileID)
	shard.Lock()
	defer shard.Unlock()

	f, ok := c.lockedEntry(fileID)
	if !ok {
		return fmt.Errorf("hub 磁盘缓存：文件 %s 不在缓存里", fileID)
	}
	now := c.now()
	for sent := int64(0); sent < n; {
		pos := start + sent
		idx := pos / blockSize
		off := pos % blockSize

		c.mu.Lock()
		if f.meta.Identity != identity {
			c.mu.Unlock()
			return fmt.Errorf("hub 磁盘缓存：内容身份已变化")
		}
		length, present := f.blocks[idx]
		path := c.blockPath(f.dir, idx)
		f.meta.LastAccessAt = now
		c.mu.Unlock()

		if !present || off >= length {
			return fmt.Errorf("hub 磁盘缓存：块 %d 缺失", idx)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("hub 磁盘缓存：读块 %d 失败：%w", idx, err)
		}
		if int64(len(data)) != length {
			return fmt.Errorf("hub 磁盘缓存：块 %d 长度不符（%d != %d）", idx, len(data), length)
		}
		want := length - off
		if rest := n - sent; want > rest {
			want = rest
		}
		if _, err := w.Write(data[off : off+want]); err != nil {
			return err
		}
		sent += want
	}
	return nil
}

// UsedBytes / FileCount 是统计口径（日志与测试）。
func (c *DiskCache) UsedBytes() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// FileCount 返回已登记（含扫描恢复）的文件数。
func (c *DiskCache) FileCount() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.files)
}

// removeFile 删除一个文件的整个目录并摘除索引，返回释放的字节数。
func (c *DiskCache) removeFile(fileID string) int64 {
	shard := c.shard(fileID)
	shard.Lock()
	defer shard.Unlock()

	c.mu.Lock()
	f, ok := c.files[fileID]
	if !ok {
		c.mu.Unlock()
		return 0
	}
	freed := f.used
	dir := f.dir
	c.used -= f.used
	delete(c.files, fileID)
	c.mu.Unlock()

	if err := os.RemoveAll(dir); err != nil {
		c.log.Warn("hub 磁盘缓存：删除文件目录失败", "file_id", fileID, "error", err)
	}
	return freed
}

// evictToBudget 按 last_access_at 升序淘汰整文件，直到回到预算内。
// 全部被 Pin（正在供流）时停手——宁可短暂超限，也不淘汰正在被读的字节。
func (c *DiskCache) evictToBudget() int {
	evicted := 0
	for {
		c.mu.Lock()
		if c.used <= c.cfg.BudgetBytes {
			c.mu.Unlock()
			return evicted
		}
		var victim string
		var victimAt time.Time
		for id, f := range c.files {
			if f.refs > 0 || f.used <= 0 {
				continue
			}
			if victim == "" || f.meta.LastAccessAt.Before(victimAt) {
				victim, victimAt = id, f.meta.LastAccessAt
			}
		}
		c.mu.Unlock()
		if victim == "" {
			return evicted
		}
		freed := c.removeFile(victim)
		c.log.Info("hub 磁盘缓存 LRU 淘汰", "file_id", victim, "bytes", freed)
		evicted++
	}
}

// EnforceBudget 在预算超限时立刻做一轮 LRU 淘汰（Put 之后调用；未超限是空操作）。
func (c *DiskCache) EnforceBudget() int {
	if !c.Enabled() {
		return 0
	}
	c.mu.Lock()
	over := c.used > c.cfg.BudgetBytes
	c.mu.Unlock()
	if !over {
		return 0
	}
	return c.evictToBudget()
}

// Sweep 做一轮全量维护（design §2）：重建磁盘索引（启动恢复/对齐计账）、
// 清理超龄文件（TTL）与崩溃残留、按预算 LRU 淘汰。Pin 中的文件本轮跳过。
func (c *DiskCache) Sweep() {
	if !c.Enabled() {
		return
	}
	started := c.now()

	// 1) 全量扫描：把磁盘上的目录合并进内存索引（内存里已有的条目优先——
	// 它们可能正被并发修改，磁盘快照反而是旧的）。
	metaFiles, err := filepath.Glob(filepath.Join(c.cfg.Dir, "*", "*", diskMetaName))
	if err != nil {
		c.log.Warn("hub 磁盘缓存：扫描失败", "error", err)
		return
	}
	type scanned struct {
		fileID string
		f      *diskFile
	}
	var found []scanned
	seen := make(map[string]struct{}, len(metaFiles))
	for _, metaPath := range metaFiles {
		dir := filepath.Dir(metaPath)
		fileID := ""
		if raw, err := os.ReadFile(metaPath); err == nil {
			var meta diskMeta
			if json.Unmarshal(raw, &meta) == nil {
				fileID = meta.FileID
			}
		}
		if fileID == "" {
			c.log.Warn("hub 磁盘缓存：无法从 meta.json 还原 file_id，删除该目录", "dir", dir)
			_ = os.RemoveAll(dir)
			continue
		}
		seen[fileID] = struct{}{}
		if f, ok := c.fileFor(fileID); ok {
			found = append(found, scanned{fileID: fileID, f: f})
		}
	}

	// 2) 内存里存在、磁盘上已无目录的条目：摘掉（含并发淘汰后的残留）。
	c.mu.Lock()
	var vanished []string
	for fileID := range c.files {
		if _, ok := seen[fileID]; !ok {
			vanished = append(vanished, fileID)
		}
	}
	c.mu.Unlock()
	for _, fileID := range vanished {
		shard := c.shard(fileID)
		shard.Lock()
		c.mu.Lock()
		if f, ok := c.files[fileID]; ok {
			c.used -= f.used
			delete(c.files, fileID)
		}
		c.mu.Unlock()
		shard.Unlock()
	}

	// 3) TTL：超龄文件整目录清除；Pin 中的本轮跳过（供流优先，下一轮再说）。
	removedFiles, removedBytes := 0, int64(0)
	for _, s := range found {
		c.mu.Lock()
		expired := c.expiredLocked(s.f) && s.f.refs == 0 && s.f.used > 0
		c.mu.Unlock()
		if !expired {
			continue
		}
		freed := c.removeFile(s.fileID)
		c.log.Info("hub 磁盘缓存 TTL 清理", "file_id", s.fileID, "bytes", freed)
		removedFiles++
		removedBytes += freed
	}

	// 4) 预算：LRU 淘汰到预算内（可能删除刚扫描出来的老文件）。
	evicted := c.evictToBudget()

	c.log.Info("hub 磁盘缓存清扫完成",
		"files", len(metaFiles),
		"ttl_removed", removedFiles,
		"ttl_bytes", removedBytes,
		"lru_evicted", evicted,
		"used_bytes", c.UsedBytes(),
		"duration_ms", time.Since(started).Milliseconds())
}

// firstMissingBlock 在 [from, last] 里找第一条缺失块；ok=false 表示区间齐全。
func (c *DiskCache) firstMissingBlock(fileID, identity string, size, from, last int64) (int64, bool) {
	for idx := from; idx <= last; idx++ {
		if !c.Has(fileID, identity, size, idx) {
			return idx, true
		}
	}
	return 0, false
}
