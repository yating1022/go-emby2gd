# implement：实时流转可视化 顺序清单

> `agent/` + 主模块 `internal/service/agentnet` + `web/src`；`export PATH=$PATH:/usr/local/go/bin`；不 commit。
> 前置：v0.4.2（agent-hub-direct-v2）完成并通过检查（同一批文件，串行施工）。

- [ ] S1 agent：事件类型与环形缓冲（emit 点挂到既有动作处：serve/warm/relink/error）；批量上报器（~1s 去抖、单批上限、失败丢批）；`events:true` 开关门；红线（无凭据/签名/完整 URL）
- [ ] S2 master：事件总线（ring 1000 + seq）；心跳响应 `events` 开关（`agent-network.events-enable`）；`/api/agent/events` 接收（复用 agent 鉴权）；自身 emit 挂点（请求拦截/307/预热/换链）
- [ ] S3 master：`POST /ge2o/agent-network/events`（secret 鉴权；since_seq 增量；返回 events+seq）
- [ ] S4 web：新路由「实时流转」+ 导航项；引入 `@xyflow/react`（唯一新增前端依赖）；**卡片画布**——5 种实体卡片（客户端/master/边缘节点[内列4台]/大盘鸡/Google）+ 拓扑连线 + 事件驱动的飞包动画（标签/颜色/方向按 prd N3 映射表）+ 卡片状态芯片（空闲淡出/错误红闪）；1s 轮询；关闭态提示；不设大段文字流水
- [ ] S5 测试与验收：agent 缓冲/批量/开关；master 合并/seq/鉴权；`-race`、`-count=3`；web typecheck+build；真链 A1–A3 快照（事件序列与链路点亮）
- [ ] S6 实现报告（偏差/证据/遗留）；spec 事件词典待主会话补录

## 回滚点

- 随下一个 master 重建 + 节点升级；回滚=关 `events-enable`（agent 停推、页面示关闭态，零功能影响）或回装旧版。
