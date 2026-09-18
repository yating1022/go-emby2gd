# 移除 OpenList 相关功能

> 父任务: `09-16-strm-proxy-play`。
> 建议排在 `09-16-strm-proxy-integration` **之后**（理由见"为什么是这个顺序"）。

## Goal

本项目部署形态已不需要 OpenList: strm 直链播放改由 `streamproxy` 代理完成。本子任务删除 OpenList 及其衍生能力, 让代码库只剩下实际使用的能力, 降低后续维护与阅读成本。

这是一次**大规模删除**, 目标不是"改对", 而是"删干净且不误删"。

## Background

### 排查陷阱（实施前必读）

`grep -r openlist .` 在本仓库里**几乎全是假阳性**: 模块路径本身是 `github.com/AmbitiousJun/go-emby2openlist/v2/...`, 因此**每一行 import 都会命中**。前端同理 —— `layout.tsx` / `index.tsx` 中绝大多数 "openlist" 是项目名、徽章链接与仓库地址。

判定真实耦合必须用以下方式之一:

- `grep -rn '"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/openlist' --include='*.go' .`（按 import 路径精确定位）
- `grep -rn 'config\.C\.\(Openlist\|Path\|VideoPreview\)' --include='*.go' .`（按配置访问点定位）
- **以编译器为准**: 删掉候选包后 `go build ./...` 的报错列表就是权威的耦合清单

### 真实耦合面

删除候选（Go 侧）:

| 范围 | 路径 |
|---|---|
| OpenList 客户端 | `internal/service/openlist/`（api / header / path / subtitle / type / walk + 测试） |
| 本地目录树 | `internal/service/openlist/localtree/`（api / localtree / model / snapshot / synchronizer / task + 测试） |
| 转码 m3u8 | `internal/service/m3u8/`（info / m3u8 / proxy / type + 测试） |
| emby→openlist 路径映射 | `internal/service/path/`（path + 测试） |
| 假音乐文件生成 | `internal/service/music/`（write + 测试） |
| ffmpeg 封装与自动下载 | `internal/service/lib/ffmpeg/` |
| 配置段 | `internal/config/openlist.go`、`internal/config/path.go`、`internal/config/video_preview.go` |
| 启动挂载 | `main.go` 中 `localtree.Init()` 与相关 import |

会被连带修改（**不是删除**）的既有文件:

| 文件 | 需要处理的内容 |
|---|---|
| `internal/web/route.go` | 移除 `Reg_UserEpisodeItems`、`Reg_ResourceMaster`、`Reg_ResourceMain`、`Reg_ProxyPlaylist`、`Reg_ProxyTs`、`Reg_ProxySubtitle`、`Route_UpdateOpenlistLocalTree` 对应规则行 |
| `internal/constant/constant.go` | 移除上述规则的正则/路由常量与 `Reg_OpenlistLocalTreeUpdatePrefix` |
| `internal/service/emby/redirect.go` | 移除 `Redirect2Transcode`; 移除 strm 分支中的转码判断与 OpenList 分支（第 6 步）; `ProxyOriginalResource` 相应简化 |
| `internal/service/emby/media.go` | 移除 `findVideoPreviewInfos`、`getAllPreviewTemplateIds` 及 `MsInfo` 相关的 templateId 解析 |
| `internal/service/emby/playbackinfo.go` | 移除 `MasterM3U8UrlTemplate` 常量与 video-preview 相关分支 |
| `internal/service/emby/items.go` | 移除 `ProxyAddItemsPreviewInfo` |
| `internal/service/emby/type.go` | 收缩 `MsInfo`（`Transcode` / `TemplateId` / `OpenlistPath` / `Format` / `SourceNamePrefix` 等字段） |
| `internal/service/emby/download.go`、`subtitles.go` | 移除其中的 OpenList 依赖 |
| `config-example.yml` | 移除 `openlist`、`video-preview`、`path` 三个顶层段 |
| 前端 | 删除 `web/src/app/routes/api/openlist_local_tree/`; 移除 `routes.ts` 中的对应 route 与 `layout.tsx:55-56` 的导航项 |

前端**保留**: `routes/index.tsx`（首页）、`routes/log/`（实时日志）、整个 `web/` 平台与 `ge2o` 自有接口（`Route_ValidateApiSecret`、`Route_SyncServerLog`）。

### 为什么是这个顺序

`remove-openlist` 要改动的文件（`redirect.go`、`media.go`、`playbackinfo.go`、`items.go`、`type.go`）与代理功能（`streamproxy-integration`）**高度重叠**。若先删再做代理:

