# 执行计划: 移除 OpenList 相关功能

## 前置条件

`09-16-strm-proxy-integration` 的端到端验证（V1/V2/V3）已通过。理由见 `prd.md`"为什么是这个顺序"。

```bash
export PATH=$PATH:/usr/local/go/bin
mkdir -p web/dist
go build ./... && go vet ./internal/...
```

先记录一个**删除前的基线**（用于 AC8 的回归对比）:

```bash
go build -o /tmp/ge2o-before .    # 删除前可执行文件, 保留作对照
```

## Step 0 影响面盘点（先做, 不要跳）

按 `prd.md`"排查陷阱"一节, 用精确查询产出权威清单:

```bash
# 1 真实 import 了 openlist 及其衍生包的 Go 文件
grep -rn '"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/\(openlist\|m3u8\|path\|music\|lib/ffmpeg\)' --include='*.go' .

# 2 访问被移除配置段的位置
grep -rn 'config\.C\.\(Openlist\|Path\|VideoPreview\)' --include='*.go' .

# 3 被移除路由常量的引用
grep -rn 'Reg_ProxyPlaylist\|Reg_ProxyTs\|Reg_ProxySubtitle\|Reg_ResourceMaster\|Reg_ResourceMain\|Reg_UserEpisodeItems\|Reg_OpenlistLocalTreeUpdatePrefix\|Route_UpdateOpenlistLocalTree' --include='*.go' .
```

把结果与 `prd.md` 的清单表格对照, **补齐表格中遗漏的项**, 再开始删除。

**不要**用 `grep -rn openlist .` 作为依据 —— 模块路径会让每个 import 行都命中。

## Step 1 删除叶子包（无内部依赖者的先删）

顺序（被依赖者后删）:

1. `internal/service/music/`
2. `internal/service/lib/ffmpeg/`
3. `internal/service/m3u8/`
4. `internal/service/path/`
5. `internal/service/openlist/`（含 `localtree/` 整个子树）

每删一个跑一次 `go build ./...`, 把报错文件记下来 —— 这些就是待处理的连带引用。

## Step 2 配置层

1. 删除 `internal/config/openlist.go`、`internal/config/path.go`、`internal/config/video_preview.go`。
2. `internal/config/config.go`: 移除 `Openlist`、`Path`、`VideoPreview` 字段及对应的初始化分支。
3. **检查 `ReadFromFile` 的反射初始化**（`config.go`）: 确认移除字段后不会因为对 nil 指针调用 `Init()` 而 panic, 也不会残留对这些类型的反射引用。
4. `config-example.yml`: 移除 `openlist`、`video-preview`、`path` 三个顶层段。

验证: 用精简后的 `config-example.yml` 作为 `config.yml` 启动, 程序应正常起来。

## Step 3 启动与路由

1. `main.go`: 移除 `localtree` import 与 `localtree.Init()` 调用块。
2. `internal/web/route.go`: 移除题述规则行（注意保持规则表顺序语义, 不要打乱其余规则的相对顺序）。
3. `internal/constant/constant.go`: 移除对应正则与路由常量。

## Step 4 emby 包连带简化

按依赖从下往上改:

1. `internal/service/emby/type.go`: 收缩 `MsInfo`（移除 `Transcode`、`TemplateId`、`OpenlistPath`、`Format`、`SourceNamePrefix` 等不再有来源的字段）, 同步修正 `String()`。
2. `internal/service/emby/media.go`: 移除 `findVideoPreviewInfos`、`getAllPreviewTemplateIds` 与 `resolveItemInfo` 中解析 templateId 的分支。
3. `internal/service/emby/redirect.go`:
   - 移除 `Redirect2Transcode`
   - 移除 strm 分支中的 `useTranscode` 判断与转码重定向
   - 移除 OpenList 分支（原第 6 步及其 `handleOpenlistResource` 闭包）
   - `ProxyOriginalResource` 相应简化
