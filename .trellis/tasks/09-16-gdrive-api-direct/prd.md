# Google Drive API 直接取流(替代网关中转)

## Goal

让 ge2o 直接调用 Google Drive API 取视频数据, **去掉 `vault.bjyt.de` 网关这一跳中转**。

```
现在:  Google Drive → OVH 网关(158.69.244.4) → Zouter(155.117.82.69) → 客户端
目标:  Google Drive → Zouter → 客户端
```

同时: 由于 Google Drive 的取流 URL 是**稳定可复用的**(`/files/{fileId}?alt=media`),
本项目的"直链缓存"将**第一次真正生效** —— 现在它 100% 不生效(日志实测: 缓存命中 0 次、写入 0 次)。

## Background

### 现状链路与它的问题

当前 strm 内容是:
```
https://vault.bjyt.de/redirect?path=/影视库/最新电影/72小时 (2026)/72小时 (2026).mkv&pickcode=1mI3SHjiwvUte39hpx79-Yor5Cbga_kXm&storage=googledrive-1
```

`vault.bjyt.de` 是一个 `uvicorn`(Python) 写的**流式网关**, 内部做:
`pickcode → 查 Google Drive fileId → 自己去 GD 取数据 → 把字节流传回来`。

实测确认(见下"实测证据"): 它**只返回 200/206 数据流, 从不返回 302**,
所以我们既看不到也没有可缓存的"直链", 每次客户端 Range 请求都必须重新敲网关。

**三重代价**:

1. **绕路**: 数据从 GD 到 OVH, 再从 OVH 到香港的 Zouter, 最后到客户端
2. **带宽**: 所有视频流量叠加在 OVH 的出口带宽上
3. **延迟**: 每次请求都要等网关重新解析 pickcode 并建立到 GD 的连接。实测每次请求首字节 **1~4 秒**; 拖动进度条时这段延迟每次都要重付

### 实测证据（本次调研期间采集）

| 项 | 结果 |
|---|---|
| 网关返回 | `HTTP/2 200` / `206` + `content-type: video/x-matroska` + `content-length: 4463754536`, **无任何 `Location` 头** |
| 直链缓存命中(生产日志) | **0 次** |
| 直链缓存写入(生产日志) | **0 次** |
| 代理请求数 = 上游请求数 | 6 = 6（每个客户端请求都重新敲网关） |
| ge2o 播放时的对外连接 | 只有 1 条, 对端 `158.69.244.4:443`（= vault.bjyt.de）; **没有任何到 Google 的连接** |

结论: ge2o 目前只是把网关当"文件服务器"用, 直链缓存代码路径从未被走到。

## Research Findings（官方文档查证, 2026-09-16）

> 以下结论来自 Google 官方文档, 不是推测。后续实现不得与此冲突; 若发现文档已变更, 回来更新本节。

### R1 没有"按路径查文件"的接口 ❌

Drive API v3 **没有 path 概念**。唯一的目录关系是 `parents` 集合, 且只接受**文件夹 ID**:

```
'<FOLDER_ID>' in parents and name = '<名称>'        ← 用 files.list 查询
```

从路径到 fileId 必须**逐层遍历**, 每层一次 `files.list`。

对 `/影视库/最新电影/72小时 (2026)/72小时 (2026).mkv` 这种 4 层路径 = **4 次 API 调用**。

**因此路径 → fileId 的解析结果必须缓存**（映射是稳定的）。这是本项目第一个真正需要缓存的映射。

### R2 取流 URL 是稳定可复用的 ✅

```
GET https://www.googleapis.com/drive/v3/files/{fileId}?alt=media
```

- 支持 `Range` 头（官方明确: blob 文件支持 `bytes=500-999`）→ **拖动进度条可用**
- URL 由 fileId 唯一确定、长期不变 → **可缓存、可复用**
- 这与当前网关"每次都重新解析"形成本质区别

### R3 鉴权只能用 OAuth 2.0, API Key 不行 ❌

- 下载需要 `Authorization: Bearer <access_token>`
- 需要**有内容读取权限**的 scope（`drive.readonly` 等）;
  官方明确指出 `drive.metadata.readonly` **不足以**下载文件内容
- API Key 不适用于下载场景

access_token 有效期约 1 小时, 需用 refresh_token 定期换取并缓存。

### R4 元数据里的 `webContentLink` 不是直链 ⚠️

`files.get` 不带 `alt=media` 时返回元数据, 其中 `webContentLink` 是**浏览器下载页链接**。
官方明确它不是稳定可复用的 CDN 端点, **不作为本方案的基础**。

### R5 配额（官方 2026-05 更新后）

| 限制 | 值 |
|---|---|
| 每项目每分钟 | 1,000,000 配额单位 |
| 每用户每分钟 | 325,000 配额单位 |
| **每项目每天出流量** | **1 TB** |
| 日计费阈值（每项目） | 400,000,000 配额单位 |
| `files.get` / `files.list` / 下载 消耗 | 5 / 100 / 200 单位 |
| 超限错误 | `403 User rate limit exceeded`、`429 Rate limit exceeded` |

对个人看片, 1TB/天 的量绰绰有余; 多人共用需要留意。

## Requirements

