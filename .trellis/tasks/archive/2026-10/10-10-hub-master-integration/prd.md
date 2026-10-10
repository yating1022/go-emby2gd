# master 侧接入（hub）

> 父任务：`10-10-cache-hub-center`。设计见 `design.md`，执行见 `implement.md`。
> 顺序说明：实现须在 v0.3.2 发布冻结之后进行（同一工作区，避免发布混入半成品）。

## Goal

master（本网关）接入 hub：注册表支持 `role`（hub/node、多实例）；客户端调度永不选 hub；浏览/播放入口把预热目标从"戳边缘"改为"下指令给 hub"（携带直链与区域集）；播放时把节点上游改写为 hub 明文口 URL；hub 不可用/指令失败自动回退现状（Google 直链）。

## Requirements

- **N1 注册表 role**：enroll 支持可选 `role`（缺省 `node`，向后兼容）；校验 role∈{node,hub}；agents.json 读旧条目缺省 node；admin 列表输出 role。
- **N2 调度隔离**：客户端服务节点选择只取 `role=node`（hub 永不参与客户端调度）；既有优先级调度逻辑（10-09）不动，只加过滤。
- **N3 hub 选择函数**：`hubFor(fileID)`——健康 hub 按（优先级, id）排序后 `hash(fileID) % len` 确定性取一台；单 hub 恒等；为多 hub 预留（后续加台不用改调用方）。
- **N4 预热改向**：沿用现有触发点（`TransferPlaybackInfo`/`LoadCacheItems`）+ 现有去重（10min/路径）；hub 健康→`POST /warm`（直链、auth、regions：头段/尾段/续播点尽力而为）；hub 不健康→保留"戳边缘"旧路径。全程异步、不阻塞播放流程。
- **N5 播放上游改写**：warm 被接受→下发给节点的上游=`http://<hub>:<hub-port>/f/<fileID>`（**不带 auth**）；否则=现状 Google 直链。fileID=直链 URL 中的 Drive file id。
- **N6 回退链**：hub 心跳失联 / warm 超时或失败 → 直接走 Google 直链（现状不动）；日志记录决策（warm 接受/失败、hub/直连选择），**不打印签名 URL**。
- **N7 配置**：`agent-network.hub-enable`（默认关）、`hub-port`（默认 8791）、`hub-warm-timeout`（默认 3s）等；关=行为与现状完全一致。
- **N8 cancel**：master 侧不发（3 分钟规则由 hub 自维护）；留作后续可选。

## Acceptance Criteria

- [ ] **A1** role 兼容性单测：旧 agents.json（无 role）读为 node；enroll role=hub 合法、非法值拒绝。
- [ ] **A2** 调度隔离：注册 hub 后客户端调度候选中永不出现 hub（含多 hub 场景）。
- [ ] **A3** 选择函数：0/1/2 台 hub、健康过滤、`hash(fileID)` 确定性（同输入同输出）、单 hub 恒等。
- [ ] **A4** 预热改向（httptest 假 hub）：warm 携带直链/auth/regions；超时/5xx 失败→回退戳边缘；去重保持。
- [ ] **A5** 上游改写与回退：warm 接受→节点链路为 hub URL 且无 auth；hub 失联/warm 失败→Google 直链；链路参数不含签名泄露。
- [ ] **A6** `hub-enable=false` 时全量主模块包与基线一致（5 个既有失败包对照式口径）；新增包 `-race`、`-count=3` 无 flake；不 commit。

## Out of Scope

- hub 侧实现（`10-10-hub-agent-mode`）；部署与真链（`10-10-hub-deploy-verify`）；cancel 下发；多 hub 实际部署。