4. `internal/service/emby/playbackinfo.go`: 移除 `MasterM3U8UrlTemplate` 与 video-preview 分支。
5. `internal/service/emby/items.go`: 移除 `ProxyAddItemsPreviewInfo`。
6. `internal/service/emby/download.go`、`subtitles.go`: 移除 OpenList 依赖。

**重点保护**: `redirect.go` 中 `09-16-strm-proxy-integration` 新增的代理分支必须原样保留。简化时逐行对照, 不要整段重写该函数。

## Step 5 前端

1. 删除 `web/src/app/routes/api/openlist_local_tree/` 整个目录。
2. `web/src/app/routes.ts`: 移除该 route 条目。
3. `web/src/app/routes/layout.tsx`: 移除 `label: "OpenList 本地目录树"` 的导航项。
4. 保留首页、日志页与所有品牌文案。

```bash
cd web && npm run build
```

## Step 6 验证

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -l internal/ main.go
go build ./...
go vet ./internal/...
go test ./internal/...
```

残留检查（应无输出）:

```bash
grep -rn 'service/openlist\|service/m3u8\|service/path"\|service/music\|lib/ffmpeg' --include='*.go' .
grep -rn 'config\.C\.\(Openlist\|Path\|VideoPreview\)' --include='*.go' .
grep -rn 'Reg_ProxyPlaylist\|Reg_ProxyTs\|Reg_ProxySubtitle\|Reg_ResourceMaster\|Reg_ResourceMain\|Reg_UserEpisodeItems\|Reg_OpenlistLocalTreeUpdatePrefix' --include='*.go' .
```

## Step 7 回归验证（AC6 / AC8）

用删除后的可执行文件, 逐项手工验证 `prd.md` R5 列出的能力:

| 能力 | 检查方式 |
|---|---|
| 命中前缀的 strm 播放 | 播放 + 日志出现 `[直链代理]` 完整链路（AC8 核心项） |
| 未命中前缀的 strm | 仍返回 302 |
| 本地媒体 | 播放正常 |
| 字幕 | 字幕接口返回正常 |
| 图片 | 图片接口返回正常 |
| websocket | 客户端播放进度上报正常 |
| 自定义 js/css | Emby Web 首页脚本注入生效 |
| `/ge2o/web` 首页 | 可打开 |
| `/ge2o/web/log` 实时日志 | 可打开且有日志流入 |
| 配置校验 | 用非法配置启动, 报中文错误 |

## Review Gate

- [ ] 删除的每个包目录都整体消失, 没有留下空目录或空文件
- [ ] 没有留下空壳函数、未使用的导出符号或 `if false` 式死分支
- [ ] `redirect.go` 中的代理分支逐行对照确认未被动过
- [ ] 没有修改 `internal/util/` 下的任何工具包
- [ ] 没有顺带重构存活代码（R7）
- [ ] 前端导航与路由表一致（无指向已删页面的入口）
- [ ] `config-example.yml` 与 `internal/config` 结构一致

## 回滚点

本子任务**独立提交一个 commit**。`git revert` 即可完整恢复 —— 前提是本次改动没有混入前三个子任务的内容。提交前用 `git diff --stat` 确认改动范围只覆盖 `prd.md` 列出的文件。

## 风险

| 风险 | 表现 | 应对 |
|---|---|---|
| 误删存活能力 | 某个既有功能失效 | Step 7 逐项回归; 保留 `/tmp/ge2o-before` 做行为对照 |
| 破坏代理功能 | AC8 失败 | Review Gate 强制对照 `redirect.go` 代理分支; 先做集成验证再删 |
| 配置结构变更引发启动 panic | 启动即失败 | Step 2 显式检查 `ReadFromFile` 反射初始化 |
| 前端构建产物与后端不同步 | 页面 404 或白屏 | Step 5 重新 `npm run build` 后再 `go build` |
| 连带依赖比预期深 | 删除过程中报错超出清单 | 记录并停下（`prd.md` Notes）: 不要临时加兼容层 |
