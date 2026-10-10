package agentnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/gdrive"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/https"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs/colors"
)

// hub 缓存中心接入(master 侧, 协议见任务 10-10-hub-master-integration design §2-§4)
//
// 职责边界:
//   - 注册表: role=hub 的记录由普通节点通道注册/心跳, 但【永不参与客户端调度】;
//   - 选点: hubFor 按 file_id 哈希确定性选一台健康 hub(单 hub 恒等);
//   - 预热: 浏览时把目标从"戳边缘节点"改为向 hub 下发 /warm(直链 + 凭据 + 区域集);
//   - 播放: warm 被接受时, 下发给节点的上游指向 hub 的内网口 /f/<fileID>(不带凭据)。
//
// 一切失败都回退现状(浏览回退"戳边缘", 播放回退 Google 直链):
// hub 是纯增益的加速层, 任何情况下都不得让播放或浏览不可用。

// 预热区域集默认值(与 hub 侧 WARM_HEAD_BYTES / WARM_TAIL_BYTES 的默认值一致)
const (
	// hubWarmHeadBytes 头段预热的默认字节数
	//
	// 覆盖播放器起播探测所需的头段数据(实测探测集中在文件头部)。
	hubWarmHeadBytes = 128 << 20
	// hubWarmTailBytes 尾段预热的默认字节数
	//
	// mkv/mp4 的索引(cues / moov)常在文件尾部, 播放器起播时也会拉这一小段。
	hubWarmTailBytes = 4 << 20
)

// hub 预热状态的两个边界(与预热去重表同款: 内存状态必须有界)
const (
	// hubWarmAcceptTTL "warm 已被接受"标记的保留时长
	//
	// 必须短于直链的有效期(约 1h, 取 30min 留足余量): 标记过期后播放链路会
	// 重新同步补发一次 /warm, 那次会带来一条新直链 —— hub 进程重启(内存里的
	// 直链丢失)或直链临近过期时, 由此自动痊愈而不是把节点指到一台接不住
	// 请求的 hub 上; 标记本身只是省掉重复下发的优化。
	hubWarmAcceptTTL = time.Minute * 30
	// hubWarmAcceptLimit 标记表容量上限
	hubWarmAcceptLimit = 1024
	// hubWarmFailuresLimit 失败冷却表的容量上限
	//
	// hub 台数极少, 这个上限只是"内存有界"的兜底。
	hubWarmFailuresLimit = 64
	// hubWarmCooldown 一次 warm 失败后的冷却窗口
	//
	// 播放路径上的 /warm 是同步补发(可能等满 hub-warm-timeout): 刚失败过的 hub
	// 在冷却窗口内直接跳过补发、直奔 Google 直链, 避免"hub 活着但指令不通"时
	// 每次起播都白等一个超时(回退链是硬要求: 任何新路径不得阻塞播放主流程)。
	// 浏览路径(异步, 不影响任何响应)同样受本窗口约束, 只是少一次尝试机会。
	hubWarmCooldown = time.Second * 30
)

// HubInfo 一台可用 hub 的内网入口
type HubInfo struct {
	// ID 节点 id(注册表主键)
	ID string
	// Name 展示名
	Name string
	// BaseURL hub 内网基址(http://host:hub-port)
	//
	// 数据面 /f 与控制面 /warm 共用该端口; 只对白名单(master + 节点)开放。
	BaseURL string
}

// HubRoutingEnabled hub 缓存中心接入是否生效
//
// 两个开关缺一不可: agent 网络本身启用(否则没有注册表与节点概念),
// 且 hub-enable 打开。关闭时所有 hub 路径都不参与, 行为与现状逐字一致。
func HubRoutingEnabled() bool {
	cfg := agentNetworkConfig()
	return cfg.IsEnabled() && cfg.HubEnabled()
}

// hubBaseURL 推导 hub 的内网基址(控制面与数据面共用)
//
// 只用来源 IP + master 配置的 hub-port 拼接, 刻意【不用】public_base_url:
// 那是给客户端看的对外地址(端口也未必是 hub 口), 而 master/节点与 hub 之间
// 走的是内网地址; 这也是"不新增协议字段"的实现方式。
func hubBaseURL(rec *agentRecord, hubPort int) string {
	if rec == nil {
		return ""
	}
	ip := strings.TrimSpace(rec.LastIP)
	if ip == "" {
		return ""
	}
	// JoinHostPort: IPv6 地址会自动补方括号
	return "http://" + net.JoinHostPort(ip, strconv.Itoa(hubPort))
}

