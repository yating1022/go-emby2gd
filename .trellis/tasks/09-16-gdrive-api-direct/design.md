# 技术设计: Google Drive API 直接取流

> 前置阅读: 本任务 `prd.md` 的 Research Findings 一节（官方文档查证结论）。
> 复用: `internal/service/streamproxy/` 的字节流转发能力；`.trellis/spec/backend/http-proxy.md` 的代理契约。

## 1. 数据源决策流程

现状是"命中 `strm.proxy.domains` → 直接请求该 URL"。本任务在中间插入一个**数据源选择**：

```
strm 地址命中 strm.proxy.domains
│
├─ gdrive.enable 为真, 且能从地址中解析出路径?
│    │
│    ├─ 是 → 数据源 = Google Drive API
│    │         ├─ 解析路径 → fileId (带缓存)
│    │         ├─ 请求 files.get?alt=media
│    │         └─ 失败(路径解析不到 / 上游 4xx5xx) → 回退到"直接请求该 URL"
│    │
│    └─ 否 → 数据源 = 直接请求该 URL (即现有行为, 走 vault.bjyt.de 网关)
│
└─ gdrive.enable 为假 → 数据源 = 直接请求该 URL (与本次改动前完全一致)
```

**回退是硬性要求（prd R5）**: Google API 侧任何环节失败都不得让播放整体不可用，一律回退到现有的"请求原 URL"。这同时给了灰度迁移的能力——先在少数文件上验证，出问题自动退回网关。

**日志必须明确打出走的是哪条数据源**，否则排查时分不清一次播放到底用了哪个通道（见 §7 的 L20）。

## 2. 配置

新增顶层配置段（放在 `emby` 之外，因为它与 Emby 无关）：

```yaml
# Google Drive API 直接取流配置
#
# 启用后, 命中 emby.strm.proxy.domains 的 strm 地址将不再请求原网关,
# 而是从地址中取出文件路径, 经 Drive API 解析为 fileId 后直接取流。
# 任何环节失败都会自动回退到请求原地址(网关), 因此可以安全地灰度启用。
gdrive:
  # 总开关; 关闭时行为与未部署本功能完全一致
  enable: false
  # OAuth 客户端凭据 (Google Cloud Console → API 和服务 → 凭据)
  # !! 这三项等同于账号访问权, config.yml 权限必须为 600 !!
  #
  # refresh-token 可留空 —— 留空时由 ge2o 内置的授权流程获取并落盘(见 §10), 这是推荐用法。
  # 若填写则以配置的为准(手工获取的备选路径见 §11)。
  # 特别注意: 同意屏幕为「外部 + 测试」状态时 refresh_token 只有 7 天有效期。
  client-id: ""
  client-secret: ""
  refresh-token: ""
  # OAuth 重定向 URI
  #
  # !! 必须与 Google Console「已获授权的重定向 URI」中登记的值【逐字符一致】,
  #    scheme、大小写、结尾斜杠任一不同都会报 redirect_uri_mismatch !!
  #
  # 官方要求必须是 HTTPS + 域名（不接受裸 IP, 仅 localhost 例外）。
  #
  # 本项目的实际值（Zouter 已就绪, 见下方"部署现状"）:
  #   https://go.bjyt.de/ge2o/gdrive/oauth/callback
  redirect-uri: "https://go.bjyt.de/ge2o/gdrive/oauth/callback"
  # 团队盘 (Shared Drive) 的 ID
  #
  # 必填。用于 files.list 的 corpora=drive + driveId 参数:
  # 官方推荐用 drive 而不是 allDrives, 因为后者可能返回 incompleteSearch(有遗漏)。
  #
  # 获取方式: 浏览器打开该团队盘, 地址栏 folders/ 后面那一串。
  drive-id: ""
  # 路径解析的起始父文件夹 ID
  #
  # strm 路径形如 /影视库/最新电影/72小时 (2026)/72小时 (2026).mkv,
  # 第一段是「影视库」, 本项填【包含影视库的那个父文件夹】的 ID。
  #
  # 留空时默认用 drive-id 作为父节点 —— 即认为第一段直接位于团队盘根目录下。
  #
  # 【已验证 · 2026-09-16】实测确认该假设成立: 用团队盘 ID 作为父母节点成功列出了
  # 根目录 (8 个条目, 第一个就是「影视库」)。因此本项目只需 drive-id 一个值。
  #
  # 该行为依赖「团队盘 ID 可作为父节点用在 q 的 parents 里」这一假设,
  # 官方文档未明确说明, 因此列入 Step 8 的真实凭据验证项;
  # 若将来更换团队盘后发现 V0 不成立, 显式填上该文件夹 ID 即可 —— 配置项已预留。
  root-folder-id: ""
  # 路径 → fileId 映射的缓存时长, 支持 s/m/h/d 单位
  # 该映射是稳定的, 可以配得很长; 默认 7d
  path-cache-expired: 7d
  # 单次路径解析的层数上限, 防止异常路径导致无限遍历
  max-path-depth: 16
```

