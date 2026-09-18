# Research: go-emby2openlist 架构与目录结构

- **Query**: 全面梳理项目架构与目录结构，为 `.trellis/spec/backend/directory-structure.md` 提供素材
- **Scope**: internal（代码阅读 + git 历史验证）
- **Date**: 2026-09-16
- **代码基线**: main 分支，HEAD = 13d21c7（v2.8.2 之后）

---

## 关键结论速览

1. **单体 Go 应用**：模块名 `github.com/AmbitiousJun/go-emby2openlist/v2`，Go 1.26.3，gin v1.10.0；共 105 个 Go 文件、约 10.7k 行（含 15 个 `_test.go`），无 `pkg/`、无 `api/`，全部业务代码在 `internal/`。
2. **启动流程**（`main.go:25-46`）：起 pprof 端口 → `parseFlag()`（端口/数据根目录写入 `webport` 全局变量）→ `config.ReadFromFile()`（反射驱动各配置段 `Init()`）→ `localtree.Init()`（可选目录树同步）→ `web.Listen()`（HTTP 8095 / HTTPS 8094 双端口）。
3. **路由是"正则规则表"而非 gin 原生路由**：`internal/web/route.go` 维护 `[][2]any{正则, handler}` 切片，gin 侧只注册一个兜底 `r.Any("/*vars", globalDftHandler)`（`internal/web/handler.go:102`），请求按顺序线性匹配正则。
4. **分层名义上是 config → model → service → web，但存在 4 处反向依赖**：`config → web/webport`、`config → service/lib/ffmpeg`、`util/https → web/webproxy`、`service/emby → web/cache`。`webport`/`webproxy` 实为全局状态工具包，放在 `internal/web/` 下造成分层混乱。
5. **`internal/util/` 下 16 个子包全部用复数命名**（strs、maps、jsons、logs…），其中 `logs` 被 28 处导入是最常用包；`https` 包（19 处）封装了全局 `http.Client` 和链式 `RequestHolder`，是所有出站请求的统一入口。
6. **前端是 React Router 7 + Vite + Tailwind 4 + shadcn 的 SPA**，构建产物经 `web/embed.go` 的 `//go:embed all:dist` 嵌入二进制；仓库不含 `web/dist`，**未先构建前端时 `go build ./...` 直接失败**（已实测：`web/embed.go:5:12: pattern all:dist: no matching files found`）。
7. **`cmd/` 下两个独立 main 包是开发期一次性生成器**：`fake_mp4` 生成假 mp4 box 结构（3 小时时长），`fake_mp3_1` 生成假 MP3 帧；运行时对应逻辑在 `internal/util/mp4s.GenWithDuration` 和 `internal/service/music.WriteFakeMP3`，cmd 版本是它们的独立原型。
8. **新增配置项类小功能的典型落点**（git 实证 images-original，commit d5a5c11）：`config-example.yml` + `internal/config/<域>.go` + `internal/service/emby/<相关>.go`，共 3 个文件；大功能（localtree，commit 1fb2ce9）则新建 `internal/service/<域>/<子包>/` + `main.go` 挂初始化 + `internal/util/` 补工具包。
9. **构建发布链**：`build_web.sh`（npm build）→ `build.sh`（14 平台交叉编译，`-tags=goexperiment.jsonv2`）；GitHub Actions `build.yml` 在 release published 时上传二进制，`docker.yml` 在 `v*.*.*` tag 时构建多架构 Docker 镜像；Dockerfile 为 node→golang→alpine 三阶段。
10. **主要技术债**：路由表 `[2]any` 类型不安全且 O(n) 匹配；全局可变状态遍布（`config.C`、`webport`、`webproxy`、`emby.validApiKeys`、m3u8 `infoMap`）；pprof 60360 端口无条件暴露；`emby.go:124` 残留测试函数 `TestProxyUri`；`internal/util/parallels` 包名拼写三个 l。

---

## 1. 项目定位与技术栈

go-emby2openlist（简称 ge2o）是一个 **Emby 反向代理中间件**：部署在 Emby 服务器前，拦截媒体播放请求，把 Emby 挂载路径转换为 OpenList（网盘挂载工具）路径，302 重定向到网盘直链或转码 m3u8，从而实现"Emby 元数据 + 网盘直链播放"。

技术栈（`go.mod`）：

| 依赖 | 用途 |
|---|---|
| `github.com/gin-gonic/gin v1.10.0` | HTTP 框架 |
| `github.com/gorilla/websocket v1.5.3` | WebSocket（日志同步、emby socket 代理） |
| `github.com/bogem/id3v2 v1.2.0` | MP3 ID3 标签写入（假音乐文件生成） |
| `github.com/google/uuid v1.6.0` | UUID |
| `golang.org/x/sync v0.13.0` | 并发控制 |
| `gopkg.in/yaml.v3` | 配置解析 |

前端（`web/src/package.json`）：React 19 + React Router 7.16（framework mode）+ Vite 8 + Tailwind 4 + radix-ui/shadcn + TypeScript。