// hubFileURL 拼 hub 数据面上的确定性地址: {base}/f/{fileID}
//
// fileID 是 Google Drive 的文件 id(从直链 URL 中解析), 对 hub 与 node 都是不透明的。
func hubFileURL(hub *HubInfo, fileID string) string {
	if hub == nil || hub.BaseURL == "" || strings.TrimSpace(fileID) == "" {
		return ""
	}
	return hub.BaseURL + "/f/" + url.PathEscape(fileID)
}

// hubCandidates 返回当前健康的 hub 候选集
//
// 健康 = role=hub + 管理员启用 + 心跳在 offline 窗口内 + 内网地址可推导
// (与节点调度的候选条件同源, 只有角色相反)。
//
// 排序固定为 (priority 升序, id 升序):
//   - priority 语义与节点一致(数字越小越优先, 0 = 未设置 = 最优先);
//   - 加 id 兜底是为了让"同优先级"也有全序, 选点结果因此与 map 遍历顺序无关 ——
//     同一 file_id 在任何时候(含重启后)都落到同一台 hub, warm 与播放取址一致。
func (r *registry) hubCandidates(now time.Time, offline time.Duration, hubPort int) ([]*agentRecord, error) {
	if err := r.ensureLoaded(); err != nil {
		return nil, err
	}

	r.mu.RLock()
	hubs := make([]*agentRecord, 0, len(r.records))
	for _, rec := range r.records {
		if normalizeRole(rec.Role) != RoleHub {
			continue
		}
		if !rec.Enabled {
			continue
		}
		if rec.LastSeenAt.IsZero() || now.Sub(rec.LastSeenAt) > offline {
			continue
		}
		if hubBaseURL(rec, hubPort) == "" {
			continue
		}
		hubs = append(hubs, rec.clone())
	}
	r.mu.RUnlock()

	sort.Slice(hubs, func(i, j int) bool {
		if hubs[i].Priority != hubs[j].Priority {
			return hubs[i].Priority < hubs[j].Priority
		}
		return hubs[i].ID < hubs[j].ID
	})
	return hubs, nil
}

// hubFor 为指定文件确定性选择一台 hub
//
// 没有健康 hub 时返回 (nil, nil): "没有 hub"是正常的回退场景, 不是错误
// (与 schedule 同款约定, 只有"读取注册表失败"才返回 error)。
func (r *registry) hubFor(fileID string, now time.Time, offline time.Duration, hubPort int) (*agentRecord, error) {
	hubs, err := r.hubCandidates(now, offline, hubPort)
	if err != nil {
		return nil, err
	}
	if len(hubs) == 0 {
		return nil, nil
	}

	// fnv(非加密哈希): 选点不涉及安全语义, 只要求"同输入同输出、分布均匀";
	// hash/fileID 除台数取模 —— 多 hub 时同一文件固定落到同一台(单副本缓存才有意义)。
	digest := fnv.New64a()
	_, _ = digest.Write([]byte(fileID))
	return hubs[int(digest.Sum64()%uint64(len(hubs)))], nil
}

// PickHub 为一次文件预热/播放选择一台健康 hub
//
// 返回:
//   - (*HubInfo, nil): 选中的 hub;
//   - (nil, nil):     hub 接入未启用, 或当前没有健康 hub(调用方走现状, 不必记 WARN);
//   - (nil, err):     注册表读取失败等内部故障(调用方记 WARN 后走现状)。
func PickHub(fileID string) (*HubInfo, error) {
	cfg := agentNetworkConfig()
	if !HubRoutingEnabled() {
		return nil, ErrDisabled
	}

	now := time.Now()
	rec, err := defaultRegistry.hubFor(fileID, now, time.Duration(cfg.OfflineSeconds)*time.Second, cfg.HubPort())
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, nil
	}
	base := hubBaseURL(rec, cfg.HubPort())
	if base == "" {
		return nil, nil
	}
	return &HubInfo{ID: rec.ID, Name: rec.Name, BaseURL: base}, nil
}

