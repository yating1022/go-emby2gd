package emby

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/agentnet"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/gdrive"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/https"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs"

	"github.com/gin-gonic/gin"
)

// 网关预热: 在用户"即将播放"前(浏览详情页 / 请求 PlaybackInfo 的时刻)异步
// 让节点把首触预取提前做完 —— 真正点播放时头尾数据已在节点本地, 起播接近秒开。
//
// 三条硬性约束, 改动时必须同时满足:
//   - 绝不阻塞、绝不影响被拦截请求的响应与耗时: 只做一次极小的同步数据提取,
//     其余全部在独立 goroutine 内完成;
//   - 一切失败静默降级(记 WARN 后忽略), 绝不影响任何主流程;
//   - 日志不含令牌与签名参数(协议地址只在进程内使用, 见 agent-network.md §3.7)。
const (
	// preheatRange 预热请求携带的 Range 头: 只命中文件头部一小段
	//
	// 与起播需要的数据量同级: 节点侧读前缓存的"首触预取"由这次请求触发。
	preheatRange = "bytes=0-65535"

	// preheatTimeout 预热请求的总超时
	//
	// 预热是尽力而为的后台动作: 超时即放弃, 不重试、不排队。
	preheatTimeout = time.Second * 8

	// preheatDedupTTL 同一文件的最短预热间隔
	preheatDedupTTL = time.Minute * 10

	// preheatDedupLimit 去重表容量上限
	preheatDedupLimit = 1024
)

// preheatDedup 预热去重表: gdPath -> 最近一次触发时刻
//
// 只保留触发记录(不落盘、不随注册表持久化): 进程重启后最多多预热一次,
// 代价可忽略, 对正确性无影响。
var preheatDedup = newPreheatRecords()

// preheatWaitGroup 允许等待所有进行中的预热结束
//
// 与 web/cache 的 cacheHandleWaitGroup 同一种做法: 预热本身是"发射后不管"的
// 后台动作, 生产路径不需要等待; 但预热 goroutine 会读取全局配置, 用例在还原
// 全局配置前必须能确定性地等到它们退出, 否则那个读与还原的写会被 -race 判成
// 数据竞争(goroutine 正常退出本身不构成同步边)。
var preheatWaitGroup = sync.WaitGroup{}

// WaitingPreheat 等待所有进行中的预热结束
func WaitingPreheat() {
	preheatWaitGroup.Wait()
}

// preheatRecords 带 TTL 与容量上限的去重表
//
// 仅由预热路径读写, 用互斥锁保护; 去重表必须内存有界
// (本项目内存状态约定, 见 spec database-guidelines.md)。
type preheatRecords struct {
	mu      sync.Mutex
	records map[string]time.Time
}

// newPreheatRecords 构造一个空的去重表
func newPreheatRecords() *preheatRecords {
	return &preheatRecords{records: make(map[string]time.Time)}
}

// markFired 判断指定路径是否允许触发, 允许时登记触发时刻
//
// 返回 false 表示 TTL 内已经触发过, 本次跳过。
//
// 登记发生在"决定打枪"的时刻而不是请求成功之后: 详情页的一次浏览可能同时命中
// PlaybackInfo 与 Items 两个挂点, 先登记才能把并发的重复触发挡在签名之前;
// 代价是失败的尝试也要等满 TTL 才会再试, 对纯增益的预热来说是可接受的保守取舍。
func (p *preheatRecords) markFired(gdPath string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 1 顺带裁剪过期记录: 表里只保留 TTL 内的条目
	//
	// 不另起后台定时器: 预热是用户浏览级别的低频动作, 触发时顺带裁剪足够,
	// 也更符合本项目"不为小状态引入常驻协程"的惯例。
	for path, firedAt := range p.records {
		if now.Sub(firedAt) >= preheatDedupTTL {
			delete(p.records, path)
		}
	}

	// 2 TTL 内重复触发: 跳过(不打日志, 属于正常浏览行为)
	if firedAt, ok := p.records[gdPath]; ok && now.Sub(firedAt) < preheatDedupTTL {
		return false
	}

	// 3 容量兜底: 裁剪后仍然满员时淘汰最老的一条, 保证内存有界
	if len(p.records) >= preheatDedupLimit {
		p.evictOldest()
	}

	p.records[gdPath] = now
	return true
}

// evictOldest 淘汰最老的一条记录
//
// 调用方必须已持有锁。
func (p *preheatRecords) evictOldest() {
	var oldestPath string
	var oldestAt time.Time
	for path, firedAt := range p.records {
		if oldestPath == "" || firedAt.Before(oldestAt) {
			oldestPath, oldestAt = path, firedAt
		}
	}
	if oldestPath != "" {
		delete(p.records, oldestPath)
	}
}

// TryFire 异步预热一个条目: 条目 → Drive 逻辑路径 → 节点签名地址 → 小 Range 打一枪
//
// 不阻塞、不返回错误; 一切失败只记 WARN 后忽略, 绝不影响调用方 handler 的响应。
// itemId 由调用方按各自 handler 内既有的方式解析后给出(两个挂点的 id 提取规则不同),
// 传入空值时直接跳过。
//
// 函数返回前只做两件同步的事: 判守卫、提取后续异步阶段需要的最小请求信息 ——
// gin.Context 会在本次请求结束后被回收复用, goroutine 内不得再触碰它。
func TryFire(c *gin.Context, itemId string) {
	// 1 守卫: 功能未启用时零开销返回(不解析、不打日志、不发起任何请求)
	if !preheatReady() {
		return
	}

	// 2 同步提取条目信息(只读请求元数据, 不发网络请求)
	itemInfo, ok := preheatItemInfo(c, itemId)
	if !ok {
		return
	}

	// 3 异步预热: 登记到等待组后另起 goroutine, 主流程立即返回
	preheatWaitGroup.Add(1)
	go func() {
		defer preheatWaitGroup.Done()
		firePreheat(itemInfo)
	}()
}