代码规模：105 个 Go 文件（不含前端），`wc -l` 合计 10722 行；最大的三个文件是 `internal/service/emby/media.go`（570 行）、`internal/service/emby/playbackinfo.go`（497 行）、`internal/service/m3u8/info_test.go`（426 行）。

## 2. 顶层目录结构总览

```
go-emby2openlist/
├── main.go                  # 唯一入口，93 行
├── go.mod / go.sum          # 模块 github.com/AmbitiousJun/go-emby2openlist/v2
├── config-example.yml       # 配置模板（8 个顶级段）
├── internal/                # 全部业务代码（Go 强制外部不可导入）
│   ├── config/             # 配置定义与加载（8 文件）
│   ├── constant/           # 版本号 + 全部路由正则/路径常量（单文件）
│   ├── model/              # 跨包通用类型（2 文件）
│   ├── service/            # 业务层
│   │   ├── emby/           # 最大业务包：emby 代理/鉴权/重定向（16 文件）
│   │   ├── openlist/       # openlist API 客户端（7 文件）
│   │   │   └── localtree/  # 本地目录树同步（7 文件）
│   │   ├── m3u8/           # 转码播放列表内存缓存与代理（5 文件）
│   │   ├── music/          # 假音乐文件生成（1 文件）
│   │   ├── path/           # emby→openlist 路径转换（1 文件）
│   │   ├── lib/ffmpeg/     # ffmpeg 外部二进制封装（5 文件）
│   │   ├── service.go / log.go / model.go  # 顶层：密钥校验 + WS 日志
│   ├── util/               # 16 个工具子包（全部复数命名）
│   └── web/                # gin 服务器与中间件
│       ├── cache/          # 响应缓存中间件（5 文件）
│       ├── webport/        # 端口全局变量（1 文件）
│       └── webproxy/       # 环境变量代理解析（1 文件）
├── cmd/                    # 两个独立开发工具
│   ├── fake_mp3_1/         # 生成假 MP3（40 行）
│   └── fake_mp4/           # 生成假 MP4 box（109 行）
├── web/                    # 前端项目
│   ├── embed.go            # //go:embed all:dist
│   └── src/                # React Router 7 项目（app/routes.ts 组织）
├── build.sh / build_web.sh # 构建脚本
├── Dockerfile              # 三阶段构建
├── .github/workflows/      # build.yml（release 二进制）、docker.yml（镜像）
├── assets/                 # README 图片
├── custom-css/ custom-js/  # 用户自定义脚本/样式目录（运行时读取，仅含 README）
├── ssl/                    # 用户证书目录（仅含 README）
└── rsrc_windows_*.syso     # Windows 图标资源（4 架构）
```

## 3. main.go 启动流程

`main.go`（93 行）按顺序做 5 件事：

1. **起 pprof**（`main.go:26`）：`go func() { http.ListenAndServe(":60360", nil) }()`，配合 `import _ "net/http/pprof"`（`main.go:8`），在 60360 端口无条件暴露性能分析接口。
2. **解析命令行**（`main.go:28` → `parseFlag()`，`main.go:49-76`）：
   - `-p` HTTP 端口（默认 8095）、`-ps` HTTPS 端口（默认 8094）、`-version` 打印 `constant.CurrentVersion` 退出、`-dr` 数据根目录（默认 `.`，须为已存在目录）；
   - 端口校验（HTTP/HTTPS 不得相同）后写入 `webport.HTTP` / `webport.HTTPS` 全局变量（`main.go:73-74`）——注意端口不是 config 配置项，而是命令行 flag。
3. **读配置**（`main.go:30`）：`config.ReadFromFile(filepath.Join(dataRoot, "config.yml"))`，即配置文件固定名为数据根目录下的 `config.yml`。
4. **初始化本地目录树**（`main.go:37`）：`localtree.Init()`——若 `openlist.local-tree-gen.enable` 为 false 直接返回 nil；否则创建全局 `Synchronizer` 并起 goroutine 定时同步（`internal/service/openlist/localtree/localtree.go:20-36`）。
5. **启动 web 服务**（`main.go:43`）：`gin.SetMode(ginMode)` 后调 `web.Listen()`。

`web.Listen()`（`internal/web/web.go:18-38`）内部：

1. `initRulePatterns()` 编译全部路由正则规则（见下文 route.go）；
2. 按 SSL 配置决定监听模式：未启用 SSL → 仅 HTTP；启用且 `single-port` → 仅 HTTPS；否则双端口同时监听（`web.go:22-29`）；
3. 每个 gin 引擎的中间件链（`initRouter`，`web.go:41-50`）：`referrerPolicySetter`（设置 `Referrer-Policy: no-referrer`，`internal/web/referer.go`）→ `emby.ApiKeyChecker()`（向 emby 源服务器校验 api_key，`internal/service/emby/auth.go:65`）→ `emby.DownloadStrategyChecker()`（`internal/service/emby/download.go:107`）→ 若 `cache.enable` 则追加 `cache.CacheableRouteMarker()` + `cache.RequestCacher()`；
4. `initRoutes` 只注册一条路由：`r.Any("/*vars", globalDftHandler)`（`internal/web/route.go:102`）。

