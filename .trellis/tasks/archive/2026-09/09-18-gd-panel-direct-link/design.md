# Design · 接入 GD 管理面板直链接口

## 1. 现状

```
emby/redirect.go:100  gdrive.MatchMountPath(embyPath)
        │  命中挂载前缀
        ▼
streamproxy.ProxyGDrive ──▶ gdrive.FetchStream
                              ├─ resolveFileId   (files.list 逐层解析, 带缓存 + singleflight)
                              └─ filesGetMedia   (files.get?alt=media)
                              └─ getAccessToken  (OAuth refresh_token 换 access_token, 落盘)

streamproxy/link.go:214  gdrive.CanHandle(normalizedURL)   ← 另一条入口: strm 是 http 地址时取 ?path=
```

要拆掉的是 `gdrive` 包内的**全部** Drive 侧实现，以及 `link.go` 里的第二条入口。

## 2. 目标模块边界

依赖方向保持不变：`gdrive -> config / util/https / util/logs / util/strs`。
gdrive **不**依赖 gin、不依赖 `internal/web/...`、不认识 Emby。
字节流的转发、响应头回写、并发槽位、回退语义仍全部由 `internal/service/streamproxy` 负责。

对外暴露面收敛为三个函数（与现在一致，调用方几乎不用改）：

```go
func IsEnabled() bool
func MatchMountPath(strmContent string) (gdPath string, ok bool)
func FetchStream(ctx context.Context, gdPath, clientRange string) (*http.Response, error)
```

## 3. 文件落点

```
internal/service/gdrive/
  gdrive.go      包文档 + IsEnabled + FetchStream(编排: 取直链 → 拉流 → 失效重试一次)
  mountpath.go   MatchMountPath + normalizeMountPrefix        (自 resolve.go 迁入)
  panel.go       /api/dl 调用、响应/错误信封解析、错误码 → 中文错误
  fetch.go       带 headers 请求 Google 下载地址, 接受 200/206
  cache.go       路径 → (url, headers, expireAt) 进程内缓存
  sanitize.go    redactSecret / sanitizeUpstreamText 体系    (自 token.go 迁入并收敛)
  type.go        内部类型
  log.go         不变
```

删除：`oauth.go`、`token.go`、`resolve.go`、`api.go` 及其全部 `_test.go`。

## 4. 配置设计

`internal/config/gdrive.go`：

| 字段 | 处理 |
|---|---|
| `Enable` | 不变 |
| `ApiBase` | 新增。`strings.TrimSpace` + `TrimRight("/")`；启用时必须为 http/https 绝对地址 |
| `ApiToken` | 新增。`TrimSpace`；启用时非空；`Init` 中先取 `GDRIVE_API_TOKEN` 环境变量，非空则覆盖 |
| `MountPrefix` | 不变（含 `validateGDriveMountPrefix`） |
| `ClientId`/`ClientSecret`/`RefreshToken`/`RedirectUri`/`DriveId`/`RootFolderId`/`PathCacheExpired`/`MaxPathDepth` | 删除 |
| `pathCacheExpire()` | 删除；缓存 TTL 改由 `expires_at` 推导（见 §6） |

环境变量名常量 `GDriveApiTokenEnvName = "GDRIVE_API_TOKEN"`，导出以便文档与测试引用。

## 5. 面板客户端

### 5.1 请求

```go
endpoint := cfg.ApiBase + "/api/dl?path=" + url.QueryEscape(gdPath)
req := https.Get(endpoint).
    AddHeader("Authorization", "Bearer "+cfg.ApiToken).
    AddHeader("Accept", "application/json").
    AddHeader("Accept-Encoding", "identity").
    Context(ctx)
resp, err := req.DoSingle()
```

- 面板调用用 `DoSingle()`：`/api/dl` 是直接返回 JSON 的同步接口，无需跟随跳转；
  跟随反而会把配置错误掩盖成"看起来正常"
- 独立超时：`context.WithTimeout(ctx, panelRequestTimeout /* 15s */)`，
  共享 client 的 `ResponseHeaderTimeout` 是 5 分钟，对毫秒级面板请求太宽
- `Accept-Encoding: identity` 与 `http-proxy.md` 规范一致，避免 Transport 追加 gzip 并透明解压

### 5.2 响应类型

