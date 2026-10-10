# design：hub 模式（agent 二进制）

## 1. 角色与配置

- 同一二进制两种角色：`ROLE=node`（默认，零变化）| `ROLE=hub`。
- hub 新增配置（config.env）：`ROLE`、`HUB_PORT`（建议 8791）、`HUB_ALLOW_IPS`（master+四节点，支持 CIDR）、`DISK_CACHE_DIR`（建议 `/home/ge2o-hub-cache`）、`DISK_BUDGET_GB`（默认 200）、`CACHE_MAX_AGE_MINUTES`（hub 上默认 2880=48h）、`WARM_HEAD_BYTES`（默认 128MiB）、`WARM_TAIL_BYTES`（默认 4MiB）、`WARM_RESUME_WINDOW_BYTES`（默认 4MiB）。
- hub 同样走 enroll/心跳：注册表带 `role=hub`（由 master 侧任务定义字段；hub 侧在 enroll 请求中携带 role 字段）。
- hub **不启用**客户端面向的路由（或加载白名单后仅服务内网口）；node 模式代码路径不变。

## 2. 磁盘块存储（diskcache）

- 目录布局：`<dir>/<hash(fileID)前2位>/<hash(fileID)>/{meta.json, <idx>.blk}`；meta 含 `file_id/identity/size/created_at/last_access_at`。
- 读写：块文件 temp+rename；identity 不匹配→整目录作废重抓（与内存版 `Put` 保护空 identity 同语义）。
- TTL：读取时惰性判（超 48h 即视为缺失并清扫）；另起周期清扫协程（如 10min 一轮）。
- LRU：容量按 `DISK_BUDGET_GB` 硬限；超限按 `last_access_at` 升序淘汰整文件；`last_access_at` 更新节流（如 ≥30s 才落盘）。
- 启动恢复：懒扫描（按目录结构重建内存索引/或首次访问时读 meta）；崩溃后已 rename 的块必须可复用（原子性保证）。
- 并发：按 fileID 分片锁 + 单飞填充（同文件仅一条在途填充流）。

## 3. `/f/<driveFileID>` 三态服务

- 语义与节点 `serveCached` 完全一致（空/部分/满；前缀先行；206/range-echo 校验；401/403→换链单次重试）。
- **命中判定不出网**：full/partial 覆盖请求区段→本地直供（不需要凭据）；miss→用 warm 时存的直链回源透传并同步落盘。
- 无该文件直链（未 warm）→ `409 {"error":"not_warmed"}`（master 侧保证只把 hub URL 交给节点前 warm 已被接受）。
- 供流即"真实播放"判据：任何 `/f/` 请求的客户端在途期 = 播放存在（复用 ClientBegin 语义）。

## 4. 控制指令契约（仅 master 白名单）

```
POST /warm   {"file_id","direct_link","auth","regions":{"head_bytes","tail_bytes","resume_offset"}}
           → 200 {"status":"warming"} | 409 已有不同直链且在途（可选：先 cancel）
POST /cancel {"file_id"} → 200 {"status":"stopped"}
```
- 幂等：同 file_id 已在途→直接 200；头段已齐且无续传→200 无动作。
- 区域集抓取：①头段 `Range: 0-(head-1)` 单流切片（v0.3.2 语义：凑满即落块、首块就绪日志）；②尾部 `Range: (size-tail)-`；③续播点 `Range: (resume-window)-(resume+window)`。②③各一条小请求。
- **3 分钟规则**：warm 接受起计时；期间出现"真实播放"→取消计时、转全量续取（后台、让路、断点续取）；计时到而无播放→停在头段（不续全量）；`/cancel` 等效于立即停止续传（已落块保留）。
- 直链过期（401/403）：调用 master 的 download-link 通道换新链（复用既有 agent 换链语义）→断点续取。

## 5. 回源加固（多地址拨号）

- 自定义 `DialContext`：解析目标全部 A 记录，逐地址（或小并发竞速）拨号，**单地址 ~1.5s 未建立即换下一个**；全部失败才报错。防 Twon→googleapis 坏 IP 踩雷 7.5–22.5s。
- 该逻辑位于 hub 出站客户端（复用/扩展现有 upstream 客户端构造）；node 模式可同样受益（如启用开关，默认不改变 node 行为）。

## 6. node 侧微调核查

- 核查节点对上游附 Authorization 的来源：若来自 master 下发的链路参数（付 payload），则 master 对 hub 链路**不下发 auth**→节点零改动；若节点无条件附→加一处"上游 host==hub 时跳过"（含测试）。
- 结论必须写进实现报告（零改动也要记录证据）。

## 7. 测试

- 单测：磁盘存储 TTL/LRU/崩溃复用/并发；三态矩阵（字节一致性）；区域集抓取请求数与区间（hook 假上游）；3 分钟→播放时间线（可注入时钟）；让路；坏地址 1.5s 跳过；短读/EOF/200 忽略 Range。
- e2e（本机）：假 Google + hub 二进制 + node 二进制；命中/部分/空；hub 断 Google 后命中仍供流；warm→播放→续全量。
- 回归：node 模式全量对照 v0.3.2 基线（5 个既有失败包对照式口径）。
- 纪律：`-race`、`-count=3` 无 flake；stdlib-only；不 commit；日志不含密钥/签名。

## 8. 边界与风险

- Twon 2G 内存：流式转发 + 小块缓冲即可，不做大内存聚合。
- 磁盘满/接近上限：LRU 兜底；写块失败→跳过该块（记 WARN，不中断供流）。
- 单条填充流中断：已落块保留 + 续取（v0.3.2 语义）。
- 48h TTL 与"追剧窗口"匹配；冷文件重拉成本已被 3T 配额覆盖。
