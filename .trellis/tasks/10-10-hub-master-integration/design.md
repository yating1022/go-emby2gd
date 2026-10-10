# design：master 侧接入（hub）

## 1. 数据模型

- `agents.json` 条目新增 `role` 字段（`node`|`hub`）；读旧文件缺省 `node`；enroll 请求新增可选 `role`（默认 node），非法值 400。
- hub 记录复用既有字段（last_ip、priority、sign_key、心跳）；hub 内网口端口取 master 配置 `agent-network.hub-port`（默认 8791，不新增协议字段）。
- 客户端调度候选：过滤 `role == node`（定位 10-09-agent-priority-scheduling 的候选集构造点，加过滤 + 测试）。

## 2. hub 选择函数

```
hubFor(fileID) *agentRecord
  健康 hub（心跳新鲜）按 (priority desc, id asc) 排序
  无 → nil；否则 hubs[hash(fileID) % len(hubs)]
```
- 单 hub 恒等；确定性保证"同一文件永远选同一台"（warm 与播放取址一致，多 hub 时缓存单副本）。
- 调用方只依赖 `hubFor`，后续扩展不改调用点。

## 3. 预热改向

- 触发点与去重沿用现有 `preheat.go`（TransferPlaybackInfo defer、LoadCacheItems；10min/路径去重；全异步）。
- hub 健康时：解析目标（fileID + 直链 + auth，复用现有 ResolveTarget/直链换取链路）→ `POST http://<hub>:<port>/warm`：
  ```
  {"file_id","direct_link","auth","regions":{"head_bytes":<默认>,"tail_bytes":<默认>,
   "resume_offset_bytes":<尽力而为：size×ticks/duration，算不出则省略>}}
  ```
  超时 `hub-warm-timeout`（默认 3s）；200=接受（记入该 fileID 的"hub 可用"状态，供 §4 决策）；失败/超时→回退戳边缘旧路径。
- hub 不健康（心跳失联）或 `hub-enable=false`：完全走现状（戳边缘）。
- 日志：`预热: hub 接受/失败(回退) file_id=... hub=...`；**不打印签名 URL**（复用 redact）。

## 4. 播放上游改写

- 播放入口（ResolveTarget 输出给节点的链路参数）决策：
  - `hub-enable` 且 hubFor(fileID)!=nil 且 该文件的 warm 已被接受（浏览预热或本次握手同步补发，短超时）→ 链路 payload：上游=`http://<hub>:<port>/f/<fileID>`，**无 auth 字段**；
  - 否则 → 现状 Google 直链 + auth。
- 节点侧无需知道"这是 hub"（就是一个普通 URL 参数）；若节点强制附 auth（由 hub 侧任务取证），master 对 hub 链路不下发 auth 已足够。
- 回退：hub 心跳失联（心跳窗口外）→ 直接不回 hub 链路；warm 失败→不回 hub 链路。

## 5. 配置（config.yml，`agent-network` 节）

- `hub-enable`（默认 false）、`hub-warm-timeout`（默认 3s）、`hub-port`（默认 8791）。
- 关闭时所有路径与现状逐字一致（对照测试）。

## 6. 测试

- 单测：role 解析/校验/旧文件兼容；调度过滤（hub 不入候选）；hubFor（0/1/2 台、健康、哈希确定性）；预热改向（httptest 假 hub：200/超时/500 → 回退戳边缘；去重保持）；上游改写（warm 接受 vs 失败 vs hub 失联；payload 无 auth 与无签名泄露）。
- 回归：`hub-enable=false` 时主模块全量对照基线；新增用例 `-race`、`-count=3` 无 flake。
- 纪律：不 commit；对照式口径（5 个既有失败包不动）。

## 7. 边界与风险

- warm 超时（3s）不阻塞任何播放路径（全异步）；最坏=回退现状。
- master↔hub 控制面走公网（OVH→Twon），指令小、频率低；失败即回退，无强依赖。
- 直链 1h 过期问题由 hub 侧换链（复用既有 download-link 通道）；master 无需为 hub 加新端点（若 hub 换链需要新链路参数形态，随之调整——以 hub 侧任务实测为准）。