```go
type directLink struct {
    URL       string            // data.url
    Headers   map[string]string // data.headers
    ExpiresAt time.Time         // data.expires_at (RFC3339, 带 Z)
    FileId    string            // data.file.id  (仅用于日志)
    FileSize  int64             // data.file.size
}
```

### 5.3 错误映射

```json
{"ok": false, "error": {"code": "PATH_NOT_IN_CACHE", "message": "中文错误信息"}}
```

- 统一构造 `取直链失败 [CODE] message`，**直接沿用面板的中文 message**，不另编文案
- 面板未给结构化错误时退化为 `面板接口返回 <status>`
- 4xx/5xx 一律不重试（401 明确要求不重试；其余重试也无意义）
- 调用方统一按「失败 → 回退原地址」处理，不按 error code 分支

### 5.4 这条链路零重定向，不做跳转跟随

**实测结论（2026-09-18）**：抽样 10 个已缓存文件（9 个视频 + 1 张 jpg，最大 9.3GB），
用面板 `/api/dl` 返回的真实直链、带 `Authorization` 请求，只读响应头：

- **10/10 返回 200，无 `Location` 头，零重定向**
- `Content-Type` 直接是 `video/x-matroska` / `image/jpeg`，字节直接从 `www.googleapis.com` 出来

带 OAuth 凭据的 `files.get?alt=media` 在共享盘这条路上就是直接吐字节的，不跳转。

**因此：**

- **不新增**自定义 `CheckRedirect`，**不改动** `internal/util/https`，不为跳转写任何代码
- ⚠️ **不要在代码注释或本文档里写「Google 会 302」「跨主机跳转会丢 Authorization」**。
  那是另一个端点的行为（`drive.google.com/uc?export=download` 会 303 到
  `drive.usercontent.google.com`，大文件还撞病毒扫描页），与本链路无关；
  写进去会让后来的人去修一个不存在的问题，比多写 25 行代码贵得多
- 外部接口文档 `GD_DIRECT_LINK_API.md` §6.2 的同类说法同样不适用本链路，不要照它实现

**状态码策略**（与跳转无关，纯防御）：只接受 200/206，其余一律判为失败并回退原地址。
若上游行为将来真的变了，回退到 Emby 直读挂载点仍能播放，日志里能看到实际状态码。

选 `DoSingle()`（不自动重定向）而不是 `Do()`，理由同样与跳转无关：

- 面板接口不应该 3xx，跟随会把配置错误掩盖成"看起来正常"
- `RequestHolder.execute` 的手写重定向循环不会 Close 中间的 3xx 响应体，
  连接在 body 关闭前不归还连接池

### 5.5 拉流

```go
header := make(http.Header, len(link.Headers)+2)
for k, v := range link.Headers { header.Set(k, v) }
if strings.TrimSpace(clientRange) != "" { header.Set("Range", clientRange) }
header.Set("Accept-Encoding", "identity")

resp, err := https.Get(link.URL).Header(header).Context(ctx).DoSingle()
```

- **只接受 200/206**；3xx 与其余状态码一律读一小段响应体用于日志后 `Close`，
  返回带状态码的错误，由调用方回退
- 返回的 `resp.Body` **不读取**，交给 `streamproxy.relay` 流式转发
- 日志只打 `status` / `content-type` / `content-length` / `accept-ranges`，**绝不打 headers**

## 6. 两级缓存

令牌与直链的**生命周期完全不同**（一个按小时过期、一个长期有效），因此分开缓存：

```
┌── 令牌槽 (全局唯一, 有有效期) ────────────────────────┐
│ headers   map[string]string   账号级 Google 凭据       │
│ expireAt  面板给的 expires_at - linkCacheSafetyMargin  │
└──────────────────────────────────────────────────────┘

┌── URL 缓存 (按 Drive 路径, 长期有效) ─────────────────┐
│ url       面板给的 data.url, 附写入时的令牌代次        │
└──────────────────────────────────────────────────────┘
```

### 6.1 令牌槽：全局唯一，失效点只有一个

面板返回的 `Authorization` 是**账号级**令牌，对共享盘内所有文件通用，
因此**全局只存一份**，不按 path 复制 N 份 —— 顺带让失效点也只有一个：

- 有效期内所有路径共用同一份 headers
- 失效时重新调**任意一次** `/api/dl` 即可换新（顺带也刷新那个路径的 URL）
- 并发失效由 singleflight 合并（见 §6.4）

