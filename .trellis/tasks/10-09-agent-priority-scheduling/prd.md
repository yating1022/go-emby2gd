# PRD：agent 节点优先级调度（策略可切换）

> 来源：用户 2026-10-09 需求（已口头确认理解无误后立项）。技术设计 = `design.md`；执行计划 = `implement.md`。

## 1. 目标与价值

给 agent 调度增加第二种选择策略——「**优先级模式**」：按管理员设置的优先级数字（越小越优先）
固定选点、忽略负载；仅当高优先级节点**心跳消失**才顺位下推。原「最少活跃连接」策略保留为默认。

典型场景：有一台"主力"节点（带宽/线路更好），希望所有新播放都压给它，直到它下线才用备用节点；
而不是按负载把流量摊到多台。随附需求：节点支持**自定义命名**（N8），解决当前展示注册主机名
（如 "server"）不直观的问题。

## 2. 已确认需求（用户逐条确认过理解）

- **N1** 两种策略并存、可切换：`least-active`（现状，默认）与 `priority`（新增）。
- **N2** 每个节点一个优先级整数，**越小越优先**；在「节点管理」页设置。
- **N3** 优先级模式下**完全忽略活跃连接数**：最高优先级的在线节点承接所有新播放，
  哪怕它已有多个活跃流、哪怕其它节点完全空闲。
- **N4** 仅当该节点**心跳消失**（超过 `offline-seconds` 未心跳）才顺位到第二、第三……优先级。
- **N5** 其心跳恢复后，新播放重新回到最高优先级节点；**已在播的会话不迁移**。
- **N6** 无任何可用节点时走既有回退（`fallback-to-local` / 503）——与现状一致。
- **N7** 管理员**禁用**的节点在两种策略下都不参与调度（禁用永远生效，与"心跳消失"是两回事）。
- **N8** 节点支持**自定义命名**：管理员可在「节点管理」页改名（现状显示注册主机名，如 "server"，
  不直观）；**重跑安装脚本（幂等重注册）不得覆盖**已设置的名称。

## 3. 背景事实（2026-10-09 核实，带锚点）

- 调度现状：`internal/service/agentnet/schedule.go:28` `schedule(now, offline)`——候选过滤
  （Enabled / 心跳新鲜 / 地址可推导）后取 `ActiveStreams` 最小、平局随机；唯一调用点
  `sign.go:96`（`PickAndSign`）。
- 落盘：`persist.go:38` `agentFileEntry`（snake_case，schema `version=1`——**加可选字段不需要升版本**，
  旧文件缺省按默认值处理）；内存结构 `type.go:15` `agentRecord` + `toFileEntry/fromFileEntry`。
- 变更模板：`registry.go:323` `setEnabled`（mutate + 落盘 + undo，先落盘后生效）→ `setPriority` 照此。
- 配置：`internal/config/agentnetwork.go`——**自定义 `UnmarshalYAML`（:73）带显式字段清单，
  新字段必须同步加入，否则会被静默丢弃**；枚举校验仿 emby 的 `validPeStrategy` 模式。
- 管理接口：`admin.go` 四个 handler（列表用脱敏 `agentView`）；路由表要求具体路径排在 `/agents` 之前
  （`route.go` 注释 + `route_internal_test.go` 钉住）。
- 前端：`web/src/app/routes/agent_network/`（`types.ts` 与 `agentView` 逐字段对齐；
  `components/agents_table.tsx` 原生表格 + 行操作按钮；页面 `index.tsx` 取数/原样提示模式）。
- 生产：master 在 `/dpanel/compose/ge2o-master`（9665），已接入节点「家人云」；
  幂等重注册**必须保留**管理员设置的优先级（设计里明确）。

## 4. 需求（第 6 节两个决策敲定后定稿）

- **R1 配置**：`agent-network.schedule-strategy: least-active | priority`，默认 `least-active`；
  非法值启动报错；`UnmarshalYAML` 显式清单同步；`config-example.yml` + `config_example_test.go` 同步。
- **R2 节点字段**：`priority` 整数，默认 **0（未设置 = 最优先）**，允许范围 **0–9999**（超范围拒绝）；
  落盘向后兼容（旧文件缺省 = 0 = 默认值）；**幂等重注册保留已设优先级**。
- **R3 调度**：`priority` 模式的平局规则按 Q1 决策；`least-active` 路径代码不动（回归用例保底）。
- **R4 管理接口**：新增 `POST /ge2o/agent-network/agents/edit` `{secret,id,name,priority}`——
  **全量资料更新**（名称与优先级一起提交、一起校验、一起生效，无"字段缺省"歧义；与
  update/delete 并列，注册在 `/agents` 之前）；`agentView` 增加 `priority` 字段；
  校验：名称 trim 后 1–64 字符、优先级 0–9999；失败返回中文 message。
- **R5 前端**：表格增加「优先级」列（显示）；行操作加「编辑」→ 弹窗**同时编辑名称与优先级**，
  保存走 `/agents/edit`，成功后刷新列表；沿用原样提示模式。
- **R6 测试**：schedule 单测（优先级选点 / 心跳消失下推 / 恢复回归 / 平局 / 禁用跳过 / 忽略活跃流）；
  config 单测（默认/枚举/Unmarshal 不丢字段）；persist 向后兼容（无 priority 旧文件 → 默认值）；
  admin API 单测；前端构建。
- **R7 文档**：spec `agent-network.md`（§2.4 配置、§3.6 调度）；README `agent-network` 段补一行。
- **R8 部署**：完成后重建 `ge2o:master` 镜像 + 重建容器；生产验证（给「家人云」设优先级 →
  播放走指定节点 → 停其心跳 → 顺位下推）。

## 5. 验收标准（草案）

- [ ] **A1** 单测全绿（对照基线不新增失败）：两策略行为与 N1–N7 逐条对齐（平局/下推/恢复/禁用/忽略活跃流各有用例）。
- [ ] **A2** 向后兼容：旧 `agents.json`（无 priority）加载取默认值；重注册不丢优先级。
- [ ] **A3** `schedule-strategy` 缺省时行为与现状字节级一致（least-active 路径未动 + 回归用例）。
- [ ] **A4** 前端构建通过；页面可设置优先级并立即生效（接口实测）。
- [ ] **A5** 生产部署后真实验证一次：设优先级 → 播放调度到指定节点 → 该节点心跳消失 → 顺位下推。
- [ ] **A6** 重注册（重跑安装脚本 → enroll 幂等复用）后，**自定义名称与优先级均保留**。

## 6. 决策记录（2026-10-09 已确认）

- **D1 平局规则 = 随机**：同优先级的多个在线节点中随机选一（与现状平局处理一致，同级节点均摊流量）。
- **D2 默认优先级 = 0（默认最高优先）**：新注册节点与旧文件缺省都会立即进入最高优先级圈
  （选型时已提示交互：默认 0 的新节点会与显式设 0 的节点平局随机，若想手动控制需给新节点设更大值）。