**globalDftHandler 兜底逻辑**（`internal/web/handler.go:20-40`）：

1. HEAD 请求直接返回 200 空串；
2. `handleWebStatic`：若 `ge2o.web.disable=false`，处理 `/ge2o/web` 前缀的前端静态资源（从 `web.EmbedFS` 的 `dist` 子目录读取，找不到的路径回退 `index.html`，即 SPA fallback，`handler.go:43-94`）；
3. 顺序遍历 `rules` 切片做正则匹配，命中则把正则串和子匹配写入 gin context（`MatchRouteKey`、`constant.RouteSubMatchGinKey`），调用对应 handler；全部未命中走最后的 `Reg_All → emby.ProxyOrigin` 回源代理。

**路由规则表**（`internal/web/route.go:19-98`）：`rules` 是包级 `[][2]any`，`initRulePatterns` 用 `compileRules` 把 `(正则字符串, func(*gin.Context))` 编译为 `(regexp.Regexp, gin.HandlerFunc)`。约 25 条规则按业务优先级排列：websocket → PlaybackInfo → 播放进度 → Items 系列 → 剧集重排 → 字幕 → 资源重定向（stream/master/main/original）→ m3u8 代理 → 下载 → 图片 → CORS → 自定义脚本 → 本地目录树手动更新 → 密钥校验 → 日志同步 → 根路径 → 兜底回源。所有正则和 `/ge2o` 前缀路由常量集中定义在 `internal/constant/constant.go:8-66`。

## 4. internal/ 各包职责

### 4.1 internal/config — 配置定义与加载

8 个文件，按配置域一域一文件：

| 文件 | 内容 |
|---|---|
| `config.go` | `Config` 结构体（8 段：Emby/Openlist/VideoPreview/Path/Cache/Ssl/Log/Ge2o）、全局 `C`、`BasePath`、`Initializer` 接口、`ReadFromFile`、`ServerInternalRequestHost()` |
| `emby.go` | `Emby` 段：host、mount-path、图片质量/原图、strm 路径映射、下载策略、本地媒体根路径、自定义 CSS/JS 开关；含 `IsLocalMediaPath()` |
| `openlist.go` | `Openlist` 段：token、host、`LocalTreeGen`（本地目录树生成全部子配置）；`Init()` 中调 `ffmpeg.AutoDownloadExec(BasePath)` 自动下载 ffmpeg 二进制（`openlist.go:83`） |
| `video_preview.go` | 网盘转码代理开关与容器白名单 |
| `path.go` | `emby2openlist` 路径前缀映射 |
| `cache.go` | 缓存开关与过期时间（字符串如 `1d` 解析为 `time.Duration`） |
| `ssl.go` | HTTPS 证书路径与单端口模式 |
| `log.go` | 彩色日志禁用开关 |
| `ge2o.go` | 程序自身配置：`api-secret`、web 平台开关 |

**加载机制**（`config.go:44-78`）：读 YAML → `yaml.Unmarshal` 到 `C` → **反射遍历 `Config` 所有字段**：nil 指针字段 `field.Set(reflect.New(...))` 补零值，实现了 `Initializer` 接口的字段调 `Init()` 做校验和派生数据初始化（如把 `path-map` 字符串切成二维数组）。因此**新增配置段的步骤是：加 yaml 字段 + 写 `Init()` 方法**，`ReadFromFile` 无需改动。

### 4.2 internal/constant — 常量中心

单文件 `constant.go`（67 行）：`CurrentVersion`（v2.8.2）、`RepoAddr`、全部 `Reg_*` 路由正则、`Route_*` 自维护路由（统一前缀 `/ge2o`，如 `Route_SyncServerLog = /ge2o/ws/log/sync`）、`RouteSubMatchGinKey`、`custom-js`/`custom-css` 目录名。被 11 处导入，是 route.go / cache.go / auth.go 共享正则的单一来源。

### 4.3 internal/model — 跨包通用类型

仅 2 文件：`http.go` 定义泛型响应 `HttpRes[T]{Code, Data, Msg}`（openlist/emby 服务层通用返回类型）；`gin.go` 定义 `Response{Success, Message}`（JSON 响应体）。被 5 处导入。

### 4.4 internal/service（顶层）— 程序自身接口

- `service.go`：`ValidateApiSecret`（POST 校验 `ge2o.api-secret`，路由 `Route_ValidateApiSecret`）；
- `log.go`：`SyncServerLog`（WebSocket 升级 → `logs.RegisterLogger` 注册 wsLogger → 循环推送日志到前端，`log.go:78-111`）；
- `model.go`：`ValidateSecretRequest` 请求体。

顶层 service 包只放"不属于 emby/openlist 域"的程序自身接口，避免再建一个包。

### 4.5 internal/service/emby — 核心业务包（16 文件）

按业务功能一功能一文件：

