# design：agent 读前缓存（v1：只缓预取块，无 tee）

## 1. 目标与约束

- 目标：把「起播探测序列」里除第一次以外的所有请求变成本地命中；第一次请求也只付一次上游首字节延迟（~1.2s）。
- 硬约束：**冻结协议零变更**（端点/签名/状态码/响应头白名单语义不动）；stdlib only；纯内存、有界；v0.2 与 v0.3 灰度混跑可互操作。
- **决策记录（2026-10-09 用户拍板）**：不做「路过 tee」。理由：滑动窗口相对整文件太小（收益限于回看等下众场景）、会挤出其它文件的头尾、与透传定位不符。**缓存内容 = 且仅 = 每文件首触预取的头尾块**。

## 2. 组件：BlockCache（新文件 `agent/internal/proxy/blockcache.go`）

```go
const blockSize = 4 << 20 // 4MiB

// 块键：fileID + 身份 + 块号。
//
// ⚠️ 身份来源（2026-10-09 真实 Google 直链实测，三种姿势均确认）：
//   响应**没有 ETag、没有 Last-Modified**；206 的 `Content-Range: bytes a-b/<total>`
//   （或 200 的 Content-Length）携带**文件总字节数**。
//   因此身份链 = ETag → Last-Modified → 总字节数（三者恒有其一，**绝不因缺身份放弃缓存**）。
//   判别力：同 id 换文件若大小不同必被识破；同大小替换是残余风险，由 TTL 兜底。
type blockKey struct { fileID, identity string; idx int64 }
```

接口（**Put 只有预取会调用**——无 tee）：

- `Get(fileID, identity string, idx int64) ([]byte, bool)` — 命中即 LRU 提升；**块龄超过 `CACHE_MAX_AGE_MINUTES` 一律不服务**（视为 miss）。
- `Put(fileID, identity string, idx int64, data []byte)` — 按预算淘汰（淘汰最久未用块）；键已存在则跳过。
- `Prefix(fileID, identity string, start, limit int64) (data []byte, n int64)` — 从 `start` 起返回**连续命中**的块前缀（上限 limit），供混合服务。
- `Observe(fileID, identity string)` — 任何**上游响应**（预取或透传）观察到身份；与登记值不同则清除该文件全部旧块；空值 = 空操作（不视为变化）。
- `Enabled() bool` — `budget > 0`。

预算算术：每文件固定 ~36MB（8+1 块）→ 256MB ≈ 7 部片的头尾，**播放本身不引起任何轮换**。

## 3. 预取（`agent/internal/proxy/prefetch.go`）

- **触发**：Handler 每次服务请求时，若该 `fileID` 在缓存中「无任何块且无在途预取」→ 异步启动。
- **头预取**：立即取 `[0, PREFETCH_HEAD_MB)`（默认 32MB，8 块），串行逐块 `Range` 上游写入缓存；从响应头解析 `身份`（ETag/Last-Modified/总大小链）与 file size。**身份恒可得（总大小必在）**，预取不会因缺身份放弃。
- **尾预取**：size 解析出后启动 `[size-PREFETCH_TAIL_MB, size)`（默认 4MB，1 块）；块 0 到手即启动，不等头部跑完。
- **上游取数**：复用 `LinkSource.Link(fileID)` 的当前直链与凭据头（与数据面同一缓存/单飞）；`401/403` 走现有 `Refresh` 单次重试语义。
- **单飞与并发**：`map[fileID]struct{}` 在途标记；全局预取并发上限（同时最多 2 个文件），避免与真实播放抢流。
- **失败处理**：单次失败记 WARN 放弃（不重试风暴）；后续真实请求自然会再次触发。

## 4. 服务路径改造（`handler.go` `serve()`）

现状：`doUpstream` → 白名单透传。改为（**只在能完整解析出 `Range: bytes=<start>-<end?>` 时启用缓存分支**，其它形态一律走现状）：

1. **纯本地命中**：`[start, end]`（end 缺省按 size-1 补全；size 已知才可）完整落在缓存块内 →
   本地拼 `206` + `Content-Range` + `Content-Length` + 缓存记录的白名单头（Content-Type/ETag/Last-Modified/Accept-Ranges）→ 写完返回。**不触上游**。