// driveFileIDFromDirectLink 从 Google 直链中解析 Drive 文件 id
//
// 面板给出的直链形如
// https://www.googleapis.com/drive/v3/files/<fileId>?alt=media&supportsAllDrives=true,
// 兼容 uc?export=download&id=<fileId> 形态。解析不出时返回空串,
// 调用方按"这次无法走 hub"(直连 Google)处理 —— 绝不因此失败。
func driveFileIDFromDirectLink(directURL string) string {
	u, err := url.Parse(strings.TrimSpace(directURL))
	if err != nil {
		return ""
	}

	// /files/<id> 形态: 取该段后再去掉可能的后续路径段
	if _, rest, ok := strings.Cut(u.Path, "/files/"); ok {
		if id, _, _ := strings.Cut(rest, "/"); id != "" {
			return id
		}
	}
	// 另一种常见形态: uc?export=download&id=<id>
	return strings.TrimSpace(u.Query().Get("id"))
}

// hubWarmRegions /warm 指令里的区域集
//
// resume_offset_bytes 是"尽力而为": master 侧算不出续播偏移时省略,
// hub 侧只按头段 + 尾段预热。
type hubWarmRegions struct {
	HeadBytes         int64 `json:"head_bytes"`
	TailBytes         int64 `json:"tail_bytes"`
	ResumeOffsetBytes int64 `json:"resume_offset_bytes,omitempty"`
}

// hubWarmRequest /warm 指令的载荷
//
// 字段名按任务 design §3 冻结; file_token 是为 hub 换新链(直链约 1h 过期)预留的
// 现有通道凭据: 直接沿用 download-link 的 file_id 形态, master 侧无需新端点。
// 载荷含账号级 Google 凭据(auth), 只允许发给白名单内的 hub, 绝不进日志。
type hubWarmRequest struct {
	// FileID Google Drive 文件 id(hub 数据面 /f/<fileID> 的键)
	FileID string `json:"file_id"`
	// FileToken base64url(Drive 路径), 即 download-link 接口的 file_id
	FileToken string `json:"file_token,omitempty"`
	// DirectLink 面板换取的 Google 直链
	DirectLink string `json:"direct_link"`
	// Auth 请求直链时必须带上的请求头(账号级凭据)
	Auth map[string]string `json:"auth,omitempty"`
	// Regions 区域集
	Regions hubWarmRegions `json:"regions"`
}

// hubWarmAcceptEntry 一条"warm 已被接受"标记
type hubWarmAcceptEntry struct {
	// hubID 接受该次预热的 hub(多 hub 时防止把 A 的标记用到 B 上)
	hubID string
	// at 标记时刻
	at time.Time
}

// hubWarmState hub 预热相关的小状态(接受标记 + 失败冷却)
//
// 纯内存、进程内: 不落盘(重启后最多损失一次"免补发"的机会), 容量与 TTL 有界。
type hubWarmState struct {
	mu sync.Mutex
	// accepted file_id → 标记
	accepted map[string]hubWarmAcceptEntry
	// failedAt hub_id → 最近一次失败时刻
	failedAt map[string]time.Time
}

// hubWarmStore 全局唯一的预热状态
var hubWarmStore = &hubWarmState{
	accepted: map[string]hubWarmAcceptEntry{},
	failedAt: map[string]time.Time{},
}

// markAccepted 登记一次"warm 已被接受"
func (s *hubWarmState) markAccepted(fileID, hubID string, now time.Time) {
	if fileID == "" || hubID == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 顺带裁剪过期标记(不另起后台定时器: 预热是用户浏览级别的低频动作)
	for id, entry := range s.accepted {
		if now.Sub(entry.at) >= hubWarmAcceptTTL {
			delete(s.accepted, id)
		}
	}
	// 容量兜底: 裁剪后仍然满员时淘汰最老的一条, 保证内存有界
	if len(s.accepted) >= hubWarmAcceptLimit {
		var oldestID string
		var oldestAt time.Time
		for id, entry := range s.accepted {
			if oldestID == "" || entry.at.Before(oldestAt) {
				oldestID, oldestAt = id, entry.at
			}
		}
		delete(s.accepted, oldestID)
	}
	s.accepted[fileID] = hubWarmAcceptEntry{hubID: hubID, at: now}
}