| 文件 | 行数 | 职责 |
|---|---|---|
| `auth.go` | 198 | `ApiKeyChecker` 中间件：把客户端 api_key 转发 emby `/emby/Auth/Keys` 校验，`validApiKeys sync.Map` 缓存已验证 key；`getApiKey` 支持 query/header 两种传递方式 |
| `api.go` | 45 | `Fetch`/`RawFetch` 请求 emby 源服务器（内部走 `util/https`） |
| `emby.go` | 217 | `ProxySocket`（websocket 反代）、`HandleImages`（图片质量参数改写/原图模式）、`StripImageParams`、`ProxyOrigin`（通用回源代理）、`ProxyRoot`、`TestProxyUri`（测试残留） |
| `media.go` | 570 | 媒体源解析核心：`resolveItemInfo`、`findVideoPreviewInfos`（并发探测网盘转码模板）、`addSubtitles2MediaStreams`、虚拟视频检测等 |
| `playbackinfo.go` | 497 | `TransferPlaybackInfo`：改写 PlaybackInfo 响应注入直链 MediaSource；`LoadCacheItems`；缓存空间读写 |
| `redirect.go` | 310 | `Redirect2Transcode`（302 到本地 m3u8 代理）、`Redirect2OpenlistLink`（302 到网盘直链）、`ProxyOriginalResource`、本地媒体内网 302 直连优化（`shouldRedirectDirectly`） |
| `items.go` | 280 | Items 接口代理与加工：随机列表重排、去 limit、加转码预览信息、Latest 解码 Path |
| `episode.go` | 101 | `ResortEpisodes` 剧集重排（未播优先） |
| `subtitles.go` | 40 | 字幕长缓存代理 |
| `download.go` | 146 | `HandleSyncDownload`、`DownloadStrategyChecker`（origin/direct/403 三策略） |
| `custom_cssjs.go` | 311 | 代理 emby 首页注入自定义脚本按钮、响应 `/ge2o/custom.js|css`、读取 `custom-js/`、`custom-css/` 目录 |
| `cors.go` | 30 | 改写 emby web 播放器模块的 CORS |
| `playing.go` | 115 | 播放停止/进度辅助上报 |
| `type.go` | 65 | `MsInfo`、`ItemInfo`、`RouteType` 等包内类型 |

