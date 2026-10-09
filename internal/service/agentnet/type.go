package agentnet

import "time"

// credentialBytes 节点凭据的随机字节数
//
// 与协议约定一致(master 侧 secrets.token_hex(32) 的 64 位 hex):
// agent 侧把 sign_key 解码为 32 字节的 HMAC 密钥。
const credentialBytes = 32

// agentIDBytes 节点 id 的随机字节数
const agentIDBytes = 16

// agentRecord 一个已注册节点在内存中的完整状态
type agentRecord struct {
	// ID 节点 id, 协议里的 agent_id, 对外可见
	ID string
	// MachineID 幂等注册键(取自 /etc/machine-id, 退化为主机名)
	MachineID string
	// Name 展示名(注册时的 hostname)
	Name string
	// Secret 心跳与直链拉取的 Bearer 凭据, 属敏感信息, 不得进日志
	Secret string
	// SignKey 客户端 URL 签名密钥(hex), 属敏感信息, 不得进日志
	SignKey string
	// PublicBaseURL 节点上报的对外地址, 可空(为空时按来源 IP 推导)
	PublicBaseURL string
	// ListenPort 节点数据面监听端口
	ListenPort int
	// Version 节点自报版本
	Version string
	// LastSeenAt 最近一次心跳时间, 零值表示从未心跳
	//
	// 【不落盘】: 重启后全部节点都需要一次心跳才回到调度池,
	// 避免"文件里的陈旧心跳"让已经下线的节点继续被调度。
	LastSeenAt time.Time
	// LastIP 最近一次心跳的来源 IP(由 master 侧记录, 不采信节点自报)
	LastIP string
	// ActiveStreams 节点上报的活跃流数, 用于最少连接调度
	//
	// 【不落盘】: 与 LastSeenAt 同理, 属于易失的运行时状态。
	ActiveStreams int
	// Enabled 是否允许被调度(管理员可禁用)
	Enabled bool
	// CreatedAt 首次注册时间
	CreatedAt time.Time
	// UpdatedAt 最近一次记录变更时间(注册 / 启停, 心跳不算)
	UpdatedAt time.Time
}

// clone 复制一份记录, 避免调用方在锁外持有可变引用
func (r *agentRecord) clone() *agentRecord {
	c := *r
	return &c
}

// toFileEntry 转换为落盘结构
//
// 易失字段(LastSeenAt / ActiveStreams)不落盘。
func (r *agentRecord) toFileEntry() agentFileEntry {
	return agentFileEntry{
		ID:            r.ID,
		MachineID:     r.MachineID,
		Name:          r.Name,
		Secret:        r.Secret,
		SignKey:       r.SignKey,
		PublicBaseURL: r.PublicBaseURL,
		ListenPort:    r.ListenPort,
		Version:       r.Version,
		LastIP:        r.LastIP,
		Enabled:       r.Enabled,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}
}

// agentRecordFromFileEntry 从落盘结构还原记录
func agentRecordFromFileEntry(e agentFileEntry) *agentRecord {
	return &agentRecord{
		ID:            e.ID,
		MachineID:     e.MachineID,
		Name:          e.Name,
		Secret:        e.Secret,
		SignKey:       e.SignKey,
		PublicBaseURL: e.PublicBaseURL,
		ListenPort:    e.ListenPort,
		Version:       e.Version,
		LastIP:        e.LastIP,
		Enabled:       e.Enabled,
		CreatedAt:     e.CreatedAt,
		UpdatedAt:     e.UpdatedAt,
	}
}