// isAccepted 判断某个文件的 warm 是否已被同一台 hub 接受且未过期
func (s *hubWarmState) isAccepted(fileID, hubID string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.accepted[fileID]
	if !ok || entry.hubID != hubID {
		return false
	}
	if now.Sub(entry.at) >= hubWarmAcceptTTL {
		delete(s.accepted, fileID)
		return false
	}
	return true
}

// inCooldown 判断一台 hub 是否在 warm 失败冷却窗口内
func (s *hubWarmState) inCooldown(hubID string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	failedAt, ok := s.failedAt[hubID]
	return ok && now.Sub(failedAt) < hubWarmCooldown
}

// recordFailure 登记一次 warm 失败
func (s *hubWarmState) recordFailure(hubID string, now time.Time) {
	if hubID == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 过期条目直接清掉: 冷却表只服务于冷却窗口, 没有长期价值
	for id, at := range s.failedAt {
		if now.Sub(at) >= hubWarmCooldown {
			delete(s.failedAt, id)
		}
	}
	if len(s.failedAt) >= hubWarmFailuresLimit {
		clear(s.failedAt)
	}
	s.failedAt[hubID] = now
}

// warmHubOnce 向 hub 下发一次 /warm 指令
//
// 只有 200 算接受(hub 侧语义: 已开始按指令取数); 其余状态码一律当失败,
// 由调用方回退现状。错误文本只含状态码与传输原因, 绝不回显请求体(含凭据)。
func warmHubOnce(ctx context.Context, hub *HubInfo, payload hubWarmRequest) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化预热指令失败: %w", err)
	}

	resp, err := https.Post(hub.BaseURL+"/warm").
		AddHeader("Content-Type", "application/json").
		Body(io.NopCloser(bytes.NewReader(body))).
		Context(ctx).
		DoSingle()
	if err != nil {
		return fmt.Errorf("请求 hub 失败: %w", err)
	}
	defer resp.Body.Close()
	// 响应体很小, 读掉一部分让连接可复用; 内容本身不需要(失败原因不取 hub 原文)
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub 返回了错误的状态码: %d", resp.StatusCode)
	}
	return nil
}

// warmHub 在冷却窗口允许时下发预热指令, 并记录成功/失败
//
// 失败只返回错误(由调用方决定回退), 同时进入冷却窗口:
// 播放路径的补发因此不会对着一台"活着但指令不通"的 hub 反复等待超时。
func warmHub(ctx context.Context, hub *HubInfo, payload hubWarmRequest, timeout time.Duration, now time.Time) error {
	if hubWarmStore.inCooldown(hub.ID, now) {
		return fmt.Errorf("hub %s(%s) 刚失败过, %v 内不再补发", hub.Name, hub.ID, hubWarmCooldown)
	}

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := warmHubOnce(callCtx, hub, payload); err != nil {
		hubWarmStore.recordFailure(hub.ID, now)
		logf(colors.Yellow, "预热: hub 失败(回退) file_id=%s, hub=%s(%s): %v", payload.FileID, hub.Name, hub.ID, err)
		return err
	}

	hubWarmStore.markAccepted(payload.FileID, hub.ID, now)
	logf(colors.Green, "预热: hub 接受 file_id=%s, hub=%s(%s)", payload.FileID, hub.Name, hub.ID)
	return nil
}

