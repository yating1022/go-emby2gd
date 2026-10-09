# 网关预热（详情页/PlaybackInfo 触发首触）

> 父任务：`10-09-playback-startup-boost`。设计见 `design.md`，执行见 `implement.md`。

## Goal

在用户「即将播放」前（进详情页 / PlaybackInfo 时刻）异步触发一次极小请求，把 agent 读前缓存（子任务 1）的**首触预取**提前完成——点播放时头/尾数据已在节点本地，起播接近秒开。

## Requirements

- **N1** 触发点：`emby.TransferPlaybackInfo`（Reg_PlaybackInfo）与 `emby.LoadCacheItems`（Reg_UserItems 单条详情）两个 handler 末尾**异步**触发；**绝不阻塞、绝不影响**被拦截请求的响应与耗时。
- **N2** 触发动作：解析条目的 gdrive 逻辑路径（同播放链路：`MatchMountPath`）→ `agentnet.PickAndSign` → 对签名地址发 `Range: bytes=0-65535` GET（超时 8s，丢弃 body）。
- **N3** 去重：按 gdPath 记录最近触发时刻（TTL 10 分钟、有界 map、定期裁剪）；TTL 内重复浏览不重复触发。
- **N4** 静默降级：无可用节点 / 签名失败 / 请求失败一律记 WARN 后忽略，**绝不影响任何主流程**；gdrive 或 agent 网络未启用时不触发。
- **N5** 开关：`agent-network.preheat-enable`（默认 true）；false 时行为与现状一致。
- **N6** 协议零变更；日志不打印令牌与签名。

## Acceptance Criteria

- [x] **A1** 单测：httptest 假 agent 捕获触发请求（断言 Range 与路径）；TTL 内不重复；开关关闭零请求；解析失败/无节点不 panic、不阻断。
- [x] **A2** 主流程无感：假 agent 挂起不响应时，PlaybackInfo / 详情页响应耗时与 baseline 一致（异步断言）。
- [ ] **A3**（父任务 A3）经详情页预热后手机起播 ≤ 2s。（真机验收，随父任务执行）

## Out of Scope

- 播放中路径的实时预取（那是子任务 1 的 tee）。
- 跨节点预热的"预热门≠播放节点"轮换问题（记为已知降级，见 design §6）。