### 6.2 缓存余量：30 秒（必须 < 面板的提前刷新窗口）

```go
// panelTokenRefreshAhead 面板侧提前多久开始发放新令牌
//
// 服务端行为, 本项目改不了; 这里只作为下面不变式的依据记录在案
const panelTokenRefreshAhead = 60 * time.Second

// linkCacheSafetyMargin 本项目缓存提前多久作废
//
// 必须【严格小于】panelTokenRefreshAhead, 否则会出现一段病态窗口:
// 本项目已判缓存失效 -> 重新调面板 -> 面板仍在发放【同一个旧令牌 + 同一个 expires_at】
// -> 算出的 TTL 仍 ≤ 0 -> 继续不缓存 -> 该窗口内【每个 Range 请求都重新打一次面板】。
//
// 余量取 5 分钟时这个窗口是 240 秒, 与「3 小时约 3~4 次面板调用」直接矛盾,
// 且每个 Range 请求多 150~500ms, 拖进度条会卡。
const linkCacheSafetyMargin = 30 * time.Second
```

余量 30 秒 < 60 秒之后该窗口消失：本项目判失效的那一刻，面板已经在发新令牌了。

不用 0 的原因：0 意味着一直用到 `expires_at` 那一瞬间，请求可能正好卡在过期边界上发出。

**不变式用测试钉住**：单独一个用例断言 `linkCacheSafetyMargin < panelTokenRefreshAhead`，
避免以后有人调大余量又把窗口调回来。

其余规则：

- 令牌 TTL = `expires_at - now - linkCacheSafetyMargin`，封顶 `maxLinkCacheTTL(1h)`
- `expires_at` 为零值、**解析失败**、或 TTL ≤ 0 时**一律不写令牌槽**。
  解析失败按"没有过期信息"处理，而不是猜一个默认值
- 只存内存，不落盘：`headers` 含账号级 Google 凭据

### 6.3 URL 缓存

- key 用替换后的 Drive 路径，与面板的 `path` 参数同源
- **不设自身的过期时间**：直链长期有效（用户实测/声明），不靠 TTL 失效
- 条目记一个**写入时的令牌代次**：失效重试要求这条直链也是在失败之后重新取过的
  （只判令牌是不够的, 见 §7）
- 只存地址与一个代次, 不随媒体体积增长；进程重启即清空；不引入后台清理协程

### 6.4 并发：singleflight 合并刷新

一次 401 波会让 N 个并发请求同时发现令牌失效，若各自重取就会打出 N 次并发面板调用。
因此**刷新动作**用 `singleflight.Group` 合并：

- key 用 **Drive 路径**（面板一次调用同时产出该路径的直链与全局令牌；
  用全局 key 时，其它路径的等待方拿不到自己的直链）
- 组内执行时用 `context.WithoutCancel(ctx)` + `panelRequestTimeout(15s)` 兜底：
  刷新结果是被所有人复用的，不能因为某一个发起方取消而让等待方一起拿到 `context.Canceled`。
  比旧实现简单得多 —— 这里只是一次 150~500ms 的面板调用，不是逐层遍历 Google API
- 合并的是**刷新动作**，不是整个 `FetchStream`：每个调用方仍各自持有自己的
  Google 响应与 `resp.Body`，不存在共享响应体的问题

## 6.5 长播放不中断（核心约束）

**约束**：`headers` 里的 Google 令牌约 1 小时过期，而一次播放可以持续数小时；
**播放过程中不得因令牌到期而中断**。

三层保护，任何一层单独成立都不会中断：

| 层 | 机制 | 覆盖的场景 |
|---|---|---|
| 1 不会用到过期令牌 | 令牌槽 TTL 由 `expires_at` 反推（提前 **30 秒**作废，见 §6.2），过期条目读时直接判失效 | 播放中途的新 Range 请求 |
| 2 过期即重新取头 | 令牌槽失效 → 重新调面板（URL 命中缓存则只更新令牌）| 播放跨越 1 小时边界 |
| 3 撞上失效也能自愈 | 401/403/404/410 → 要求比失败的那一份**更新**的令牌与直链，触发重取一次（见 §7）| 时钟偏差、令牌被提前吊销、`expires_at` 不准、直链失效 |