配置校验（`Init()`）:

- `enable` 为真时, `client-id` / `client-secret` / `drive-id` / `redirect-uri` 不得为空
- `refresh-token` **允许为空**（留空表示走内置授权流程, 见 §10）
- `redirect-uri` 必须是 `https://` 开头（官方要求）; 是 `http://` 且非 localhost 时**启动即报错**并给出中文说明, 而不是等到授权时才发现被 Google 拒绝
- `root-folder-id` **允许为空**, 为空时在解析时回落到 `drive-id`（见上方注释）
- `path-cache-expired` 单位合法、数值 ≥ 1
- `max-path-depth` ≥ 1 且 ≤ 64
- 凭据类字段**不得出现在任何日志中**（校验错误消息里也只提字段名，不回显值）

## 3. OAuth 令牌管理

access_token 有效期约 1 小时，需要用 refresh_token 定期换取。

```
POST https://oauth2.googleapis.com/token
Content-Type: application/x-www-form-urlencoded

client_id=<client-id>&client_secret=<client-secret>
&refresh_token=<refresh-token>&grant_type=refresh_token
```

响应含 `access_token` 与 `expires_in`(秒)。

实现要点:

- 令牌缓存在包内（`sync.Mutex` 保护的单个结构），**不是**每个请求都换一次
- 提前刷新: 距过期 `tokenRefreshAhead = 5 * time.Minute` 时视为已过期，重新换取
- **并发去重**: 多个请求同时发现令牌过期时，只发一次换取请求（用一个 `sync.Mutex` + 二次检查即可，不必引入 singleflight）
- 换取失败: 返回错误 → 上层回退到"直接请求原 URL"，并记 Error 日志（**不含凭据**）
- 令牌请求也走 `internal/util/https`（项目约束）

**不复用 `util/https` 的共享 client 的 cookie/凭据机制** —— Drive 的鉴权是每次请求加 `Authorization: Bearer <token>` 头，无状态。

## 4. 路径解析（path → fileId）

### 4.1 路径来源

从 strm 地址的 **`path` 查询参数**取值。例如:

```
https://vault.bjyt.de/redirect?path=/影视库/最新电影/72小时 (2026)/72小时 (2026).mkv&pickcode=...&storage=googledrive-1
                              ↑ 取这个
```

- 用 `url.Parse` → `Query().Get("path")` 取出（**自动完成 percent-decode**，得到含 `/` 与中文的真实路径）
- 路径必须以 `/` 开头；按 `/` 切分，丢弃空段
- 取不到 `path` 参数或路径为空 → 视为"无法用 Drive 取流"，回退现有行为（**不是错误**，只记 Info）

### 4.2 逐层解析

Drive API **没有按路径查文件的接口**（官方确认，见 prd R1），必须逐层遍历:

```
parentId = root-folder-id
for 每一段 name:
    files.list(
        q = "'<parentId>' in parents and name = '<name>' and trashed = false",
        corpora = drive,
        driveId = <drive-id>,
        includeItemsFromAllDrives = true,
        supportsAllDrives = true,
        fields = nextPageToken, files(id, name, mimeType)
    )
    → 取第一个结果作为下一层的 parentId
    → 取不到 → 解析失败
```

**共享云端硬盘的必需参数**（官方: 缺失会导致"shared drive items are not included in the response"，表现为查不到任何东西）:

| 参数 | 值 | 作用 |
|---|---|---|
| `supportsAllDrives` | `true` | 声明应用支持共享云端硬盘 |
| `includeItemsFromAllDrives` | `true` | 结果中包含共享云端硬盘条目 |
| `corpora` | `drive` | 只在指定团队盘内搜索 |
| `driveId` | 配置值 | 指定团队盘 |

