# 执行计划: Google Drive API 直接取流

## 前置

- 设计以 `design.md` 为准；官方文档结论以 `prd.md` 的 Research Findings 为准。
- 复用既有实现：`internal/service/streamproxy/`（转发/回写/并发/回退）、`internal/util/https`（出站）、`internal/util/logs`（日志）。
- 环境：`export PATH=$PATH:/usr/local/go/bin`；`web/dist` 已有占位文件。

## Step 1 配置层

1. `internal/config/gdrive.go`（新文件）：`GDrive` 结构体 + `Init()` 校验，字段与默认值见 `design.md` §2。
2. `internal/config/config.go`：挂上 `GDrive *GDrive` 字段，让反射初始化能走到 `Init()`。
3. `config-example.yml`：新增 `gdrive:` 段，含完整中文注释；**默认 `enable: false`**，凭据字段留空。
4. 校验规则按 design §2 实现；**错误消息只提字段名，绝不回显凭据值**。

## Step 2 `token.go` —— OAuth 令牌

1. 换取 access_token：`POST https://oauth2.googleapis.com/token`，表单编码，走 `util/https`。
2. 进程内缓存 + `sync.Mutex`；提前 `tokenRefreshAhead = 5m` 视为过期。
3. **并发去重**：持锁 → 二次检查是否已被别的 goroutine 刷新 → 否则自己刷新。
4. 失败返回错误（由上层回退原地址），日志 L26/L27，**不含凭据**。
5. 测试：
   - 未过期时复用令牌（假 token 端点计数为 1）
   - 过期时重新换取（计数为 2）
   - **并发 20 个 goroutine 同时请求 → token 端点只被调用 1 次**（并发去重的核心断言）
   - 刷新失败返回错误且不 panic

## Step 2b `oauth.go` —— 内置授权流程

按 `design.md` §11 实现。

1. `internal/constant/constant.go`: 新增两个路由常量（`Route_GDriveOAuthStart` / `Route_GDriveOAuthCallback`）。
2. `internal/web/route.go`: 规则表加两行, **必须排在 `Reg_All` 之前**（否则会被兜底规则吃掉）。
3. `/oauth/start` handler:
   - 校验 `ge2o.api-secret`（缺失或不匹配 → 拒绝, 记 Warn）
   - 生成 **密码学安全**的随机 state（`crypto/rand`, 不可用 `math/rand`）
   - state 入内存**一次性**存储（用后即删）
   - 302 到 Google 授权端点, 参数含 `access_type=offline` 与 `prompt=consent`（**缺任一个都拿不到 refresh_token**）
4. `/oauth/callback` handler:
   - 校验 state（**不匹配或已被用过 → 拒绝**, 记 Error）
   - 用 `code` 换取令牌（POST `https://oauth2.googleapis.com/token`）
   - 拿到 `refresh_token` → 写 `<dataRoot>/gdrive-token.json`, 权限 **0600**
   - 返回一个简单 HTML 结果页（成功/失败 + 原因）；**失败原因不得包含凭据**
5. `token.go` 侧的读取优先级: 配置的 `refresh-token` > 落盘文件 > 报错（提示需要先授权）。
6. 测试（每条都要做"删掉被测逻辑测试必须变红"的验证）:
   - **state 不匹配 → 拒绝**
   - **state 重用 → 拒绝**（一次性）
   - 回调缺少 state → 拒绝
   - `/start` 缺少 api-secret → 拒绝
   - 换取的令牌被正确落盘（写到 `t.TempDir()`, **不得写进仓库**）
   - 配置的 refresh-token 优先于落盘文件
   - 两者都没有时返回明确错误（提示需先授权）
   - `redirect-uri` 配成 `http://` 且非 localhost → **启动即报错**（中文说明）

## Step 3 `resolve.go` —— 路径解析

