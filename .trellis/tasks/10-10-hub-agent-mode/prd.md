# hub 模式（agent 二进制）

> 父任务：`10-10-cache-hub-center`。设计见 `design.md`，执行见 `implement.md`。
> 顺序说明：实现须在 v0.3.2 发布冻结之后进行（同一工作区，避免发布混入半成品）。

## Goal

给 agent 二进制增加 **hub 角色**：磁盘块缓存（48h TTL + LRU 上限）、对内明文 `/f/<driveFileID>` 三态服务、master 预热/取消控制指令、回源多地址快速失败拨号；node 侧核查 hub 上游的 Authorization 行为（预计零改动）。

## Requirements

- **N1 角色开关**：`ROLE=node|hub`（node 默认，**零行为变化**）；hub 复用既有 enroll/心跳/换链通道，仅新增 hub 配置项与监听口。
- **N2 磁盘块存储**：Twon `/home`；块 4MiB 与现有一致；temp+rename 原子落盘；identity 语义同内存版（ETag→Last-Modified→size:N）；48h TTL（惰性 + 周期清扫）；LRU 容量硬上限（`DISK_BUDGET_GB`，默认 200）按 atime 淘汰。
- **N3 `/f/<driveFileID>` 三态服务**：空→回源透传并同步落盘（头段先行预算后停）、部分→前缀先行混合、满→磁盘直供；**字节一致性矩阵零回归**；range echo 校验、401/403 换链重试语义保留；**命中供流不需要凭据（不出网）**。
- **N4 控制指令**（仅 master IP）：`POST /warm {file_id, direct_link, auth?, regions{head_bytes,tail_bytes,resume_offset?}}`（幂等、单飞）；`POST /cancel {file_id}`；区域集抓取=头段一条单流切片（复用 v0.3.2）+ 尾一条 Range + 续播点一条 Range；**3 分钟无"真实播放"（无客户端流在途）停止续传**；播放出现→取消计时并转全量续取（让路照旧）。
- **N5 回源加固**：多 A 记录快速失败拨号（单地址 ~1.5s 未建立即换下一个；应对 Twon→googleapis 踩雷 7.5–22.5s）；Authorization 过期→走 download-link 换新链 + 断点续传。
- **N6 node 侧微调核查**：上游为 hub 时不得附 Google Authorization（若 auth 来自 master 下发的链路参数、缺省即不附，则确认零改动并记录结论）。
- **N7 IP 白名单**：拉流口/控制口白名单（master + 四节点 IP/CIDR，应用层实现，可测）；白名单为 hub 专属配置项，node 模式不读取。

## Acceptance Criteria

- [ ] **A1 单测**：磁盘块存储（TTL/LRU/崩溃后复用/并发）、三态字节一致性矩阵、区域集抓取请求数精确、3 分钟规则与播放续传时间线、让路、多地址拨号（坏地址 ≤~1.5s 跳过）、短读/EOF/200 边界。
- [ ] **A2 本机 e2e**：假 Google + hub + node 全链：命中/部分/空三态、hub→Google 断网命中、warm→播放→续全量、3 分钟无播放停止。
- [ ] **A3 回归对照**：node 模式在 v0.3.2 基线上全绿；hub 注册不改变既有行为。
- [ ] **A4**：`-race`、`-count=3` 无 flake；stdlib-only；不 commit；gofmt/vet 干净。

## Out of Scope

- master 侧接入（`10-10-hub-master-integration`）；部署与真链（`10-10-hub-deploy-verify`）；多 hub 实际部署；hub 间同步。
