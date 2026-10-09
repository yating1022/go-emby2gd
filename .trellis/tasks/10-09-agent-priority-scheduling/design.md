# design：agent 节点优先级调度（策略可切换）

> 需求 = 同目录 `prd.md`（N1–N7、D1/D2）；协议契约 = `.trellis/spec/backend/agent-network.md`
> （本功能只动调度与配置/管理面，**不动 agent 侧与线协议**）。

## 1. 配置（`internal/config/agentnetwork.go`）

```yaml
agent-network:
  schedule-strategy: least-active   # least-active(默认, 现状) | priority
```

- 常量（config 包导出，供 agentnet 比较）：`ScheduleStrategyLeastActive = "least-active"`、
  `ScheduleStrategyPriority = "priority"`。
- **⚠ `UnmarshalYAML` 的 `plainAgentNetwork` 是显式字段清单，必须同步加入
  `ScheduleStrategy string \`yaml:"schedule-strategy"\``**——漏加会被静默丢弃（单测钉住）。
- `Init`：trim 首尾空白；缺省 → `least-active`；非法值报中文错误并列出合法值
  （与 emby 枚举校验同款，与 enable 无关地提前校验）。
- getter：`ScheduleStrategy() string`（nil 接收者返回默认值，与其他 getter 一致）。
- `config-example.yml` 加注释行；`config_example_test.go` + `agentnetwork_test.go` 加断言。

## 2. 节点字段与持久化

- `agentRecord`（type.go）新增 `Priority int`（默认 0；注释写明语义"越小越优先"）。
- `agentFileEntry` 新增 `Priority int \`json:"priority,omitempty"\``：
  **不需要指针**——省略场外值恰好 0，而 0 就是默认值；旧文件缺省 → 0 → 语义正确。
- `toFileEntry`/`agentRecordFromFileEntry` 双向带上该字段。
- `enroll`（registry.go:169）：**新建**记录置 `Priority: 0`、`Name` = 上报主机名；幂等**复用**分支
  **不得触碰 Priority 与 Name**（管理员状态，重注册只轮换凭据）——⚠ 这是**行为变更**：
  现状 `registry.go:195` 会把 `Name` 覆盖为主机名，改名功能要求删除该行（否则每次升级重跑
  安装脚本就把自定义名擦掉）。
- `setProfile(id string, name string, priority int, now time.Time) (*agentRecord, error)`：一次 mutate
  同时更新 Name 与 Priority、一次落盘（仿 `setEnabled`：undo + `errAgentNotFound`）；校验放 admin 层。

## 3. 调度（`internal/service/agentnet/schedule.go`）

```go
func (r *registry) schedule(now time.Time, offline time.Duration, strategy string) (*agentRecord, error)
```

- 候选过滤**不变**（Enabled / 心跳新鲜 / 地址可推导）——N4/N5/N7 由该过滤自然满足：
  离线即出池（下推）、恢复即回池（回归）、禁用永远出池。
- 选点分派：
  - `least-active`（默认）：与现状逐字一致（`ActiveStreams` 最小 → 平局随机）；
  - `priority`：`Priority` **最小** → 平局随机（D1）——**完全忽略 `ActiveStreams`**（N3）。
- 实现：抽一个 `minKey` 工具（key 函数 + 平局随机）供两条分支复用，保持"最小 → 并列 → 随机"
  同一形状；`ActiveStreams` 的上报/展示/日志不变。
- 调用点：`sign.go:96` `PickAndSign` 传入 `cfg.ScheduleStrategy()`；调度日志在 priority 模式下
  追加优先级数字（活跃流照常打印，便于对比）。

## 4. 管理接口（`admin.go` + `constant.go` + `route.go`）

- 新常量 `Route_AgentNetworkAgentsEdit = Route_SelfBase + "/agent-network/agents/edit"`；
  规则行**必须排在 `/agents` 之前**（与 `/agents/update`、`/agents/delete` 同级），
  `route_internal_test.go` 的 `agentRules` map 与顺序断言同步加一条。