1. 从 strm 地址取 `path` 参数（`url.Parse` + `Query().Get("path")`，自动解码）。
2. 按 `/` 切分、丢弃空段；段数超过 `max-path-depth` 报错。
3. 逐层 `files.list`，参数严格按 design §4.2 的表格（`supportsAllDrives` / `includeItemsFromAllDrives` / `corpora=drive` / `driveId`）。
4. **`q` 中的名称转义**：转义 `\` 与 `'`。这是必须的 —— 中文媒体库里带英文撇号的片名很常见。
5. 解析结果进 `sync.Map` 缓存（key = 完整路径，value = fileId），TTL 取配置，读时惰性清理。
6. 同名多条时取第一条并记 Warn（L25）。
7. 测试：
   - 正常 4 层路径解析成功
   - **名称含 `'` 的路径段能被正确转义并解析**（回归测试，见 §Review Gate）
   - 名称含 `\` 的路径段
   - 某一层不存在 → 返回错误且日志含失败段名
   - 超长路径超过 `max-path-depth` → 报错
   - 缓存命中时不发起 API 调用（假 API 计数保持不变）
   - 缓存过期后重新解析
   - `path` 参数缺失 / 空路径 → 返回 `ok=false`（不是错误）
   - 同名多条 → 取第一条 + Warn

## Step 4 `api.go` —— Drive API 封装

1. `filesList(ctx, q, pageToken)`：拼装 URL 与参数，加 `Authorization` 头，解析 JSON 响应。
2. `filesGetMedia(ctx, fileId, rangeHeader)`：`?alt=media&supportsAllDrives=true`，透传 `Range`，返回原始 `*http.Response`（**不读 body**，交给上层流式转发）。
3. 参数拼接一律用 `url.Values`，不手工拼字符串。
4. 测试：断言请求 URL 里各参数齐全（尤其 4 个团队盘参数）、`Authorization` 头存在、`Range` 被透传。

## Step 5 `gdrive.go` —— 对外入口

按 `design.md` §9 的接口草案实现 `CanHandle` 与 `FetchStream`。

测试：`enable: false` 时 `CanHandle` 恒为 false，且零网络请求。

## Step 6 接入 `streamproxy`（唯一改动既有代码的地方）

在 `internal/service/streamproxy/link.go` 的 `resolveLink` 中插入数据源选择：

```
命中前缀后, 若 gdrive.CanHandle(rawURL) 成功:
    → 记 L20
    → 用 gdrive.FetchStream 取流
    → 失败则记 L28 并回退到"请求原 URL"
否则:
    → 现有逻辑不变
```

**约束**:
- 只加分支，不改动既有的归一化、请求头构造、重试、缓存逻辑
- 回退路径必须与改动前**逐字节一致**
- 不新增反向依赖（`streamproxy` 与 `gdrive` 是兄弟包，不互相依赖；接入点放在 `streamproxy` 里调用 `gdrive`，方向单一）

测试：`gdrive.enable: false` 时 `streamproxy` 的现有测试**全部不受影响**（回归基线）。

## Step 7 全量验证

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -l internal/service/gdrive/ internal/service/streamproxy/ internal/config/
go vet ./internal/...
go build ./...
go test ./internal/service/gdrive/... ./internal/service/streamproxy/... ./internal/config/... -count=1
go test -race ./internal/service/gdrive/... ./internal/service/streamproxy/... -count=1
```

不要跑 `go test ./internal/...` 全量（`lib/ffmpeg` / `m3u8` / `music` / `openlist` / `util/jsons` 有既有失败，与本任务无关）。

## Step 8 真实环境验证（需要开发者提供凭据）

> **前置**: 开发者需先在 Google Console 登记重定向 URI。
>
> 基础设施**已实测就绪**（2026-09-16）: `go.bjyt.de` 已指向 Zouter、泛域名证书已就绪、
> 域名到 ge2o 的转发已生效（带标记请求出现在 ge2o 访问日志中）。
> 唯一待办是在 Google Console 登记 `https://go.bjyt.de/ge2o/gdrive/oauth/callback`。

与网关那次一样，**用真实凭据对真实文件验证**，不做假环境验收：