关键性质：**第 2 层和第 3 层对客户端完全透明** —— 它们发生在 `ProxyGDrive` 写出任何响应之前，
客户端看到的仍是一次正常的 206。

开销：一次 3 小时的播放大约触发 3~4 次面板调用（每次 Range 请求都先查缓存，只有缓存失效时才打面板），
不是每个 Range 请求都打。§6.2 的 30 秒余量正是为了让这句成立 —— 余量取 5 分钟时，
每跨一次小时边界会出现 240 秒的"每个 Range 请求都打面板"窗口。

### ⚠️ 一条未经实测量化的假设

**已经在传输中的那条连接**，如果在播放中途跨过了 1 小时令牌边界，Google 是否会把它掐断 ——
**我没有验证过，也不打算在文档里断言。** 需要一次跨越 1 小时的真实播放才能测出来。

如果会掐断，现象是：客户端那条连接提前结束 → 播放器按 Range 重新请求 → 命中第 2/3 层拿到新头
→ 从断点继续。也就是**短暂卡顿，不是播放失败**。如果不会掐断，则连卡顿都没有。

这一条建议上线后在真实环境验一次（播一部超过 1 小时的片子，看 `[直链代理]` 日志里
是否出现「清除缓存重取」以及传输是否连续）。

## 7. 失效重试

```go
func FetchStream(ctx, gdPath, clientRange) (*http.Response, error) {
    for attempt := 0; ; attempt++ {
        target, err := resolveTarget(ctx, gdPath)   // URL 缓存 + 令牌槽, 各自命中即不请求面板
        if err != nil { return nil, err }

        resp, err := fetchDirect(ctx, target, clientRange)
        if err == nil { return resp, nil }

        var fe *fetchError
        if attempt == 0 && errors.As(err, &fe) && retryable(fe.StatusCode) {
            // 不作废缓存, 而是要求"比刚才失败的那一份更新":
            // 令牌与直链各带一个代次, 两者都必须比 minGeneration 新才算命中,
            // 否则 singleflight 刷新(并发共享同一次面板调用)
            continue
        }
        return nil, err
    }
}
```

**只重试一次**：`attempt == 0` 是唯一的重试判据，`attempt == 1` 时任何失败都直接返回错误，
交由上层回退。不存在循环重试或退避。

**并发共享刷新结果**：刷新走 §6.4 的 singleflight（按 Drive 路径合并）。
一波 401 打来时，N 个并发请求里只有第一个真正调用面板，其余等待并复用同一份新令牌 ——
**不会打出 N 次并发重取**。每个请求随后各自重试自己的那次 Google 请求（各自持有独立的响应体）。

- `retryable` 用**本包自己的常量**：`panelRetryStatusCodes = []int{401, 403, 404, 410}`
- **必须包含 401**：Google API 对过期/无效凭据返回 401（`Invalid Credentials`），不是 403。
  漏掉它，"令牌过期"这个最该自愈的情况反而会掉到回退分支上。
  （注：这是 Google API 的通用约定，**未在本链路上实测**；但把 401 放进重试集没有任何代价 ——
  它只多覆盖一种失败，不会改变成功路径，上线时随 §6.5 的长播放测试一起验证）
- **不要复用 `emby.strm.proxy.retry-status-codes`**：那个旋钮属于网关代理分支，
  语义是"网关直链失效"。面板路径的这组码是 Google API 的性质，与用户怎么配网关无关；
  复用会造成"改了 strm-proxy 配置却悄悄改变了 GD 面板行为"的隐蔽耦合
- `gdrive.enable` 可以为 true 而 `emby.strm.proxy.enable` 为 false，两者本就不该互相牵连
- **必须在写出任何响应之前完成**：`streamproxy.ProxyGDrive` 拿到 `resp` 才调 `relay`，
  重试发生在 `resp` 之前，因此回退能力不受影响
- 并发槽位在取直链**之前**获取（`acquireSlot` 是 `ProxyGDrive` 的第一步）：
  即使一个请求在等槽位时排了很久，拿到槽位之后才去取头，不会拿着等待期间变旧的头去请求

## 8. streamproxy 改动

- `link.go`：删除 `resolveLink` 的第 0 步（`gdrive.IsEnabled()` / `gdrive.CanHandle` 分支）
  及 `gdrive` import。其余重定向跟随、直链缓存、失效重试（网关侧）逻辑不动