- 新 handler `AdminEditAgent`：`{secret, id, name, priority}`，**全量资料更新**（两字段一起校验）；
  校验：id 非空（「缺少节点 id」）；name trim 后 1–64 字符（「节点名称不能为空」/
  「节点名称过长(最多 64 字符)」，按 rune 计数）；priority ∈ [0,9999]（「优先级必须是 0-9999 的整数」）；
  节点不存在 → 「节点不存在」（复用 errAgentNotFound 分支）；成功 → 「更新成功」。
- `agentView` 新增 `Priority int \`json:"priority"\``（脱敏视图无凭据，字段照加）。

## 5. 前端（`web/src/app/routes/agent_network/`）

- `types.ts`：`AgentView` += `priority: number`（与 agentView 逐字段对齐的注释保持成立）。
- `components/agents_table.tsx`：
  - 表头在「状态」后插入「**优先级**」列（显示数字；0 照常显示）；
  - 行操作区新增「编辑」按钮 → 小弹窗（shadcn Dialog）：**名称输入（文本）+ 优先级输入
    （数字 min=0）**，预填当前值，保存/取消；回调走页面传入的 `onEdit`。
- `index.tsx`：新增 `editAgent(agent, name, priority)` 处理器——照既有模式（secret 缺失提示 /
  `fetch POST /agents/edit` → `!res.success` 时 `message` 原样 toast / 成功后刷新列表）；
  在 `actingID` 行操作期间禁用按钮的既有语义保持。
- 表格排序维持现状（不按优先级重排，避免引入额外复杂度；如需后续再加）。

## 6. 兼容、测试与部署

- **向后兼容**：旧 `agents.json`（无 priority 字段）→ 0 → 行为 = 默认；旧配置（无
  schedule-strategy）→ least-active → 行为与现状字节级一致（A3 回归用例钉住）。
- **测试清单**（对应 PRD R6）：
  - `schedule_internal_test.go`：priority 选点；**顶层心跳消失 → 次优先接管**；心跳恢复 → 回归；
    同优先级平局（构造多节点，多次调用覆盖随机池）；禁用跳过；priority 模式下
    ActiveStreams 不影响选点（构造活跃流悬殊的两节点断言仍选优先级高者）；
    least-active 回归（既有用例原样通过）。
  - `registry_internal_test.go` / `persist` 侧：setProfile 落盘 + 重启加载保留（name 与 priority 均验）；
    **无 priority 的旧文件加载 → 0**；有值文件加载 → 保留；**re-enroll 不重置名称与优先级**。
  - `config/agentnetwork_test.go`：缺省 / 非法值报错 / UnmarshalYAML 不丢字段。
  - `admin_internal_test.go`：edit 全分支（name 空/超 64 字符、priority 0/9999 边界、-1 与 10000 拒绝、
    节点不存在、secret 错误）。
  - `route_internal_test.go`：priority 路由排在 `/agents` 之前。
  - 前端：`./build_web.sh` 构建通过。
- **E2E 不加新用例**（谨慎范围）：选点逻辑已由 master 侧单测穷举覆盖；真机验证放在生产
  （A5，见 implement.md §部署）。如后续需要，可在 `internal/e2e/` 加"双节点"用例（现状 harness
  只起一个 agent 子进程，扩展成本另计）。
- **文档**：`.trellis/spec/backend/agent-network.md` §2.4 配置表 + §3.6 调度（两策略、默认 0、
  平局随机、忽略活跃流）；README `agent-network` 段 yaml 片段 + 一句话说明。
- **部署（R8）**：`sudo docker build -t ge2o:master /home/debian/project/go-emby2openlist` →
  `cd /dpanel/compose/ge2o-master && sudo docker compose up -d --force-recreate`；
  配置里把 `schedule-strategy: priority` 打开（用户可切回 least-active）。