| 验证 | 做法 | 期望 |
|---|---|---|
| VA **授权流程** | 浏览器打开 `http://<ge2o>:8099/ge2o/gdrive/oauth/start?secret=<api-secret>` 走完授权 | 授权成功后页面提示成功；`<dataRoot>/gdrive-token.json` 出现且权限 0600；日志无任何凭据 |
| VB **重新授权** | 再次打开 `/oauth/start` | 页面提示已有授权、重新授权将覆盖；确认后能覆盖成功（这是 token 失效后的恢复路径） |
| V0 **父节点假设验证** | `root-folder-id` 留空, 用 `drive-id` 作为父节点发起第一次 `files.list` | 若能列出团队盘第一层内容 → 假设成立, 配置只需 `drive-id` 一个值; 若返回空或报错 → 假设不成立, 需显式提供 `root-folder-id`。**这一项决定配置有多简单, 优先验证** |
| V1 路径解析 | 用真实的 `root-folder-id` / `drive-id` 解析一个已知文件的路径 | 得到 fileId；日志 L21→L22 |
| V2 取流 | `files.get?alt=media` 取该文件 | 200 + 正确的 `Content-Length` |
| V3 Range | 带 `Range` 请求 | 206 + 正确的 `Content-Range`，内容是真实视频数据（魔数校验） |
| V4 缓存 | 同一文件解析两次 | 第二次命中缓存，**API 调用次数不增加** |
| V5 回退 | 把 `root-folder-id` 改成一个不存在的 ID | 日志出现 L24 + L28，播放**仍然成功**（走网关） |
| V6 凭据不泄漏 | `grep -i 'client_secret\|refresh_token\|access_token'` 全量日志 | 无命中 |

## Review Gate

- [ ] `state` 用 `crypto/rand` 生成，且**不匹配 / 重用时都被拒绝**（有测试钉住）
- [ ] `/oauth/callback` **不**要求 api-secret（Google 跳转无法携带），只靠 state 保护
- [ ] `/oauth/start` 要求 api-secret
- [ ] `access_type=offline` 与 `prompt=consent` 都在授权 URL 里（缺任一个拿不到 refresh_token）
- [ ] `redirect-uri` 配成非 localhost 的 `http://` 时**启动即报错**
- [ ] token 落盘文件权限为 0600，且测试用 `t.TempDir()` 不写进仓库
- [ ] `refresh-token` 三项来源的优先级正确（配置 > 落盘 > 报错）
- [ ] 两个新路由排在 `Reg_All` 之前（否则被兜底吃掉）
- [ ] `q` 中名称的转义有测试，且**删掉转义后该测试变红**
- [ ] 团队盘 4 个必需参数齐全，且有测试断言（删掉任一个测试变红）
- [ ] `supportsAllDrives=true` 出现在 `files.get?alt=media` 上
- [ ] 令牌并发去重有测试（并发 20 → 端点调用 1 次）
- [ ] 任何 Google 侧失败都回退到"请求原 URL"，且回退后**播放仍成功**
- [ ] `gdrive.enable: false` 时 `streamproxy` 行为与改动前逐字节一致
- [ ] 日志中无 `client_secret` / `refresh_token` / `access_token`
- [ ] 无按响应体增长的常驻内存（Zouter 可用内存 612MB）
- [ ] 包不依赖 gin / `internal/web/...`
- [ ] 新配置项同步到 `config-example.yml`，默认关闭

## 回滚点

| 手段 | 说明 |
|---|---|
| `gdrive.enable: false` + 重启 | **首选**。立即回到"走网关"的行为，无需改代码 |
| `git checkout -- internal/service/streamproxy/link.go` | 接入点回滚（本仓库不提交，改动在工作区） |
| 删除 `internal/service/gdrive/` + `internal/config/gdrive.go` | 彻底移除；需同步删掉 `config.go` 的字段与 `config-example.yml` 的段 |

## 风险

| 风险 | 表现 | 应对 |
|---|---|---|
| OAuth 凭据权限不足 | 首次调用即 401/403 | 按官方文档核对 scope 必须含内容读取权限; 错误日志会明确指向 token 或 API 响应 |
| 团队盘 ID / 起始文件夹 ID 配错 | 解析在第 1 层就失败 | L24 会打出「失败段」, 一眼能看出是路径哪一段没找到 |
| 每天 1TB 出流量上限 | 大量播放后 API 返回 403/429 | L28 会记录, 且自动回退网关; 个人使用基本不会触及 |
| 同名文件导致解析到错误的文件 | 播放出错误的内容 | L25 会记 Warn; 若实际发生需要引入更强的定位策略(如改用 fileId 直接寻址) |
| 凭据泄漏 | — | 配置文件 600 权限; 日志过滤; 不写入任何错误消息 |