// preheatReady 判断网关预热所需的开关是否全部打开
//
// 三个条件缺一不可: agent 网络启用(否则没有可调度的节点)、预热开关打开、
// gdrive 启用(路径来源与 MatchMountPath 都属于该功能)。
func preheatReady() bool {
	if config.C == nil {
		return false
	}
	return config.C.AgentNetwork.IsEnabled() &&
		config.C.AgentNetwork.PreheatEnabled() &&
		gdrive.IsEnabled()
}

// preheatItemInfo 由条目 id 与当前请求构造预热所需的最小条目信息
//
// 这里不直接用 resolveItemInfo: 它的 itemId 提取强依赖 RouteType(单条详情取路径
// 末段, PlaybackInfo 取父目录名), 两个挂点的路由类型不同, 由调用方给出更直接。
// uri 的构造复用 buildPlaybackInfoUri, 与播放链路保持一致。
//
// 预热只取条目的首个媒体源(不指定 MediaSourceId): 预热的目标是把"这个文件"的
// 首触数据提前拉取, 多版本条目预热到哪一份只是收益差异, 不会带来副作用。
func preheatItemInfo(c *gin.Context, itemId string) (ItemInfo, bool) {
	if c == nil || c.Request == nil {
		return ItemInfo{}, false
	}

	itemId = strings.TrimSpace(itemId)
	if itemId == "" {
		return ItemInfo{}, false
	}

	itemInfo := ItemInfo{Id: itemId, RouteType: RoutePlaybackInfo}
	itemInfo.ApiKeyType, itemInfo.ApiKeyName, itemInfo.ApiKey = getApiKey(c)

	uri, err := buildPlaybackInfoUri(itemId, itemInfo.ApiKeyType, itemInfo.ApiKeyName, itemInfo.ApiKey, MsInfo{Empty: true})
	if err != nil {
		return ItemInfo{}, false
	}
	itemInfo.PlaybackInfoUri = uri

	return itemInfo, true
}

// firePreheat 预热主流程(在独立 goroutine 内执行)
//
// 全程不依赖 gin.Context; 任何一步失败都只记日志, 绝不 panic、绝不重试。
func firePreheat(itemInfo ItemInfo) {
	// 1 解析条目在 Drive 中的逻辑路径(与播放链路同一个取路径口)
	embyPath, err := getEmbyFileLocalPath(itemInfo)
	if err != nil {
		logs.Warn("[网关预热] 获取条目路径失败, 跳过: %v", err)
		return
	}
	gdPath, ok := gdrive.MatchMountPath(embyPath)
	if !ok {
		// 非 Google Drive 挂载路径: 绝大多数条目都属于这种正常情况, 不打日志
		return
	}

	// 2 去重: TTL 内同一个文件只预热一次
	if !preheatDedup.markFired(gdPath, time.Now()) {
		return
	}

	// 3 选点并签发地址(与播放入口同一条调度路径; 日志只含节点与文件, 不含签名)
	signedURL, err := agentnet.PickAndSign(gdPath)
	if err != nil {
		logs.Warn("[网关预热] 无可用节点或签发失败, 跳过: %v, 文件: %s", err, gdPath)
		return
	}

	// 4 小 Range 打一枪, 触发节点侧读前缓存的首触预取
	if err := preheatRequest(signedURL); err != nil {
		logs.Warn("[网关预热] 预热请求失败: %v, 文件: %s", err, gdPath)
		return
	}
	logs.Info("[网关预热] 已触发: %s", gdPath)
}

// preheatRequest 向节点上的签名地址发一次小 Range 请求
//
// 只读取并丢弃响应体的一小段: 目的是让节点把首触预取的请求打出去,
// 而不是由本进程搬运数据。签名地址含 s 参数, 不得写进日志(失败原因已足够定位)。
func preheatRequest(signedURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), preheatTimeout)
	defer cancel()

	resp, err := https.Get(signedURL).
		Header(http.Header{"Range": []string{preheatRange}}).
		Context(ctx).
		DoSingle()
	if err != nil {
		return fmt.Errorf("请求节点失败: %s", redactSignedURL(err, signedURL))
	}
	defer resp.Body.Close()

	// 200 表示节点忽略了 Range 直接返回整份数据, 同样算预热成功
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("节点返回了错误的响应码: %d", resp.StatusCode)
	}

	// 丢弃响应体: 节点已经收到并处理了这次 Range 请求, 数据不必留在本进程
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024)); err != nil {
		return fmt.Errorf("读取预热响应失败: %v", err)
	}
	return nil
}

// redactSignedURL 从错误文本中抹掉完整签名地址
//
// net/http 的传输错误会把请求地址原样写进错误文本, 例如
// `Get "http://node:8790/dl/xx?e=173...&s=ab..": dial tcp ...` —— 地址里的 s
// 是节点侧验签用的凭据; 而"节点不可达 / 超时"恰恰是这条错误最常见的场景,
// 直接回显就等于把签名写进日志(agent-network.md §3.7: 日志不得含 s 或完整
// 签名地址)。这里按地址原样替换, 只把失败原因留给日志。
func redactSignedURL(err error, signedURL string) string {
	if err == nil {
		return ""
	}
	return strings.ReplaceAll(err.Error(), signedURL, "<签名地址>")
}