2. **混合**：`Prefix(start)` 得连续命中前缀 `c > 0` 且请求延伸超过 `start+c` →
   **先打开上游 `[start+c, end]` 请求**（拿到响应后才写任何字节——失败则完整回退第 3 态；并校验上游身份与本地前缀一致，不符亦整体回退）→
   写 `206`（Content-Range 覆盖整个 `[start, end]`）→ 写缓存前缀 → `io.Copy` 上游余下部分（沿用 flushWriter）。
3. **未命中**：现状透传，**不缓存任何字节**（只 `Observe` 响应头身份，变化则清旧块）。

- `HEAD` 请求不进缓存分支；无 `Range` 的 200 全流纯透传（可 `Observe` 身份）。
- 缓存元数据：每文件记录白名单头 + size + 身份（来自预取响应），供纯本地/混合拼响应头。

## 5. 配置（`agent/internal/config/config.go` + `config.env` 新键，均有默认值）

| 键 | 默认 | 说明 |
|---|---|---|
| `CACHE_BUDGET_MB` | 256 | 内存预算；0 = 关闭（行为与 v0.2 逐字节一致） |
| `CACHE_MAX_AGE_MINUTES` | 1440 | 块最大可服务年龄（24h）；过期不服务（LRU 之外的第二道兜底，防同大小替换） |
| `PREFETCH_HEAD_MB` | 32 | 首触头预取大小 |
| `PREFETCH_TAIL_MB` | 4 | 首触尾预取大小（size 未知时不启动） |

键名风格与 `config.env` 现有键对齐（实现时以 `config.go` 现有解析为准）。时钟需可注入（测试用）。

## 6. 正确性论证（测试钉死）

- **字节一致性（最重要）**：同一 Range 在「缓存关（v0.2 路径）」与「缓存开（全命中 / 混合）」下响应 sha256 相等（随机数据 + 覆盖块边界用例）。
- **身份链**：ETag 路径 / Last-Modified 路径 / **总大小路径**（真实 Google 形态）各有用例；`Observe` 换身份 → 旧块不可命中。
- **⚠️ 假上游必须复刻真实 Google 头部形态**：206 **不带 ETag/Last-Modified**、只带 `Content-Range` 与 `Content-Length`（本轮教训：此前 mock 恰好带了 Last-Modified，掩盖了"生产无身份可用"的缺口——测试假体与真实头部不一致，验出来的收益是虚的）。
- **TTL**：`CACHE_MAX_AGE_MINUTES` 到期后块不被服务（注入时钟推进断言）；过期块可由 LRU 回收。
- **边界**：start/end 在块中间、跨块、尾部不足一块、Range 超出 size（原样交上游，保持 416 语义）。
- **混合态上游失败/身份不符**：写首字节前失败 → 完整回退纯透传；写后失败 → 连接关闭（等价网络断开，客户端自会重试；记日志）。
- **并发**：预取写入与命中读取并发、两次预取单飞、-race 全绿。
- **关闭等价**：`CACHE_BUDGET_MB=0` 时上游请求序列与响应与 v0.2 逐字节一致（对照测试）。

## 7. 可观测性

- INFO：`预取开始/完成`（file_id、块数、字节、耗时）、`缓存命中服务`（块数/字节）、`混合服务`（前缀字节）。
- WARN：预取失败、上游截断。
- **不打印令牌与签名**；关闭时不多打一行。

## 8. 修订记录

- **R1（2026-10-09 晚，实测驱动）**：身份链由「ETag→Last-Modified→放弃」修订为「ETag→Last-Modified→**总字节数**」+ 新增 `CACHE_MAX_AGE_MINUTES`（默认 24h）。触发原因：真实 Google 直链响应实测无 ETag/Last-Modified（206 仅 `Content-Range: bytes a-b/<total>`，200 仅 Content-Length + `x-goog-hash`，HEAD 同），原设计会导致**生产环境零缓存空转**。
