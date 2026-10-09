package agentnet

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/cryptos"
)

// DirName 状态目录名(位于配置基础路径下)
const DirName = "agent-network"

// fileName 注册表文件名
const fileName = "agents.json"

// 调度判定的哨兵错误
//
// 调用方(播放入口)必须能区分三类结果, 因为只有"没有可用节点"才适用
// fallback-to-local: 内部故障时回退本机代理并不解决问题, 但也不能让播放挂掉。
var (
	// ErrDisabled agent 代理网络未启用
	ErrDisabled = errors.New("agent 代理网络未启用")
	// ErrNoAgent 当前没有可调度的节点
	ErrNoAgent = errors.New("无可用 agent 节点")
)

// registry 节点注册表(内存态 + agents.json 持久化)
type registry struct {
	mu sync.RWMutex
	// loadedPath 已加载的注册表文件路径
	//
	// 与当前配置推导出的路径不一致时重新加载: 生产环境 BasePath 启动后不再变化,
	// 该字段让每个测试用例都能用独享的临时目录(而不需要暴露测试专用的重置接口)。
	loadedPath string
	// records 节点 id → 节点记录
	records map[string]*agentRecord
	// byMachine machine_id → 节点 id(幂等注册的索引)
	byMachine map[string]string
}

// defaultRegistry 全局唯一的注册表实例
var defaultRegistry = &registry{
	records:   map[string]*agentRecord{},
	byMachine: map[string]string{},
}

// agentNetworkConfig 获取 agent 网络配置(未初始化时为 nil)
func agentNetworkConfig() *config.AgentNetwork {
	if config.C == nil {
		return nil
	}
	return config.C.AgentNetwork
}

// stateFilePath 计算 agents.json 的路径
//
// BasePath 尚未初始化(在启动流程之外被调用)时返回错误,
// 避免把注册表悄悄写到进程当前目录, 出现"两个进程各写一份"的分裂状态。
func stateFilePath() (string, error) {
	if strings.TrimSpace(config.BasePath) == "" {
		return "", errors.New("agent 状态目录不可用: 配置基础路径尚未初始化")
	}
	return filepath.Join(config.BasePath, DirName, fileName), nil
}

// Init 启动期加载注册表(main 在配置加载完成后调用)
//
// 功能未启用时直接返回, 不触碰磁盘; 启用时读入 agents.json:
// 文件不存在视为首次启动, 解析失败返回中文错误 —— 由调用方按"启动失败"处理
// (宁可启动失败, 也不静默清空注册表, 理由见 loadAgentsFile 注释)。
func Init() error {
	if !agentNetworkConfig().IsEnabled() {
		return nil
	}
	return defaultRegistry.ensureLoaded()
}

