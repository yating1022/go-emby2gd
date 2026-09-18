# PRD · 接入 GD 管理面板直链接口，替代自建 Drive API

## 1. 背景

当前 `internal/service/gdrive` 是一套**自建**的 Google Drive 客户端：

- 自带 OAuth 授权流程（`/ge2o/gdrive/oauth/start`、`/ge2o/gdrive/oauth/callback`、令牌落盘 `gdrive-token.json`）
- 自己用 `files.list` 逐层把路径解析成 `fileId`（带进程内缓存 + singleflight）
- 自己用 `files.get?alt=media` 取流

维护成本高（OAuth 同意屏幕、refresh_token 7 天有效期、团队盘参数、配额），且与已有的
**GD 管理面板**能力重复。

外部已提供《GD 管理面板 · 直链接口对接文档》（仓库根目录 `GD_DIRECT_LINK_API.md`）：
面板负责「路径 → Google 下载地址 + 请求头」的换算，**不转发任何字节**。

## 2. 目标

把取流数据源从「自建 Drive API」换成「GD 管理面板直链接口」，由**本项目的服务器**代理拉流，
既不经过自建 Drive API，也不经过 Emby 服务器。

## 3. 新流程（唯一数据流）

```
strm 内容                    一次替换                  面板 /api/dl
/home/googleDrive/影视库/x.mkv ──▶ /影视库/x.mkv ──▶ url + headers
                                                          │
                          客户端 ◀── 本项目流式代理 ◀── 带 headers 请求 url
```

1. 从 strm 内容取到本地挂载路径（如 `/home/googleDrive/影视库/电影/x.mkv`）
2. 做**一次替换**：命中 `gdrive.mount-prefix` 时去掉该前缀，得到 Drive 内逻辑路径
3. 用替换后的路径调用面板 `GET {api-base}/api/dl?path=<URL 编码>`，带 `Authorization: Bearer <token>`
4. 拿到 `data.url` 与 `data.headers`（至少含 `Authorization`）
5. 带上 `headers`（以及客户端的 `Range`）请求 `url`，拿到字节流
6. 由本项目把字节流流式转发给客户端

## 4. 范围

### 4.1 做

- 重写 `internal/service/gdrive` 为**面板直链客户端**（保留包名与对外函数名）
- 新增配置项 `gdrive.api-base`、`gdrive.api-token`（支持环境变量覆盖）
- 删除自建 Drive API 的全部实现：OAuth 授权流程、令牌落盘、路径逐层解析、`files.get` 取流
- 删除 `streamproxy.resolveLink` 中「strm 是 http 地址时从中取 `?path=` 走 Drive API」的分支
- 删除两个 OAuth 路由与对应常量
- 删除不再需要的配置项：`client-id`、`client-secret`、`refresh-token`、`redirect-uri`、
  `drive-id`、`root-folder-id`、`path-cache-expired`、`max-path-depth`
- 更新 `config-example.yml` 中 `gdrive:` 段
- 保留「任何环节失败都回退到请求原地址」的硬性语义

### 4.2 不做

- 不接入面板的 `POST /api/cache`（缓存登记），也不为任何面板错误码做特殊分支——
  面板侧会保证「未命中时自行实时获取」，本项目只依赖固定的响应结构
- 不改动 `emby.strm.proxy` 的网关代理分支（`streamproxy.Proxy`）与 `emby.strm.path-map`
- 不改动响应缓存中间件的路由白名单
- 不持久化面板返回的 `headers`（含账号级令牌，只存进程内存）

## 5. 配置契约

```yaml
gdrive:
  enable: false
  api-base: "https://gd.bjyt.de"
  api-token: ""
  mount-prefix: /home/googleDrive
```

- `api-base`：面板地址。归一化去首尾空白与结尾 `/`；必须是 http/https 绝对地址
- `api-token`：面板**直链服务** Token（不是缓存服务 Token）。允许由环境变量
  `GDRIVE_API_TOKEN` 覆盖（非空即覆盖），便于容器部署不往配置文件写明文
- `mount-prefix`：语义不变，仍只做「命中前缀 → 去掉前缀」这一次替换
- `enable` 为 false 时行为与未部署本功能逐字节一致：不发起任何面板请求，不打任何相关日志
- `enable` 为 true 时 `api-base`、`api-token` 必须非空，否则启动即报错（错误消息只提字段名，不回显值）

## 6. 行为契约与验收标准

