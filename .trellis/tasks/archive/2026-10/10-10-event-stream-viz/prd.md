# 实时流转可视化（事件流 + 面板动态图）

> 用户需求（2026-10-10）：web 面板里加一个"日志可视化"——有请求时能看到数据流怎么走：客户端发起了什么、master 做了什么、大盘鸡干了什么、节点在干什么。

## Goal

把全链路的实时动作变成**结构化事件流**（不是抓日志文本），汇聚到 master，在面板上做成：

- **卡片画布（主角，可视化形态而非文字流）**：五个实体卡片 —— `客户端` / `master` / `边缘节点（卡内列 4 台）` / `大盘鸡` / `Google`，按真实拓扑连线；
- **数据在卡片间流转**：事件发生时，一枚**数据包**（带小标签的动画粒子）沿对应连线飞行、到达时目标卡片脉冲高亮并显示状态芯片（如 master 卡："307 → Vimes（上游=大盘鸡）"；大盘鸡卡："磁盘命中 · 零出网 4ms"；Google 卡在纯命中时保持灰暗"未参与"）；
- 标签用真实数据：`307`、`预热`、`Range 8.6M-`、`磁盘命中 4ms`、`206 · 25MB/s`、回退时红色虚线包 `换链 → Google 直连`。
- 实现：前端引入 `@xyflow/react`（业界标准的节点画布库，卡片节点 + 自定义动画边正是其用途；手写 SVG 效果差且多花数倍工，故接受这一前端依赖）。
- 不设大段文字流水（用户明确否掉"文字形态"）；如需排查可后续加折叠的迷你 ticker，v1 不做。

## Background

- 事件天然产生在四处：master（拦截/调度/307/预热/发链）、节点（代理请求/命中/回退）、大盘鸡（/f/ 服务/warm/让路/转全量）、客户端（由 master 侧观测推断）。
- 现有日志是人类可读文本（中英混合、含偶发多行），**不做日志解析**；改为在各产生点直接 emit 结构化事件。
- v0.4.2（直连大盘鸡）后 master 在稳态看不到数据面动作——**节点/大盘鸡必须主动上报**，事件流才完整（这也正是本功能的价值：把"看不见的稳态"可视化）。
- 已有设施可复用：agent 每 15s 心跳的认证通道、`/ge2o` 管理接口模式、web 面板的 agent_network 页面与表格组件。

## Requirements

- **N1 agent 侧（节点 + hub 同一份代码）**：
  - 进程内事件环形缓冲（默认 256 条），在既有动作点 emit：`serve`（file/range/status/bytes/dur/hit=local|mixed|upstream/upstream=hub|google）、`warm.accept`/`warm.first_block`/`warm.full_start`（hub）、`relink`（连接失败换链）、`error`；
  - **推送**：批量 POST master `/api/agent/events`（复用既有 agent 凭据认证，同心跳）；新事件到达后 ~1s 去抖批量发送；仅当最近一次心跳响应里 `events:true` 时推送（master 总开关）；
  - **红线**：事件里绝不出现凭据/签名/完整 URL；file 用短名（文件名或短 id）。
- **N2 master 侧**：
  - 事件总线（内存环形，默认 1000 条 + 单调递增 seq）；合并两类来源：自身 emit（client 请求拦截/调度/307 签发（含上游=hub|google）/预热发送/换链应答）+ 各 agent 上报；
  - 对接点 emit：redirect 分支（请求、307）、preheat（发送/接受）、download-link（应答上游）、fallback 路径；
  - 心跳响应新增 `events: <bool>`（总开关 `agent-network.events-enable`，默认开；关=agent 不推送）。
- **N3 面板（卡片画布，浅色主题——用户指定，与面板浅色底一致）**：
  - 新页面「实时流转」（agent_network 下新路由 + 导航项）：1s 轮询 `POST /ge2o/agent-network/events {secret, since_seq}`（返回增量 + 最新 seq）；
  - 画布：`@xyflow/react` 自绘五种卡片节点与连线（客户端↔master / 客户端↔节点 / master↔节点 / master↔大盘鸡 / 节点↔大盘鸡 / 大盘鸡↔Google / 节点⇢Google 仅回退虚线红）；
  - 事件→动画映射（部分）：`client.request`→客户端→master 飞"播放/浏览"；`redirect.issued`→master→客户端 飞"307·上游=X"；`preheat.*`→master→大盘鸡 飞"预热"、大盘鸡卡亮"首块就绪 439ms"；`serve(hit=local,上游=大盘鸡)`→大盘鸡→节点→客户端 连续两段飞"磁盘命中 4ms"/"206·字节流"，Google 卡灰暗；`serve(上游=Google,miss)`→大盘鸡→Google 飞"回源"、Google→大盘鸡→节点→客户端 回流"回填中"；`relink/回退`→节点→master"换链"、节点⇢Google 红色虚线"Google 直连"；
  - 卡片状态芯片：展示最近一条相关事件的短语；空闲 10s 后淡出；错误事件使卡片红闪。
- **N4 事件词典冻结**（v1）：上列 kind 及其字段，写入 spec；未知 kind 前端宽容渲染为原文。
- **N5 工程口径**：零新依赖；轮询（不引 WebSocket）；事件尽力而为（可丢，不重放、不持久化）；不影响任何既有路径性能（emit 为内存追加 + 异步 flush）。
- **N6 测试**：agent 缓冲/批量/开关行为；master 合并/seq/增量 API（含鉴权）；两模块 `-race`；web typecheck/build。

## Acceptance Criteria

- [ ] **A1** 真实播放一次：画布上按序可见——客户端→master 飞包"播放" → master→客户端"307" →（节点→大盘鸡 请求）→ 大盘鸡→节点→客户端 回流"磁盘命中/字节流"；Google 卡全程灰暗（证明未参与）；各卡片芯片显示对应短语。
- [ ] **A2** 浏览详情页：出现 `预热发送/接受 → 首块就绪` 事件。
- [ ] **A3** 停大盘鸡再播：出现 `连接失败 → 换链 → Google 直链服务` 事件链（回退可视化）。
- [ ] **A4** 关闭开关：agent 不再推送、页面显示"事件上报已关闭"；开启即恢复。
- [ ] **A5** 单测/请求计数/鉴权（错误 secret 401 一视同仁）；`-race`；不 commit。

## Out of Scope

- 历史存储与回放；日志文本解析；WebSocket；事件驱动的告警；权限分级。
