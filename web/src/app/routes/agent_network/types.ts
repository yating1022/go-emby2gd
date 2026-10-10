// 与网关后端 internal/service/agentnet/admin.go 的 agentView 逐字段对齐,
// 字段名不得改动(后端 JSON tag 即此处键名)。

/** 节点视图(已脱敏: 不含 secret / sign_key 等凭据) */
export type AgentView = {
  id: string;
  name: string;
  /** 角色: "hub"=缓存中心(从 Google 拉流并缓存, 不参与客户端调度) / "node"=边缘节点(面向客户端透传) */
  role: string;
  machine_id: string;
  enabled: boolean;
  online: boolean;
  /** 调度优先级: 数字越小越优先, 0 = 默认(最优先) */
  priority: number;
  version: string;
  /** RFC3339 UTC 时间串, 从未心跳时为空串 */
  last_seen_at: string;
  last_ip: string;
  active_streams: number;
  public_base_url: string;
  address: string;
  listen_port: number;
  created_at: string;
  updated_at: string;
};

/** /ge2o 管理接口的恒 200 信封(失败原因在 message 里, 前端按 message 提示) */
export type AdminResponse<T = unknown> = {
  success: boolean;
  message: string;
  data?: T;
};

/** POST /ge2o/agent-network/agents 的 data */
export type AgentsData = {
  agents: AgentView[];
};

/** POST /ge2o/agent-network/install-command 的 data */
export type InstallCommandData = {
  command: string;
  master_url: string;
};