// WarmFile 尝试把一次浏览预热改向到 hub(浏览触发点的唯一入口)
//
// 结果三分:
//   - (true, nil):  hub 已接受预热指令, 调用方【不要】再戳边缘节点;
//   - (false, nil): 没有健康 hub 或 hub 接入未启用 —— 正常回退, 调用方静默走现状;
//   - (false, err): hub 路径失败(取直链失败 / 解析不出文件 id / 指令失败),
//     调用方记 WARN 后回退现状(戳边缘)。
//
// 本函数只做"下指令", 不碰媒体字节, 也不阻塞调用方的主流程(调用方在独立
// goroutine 内使用)。
func WarmFile(ctx context.Context, gdPath string) (bool, error) {
	cfg := agentNetworkConfig()
	if !HubRoutingEnabled() {
		return false, nil
	}
	if strings.TrimSpace(gdPath) == "" {
		return false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// 先看有没有可用的 hub: 一台都没有时直接回退, 连直链都不必换 ——
	// hub 不在线(或刚下线)正是回退路径, 这时浏览不应产生任何额外网络开销。
	hubs, err := defaultRegistry.hubCandidates(time.Now(), time.Duration(cfg.OfflineSeconds)*time.Second, cfg.HubPort())
	if err != nil {
		return false, err
	}
	if len(hubs) == 0 {
		return false, nil
	}

	directURL, auth, _, err := gdrive.ResolveTarget(ctx, gdPath)
	if err != nil {
		return false, fmt.Errorf("取直链失败: %w", err)
	}
	fileID := driveFileIDFromDirectLink(directURL)
	if fileID == "" {
		return false, errors.New("无法从直链解析出 Drive 文件 id")
	}

	hub, err := PickHub(fileID)
	if err != nil {
		return false, err
	}
	if hub == nil {
		// 没有健康 hub: 不记录错误, 由调用方静默回退现状
		return false, nil
	}

	payload := hubWarmRequest{
		FileID:     fileID,
		FileToken:  fileToken(gdPath),
		DirectLink: directURL,
		Auth:       auth,
		Regions: hubWarmRegions{
			HeadBytes: hubWarmHeadBytes,
			TailBytes: hubWarmTailBytes,
		},
	}
	if err := warmHub(ctx, hub, payload, cfg.HubWarmTimeout(), time.Now()); err != nil {
		return false, err
	}
	return true, nil
}

// hubUpstreamFor 决策节点的上游是否改向 hub
//
// 返回 (hub 数据面地址, true) 表示本次下发给节点的上游应指向 hub;
// 返回 false 表示走现状(Google 直链 + 凭据)。判定顺序(design §4):
//   - 接入未启用 / 解析不出 Drive 文件 id / 没有健康 hub → 不改写;
//   - 该文件的 warm 已被这台 hub 接受 → 直接改写(不再打扰 hub 与面板);
//   - 否则按"本次握手同步补发"处理: 短超时下发一次 /warm, 接受才改写,
//     失败即回退(冷却窗口内的失败会直接跳过, 不阻塞起播)。
func hubUpstreamFor(ctx context.Context, gdPath, directURL string, auth map[string]string) (string, bool) {
	cfg := agentNetworkConfig()
	if !HubRoutingEnabled() {
		return "", false
	}
	if ctx == nil {
		ctx = context.Background()
	}

	fileID := driveFileIDFromDirectLink(directURL)
	if fileID == "" {
		logf(colors.Yellow, "无法从直链解析 Drive 文件 id, 本次直连 Google: %s", gdPath)
		return "", false
	}

	hub, err := PickHub(fileID)
	if err != nil {
		logf(colors.Yellow, "hub 选点失败, 本次直连 Google: %v, 文件: %s", err, gdPath)
		return "", false
	}
	if hub == nil {
		logf(colors.Yellow, "当前无健康 hub, 本次直连 Google: %s", gdPath)
		return "", false
	}

	now := time.Now()
	if !hubWarmStore.isAccepted(fileID, hub.ID, now) {
		payload := hubWarmRequest{
			FileID:     fileID,
			FileToken:  fileToken(gdPath),
			DirectLink: directURL,
			Auth:       auth,
			Regions: hubWarmRegions{
				HeadBytes: hubWarmHeadBytes,
				TailBytes: hubWarmTailBytes,
			},
		}
		if err := warmHub(ctx, hub, payload, cfg.HubWarmTimeout(), now); err != nil {
			logf(colors.Yellow, "hub 预热未被接受, 本次直连 Google: %s", gdPath)
			return "", false
		}
	}

	// 数据面地址不带任何凭据: hub 命中时不出网, 未命中时由 hub 用自己存的直链回源
	upstream := hubFileURL(hub, fileID)
	if upstream == "" {
		return "", false
	}
	return upstream, true
}
