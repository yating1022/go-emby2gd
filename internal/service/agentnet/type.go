package agentnet

import "time"

// credentialBytes 节点凭据的随机字节数
//
// 与协议约定一致(master 侧 secrets.token_hex(32) 的 64 位 hex):
// agent 侧把 sign_key 解码为 32 字节的 HMAC 密钥。
const credentialBytes = 32

// agentIDBytes 节点 id 的随机字节数
const agentIDBytes = 16

// 节点角色取值(enroll 请求里的 role 字段, 也是注册表落盘的取值)
//
// 两种角色跑的是同一个 agent 二进制, 差别在职责:
//   - node: 客户端数据面中继, 参与客户端调度;
//   - hub:  缓存中心(全网红源点), **永不参与客户端调度**, 只接受 master 的
//     预热指令与白名单内节点的回源拉流。
const (
	// RoleNode 客户端服务节点(缺省角色)
	RoleNode = "node"
	// RoleHub 缓存中心
	RoleHub = "hub"
)

// validAgentRoles 合法的 role 取值(用于 enroll 请求校验)
var validAgentRoles = map[string]struct{}{RoleNode: {}, RoleHub: {}}

// normalizeRole 归一化角色取值
//
// 旧版 agents.json(以及 v0.3.2 节点的 enroll 请求)没有 role 字段,
// 一律按 node 处理 —— 这一条是向后兼容的底线: 读旧文件绝不能报错,
// 更不能让旧节点凭空获得/失去 hub 身份。
func normalizeRole(role string) string {
	if role == RoleHub {
		return RoleHub
	}
	return RoleNode
}

// agentRecord 一个已注册节点在内存中的完整状态
type agentRecord struct {
	// ID 节点 id, 协议里的 agent_id, 对外可见
	ID string
	// MachineID 幂等注册键(取自 /etc/machine-id, 退化为主机名)
	MachineID string
	// Name 展示名: 新建记录时取上报主机名, 之后由管理员在节点管理页维护
	//
	// 【落盘】, 幂等重注册(升级重跑安装脚本)不覆盖。
	Name string
	// Secret 心跳与直链拉取的 Bearer 凭据, 属敏感信息, 不得进日志
	Secret string
	// SignKey 客户端 URL 签名密钥(hex), 属敏感信息, 不得进日志
	SignKey string
	// Role 节点角色(node / hub)
	//
	// 【落盘】, 但属于"部署上报"而不是管理员状态: 重注册(hub/node 换代)
	// 时按请求里的 role 覆盖 —— 与 Name / Priority 的语义不同, 后者不覆盖。
	Role string
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
	// Priority 调度优先级: 数字越小越优先
	//
	// 仅 priority 调度策略下参与选点(该策略完全忽略 ActiveStreams);
	// 默认 0, 即"未设置 = 最优先"。由管理员在节点管理页设置, 【落盘】,
	// 重注册(幂等复用)不得覆盖。
	Priority int
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
	// 只有 hub 才写 role: node 是缺省角色, 不写才能保证 node 注册表的
	// 落盘内容与新增 role 字段之前逐字节一致(omitempty)。
	role := ""
	if normalizeRole(r.Role) == RoleHub {
		role = RoleHub
	}

	return agentFileEntry{
		ID:            r.ID,
		MachineID:     r.MachineID,
		Name:          r.Name,
		Secret:        r.Secret,
		SignKey:       r.SignKey,
		Role:          role,
		PublicBaseURL: r.PublicBaseURL,
		ListenPort:    r.ListenPort,
		Version:       r.Version,
		LastIP:        r.LastIP,
		Priority:      r.Priority,
		Enabled:       r.Enabled,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}
}

// agentRecordFromFileEntry 从落盘结构还原记录
func agentRecordFromFileEntry(e agentFileEntry) *agentRecord {
	return &agentRecord{
		ID:        e.ID,
		MachineID: e.MachineID,
		Name:      e.Name,
		Secret:    e.Secret,
		SignKey:   e.SignKey,
		// 旧文件没有 role 字段: 按缺省角色 node 载入
		Role:          normalizeRole(e.Role),
		PublicBaseURL: e.PublicBaseURL,
		ListenPort:    e.ListenPort,
		Version:       e.Version,
		LastIP:        e.LastIP,
		Priority:      e.Priority,
		Enabled:       e.Enabled,
		CreatedAt:     e.CreatedAt,
		UpdatedAt:     e.UpdatedAt,
	}
}