**`q` 中的名称必须转义**（官方: 需转义特殊字符，如 `name contains 'quinn\'s paper\\essay'`）:
单引号与反斜杠要转义，否则名称里带 `'` 的文件会查询失败或注入查询语法。

**路径段名称里的 `'` 与 `\` 是真实存在的风险**（中文媒体库里带英文撇号的片名很常见），必须有转义测试。

### 4.3 重名处理

官方**不保证**同一文件夹内名称唯一（见 prd Q5）。取舍:

- 若查询返回多条，**取第一条**并在日志中记 Warn（含命中条数），便于事后发现
- 不引入更复杂的策略（如按修改时间取最新）——在需求明确之前，简单可预测优于聪明

## 5. 取流

```
GET https://www.googleapis.com/drive/v3/files/{fileId}?alt=media&supportsAllDrives=true
Authorization: Bearer <access_token>
Range: <透传客户端的 Range>
```

- `supportsAllDrives=true` 必需（团队盘文件）
- 响应:**原样透传** `Range` 相关语义 —— 206 / `Content-Range` / `Content-Length` / `Content-Type`
- 与现有实现一致: 206 缺 `Accept-Ranges` 时补 `bytes`；逐跳头剔除
- **数据源切换对客户端完全透明**: 客户端拿到的仍然只是字节流，不知道数据来自网关还是 Google

## 6. 缓存

新增一类缓存:**路径 → fileId**（与现有的"直链缓存"不同，后者在此场景下没有对象可缓存）。

| 项 | 设计 |
|---|---|
| key | 归一化后的路径（如 `/影视库/最新电影/72小时 (2026)/72小时 (2026).mkv`） |
| value | `fileId` |
| 存储 | 进程内 `sync.Map`，读时惰性清理过期项 |
| TTL | 配置 `path-cache-expired`，默认 `7d` |
| 不落盘 | 重启后每个文件重新解析一次（4 层 = 4 次 `files.list` = 400 配额单位）。以项目配额 325,000 单位/分钟衡量，即使一次重启后播放上百个文件也不构成压力。落盘引入文件读写与并发一致性成本，收益不足以抵消 |

**注意与现有直链缓存的关系**: 解析出 fileId 后，取流 URL 是 `{fileId}` 直接拼出来的，**不需要再缓存 URL**。所以现有的 `linkCache`（缓存 3xx 跳转后的最终地址）在本数据源下同样不会生效——这没问题，因为它缓存的对象在这里不存在。

**真正的收益**: 路径解析从"每次请求 4 次 API 调用"变成"每个文件一次"。这是本任务相对现状的主要性能改善。

## 7. 日志

沿用 `[直链代理]` 前缀（同一套排查入口），新增条目:

| # | 时机 | 级别 | 消息模板 |
|---|---|---|---|
| L20 | 选定数据源 | Info | `[直链代理] 数据源: Google Drive API` / `[直链代理] 数据源: 原地址(网关)` |
| L21 | 路径解析开始 | Info | `[直链代理] 解析 Google Drive 路径: %s` |
| L22 | 路径解析成功 | Success | `[直链代理] 路径解析成功: %s -> fileId=%s, 用时 %s` |
| L23 | 路径解析命中缓存 | Info | `[直链代理] 路径缓存命中: %s -> fileId=%s` |
| L24 | 路径解析失败 | Warn | `[直链代理] 路径解析失败: %s, 失败段: %s, err: %v` |
| L25 | 同名多条 | Warn | `[直链代理] 路径段命中 %d 条同名条目, 取第一条: %s` |
| L26 | 令牌刷新 | Info | `[直链代理] Google 访问令牌已刷新, 有效期 %s` |
| L27 | 令牌刷新失败 | Error | `[直链代理] Google 访问令牌刷新失败: %v` |
| L28 | 回退到原地址 | Warn | `[直链代理] Google Drive 取流失败, 回退原地址: %v` |

**严禁打印**: `client_secret`、`refresh_token`、`access_token`。错误信息里若可能包含这些（如 OAuth 错误响应体），需要先过滤。
**注意**: L21/L22 打印的是文件路径，可能包含媒体库结构信息，但**不含凭据**，与现有日志级别一致（现有日志已打印完整 strm 地址）。

## 8. 错误与回退

| 阶段 | 失败情形 | 处理 |
|---|---|---|
| 配置 | `enable: false` 或凭据不全 | 完全走现有流程，零 Google 相关日志 |
| 路径提取 | 地址里没有 `path` 参数 | Info 日志，回退原地址 |
| 令牌 | refresh 失败 | Error + 回退原地址；**不重试**（回退本身就是更好的兜底） |
| 解析 | 某一层查不到 | Warn(L24) + 回退原地址 |
| 解析 | 超过 `max-path-depth` | Warn + 回退原地址 |
| 取流 | Drive 返回 4xx/5xx | Warn(L28) + 回退原地址 |
| 取流 | 已开始传输后中断 | 与现有实现一致：无法回退，记 Warn |

**统一原则**: 所有 Google 侧失败都回退到"请求原 strm 地址"。由于该地址仍然可用（网关还在），用户不会因为新功能出问题而看不了片。

## 9. 模块落点

新增包 `internal/service/gdrive/`:

| 文件 | 职责 |
|---|---|
| `gdrive.go` | 对外入口: 判断能否接管、解析并取流 |
| `token.go` | access_token 换取与缓存（含并发去重）；refresh_token 的读取（配置优先, 其次落盘） |
| `oauth.go` | 内置授权流程: `/oauth/start` 与 `/oauth/callback` 两个 handler、state 管理、token 落盘 |
| `resolve.go` | 路径 → fileId（含缓存与转义） |
| `api.go` | Drive API 调用封装（files.list / files.get） |
| `type.go` | 纯类型定义 |
| `log.go` | `[直链代理]` 前缀日志封装（与 streamproxy 的一致） |

token 落盘路径: `<dataRoot>/gdrive-token.json`（`dataRoot` 即启动参数 `-dr` 指向的目录）, 权限 **0600**。

依赖方向: `gdrive` → `config` / `util/https` / `util/logs` / `util/strs`。**不依赖 `internal/web/...`**（与 `streamproxy` 同规矩）。

**关于 gin 的例外**: `token.go` / `resolve.go` / `api.go` 是纯逻辑, **不得依赖 gin**（便于单测）;
但 `oauth.go` 里的两个函数是 gin 处理器, 必须用 `*gin.Context`。
这与项目既有惯例一致 —— `internal/service/emby/*` 与 `internal/service/log.go` 的处理器都在 service 包里用 gin。
因此**只对 oauth.go 放开 gin 依赖**, 其余文件保持纯净。

**与 `streamproxy` 的分工**（避免职责重叠）:

- `streamproxy` 负责: 命中判断、归一化、响应头回写、流式转发、并发槽位、回退语义
- `gdrive` 负责: 把"取流目标"从"一个 URL"变成"一个 Drive 文件"

因此 `gdrive` 不重复实现流式转发，它只提供**取流地址与请求头**（`{apiURL}?alt=media` + `Authorization`）交给 `streamproxy` 已有的转发逻辑。

### 接口草案

```go
// CanHandle 判断该 strm 地址能否用 Google Drive 直接取流
//
// 只做静态判断(开关、能否取出 path 参数), 不发起任何网络请求
func CanHandle(rawURL string) (filePath string, ok bool)

// FetchStream 解析路径并请求媒体数据
//
// 返回的 http.Response 已带 Range, 调用方负责关闭 Body
func FetchStream(ctx context.Context, filePath string, clientRange string) (*http.Response, error)
```

`streamproxy` 侧需要的改动: 在 `resolveLink` 里插入一次数据源选择，其余（转发、回写、日志、并发）保持不动。

## 10. 内置 OAuth 授权流程

### 11.1 为什么内置

refresh_token 会在这些情况下失效: OAuth 同意屏幕为「外部+测试」时的 7 天有效期、管理员撤销授权、账号密码变更等。
如果靠人工到外部工具换取, **每次失效都要重走一遍完整流程**, 实际不可维护。
内置之后, 重新授权 = 浏览器打开一个链接点同意。

### 11.2 硬性前提: 重定向 URI 必须 HTTPS + 域名

官方规则（原文）:

> **Redirect URIs must use the HTTPS scheme, not plain HTTP.**
> **Hosts cannot be raw IP addresses**（仅 localhost IP 例外）

因此 `http://<公网IP>:8099/ge2o/gdrive/oauth/callback` **会被 Google 直接拒绝**。
部署方必须:

1. 用域名 + TLS 暴露 ge2o 的回调路径（Zouter 上已有 `lucky` 监听 443, 可加一条反代规则）
2. 在 Google Console 的「已获授权的重定向 URI」中登记**完全一致**的 URI

> ⚠️ Google 的匹配规则是 **scheme、大小写、结尾斜杠都必须完全一致**, 否则报 `redirect_uri_mismatch`。
> 配置项 `redirect-uri` 的值必须与 Console 里登记的逐字符相同。

**注意**: 这不影响客户端播放 —— 客户端仍可用 `Zouter:8099` 明文连接。
HTTPS 域名只需要覆盖 `/ge2o/gdrive/oauth/*` 两个路径。

### 部署现状（2026-09-16 实测确认, 基础设施已就绪）

| 项 | 状态 |
|---|---|
| `go.bjyt.de` DNS | 已指向 Zouter `155.117.82.69` |
| TLS 证书 | 已就绪（泛域名 `*.bjyt.de`, ZeroSSL 签发） |
| 域名 → ge2o 转发 | **已生效**。实测带标记请求 `GET /ge2o/gdrive/oauth/callback?marker=...` 出现在 ge2o 访问日志中, 客户端 IP 为 `155.117.82.69`（本机 lucky）, 证明流量确实到达 ge2o |
| Google Console 登记 | **待开发者完成** —— 需加入 `https://go.bjyt.de/ge2o/gdrive/oauth/callback` |
| OAuth 客户端类型 | 必须是「Web 应用」; 桌面/移动类型不支持 HTTPS 重定向 |

### 11.3 流程

```
1) 管理员浏览器打开:  http://<ge2o>:8099/ge2o/gdrive/oauth/start?secret=<ge2o api-secret>
2) ge2o 生成随机 state 并暂存, 302 到 Google 授权端点:
     https://accounts.google.com/o/oauth2/v2/auth
       ?client_id=<client-id>
       &redirect_uri=<redirect-uri>
       &response_type=code
       &scope=https://www.googleapis.com/auth/drive.readonly
       &access_type=offline        ← 不带的活拿不到 refresh_token
       &prompt=consent             ← 再次授权时也强制返回 refresh_token
       &state=<random>
3) 管理员登录 Google 并同意授权
4) Google 302 回: <redirect-uri>?code=...&state=...
5) ge2o 校验 state(不匹配则拒绝), 再 POST 换取令牌:
     POST https://oauth2.googleapis.com/token
       code / client_id / client_secret / redirect_uri / grant_type=authorization_code
6) 响应中的 refresh_token → 落盘持久化; 页面显示成功
```

### 11.4 关键设计点

| 点 | 决定 | 理由 |
|---|---|---|
| `state` 参数 | 随机生成、回调时校验，**每个 state 一次性有效** | 防 CSRF。没有它, 攻击者可诱导管理员完成一次指向攻击者账号的授权 |
| `/oauth/start` 的保护 | 要求携带 `ge2o.api-secret` | 该路由不应被任意人随意触发 |
| 回调路由 | **不能**要求 api-secret | Google 的跳转无法携带我们的自定义参数, 只能靠 `state` 保护 |
| refresh_token 持久化 | 写入 `<dataRoot>/gdrive-token.json`, 权限 **0600** | 内存存储会在重启后丢失, 而重启后管理员无法察觉, 只表现为"又要重新授权" |
| 已存在有效 token 时 | `/start` 页面提示"当前已有授权, 重新授权将覆盖", 不自动覆盖 | 避免误操作把好 token 冲掉 |
| 授权失败 | 页面显示错误原因（**不含凭据**）, 日志记 Error | 管理员需要知道是 scope 不对、还是账号无权限 |
| `refresh-token` 配置项 | 变为**可选**: 配置了就用配置的, 没配置则用落盘的; 两者都有时以配置为准 | 兼容"手工获取"与"内置流程"两种用法 |

### 11.5 新增路由

遵循项目的路由约定（`internal/constant/constant.go` 定义常量 + `internal/web/route.go` 加一行规则，**必须排在 `Reg_All` 之前**）:

| 路由 | 处理 |
|---|---|
| `/ge2o/gdrive/oauth/start` | 校验 api-secret → 生成 state → 302 到 Google |
| `/ge2o/gdrive/oauth/callback` | 校验 state → 换令牌 → 落盘 → 返回结果页 |

`Route_SelfBase = "/ge2o"`, 与既有 `Route_ValidateApiSecret` / `Route_SyncServerLog` 同级。

## 11. 附录: 手工获取 refresh_token（备选路径）

`client_id` / `client_secret` 在 Google Cloud Console 里建 OAuth 客户端即可得到。
**`refresh_token` 必须通过一次授权流程换取**, 不能凭空生成。

### 操作步骤（官方 OAuth 2.0 Playground）

1. 打开 https://developers.google.com/oauthplayground/
2. 右上角**齿轮 ⚙️** → 勾选 **"Use your own OAuth credentials"** → 填 `client_id` / `client_secret`
3. 左侧选 **Drive API v3** → 勾选 **`https://www.googleapis.com/auth/drive.readonly`**
4. 点 **"Authorize APIs"** → 用**有团队盘访问权**的 Google 账号登录并同意
5. 点 **"Exchange authorization code for tokens"**
6. 响应中的 **`refresh_token`** 即为所需值

### 四个已知的坑

| # | 坑 | 后果 | 规避 |
|---|---|---|---|
| 1 | **同意屏幕为「外部 + 测试」状态时, refresh_token 只有 7 天有效期** —— 官方原文: *A GCP project whose consent screen is set to "external user type and a publishing status of 'Testing'" is issued a refresh token expiring in 7 days* | **最恶性的一类故障**: 部署后能正常用, 7 天后突然全部失败, 且现象与"代码坏了"无法区分 | 有 Workspace 时把同意屏幕用户类型设为 **Internal**（推荐）; 否则将应用 **Publish** |
| 2 | scope 必须是 `drive.readonly`（内容读取）, **不是** `drive.metadata.readonly` | 官方明确后者不足以下载文件内容; 表现为 403 | Playground 里勾选时确认 |
| 3 | 授权账号必须能访问目标团队盘 | 表现为路径解析在第一层就查不到 → 但**看起来像配置错误**, 容易误判 | 登录时确认账号 |
| 4 | 响应里没有 `refresh_token` | Google 只在**首次授权**返回 refresh_token | 到 https://myaccount.google.com/permissions 撤销该应用授权后重走一遍 |

### 对排查的意义

第 1 条决定了本项目**必须能区分**"凭据失效"与"其它故障"。
`design.md` §7 的 L27（令牌刷新失败）与 §8 的回退表就是为此设计的:
令牌刷新失败会记 Error 且自动回退到网关, 播放不会中断, 但日志会明确指出是令牌问题。
**若部署 7 天后出现 L27, 第一嫌疑就是同意屏幕的测试状态。**

## 12. 触发条件修订: 本地挂载路径（2026-09-16 实现阶段, 实测后修订）

### 12.1 初版假设不成立

设计初版假设 strm 内容是 **http 地址**（`https://vault.bjyt.de/redirect?path=...`），gdrive 在 `streamproxy.resolveLink` 里按 URL 的 `path` 参数触发。

**实测（2026-09-16 首次真实播放）发现实际情况完全不同**:

```
Emby 报出的 MediaSources[].Path =
  /home/googleDrive/影视库/媒体库/电视剧/国产剧/知否知否应是绿肥红瘦 (2018) {tmdb-81502}/Season 1/...第3集.mp4
```

**这是本地文件系统路径, 不是 URL。** 后果:

- `urls.IsHttpRemote(embyPath)` 为 **false** → 代码根本进不了 strm 分支
- 于是掉进 OpenList 分支 → `openlist.host 配置为空` 报错 → `checkErr` → **回源**
- 实测那次播放的日志里**没有任何 `[直链代理]` 输出**, 数据由 Emby 自己从挂载点读取（几个 206 请求耗时 5~34 秒）

### 12.2 实际拓扑（实测确认）

`gmby_server` 容器（OVH, 对外 8099）内有两个 fuse 挂载:

```
google{_p9vl}  on /home/googleDrive   ← rclone 挂载的 Google Drive 团队盘
CloudFS        on /home/CloudNAS      ← CloudDrive2 挂载的 115 网盘
```

strm 内容是对应挂载点下的**文件路径**:

| strm 内容 | 存储 | 本功能是否处理 |
|---|---|---|
| `/home/googleDrive/影视库/...` | Google Drive 团队盘 | **是** |
| `/home/CloudNAS/115/影视库/...` | 115 网盘 | 否（Drive API 取不到, 保持现有行为） |

**挂载前缀的作用**: 让 Emby 能通过挂载点读到文件元信息（时长/编码）。团队盘里**不存在这一层**, 因此必须先去掉前缀再解析。

### 12.3 修订后的触发条件

```
embyPath 以配置的挂载前缀开头 (默认 /home/googleDrive)
  → 去掉前缀, 得到团队盘内路径
  → 解析为 fileId → files.get?alt=media → 代理给客户端
  → 失败则回源 (Emby 自己从挂载点读, 即改动前的行为)
```

**边界规则**: 前缀之后必须紧跟 `/` 或字符串结束 —— 防止 `/home/googleDriveBackup/...` 被误命中。

### 12.4 配置

新增:

```yaml
gdrive:
  # strm 内容里指向 Google Drive 挂载点的前缀
  #
  # 命中后去掉该前缀, 剩下的部分即团队盘内的路径。
  # 该前缀是为 Emby 读取元数据而存在的, 团队盘里并没有这一层。
  # 用 /home/googleDrive 之外的前缀时改这里。
  mount-prefix: /home/googleDrive
```

校验: 必须以 `/` 开头且不以 `/` 结尾（结尾斜杠会导致拼出 `//`）。

### 12.5 代码落点

| 位置 | 改动 |
|---|---|
| `internal/config/gdrive.go` | 新增 `MountPrefix` 字段 + 校验 |
| `internal/service/gdrive/resolve.go` | 新增 `MatchMountPath(strmContent) (gdPath string, ok bool)` —— 前缀匹配 + 边界校验 + 去前缀 |
| `internal/service/streamproxy/streamproxy.go` | 把 `Proxy` 的"回写响应头 + 流式传输"后半段抽成内部 `relay(w, r, resp)`；新增导出 `ProxyGDrive(w, r, gdPath)`, 与 `Proxy` **共用** `relay` |
| `internal/service/emby/redirect.go` | **新增分支**（见下） |

`redirect.go` 的分支必须放在 `urls.IsHttpRemote(embyPath)` **之外、之前**（挂载路径不是 http, 放在里面永远进不去）:

```go
// 4 Google Drive 直连: strm 内容是 rclone 挂载路径时, 去掉挂载前缀后从 Drive API 取流
if gdPath, ok := gdrive.MatchMountPath(embyPath); ok {
    logs.Info("[直链代理] 检测到 Google Drive 挂载路径: %s -> %s", embyPath, gdPath)
    go sendOpenStreamPlaybackInfoReqToOrigin(itemInfo)
    written, proxyErr := streamproxy.ProxyGDrive(c.Writer, c.Request, gdPath)
    if written {
        return
    }
    // 尚未写入任何响应 → 回源, 由 Emby 从挂载点直接读取（即改动前的行为）
    logs.Warn("[直链代理] Google Drive 取流失败, 回源处理: %v", proxyErr)
    ProxyOrigin(c)
    return
}
```

**注意**: 失败时直接 `ProxyOrigin(c)` 而不是"落回原有流程"。因为原有流程会走到 OpenList 分支并打出误导性的 `openlist.host 配置为空` 错误, 而最终结果同样是回源。

### 12.6 与初版 URL 触发的关系

`streamproxy` 里基于 URL 域名匹配的触发**保留不动**（已测试、零成本）。它现在只是不再被实际用到 —— 所有 strm 都是挂载路径了。将来确认无用了可以单独清理。

## 13. 已排除的方案

| 方案 | 排除理由 |
|---|---|
| 用 `webContentLink` 当直链 | 官方明确它不是稳定可复用的 CDN 端点；且大文件有病毒扫描确认页问题 |
| 用 API Key（不用 OAuth） | 官方下载接口不支持 API Key |
| 每次请求都重新解析路径 | 4 层路径 = 4 次 `files.list` = 400 配额单位/请求；播放器一次播放发多个 Range 请求，浪费且增加延迟 |
| 用 `corpora=allDrives` 代替 `corpora=drive`+`driveId` | 官方明确它可能返回 `incompleteSearch: true`（结果有遗漏），且效率更低 |
| 缓存取流 URL | 不需要——URL 由 fileId 直接拼出，缓存 fileId 已经足够 |
| 让客户端 302 到 Google API | 客户端无法携带 `Authorization` 头，且这正是当初需要代理的原因 |
| 路径解析结果落盘 | 收益（省 4 次 API 调用/文件/重启）远小于成本（文件读写、并发一致性、权限）。见 §6 |