1. 两个大改动的 diff 混在一起, 出问题无法判断是删除还是新功能导致的;
2. 删除 `MsInfo` 字段会改动 `resolveItemInfo` 这条**代理功能所依赖的**媒体信息解析链路, 在新功能尚未验证稳定时动它, 风险叠加。

因此本子任务排在最后, 以"代理功能已验证通过"为前提。

### 依赖检查（已核实）

- 新的 `streamproxy` 代理路径位于 strm 分支（第 4 步）, **早于** OpenList 分支（第 6 步）, 不依赖 `internal/service/path`, 因此移除 OpenList 不会破坏代理功能。
- `internal/service/m3u8`、`music`、`lib/ffmpeg` 仅被 localtree / 转码链路引用, 删除它们是干净的子图。

## Requirements

- **R1 影响面盘点**: 先按"排查陷阱"一节的方法产出权威耦合清单, 再动手删除。不得凭 `grep openlist` 的结果直接删。
- **R2 删除范围**: 覆盖上表"删除候选"全部范围, 加前端 `openlist_local_tree` 页面及其导航/路由项。
- **R3 连带简化**: 上表"会被连带修改"的文件需一并简化, 不得留下空壳函数、未使用的导出符号或 `if false` 式死分支。
- **R4 配置清理**: `config-example.yml` 与 `internal/config` 中不再有 `openlist` / `video-preview` / `path` 三个配置段; 若 `Config` 结构体因此有字段被移除, 需确认 `ReadFromFile` 的反射初始化不会对 nil 指针 panic。
- **R5 不误删**: 以下能力必须保持可用 —— PlaybackInfo 反代与缓存、Items/Latest/剧集排序、字幕缓存、图片处理、本地媒体代理回源、websocket 代理、自定义 js/css 注入、响应缓存、`ge2o` 自有接口、web 平台首页与实时日志页。
- **R6 编译与静态检查通过**: `go build ./...`、`go vet ./internal/...` 无错误, 且无未使用的 import。
- **R7 不引入新行为**: 本子任务只应删除能力与简化代码, 不应顺带优化或重构存活代码。发现存活代码问题记录到父任务 Notes, 不在本子任务处理。

## Acceptance Criteria

- [ ] **AC1** 上表"删除候选"的包目录已全部不存在。
- [ ] **AC2** `go build ./...` 与 `go vet ./internal/...` 通过, `gofmt -l` 无输出。
- [ ] **AC3** `grep -rn 'service/openlist\|service/m3u8\|service/path"\|service/music\|lib/ffmpeg' --include='*.go' .` 无结果。
- [ ] **AC4** `grep -rn 'config\.C\.\(Openlist\|Path\|VideoPreview\)' --include='*.go' .` 无结果。
- [ ] **AC5** `config-example.yml` 中不存在 `openlist` / `video-preview` / `path` 三个顶层段; 用该配置启动程序正常。
- [ ] **AC6** R5 列出的每一项能力手工抽查可用（至少覆盖: 播放命中前缀的 strm、播放未命中前缀的 strm、字幕、图片、`/ge2o/web` 首页与日志页）。
- [ ] **AC7** 前端 `npm run build` 通过, 导航中不再有 OpenList 本地目录树入口。
- [ ] **AC8** 删除后 strm 代理播放功能仍正常（回归验证 `09-16-strm-proxy-integration` 的 V1/V2）。

## Constraints

- 按包整体删除, 不做"注释掉保留"。
- 不修改 `internal/util/` 下的工具包（它们被存活代码共用; 若发现某工具包在删除后**零引用**, 记录到 Notes 由开发者决定, 不在本子任务删除）。
- 保留下载、图片、字幕、websocket、自定义 js/css 等无关能力的全部既有行为。

## Out of Scope

- 项目更名 / 品牌文案 / 仓库地址 / 徽章（前端首页与 README 中的 "OpenList" 字样属于项目名, 保留）。
- 删除零引用的 util 工具包（`slices`、`structs` 等既有技术债）。
- `cmd/` 下的开发期工具: `cmd/fake_mp3_1`（服务于已删除的 music 模块）与 `cmd/fake_mp4`（与 `util/mp4s` 重复）在删除后大概率失去意义, **由开发者确认后再删**, 本子任务默认保留并记录。

## Notes

- 本子任务**独立提交**, 不与前三个子任务合并。这样 revert 它不会连带回滚代理功能。
- 若删除过程中发现某能力被存活代码意外依赖, **停下并记录**, 不要临时加兼容层 —— 那会让"移除 OpenList"变成"OpenList 换个名字继续存在"。