- **R1 路径解析**: 从 strm 地址中取出路径, 经 Drive API 逐层解析为 `fileId`。解析结果**必须缓存**（否则每次播放 4 次 API 调用）。
- **R2 取流**: 用 `files.get?alt=media` 取数据, **透传客户端的 `Range`**, 正确回写 206 / `Content-Range` / `Content-Length`。
- **R3 OAuth 令牌管理**: 用 refresh_token 换 access_token 并缓存, 到期前自动刷新; 刷新失败要有明确日志。
- **R4 复用现有代理链路**: 字节流的转发、错误回退、日志契约、并发控制**复用** `internal/service/streamproxy/` 已有的实现, 不重复造。
- **R5 数据源可切换**: 保留现有的"经网关代理"作为可选路径, 通过配置决定走哪条。**在 Google API 未配置或失败时不得让播放整体不可用。**
- **R6 日志**: 沿用 `[直链代理]` 前缀体系, 新增条目见 design.md（规划阶段补充）。
- **R7 凭据安全**: client_secret / refresh_token 属敏感凭据, 不得出现在任何日志中; 配置文件权限需说明。
- **R8 内置 OAuth 授权流程**: ge2o **自身**提供授权入口与回调, 由管理员在浏览器中一键完成授权, 换取并**持久化** refresh_token。不得要求管理员手工到外部工具里去取 token。
  - 理由: token 可能失效（7 天有效期、被撤销、密码变更等）, 内置流程让重新授权变成点一下链接的事; 手工流程每次都要重走一遍, 实际不可维护。
  - **硬性前提（官方规则）**: Google 要求重定向 URI 必须是 **HTTPS 且使用域名**（不接受裸 IP, 仅 localhost 例外）。因此 ge2o 必须以 HTTPS + 域名对外提供该回调路径, 由部署方在 Google Console 注册完全一致的 URI。
  - **本项目实际采用的值**: `https://go.bjyt.de/ge2o/gdrive/oauth/callback`。
    `go.bjyt.de` 已指向 Zouter、泛域名证书已就绪、域名到 ge2o 的转发**已实测生效**（见 design.md §10 的"部署现状"）。
    剩余待办仅一项: 在 Google Console 登记该 URI。

## Constraints

- 部署机器 Zouter 内存不足 1GB（当前可用 612MB）, **不得引入按响应体大小增长的内存结构**。
- 所有出站 HTTP 走 `internal/util/https`（项目既有约束）。
- 路径解析的缓存**必须持久化或可重建** —— 进程重启后不得退回"每次播放 4 次 API 调用"（待设计定夺: 内存缓存 vs 落盘）。
- 不得改动 `internal/web/cache` 的响应缓存契约（字节流路由已移出白名单, 见 `response-cache.md`）。

## Open Questions（需要开发者回答, 阻塞设计定稿）

- **Q1 路径起点**（**已答复**）: 起始文件夹做成配置项, 由开发者提供。设计上把 `root-folder-id` 做成**可留空**, 留空时回落到 `drive-id`（即认为第一段直接位于团队盘根目录下）; 该假设官方文档未明确, 列入 Step 8 的 V0 验证项。若验证不通过, 显式填该项即可, 不需要改代码。
- **Q2 路径是否一致**: strm 里的 `path` 参数（如 `/影视库/最新电影/72小时 (2026)/72小时 (2026).mkv`）与 Google Drive 里的真实路径**是否完全相同**? 中间的 `/影视库` 在 GD 里是否也是这个名字?
- **Q3 文件归属**（**已答复**）: 是开发者管理的**团队盘(Shared Drive)**, 有充足配额。**这一条改变了 API 设计** —— 共享云端硬盘需要一组必需参数（`supportsAllDrives` / `includeItemsFromAllDrives` / `corpora=drive` / `driveId`），缺任一项都会表现为"查不到任何文件"，而 API 返回的是空结果而非错误，极难排查。详见 design.md §4.2。
- **Q4 OAuth scope**: 现有 refresh_token 是用哪些 scope 授权的? 必须包含内容读取权限（如 `drive.readonly`）, `drive.metadata.readonly` 不够。
- **Q5 重名**: 同一文件夹下是否存在同名文件? Drive 不保证名称唯一, 遍历时若命中多个需要明确的取舍策略。
- **Q6 网关的未来**: 此功能落地后 `vault.bjyt.de` 网关是保留作为回退, 还是彻底弃用? 影响"数据源可切换"这条需求的具体形态。
- **Q7 解析缓存的形态**: 路径→fileId 的映射缓存放内存（重启即失效）还是落盘（重启仍在）?

## Out of Scope（本任务不做）

- 移除 `vault.bjyt.de` 网关本身（那是网关侧的事）。
- 115 网盘 pickcode 体系的任何处理 —— 本方案**完全绕过 pickcode**, 直接用 GD 路径。
- 转码 / 字幕 / 图片等其它 Google Drive 资源类型。
- 多 Google 账号 / 多凭据轮换（除非 Q6 的答案要求）。

## Notes

- 本任务与已完成的 `09-16-strm-proxy-play` 是**并列关系**, 不是它的子任务: 前者解决"谁去取数据、怎么转发", 本任务解决"数据源换成 Google Drive API 之后取什么"。字节流转发那部分直接复用前者的成果。
- 当前生产状态（供参考）: Zouter 上 ge2o 已接管 8099, MediaWarp 已停用但保留; 详见 Zouter 主机的运维记忆。