// ensureLoaded 确保注册表已从当前配置对应的文件加载
func (r *registry) ensureLoaded() error {
	path, err := stateFilePath()
	if err != nil {
		return err
	}

	r.mu.RLock()
	upToDate := r.loadedPath == path
	r.mu.RUnlock()
	if upToDate {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loadedPath == path {
		return nil
	}

	records, err := loadAgentsFile(path)
	if err != nil {
		// 不更新 loadedPath: 用户修复文件后下一次访问会自动重试加载
		return err
	}
	r.records = records
	r.byMachine = machineIndex(records)
	r.loadedPath = path
	return nil
}

// mutate 在写锁下应用一次状态变更并落盘
//
// apply 直接修改内存表, 并返回一个用于回滚的 undo 函数;
// 落盘失败时调用 undo 恢复 —— 内存与磁盘保持一致, 且变更绝不在
// 落盘成功之前对外生效(注册/启停/删除都是"先落盘, 再响应")。
func (r *registry) mutate(apply func() func()) error {
	if err := r.ensureLoaded(); err != nil {
		return err
	}
	path, err := stateFilePath()
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	undo := apply()
	if err := r.persistLocked(path); err != nil {
		// apply 判定"目标不存在"时返回 nil(没有更改可以回滚)
		if undo != nil {
			undo()
		}
		return err
	}
	return nil
}

// enrollParams 一次注册请求的输入
type enrollParams struct {
	MachineID     string
	Hostname      string
	Version       string
	ListenPort    int
	PublicBaseURL string
	LastIP        string
	Now           time.Time
}

// enrollResult 注册成功后下发给节点的凭据
type enrollResult struct {
	AgentID string
	Secret  string
	SignKey string
	// Reused 是否复用了已有记录(幂等重新注册)
	Reused bool
	// Name 节点展示名(用于日志)
	Name string
}

// enroll 注册一个节点(以 machine_id 幂等)
//
// 同一台机器重复注册会【复用】原记录并轮换 secret / sign_key:
// 旧凭据立即失效, 节点不会攒出一堆重复条目。
func (r *registry) enroll(p enrollParams) (enrollResult, error) {
	// 凭据先生成: 随机源异常时不落盘、不响应
	secret := cryptos.RandomHex(credentialBytes)
	signKey := cryptos.RandomHex(credentialBytes)
	if secret == "" || signKey == "" {
		return enrollResult{}, errors.New("生成节点凭据失败: 系统随机源不可用")
	}

	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	name := strings.TrimSpace(p.Hostname)
	if name == "" {
		name = p.MachineID
	}

	var result enrollResult
	err := r.mutate(func() func() {
		existingID, ok := r.byMachine[p.MachineID]
		if ok {
			// 幂等复用: 换新凭据, 清零运行时状态(待下一次心跳重新探测)
			rec := r.records[existingID]
			prev := rec.clone()
			rec.Secret = secret
			rec.SignKey = signKey
			rec.Name = name
			rec.PublicBaseURL = p.PublicBaseURL
			rec.ListenPort = p.ListenPort
			rec.Version = p.Version
			rec.LastIP = p.LastIP
			rec.LastSeenAt = time.Time{}
			rec.ActiveStreams = 0
			rec.UpdatedAt = now
			result = enrollResult{AgentID: rec.ID, Secret: secret, SignKey: signKey, Reused: true, Name: rec.Name}
			return func() { *rec = *prev }
		}

		id := cryptos.RandomHex(agentIDBytes)
		if id == "" {
			// 理论不可达(enroll 开头已用过随机源), 保留兜底
			id = fmt.Sprintf("agent-%d", now.UnixNano())
		}
		rec := &agentRecord{
			ID:            id,
			MachineID:     p.MachineID,
			Name:          name,
			Secret:        secret,
			SignKey:       signKey,
			PublicBaseURL: p.PublicBaseURL,
			ListenPort:    p.ListenPort,
			Version:       p.Version,
			LastIP:        p.LastIP,
			Enabled:       true,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		r.records[id] = rec
		r.byMachine[p.MachineID] = id
		result = enrollResult{AgentID: id, Secret: secret, SignKey: signKey, Name: name}
		return func() {
			delete(r.records, id)
			delete(r.byMachine, p.MachineID)
		}
	})
	if err != nil {
		return enrollResult{}, err
	}
	return result, nil
}

// authenticate 校验节点凭据
//
// 节点不存在时也走一次常数时间比较(与一枚固定长度的假凭据比),
// 避免"先看节点在不在、再比凭据"泄漏节点是否存在;
// 无论何种失败原因都只返回 nil, 不给出任何细节。
func (r *registry) authenticate(id, secret string) (*agentRecord, error) {
	if err := r.ensureLoaded(); err != nil {
		return nil, err
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	rec, ok := r.records[id]
	candidate := dummyAgentSecret
	if ok {
		candidate = rec.Secret
	}
	if !cryptos.Equal(secret, candidate) || !ok {
		return nil, nil
	}
	return rec.clone(), nil
}

// heartbeatParams 一次心跳上报的输入
type heartbeatParams struct {
	// LastIP 来源 IP, 由 master 侧记录
	LastIP string
	// ActiveStreams 节点上报的活跃流数
	ActiveStreams int
	// Version 节点自报版本, 空串不覆盖
	Version string
	// ListenPort 节点监听端口, <=0 不覆盖
	ListenPort int
	// PublicBaseURL 节点对外地址, 空串不覆盖
	PublicBaseURL string
	// Now 心跳时间
	Now time.Time
}

// touch 刷新心跳
//
// 只更新内存, 不落盘: 心跳每 15 秒一次, 每次都写盘既无必要也会磨损磁盘。
// 代价是重启后所有节点都要重新心跳一次才回到调度池, 这正是期望行为。
func (r *registry) touch(id string, p heartbeatParams) (*agentRecord, error) {
	if err := r.ensureLoaded(); err != nil {
		return nil, err
	}

	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.records[id]
	if !ok {
		return nil, nil
	}
	rec.LastSeenAt = now
	rec.ActiveStreams = p.ActiveStreams
	// 非空才覆盖: 来源 IP 拿不到时(非常规连接)不能把已知地址清空,
	// 那会让节点直到下次心跳前都不可调度
	if strings.TrimSpace(p.LastIP) != "" {
		rec.LastIP = strings.TrimSpace(p.LastIP)
	}
	if strings.TrimSpace(p.Version) != "" {
		rec.Version = strings.TrimSpace(p.Version)
	}
	// 非空才覆盖: agent 侧这两个字段可空(如未配置 public_base_url),
	// 不能因为一次"没带"就把已知的地址清空
	if p.ListenPort > 0 {
		rec.ListenPort = p.ListenPort
	}
	if strings.TrimSpace(p.PublicBaseURL) != "" {
		rec.PublicBaseURL = strings.TrimSpace(p.PublicBaseURL)
	}
	return rec.clone(), nil
}

// setEnabled 启用 / 禁用节点并落盘
func (r *registry) setEnabled(id string, enabled bool, now time.Time) (*agentRecord, error) {
	if now.IsZero() {
		now = time.Now()
	}

	var updated *agentRecord
	err := r.mutate(func() func() {
		rec, ok := r.records[id]
		if !ok {
			return nil
		}
		prev := rec.clone()
		rec.Enabled = enabled
		rec.UpdatedAt = now
		updated = rec.clone()
		return func() { *rec = *prev }
	})
	if err != nil {
		return nil, err
	}
	if updated == nil {
		return nil, errAgentNotFound
	}
	return updated, nil
}

// remove 删除节点并落盘
//
// 删除即吊销: 节点记录的 secret 与 sign_key 一起消失, 节点凭据立即失效;
// 已签发的客户端 URL 也随之失效(按各自节点密钥验签)。
func (r *registry) remove(id string) (*agentRecord, error) {
	var removed *agentRecord
	err := r.mutate(func() func() {
		rec, ok := r.records[id]
		if !ok {
			return nil
		}
		removed = rec.clone()
		machineID := rec.MachineID
		delete(r.records, id)
		if r.byMachine[machineID] == id {
			delete(r.byMachine, machineID)
		}
		return func() {
			r.records[id] = rec
			r.byMachine[machineID] = id
		}
	})
	if err != nil {
		return nil, err
	}
	if removed == nil {
		return nil, errAgentNotFound
	}
	return removed, nil
}

// snapshot 返回全部节点记录(按注册时间从新到旧)
func (r *registry) snapshot() ([]*agentRecord, error) {
	if err := r.ensureLoaded(); err != nil {
		return nil, err
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	list := make([]*agentRecord, 0, len(r.records))
	for _, rec := range r.records {
		list = append(list, rec.clone())
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].CreatedAt.After(list[j].CreatedAt)
		}
		return list[i].ID < list[j].ID
	})
	return list, nil
}

// errAgentNotFound 目标节点不存在
var errAgentNotFound = errors.New("节点不存在")

// dummyAgentSecret 节点不存在时用于常数时间比较的假凭据
//
// 长度与真实凭据一致(32 字节 → 64 位 hex), 保证"节点不存在"与
// "凭据错误"两条路径的耗时量级相同。
var dummyAgentSecret = strings.Repeat("0", credentialBytes*2)