| # | 场景 | 期望 |
|---|---|---|
| A1 | `enable: false` | `MatchMountPath` 恒返回 false；不产生任何面板请求与 `[直链代理]` 之外的日志 |
| A2 | strm 内容不以 `mount-prefix` 开头 | 走原有流程，不算错误、不记错误日志 |
| A3 | 前缀之后无内容（如 `/home/googleDrive`）| 视为不命中（目录不是文件） |
| A4 | 前缀边界（`/home/googleDriveBackup/x.mkv`）| 不命中 |
| A5 | 命中后路径含中文/空格/括号 | 请求面板时 `path` 参数按 `url.QueryEscape` 编码，与面板逐字符一致 |
| A6 | 面板返回 200 | 解析出 `url` / `headers` / `expires_at`，带上 `headers` 请求 `url` |
| A7 | 面板返回 401 / 404 / 400 / 422 / 502 | 记日志并回退原地址，**不重试面板**；日志里直接打印面板给的 `message` 原文 |
| A8 | 带 `Authorization` 请求面板给的 `url` | Google 直接 200 返回媒体字节（实测 10/10 无 `Location`、零重定向）；出现 3xx 一律判为失败并回退 |
| A9 | 客户端带 `Range: bytes=0-` | 原样转发；上游 206 的 `Content-Range`/`Content-Length` 原样回写 |
| A10 | 同一文件连续多次 Range 请求 | 命中进程内缓存，不重复打面板；令牌 TTL 由 `expires_at` 推导 |
| A10.1 | 令牌的有效性 | **全局唯一一份**，不按路径复制；对共享盘所有文件通用；失效点只有一个 |
| A10.2 | 缓存余量与面板的提前刷新窗口 | 余量必须**严格小于**面板的提前刷新窗口（30s < 60s）。取 5 分钟时会出现 240 秒的「缓存已作废但面板仍在发同一个旧令牌」窗口，该窗口内每个 Range 请求都重新打面板 |
| A10.3 | 一波并发请求同时撞上 401 | 只触发**一次**面板重取，其余请求复用同一结果；不得打出 N 次并发重取 |
| A10.4 | 任何情况下 | 单个请求最多重试**一次**，不做循环重试或退避 |
| A11 | 缓存中令牌临近过期（`expires_at - now <= 30s`）| 判为不可用，下次重新取 |
| A12 | 用缓存直链请求 Google 返回 **401**/403/404/410 | 清除该路径缓存，重新取一次直链再试；仍失败则回退原地址。**401 必须在列**：Google 对过期凭据返回的是 401 |
| A16 | 一次播放持续超过请求头有效期（约 1h）| **不得中断**：客户端后续的 Range 请求应透明地拿到新请求头并继续，客户端侧无感知 |
| A17 | 缓存里的请求头已过期或临近过期（距 `expires_at ≤ 30s`）| 该条目判为不可用，重新调用面板取新头 |
| A18 | 面板返回的 `expires_at` 缺失或无法解析 | 不写缓存，每个请求都重新调面板；**不得**用一个猜测的时长缓存 |
| A13 | 任何环节失败且尚未写出响应 | 回退到 `ProxyOrigin`（Emby 从挂载点读取），播放不中断 |
| A14 | 面板 Token 出现在日志中的可能 | `Authorization`、Token 值一律不进日志；面板错误文案里的 Token 被替换为 `***` |
| A15 | `GDRIVE_API_TOKEN` 环境变量非空 | 覆盖 `api-token` 配置值 |

## 7. 交付物

- 重写后的 `internal/service/gdrive`（面板直链客户端）
- `internal/config/gdrive.go` 新配置与校验
- `internal/service/streamproxy` 去掉自建 Drive 分支
- `internal/util/https` **不改动**（实测带 `Authorization` 的 `alt=media` 请求零重定向，见 design §5.4）
- `internal/web/route.go`、`internal/constant/constant.go` 去掉 OAuth 路由
- `config-example.yml` 更新
- 相应单元测试

## 8. 约束

- **播放不中断是硬约束**：面板返回的请求头约 1 小时过期，而一次播放可持续数小时。
  令牌到期必须由本项目透明地重新取头解决，不得表现为播放失败。
  设计与三层保护见 design §6.5
- **不提交代码**：改动只留工作区（本仓库约定）
- 面板返回的 `headers` 是**账号级** Google 凭据，只用于本项目与 Google 之间：
  绝不回写给客户端、绝不进日志、绝不落盘