- `streamproxy.go`：`ProxyGDrive` 保留，仅更新文档注释为「从 GD 管理面板取直链并代理」；
  `fetchGDriveMedia = gdrive.FetchStream` 保留（测试注入点不变）
- `writeResponseHeader` / `passthroughResponseHeaders` 不动：
  Google 返回的 `Content-Type`/`Content-Length`/`Content-Range`/`Accept-Ranges`/`ETag`/`Last-Modified`
  都在白名单内

## 9. emby/redirect.go 改动

`MatchMountPath` 分支的形状、位置、回退方式全部不变，只改注释措辞与日志文案
（「从 Drive API 取流」→「从 GD 管理面板取直链取流」）。该分支仍位于
`urls.IsHttpRemote` 判断**之外、之前**，不得移动。

## 10. 清理清单

| 位置 | 动作 |
|---|---|
| `internal/constant/constant.go` | 删 `Route_GDriveOAuthStart`、`Route_GDriveOAuthCallback` |
| `internal/web/route.go` | 删两条路由注册与 `gdrive` import |
| `internal/web/route_internal_test.go` | 删对应用例 |
| `internal/service/gdrive/{oauth,token,api,resolve}.go` | 删 |
| `internal/service/gdrive/*_internal_test.go`（除 mountpath） | 删；`sanitize` 的用例迁到新文件 |
| `internal/config/gdrive_test.go` | 按新字段重写 |
| `internal/config/gdrive_mount_test.go` | 保留，去掉与新字段无关的断言 |
| `internal/config/config_example_test.go` | 改断言为 `api-base`/`api-token` |
| `internal/service/emby/redirect_gdrive_test.go` | 按新面板端点改写假上游 |
| `internal/service/streamproxy/streamproxy_gdrive_test.go` | 同上；删 `CanHandle` 相关用例 |
| `internal/service/streamproxy/link_internal_test.go` | 删 gdrive 分支用例 |
| `config-example.yml` | 重写 `gdrive:` 段 |

## 11. 安全

- `headers` 里的 `Authorization` 是**账号级** Google 凭据：
  - 不回写给客户端（`passthroughResponseHeaders` 白名单天然挡住）
  - 不进日志（新增代码不打印任何 headers 映射）
  - 不落盘
- 面板 Token：只放在请求头；`redactSecret(text, token)` 用于面板错误文案的兜底脱敏
- **不要**对面板返回的文本套用 `whitelistUpstreamText`：那个白名单只放行 ASCII，
  会把中文 message 全打成 `?`，正好毁掉文档强调的「直接展示面板文案」这条诊断链
- 配置校验错误只提字段名，不回显凭据值

## 12. 兼容性、灰度与回滚

- **灰度**：`enable: false` 时与未部署本功能逐字节一致，可先在测试实例开启验证
- **回滚**：改回 `enable: false` 即恢复原行为。需要恢复旧实现时按文件
  `git checkout` 回旧版 `internal/service/gdrive`、`internal/config/gdrive.go`、
  `streamproxy/link.go`，并还原 `route.go` / `constant.go` 两处路由
- 配置向后兼容：旧配置里的 `client-id` 等键在新结构下会被 yaml 忽略，不会导致启动失败；
  但 `enable: true` 且未填 `api-base`/`api-token` 会启动报错（正是期望行为）

## 13. 风险

| 风险 | 应对 |
|---|---|
| 面板侧路径未命中（`PATH_NOT_IN_CACHE`）| **不由本项目处理**：面板侧会改为实时获取，本项目只依赖「永远返回固定响应结构」这一契约，不做任何按错误码的特殊分支 |
| 账号级令牌泄漏 | §11 三条约束 + 单测断言日志与响应头都不含令牌 |
| 缓存 TTL 用错导致过期令牌 | TTL 严格由 `expires_at` 推导并留 **30 秒**边距（必须 < 面板的 60 秒提前刷新窗口，见 §6.2）；边距不足或无法解析则不复用 |
| 面板不可用 | 全部错误走回退，播放退化为 Emby 直读挂载点 |
| 上游状态码策略过严，把可用的响应也判失败 | 只接受 200/206 是防御性下限；失败一律回退原地址，播放不中断且日志可见实际状态码 |
| 测试依赖公网 | 全部用 `httptest` 假面板 + 假 Google 端点 |