依赖方向：config、model、constant、util/*、`service/openlist`、`service/path`、**`web/cache`（反向，4 个文件导入）**。

### 4.6 internal/service/openlist — OpenList API 客户端（7 文件）

- `api.go`（225 行）：`FetchResource`（原画/转码直链获取，转码失败可递归回退原画）、`FetchFsList/FetchFsGet/FetchFsOther`（openlist fs 系列接口）、通用 `Fetch`；`addMainApiRunner/removeMainApiRunner` 控制并发。
- `walk.go`：`WalkFsList` 分页遍历远端目录（生成器模式）。
- `path.go`/`subtitle.go`/`header.go`/`type.go`：路径编解码、字幕语言名、请求头清洗、`FetchInfo`/`Resource`/`FsList` 等类型。

**localtree 子包**（7 文件，本地目录树同步，commit 1fb2ce9 引入）：

- `localtree.go`：`Init()` 入口 + 定时全量同步循环；
- `synchronizer.go`（410 行）：`Synchronizer.Sync` 增量同步、快照对比、任务管道；
- `snapshot.go`：`Snapshot`（`map[string]bool`）本地文件快照；
- `task.go`（295 行）：四种 `TaskWriter`——`VirtualWriter`（假 mp4，调 `mp4s.GenWithDuration`）、`StrmWriter`（.strm 文件）、`MusicWriter`（假 MP3 + NFO，调 ffmpeg 提取元数据）、`RawWriter`（原样落盘）；
- `api.go`：`UpdateManually`（手动触发同步的 HTTP 接口）；
- `model.go`：`FileTask` 等类型。

依赖：`service/openlist`（父包）、`service/lib/ffmpeg`、`service/music`、`util/*`。

### 4.7 internal/service/path — 路径转换（1 文件）

`path.go`：`Emby2Openlist(embyPath)` 是所有媒体请求路径转换的入口——URL 解码 → 反斜杠转换 → 去 `emby.mount-path` 前缀 → 查 `path.emby2openlist` 映射；`Range()` 遍历 openlist 根路径生成候选路径。依赖 config 和 openlist（`FetchFsList`）。

### 4.8 internal/service/m3u8 — 转码播放列表缓存（5 文件）

- `m3u8.go`：内存 LRU 播放列表缓存（`MaxPlaylistNum=10`），`init()` 起维护 goroutine；**`GetPlaylist`/`GetTsLink`/`GetSubtitleLink` 声明为包级函数变量并在 `m3u8.go:145-164` 自行赋值**（函数变量注入模式）；
- `info.go`（248 行）：m3u8 文本解析为 `Info`（ts/字幕链接、master/content 重写）；
- `proxy.go`：`ProxyPlaylist`/`ProxyTsLink`/`ProxySubtitle` 三个 HTTP handler；
- `type.go`：`ProxyParams`。

注意：`info.go` 导入 `service/emby` 仅为使用常量 `emby.QueryApiKeyName`（`info.go:132`）。

### 4.9 internal/service/music — 假音乐文件（1 文件）

`write.go`：`WriteNFO`（XML 元数据）、`WriteFakeMP3`（ID3v2 标签 + `ffmpeg.GenSilentMP3Bytes` 静音帧）。依赖 `lib/ffmpeg` 和 `bogem/id3v2`。

### 4.10 internal/service/lib/ffmpeg — ffmpeg 二进制封装（5 文件）

- `ffmpeg.go`：`InspectInfo`/`InspectMusic`（`exec.Command` 调 ffmpeg 解析元信息，全局互斥锁串行）、`GenSilentMP3Bytes`；
- `auto_download.go`：按 `runtime.GOOS/GOARCH` 从 GitHub release 镜像自动下载对应 ffmpeg 二进制到数据目录（`arch2ExecNameMap`）；
- `util.go`：`getProxyUrlByPath`（**导入 `web/webproxy`**）、正则解析；
- `type.go`：`Info`/`Music` 类型。

被 config（openlist.go）、music、localtree/task.go 导入——即 **config 层依赖了 service 层的此包**。

### 4.11 internal/util — 16 个工具子包

全部复数命名（作者风格），文件名 = 包名.go（个别加辅助文件）：

| 包 | 文件 | 职责 | 被导入次数 |
|---|---|---|---|
| `logs` (+`colors`) | logs.go, logger.go, colors/colors.go | 彩色日志、`RegisterLogger` 多输出器 | 28 |
| `https` | https.go, request.go, web.go | 全局 `http.Client`（init 时组装，禁自动重定向）、链式 `RequestHolder`（Do/DoSingle/DoRedirect）、`ProxyPass` 回源代理 | 19 |
| `strs` | strs.go | 字符串判空/截断等 | 14 |
| `urls` | urls.go | URL 解码、斜杠转换、资源名解析 | 12 |
| `jsons` | jsons.go, item.go, deep_get.go, serialize.go, web.go | `Item` 动态 JSON 节点（Get/Set/DeepGet）、序列化 | 12 |
| `bytess` | bytess.go | `sync.Pool` 复用缓冲区 | 6 |
| `encrypts` | encrypts.go | MD5 等（cache key 计算） | 1 |
| `files` | files.go | 本地文件遍历/操作 | 1 |
| `maps` | maps.go | Keys 等工具 | 2 |
| `mp4s` | mp4s.go | `GenWithDuration` 生成假 mp4 字节 | 2 |
| `parallels` | paralllels.go（拼写三个 l） | 并发任务编排 | 2 |
| `randoms` | randoms.go | 随机 hex 等 | 2 |
| `slices` | slices.go | 切片工具 | 0（当前无内部导入） |
| `structs` | structs.go | 结构体工具 | 0 |
| `trys` | trys.go | `Try` 重试 | 3 |

`https` 包是所有出站 HTTP 的唯一出口：`https.go:30-53` 的 `init()` 创建全局 client，其 `Transport.Proxy` 回调读取 `webproxy.HttpUrl/HttpsUrl`（环境变量 `HTTP_PROXY/HTTPS_PROXY`）——这是 **util → web 反向依赖**的根源。

### 4.12 internal/web — gin 服务器（4 文件 + 3 子包）

- `web.go`：`Listen`/`initRouter`/`listenHTTP`/`listenHTTPS`（HTTPS 显式禁用 HTTP/2，`web.go:88`）；
- `route.go`：路由规则表（见第 3 节）；
- `handler.go`：`globalDftHandler`、`handleWebStatic`（前端 embed 资源 + SPA fallback）、`compileRules`；
- `log.go`：`CustomLogger` 访问日志中间件（带颜色/端口/命中路由）；
- `referer.go`：`Referrer-Policy: no-referrer`。

子包：

- **`cache/`**（5 文件）：响应缓存中间件。`cache.go` 的 `CacheableRouteMarker` 用 6 个白名单正则标记可缓存路由，`RequestCacher` 拦截响应写入缓存；`holder.go` 内存缓存持有者 + `init()` 起淘汰循环；`space.go` 按 space/spaceKey 二级组织（PlaybackInfo 等复用）；`type.go` 的 `respCacheWriter` 包装 gin Writer 实现响应复制。**被 `service/emby` 的 4 个文件反向导入**（items.go、playbackinfo.go、redirect.go、subtitles.go）。
- **`webport/`**（1 文件）：纯全局变量 `HTTP`/`HTTPS` 端口 + `GinKey`。被 main.go、config、web、emby(custom_cssjs) 导入——**config → web 反向依赖**。
- **`webproxy/`**（1 文件）：`init()` 解析 `HTTP_PROXY`/`HTTPS_PROXY` 环境变量为 `*url.URL` 全局变量。被 `util/https`、`lib/ffmpeg` 导入——**util → web 反向依赖**。

## 5. 分层与依赖方向

名义分层（自下而上）：`constant` / `model` → `config` → `util` → `service` → `web`。实际 import 关系（逐文件 grep 验证）：

```
main.go ──→ config, constant, localtree, logs, web, webport

web ──→ config, constant, service(顶层), emby, m3u8, localtree, cache, webport, logs
         └─ handler.go ──→ web(embed 前端)

emby ──→ config, constant, model, util/*, openlist, path, 【web/cache】
m3u8 ──→ openlist, util/*,【emby（仅 QueryApiKeyName 常量）】
openlist ──→ config, model, util/*
openlist/localtree ──→ config, constant, model, openlist, lib/ffmpeg, music, util/*
path ──→ config, openlist, util/*
music ──→ lib/ffmpeg
lib/ffmpeg ──→ constant, util/*,【web/webproxy】

config ──→ util/*,【web/webport】,【service/lib/ffmpeg】
util/https ──→【web/webproxy】
util/jsons ──→ util/maps, parallels, strs
```

**结论**：

1. **主流方向正确**：web（路由/中间件）→ service（业务）→ config/util（基础），未出现 service 导入 web 主包（`internal/web`）的情况。
2. **4 处反向/跨层依赖**（全部有代码证据）：
   - `internal/config/config.go:9` 导入 `internal/web/webport`（读端口算内部请求 host）；
   - `internal/config/openlist.go:7` 导入 `internal/service/lib/ffmpeg`（Init 时自动下载 ffmpeg）；
   - `internal/util/https/https.go:16` 导入 `internal/web/webproxy`（client 代理回调）；
   - `internal/service/emby` 4 个文件导入 `internal/web/cache`（业务层直接读写缓存空间）。
   根因：`webport`/`webproxy`/`cache` 实质是"全局状态/基础设施"，但物理上放在 `internal/web/` 下。
3. **service 内部横向依赖**：`emby → openlist/path`、`m3u8 → emby/openlist`、`path → openlist`、`localtree → openlist/ffmpeg/music`。emby 是汇聚点，openlist 是被依赖最多的底层服务包。
4. **无循环依赖**：Go 编译器保证了这一点；m3u8→emby 仅取常量，未形成环。

## 6. cmd/ 辅助程序

两个独立 `package main`，**不导入任何 internal 包**，是开发期手工工具：

- **`cmd/fake_mp4/main.go`**（109 行）：手写 mp4 box 二进制结构（ftyp/moov/mvhd/trak/tkhd/mdia/mdhd/hdlr），生成 3 小时时长（10800s）的 `fake_3h.mp4` 占位视频。其逻辑与 `internal/util/mp4s.GenWithDuration`（mp4s.go:10-96）**几乎逐行相同**（同样的 `writeBox` 递归结构、同样的 box 布局），cmd 版本是硬编码 3 小时的原型，运行时用的是 util 版本（`localtree/task.go:109,121` 调用）。
- **`cmd/fake_mp3_1/main.go`**（40 行）：生成 MPEG-1 Layer3 32kbps 44100Hz 假帧（帧头 `FF FB 84 64` + 零填充），输出 `fake_3h.mp3`。注意代码内 `duration = time.Minute * 4`（main.go:12）实际是 4 分钟，但输出文件名仍叫 `fake_3h.mp3`，名实不符。运行时对应物是 `music.WriteFakeMP3`（用 ffmpeg 生成静音帧而非固定帧填充）。

用途背景：localtree 功能在本地生成"虚拟媒体文件"（假 mp4/假 mp3 + strm + NFO）供 Emby 扫描建库，播放时再 302 到网盘直链。cmd 下两个程序用于开发期验证这类假文件能否被 Emby/播放器识别。

## 7. web/ 前端组织与集成

```
web/
├── embed.go            # package web; //go:embed all:dist → EmbedFS
└── src/                # React Router 7 framework mode
    ├── package.json     # name: ge2o-web
    ├── react-router.config.ts / vite.config.ts / tsconfig.json
    ├── public/          # favicon.ico
    └── app/
        ├── root.tsx     # HTML 壳（ThemeProvider/TooltipProvider/Toaster）
        ├── routes.ts    # 路由声明：layout(layout.tsx) 包裹 index、api/openlist_local_tree、log
        ├── routes/
        │   ├── index.tsx                       # 首页（设置入口）
        │   ├── layout.tsx
        │   ├── log/index.tsx                   # 日志页（WebSocket 拉取）
        │   └── api/openlist_local_tree/        # 目录树手动更新页
        │       ├── index.tsx
        │       └── components/update_request_collapse.tsx
        ├── components/  # mode_toggle、settings_modal、theme_provider、ui/（shadcn 全套）
        ├── lib/utils.ts
        └── assets/
```

**前后端集成方式**：

1. **嵌入**：`web/embed.go` 的 `//go:embed all:dist` 把前端构建产物打进二进制；`internal/web/handler.go:55` 用 `fs.Sub(web_static.EmbedFS, "dist")` 挂载。
2. **路由前缀**：前端静态资源挂在 `/ge2o/web/`（`constant.Route_Web`），SPA fallback 到 index.html（`handler.go:65-88`）；`root.tsx` 里 favicon 也引用 `/ge2o/web/favicon.ico`。
3. **API 调用**：前端 fetch 相对路径 `/ge2o/openlist/local_tree/update`（`update_request_collapse.tsx:89`）、WebSocket `wss://{host}/ge2o/ws/log/sync?secret=xxx`（`log/index.tsx:32`）、密钥校验 `/ge2o/secret/validate`。这些后端端点全部在 `internal/web/route.go` 规则表中由 `service`/`localtree` 包的 handler 处理。
4. **注入**：`emby/custom_cssjs.go` 在代理 emby 首页时注入按钮跳转 `/ge2o/web/`（`innerJsAddGe2oWebButton`）。
5. **构建产物位置**：`build_web.sh` 在 `web/` 目录内把 `src/build/client` 移为 `web/dist`（`web/src/.gitignore` 忽略 `/build/`，根 `.gitignore` 忽略 `dist`）。**仓库不含 `web/dist`，直接 `go build ./...` 报错 `web/embed.go:5:12: pattern all:dist: no matching files found`（已实测）**，必须先跑前端构建。

## 8. 构建体系

### 本地构建

- **`build_web.sh`**（web 目录内执行）：`npm ci && npm run build`，然后 `rm -rf ./dist && mv src/build/client ./dist`。
- **`build.sh`**（根目录）：先调 `build_web.sh`；再删根 `dist/` 并循环 **14 个平台**（darwin/amd64|arm64、linux/386|arm|amd64|arm64、windows/386|arm|amd64|arm64、freebsd/386|arm|amd64|arm64）执行 `CGO_ENABLED=0 GOOS=… GOARCH=… go build -tags=goexperiment.jsonv2 -ldflags="-X main.ginMode=release" -o dist/ge2o-${GOOS}_${GOARCH}[.exe] .`。注意构建目标是根目录 `.`（main 包），cmd/ 下的工具不会被交叉编译发布。

### Docker（`Dockerfile`，三阶段）

1. `node:24-alpine`：`npm ci && npm run build` 产前端；
2. `golang:1.26-alpine`：`go mod download` 后 `COPY --from=web-builder … ./web/dist`，`CGO_ENABLED=0 go build -tags=goexperiment.jsonv2 -ldflags="-X main.ginMode=release" -o main .`（GOPROXY 设为 goproxy.cn）；
3. `alpine:latest`：仅拷贝二进制，TZ=Asia/Shanghai，EXPOSE 8095/8094，CMD `./main`。

### CI/CD（`.github/workflows/`）

- **`build.yml`**：`on: release: published` → checkout + Node 24 + Go 1.26.3 → 跑 `./build.sh` → `softprops/action-gh-release@v2` 把 `dist/**` 上传到该 release。
- **`docker.yml`**：`on: push: tags: v*.*.*` → buildx + QEMU → `linux/amd64,linux/arm64` 双架构 → 推 `ambitiousjun/go-emby2openlist:{tag},{latest}`（Docker Hub 凭据 `DOCKER_PASSWORD`）。

**发布流程**：合入 main → 改 `constant.CurrentVersion`（version commit，如 fe4aa4c "version: v2.8.2"）→ 打 tag / 发 release → 两个 workflow 分别产二进制和镜像。

## 9. 文件命名惯例

1. **包名 = 目录名**（标准 Go），util 子包一律**复数**（strs/maps/jsons/logs…），连 `logs/colors` 也是复数。
2. **包内按职责拆文件**，高频固定名：
   - `type.go`：纯类型定义（emby、openlist、m3u8、localtree、ffmpeg、cache 都有）；
   - `model.go`：请求/响应模型（service、localtree）——注意 `type.go` 与 `model.go` 在 localtree 中并存，边界是"核心结构 vs 数据传输"；
   - `api.go`：对外部系统的 HTTP 封装（emby/api.go、openlist/api.go、localtree/api.go——但 localtree/api.go 实际是 gin handler）；
   - `log.go`：日志相关（config/log.go 是日志配置，web/log.go 是访问日志中间件，service/log.go 是 WS 日志同步——同名不同义）；
   - `service.go`：包主入口/门面（service/service.go）；
   - `handler.go` / `route.go` / `web.go`：web 层的处理器/路由/服务器；
   - `init` 语义的 `localtree.go`、`config.go`：包初始化入口。
3. **业务包按功能一文件**（emby 包最典型：auth、media、redirect、items、episode、download、subtitles、playing、cors、custom_cssjs）。
4. **测试与被测文件同目录同名**：`media_test.go`、`api_test.go`、`walk_test.go`、`synchronizer_test.go` 等 15 个。
5. **Go 文件与目录对应**：多数 util 包是单文件包（文件名=包名.go）；多文件包用 `web.go`/`request.go`/`logger.go` 等辅助名。

## 10. 新增功能时代码落在哪（git 实证）

用 `git log --oneline -20` + `git show <hash> --stat` 验证近期功能：

| Commit | 功能 | 改动文件 | 模式 |
|---|---|---|---|
| d5a5c11 | images-original（原图开关） | `config-example.yml`(+4)、`internal/config/emby.go`(+2)、`internal/service/emby/emby.go`(+43) | **配置项 + 配置结构 + 业务处理** 三点式 |
| bfa2ec2 / f1e74b0 | local-media 内网 302 优化 | `internal/service/emby/redirect.go` 单文件（+45/-32） | 纯业务逻辑改动 |
| 4c79c08 | 代理内存泄漏修复 | `internal/service/emby/redirect.go`、`internal/util/https/request.go`、`internal/util/https/web.go` | 业务 + 工具层 |
| e87ca20 | ge2o web 配置 | `config-example.yml`、`internal/config/ge2o.go`、`internal/service/emby/custom_cssjs.go`、`internal/web/handler.go` | 跨 config/service/web 三层 |
| 1fb2ce9 | localtree 新功能（核心） | 新建 `internal/service/openlist/localtree/`（6 文件）、`internal/util/files/files.go`、`main.go`(+6)、`.gitignore` | **大功能 = 新建子包 + util 补工具 + main.go 挂初始化** |
| 1f69501 | 前端重构 | `web/src/app/` 下 24 个 tsx 文件 | 前端独立 |

**规律总结**：

- 小功能（一个配置开关/一个接口行为）：`config-example.yml` + `internal/config/<域>.go` + `internal/service/emby/<功能>.go`（或改已有文件）。
- 新路由端点：`internal/constant/constant.go` 加 `Reg_*`/`Route_*` + `internal/web/route.go` 规则表加一行 + handler 落在对应 service 包。
- 大功能：在 `internal/service/<相关域>/` 下建子包（含 localtree.go 入口、type.go、model.go、`*_test.go`），需要初始化的在 `main.go` 挂 `Init()`，缺的工具在 `internal/util/` 新建子包。
- 前端页面：`web/src/app/routes.ts` 注册 + `routes/<path>/index.tsx` + 组件放 `components/`。

## 11. 反模式与技术债（如实记录）

1. **`go build ./...` 在干净 checkout 上失败**：`web/embed.go` 的 `//go:embed all:dist` 依赖前端先构建，但 `web/dist` 被 gitignore。开发者克隆后必须先跑 `build_web.sh`（或手动 `mkdir web/dist`）才能编译 Go 代码，对 IDE/CI 单独跑 Go 测试不友好（已实测报错）。
2. **路由规则表类型不安全**：`rules [][2]any`（`route.go:17`）用 `any` 装正则和 handler，`compileRules` 里两次类型断言（`handler.go:100,107`）；匹配是 O(n) 线性正则，规则约 25 条，每请求全量扫描。
3. **全局可变状态多**：`config.C`、`config.BasePath`、`webport.HTTP/HTTPS`、`webproxy.HttpUrl/HttpsUrl`、`emby.validApiKeys`、m3u8 的 `infoMap`/函数变量、ffmpeg 的 `execPath/execOk`。测试需要先初始化 config（`m3u8_test.go`、`api_test.go` 均可见）。
4. **pprof 无条件暴露**：`main.go:26` 在 60360 端口起 pprof，无开关无鉴权，生产部署存在信息泄露面。
5. **测试代码残留**：`internal/service/emby/emby.go:124` 的 `TestProxyUri`（内部 `testUris` 为空切片，永远返回 false）留在生产包中。
6. **m3u8 的函数变量注入**：`GetPlaylist` 等声明为 `var` 再在自身初始化时赋值（`m3u8.go:27-33, 145-164`），只有一处赋值，徒增间接层；且 m3u8 导入 emby 仅为一个常量。
7. **命名瑕疵**：`internal/util/parallels/paralllels.go`（包名和文件名都是三个 l）；`cmd/fake_mp3_1` 生成 4 分钟音频却命名 `fake_3h.mp3`；`localtree/api.go` 内容是 gin handler 而非 API 客户端（与 emby/api.go、openlist/api.go 语义不一致）。
8. **cmd/ 与 util 代码重复**：`cmd/fake_mp4/main.go` 与 `internal/util/mp4s/mp4s.go` 的 `writeBox`/box 结构几乎完全重复，两处需同步维护。
9. **大文件**：`emby/media.go` 570 行、`emby/playbackinfo.go` 497 行、`localtree/synchronizer.go` 410 行，单文件承担多个职责（解析+改写+缓存）。
10. **Dockerfile 硬编码中国镜像** `GOPROXY=https://goproxy.cn`（`Dockerfile` builder 阶段），海外构建可能变慢或失败。
11. **`slices`/`structs` 工具包当前无内部使用者**（grep 验证 0 导入），属预留或死代码。

## Caveats / Not Found

- 未运行程序验证运行时行为，全部结论来自代码静态阅读与 git 历史。
- `internal/web/cache/public.go`、`internal/config/ssl.go`、`internal/config/log.go` 仅快速浏览，未逐行分析。
- 前端 `web/src` 的组件细节（settings_modal 等）只看了结构和 API 调用点，未深入。
- "新功能落点"规律基于 6 个样本 commit，更早的历史（50 个 localtree 相关 commit）未逐一核对。
