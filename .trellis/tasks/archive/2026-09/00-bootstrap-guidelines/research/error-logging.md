# Research: 错误处理模式与日志规范

- **Query**: 全面梳理 go-emby2openlist 项目的错误处理模式与日志规范，为撰写 `.trellis/spec/backend/error-handling.md` 和 `.trellis/spec/backend/logging-guidelines.md` 提供素材
- **Scope**: internal（代码研究，全部结论基于真实代码与行号）
- **Date**: 2026-09-16

---

## 关键结论速览

1. **日志全部走自研 `internal/util/logs` 包**：7 个级别函数 `Info/Success/Warn/Error/Tip/Progress/Raw`（printf 风格），底层是 `fmt.Print` 到 stdout；**没有**级别过滤、动态级别、结构化字段、调用方信息。
2. **没有日志文件输出与轮转**：唯一的扩展点是 `logs.RegisterLogger(Logger)`，当前只有两个实现——stdout 的 `defaultLogger` 和 WebSocket 推送器 `wsLogger`（`internal/service/log.go`）。
3. **注意：`logs.Error` 输出的是灰色（`colors.ToGray`），不是红色**（`internal/util/logs/logs.go:53`）；红色 `colors.Red` 只在 localtree 的私有 `logf(colors.Red, ...)` 和 `main.go` 的 `colors.ToRed(err.Error())` 中出现。
4. **错误创建以 `fmt.Errorf` 为主（116 处），`errors.New` 次之（27 处）**；无 `errors.Wrap/WithStack/Is/As/Join`，无实现 `Error()` 接口的自定义错误类型；格式化动词混用：`%v`(54)、`%s`(51)、`%w`(29)。
5. **存在 3 类哨兵错误**：包级 `openlist.ErrWalkEOF`、包级 `jsons.ErrBreakRange`、以及 emby 包内**每次现场 `errors.New("have returned")`** 的控制流信号（`playbackinfo.go:90,201`）。
6. **service → web 的错误传播有两条平行通道**：Go 惯用的 `error` 返回值，以及 `model.HttpRes[T]{Code,Data,Msg}` 结构体（openlist/emby 的 Fetch 系列把错误编码进 `Code/Msg`）。
7. **web 层统一错误出口是 `checkErr(c, err)`**（`internal/service/emby/redirect.go:216-234`）：按 `config.C.Emby.ProxyErrorStrategy` 决定「拒绝（500 纯文本）」还是「回源代理」；代理类接口错误响应体是固定文案（如 `"代理接口失败, 请检查日志"`），不向客户端暴露 err 详情。
8. **panic/recover 仅两处**：gin 自带 `gin.Recovery()` 中间件（`web.go:57,74`）+ `jsons.New` 里的 `defer recover` 转 `fmt.Errorf`；`ProxySocket` 初始化时 `url.Parse` 失败会直接 `panic`（`emby/emby.go:31`），依赖 `gin.Recovery` 兜底。
9. **goroutine 错误处理是「记日志即丢弃」的 fire-and-forget 风格**：异步辅助请求失败仅 `logs.Warn`（`playing.go:106-113`）；并发收集用 `errgroup`（`localtree/synchronizer.go`、`custom_cssjs.go`）或带缓冲 channel 传 `nil` 表示失败（`playbackinfo.go:151-175`）。
10. **`internal/util/trys.Try` 是同步重试工具**：`Try(fn, tryNum, interval)`，注意 `tryNum <= 0` 时直接返回 `nil`（调用方无法区分「成功」与「未尝试」）。

---

## 一、日志基础设施

### 1.1 `internal/util/logs/logs.go` —— 级别函数

每个级别函数结构完全相同：`时间戳 + 彩色级别前缀 + fmt.Sprintf 格式化消息 + '\n'`，最后统一交给 `writeLog`：

| 函数 | 行号 | 颜色 | 前缀 | 全项目调用数（不含测试） |
|---|---|---|---|---|
| `Info` | logs.go:12-21 | Blue | `[INFO]` | 22 |
| `Success` | logs.go:24-33 | Green | `[SUCCESS]` | 12 |
| `Warn` | logs.go:36-45 | Yellow | `[WARN]` | 10 |
| `Error` | logs.go:48-57 | **Gray** | `[ERROR]` | 29 |
| `Tip` | logs.go:60-66 | Gray | 无前缀 | 7 |
| `Progress` | logs.go:69-75 | Purple | 无前缀 | 2 |
| `Raw` | logs.go:78-80 | 无 | 无 | 2（访问日志、localtree logf） |

关键代码（logs.go:47-57）：

```go
// Error 输出红色 Error 日志   <-- 注释写"红色"，实际实现是 ToGray
func Error(format string, v ...any) {
	var sb strings.Builder
	sb.WriteString(Now())
	msg := fmt.Sprintf(format, v...)
	sb.WriteString(colors.ToGray("[ERROR] " + msg))
	sb.WriteByte('\n')
	writeLog(sb.String())
}
```

注意：`Error` 的函数注释声称"输出红色 Error 日志"，但实现用的是 `colors.ToGray`——注释与实现不一致（现状记录）。

`Now()`（logs.go:83-85）返回 `time.Now().Format("2006-01-02 15:04:05") + " "`，即日志时间戳格式固定为 `YYYY-MM-DD HH:MM:SS`。

`writeLog`（logs.go:88-101）先写 `DefaultLogger`，再遍历 `otherLoggers`（`sync.Map`）把同一条内容广播给所有注册的 Logger。

### 1.2 `internal/util/logs/logger.go` —— Logger 抽象与注册机制

```go
// logger.go:10-12
type Logger interface {
	Log(content string)
}
```

- `defaultLogger`（logger.go:14-18）实现即 `fmt.Print(content)`，实例化为全局 `DefaultLogger`（logger.go:21）。
- `RegisterLogger(logger) (id, ok)`（logger.go:27-36）用 `uuid.NewString()` 作 key 存入 `otherLoggers sync.Map`；`RemoveLogger(id)`（logger.go:39-41）移除。
- 该抽象**只传递已格式化的字符串**，注册方拿不到级别、结构化字段等元信息。

### 1.3 `internal/util/logs/colors/colors.go` —— ANSI 真彩色

- 6 个 24-bit 颜色常量（colors.go:7-13）：`Blue/Green/Yellow/Red/Purple/Gray`，值为 `\x1b[38;2;R;G;Bm` 形式的 ANSI 转义序列，结尾统一 `\x1b[0m`（reset，colors.go:14）。
- `Enabler` 接口（colors.go:18-22）+ 全局 `enabler`（colors.go:24）：`WrapColor`（colors.go:62-67）在 `enabler != nil && !enabler.EnableColor()` 时返回原字符串（剥离颜色码），否则包裹颜色。
- 配置入口在 `internal/config/log.go`：`Log` 结构只有 `DisableColor bool`（yaml: `disable-color`，log.go:6-8），`Init()` 调用 `colors.SetEnabler(lc)`（log.go:11-14）。**这是日志相关的唯一配置项**。

### 1.4 WebSocket 实时日志 —— `internal/service/log.go`

`wsLogger`（log.go:15-20）把日志推送到 WebSocket 连接：

- 字段：`conn`、`channel chan string`（缓冲 100）、`done chan struct{}`、`closed atomic.Bool`。
- `close()`（log.go:32-39）用 `CompareAndSwap` 防止重复关闭。
- `consume()`（log.go:42-59）循环 `WriteMessage`，写出失败即 `close()` 退出。
- `Log(content string)`（log.go:62-68）实现 `logs.Logger` 接口，用 `select ... default` **非阻塞**投递——通道满时直接丢弃日志（不阻塞业务）。
- `SyncServerLog(c *gin.Context)`（log.go:78-111）：校验 query 中的 `secret`（错误时返回 `c.JSON(200, model.Response{Message: "密钥错误"})`）→ `logUpgrader.Upgrade` 升级 WS（失败 `logs.Error`）→ `logs.RegisterLogger(wsLogger)` → `defer logs.RemoveLogger(id)` → 启动消费 goroutine 并循环 `ReadMessage` 探活。

### 1.5 HTTP 访问日志中间件 —— `internal/web/log.go`

`CustomLogger(port string) gin.HandlerFunc`（log.go:14-34）：请求前后计时，用一条 `logs.Raw` 输出单行访问日志，字段依次为：黄色版本头 `[ge2o:版本]`、开始时间、状态码（着色）、耗时、ClientIP、蓝色端口、蓝色匹配路由（`MatchRouteKey`）、蓝色 Method、RequestURI。

`colorStatusCode`（log.go:37-46）：2xx/3xx → 绿，4xx/5xx → 红（复用 `internal/util/https` 的 `IsSuccessCode/IsRedirectCode/IsErrorCode`，https.go:58-77，判断方式是状态码字符串前缀 `"2"`/`"4"`,`"5"`），其余蓝。

挂载位置：`internal/web/web.go:58`（HTTP）与 web.go:75（HTTPS），紧跟在 `gin.Recovery()` 之后。

### 1.6 混用的标准库 `log` 与裸 `fmt.Print`

自研包之外还存在三类输出（互不统一）：

| 位置 | 用法 |
|---|---|
| `main.go:31,38,44,65,71` | `log.Fatal` / `log.Fatalf`——启动阶段致命错误（配置读取、localtree 初始化、web 启动、端口冲突），其中 38/44 行还先 `colors.ToRed(err.Error())` 染色 |
| `internal/web/web.go:33,35` | `log.Fatal("http 服务异常: ", err)`——任一 listener 返回错误即整个进程退出 |
| `internal/util/https/https.go:86`、`internal/util/urls/urls.go:81` | `log.Printf`——util 层降级场景 |
| `internal/util/jsons/jsons.go:116` | `log.Panicf("无效的数据类型, ...")`——反射类型不支持时 panic |
| `internal/service/lib/ffmpeg/auto_download.go:60-61,136` | 裸 `fmt.Printf` 输出下载进度条（`\r` 覆盖式） |

### 1.7 能力边界（现状确认）

以下能力**均不存在**（已 grep 确认）：日志级别过滤/动态调整（如 debug 开关）、按模块开关日志、日志写入文件、轮转（无 lumberjack 等依赖，go.mod 仅有 gin/uuid/websocket/yaml/id3v2/sync）、结构化日志（无 slog/zap/logrus，`slog.` 零命中）、日志异步刷盘、调用文件/行号输出。

---

## 二、典型日志调用模式

### 2.1 场景 → 级别映射（真实调用点）

以下是从 29 处 `logs.Error`、22 处 `logs.Info` 等调用中归纳出的实际使用惯例：

| 场景 | 惯用级别 | 真实调用点示例 |
|---|---|---|
| 请求链路关键节点（解析出 itemInfo） | `Info` | `redirect.go:71`（`logs.Info("解析到的 itemInfo: %v", itemInfo)`）、`playbackinfo.go:49`、`download.go:26` |
| 重定向/代理成功 | `Success` | `redirect.go:83`（`logs.Success("重定向 playlist: %s", u.String())`）、`redirect.go:98`、`redirect.go:147`、`m3u8/proxy.go:87`（`logs.Success("重定向 ts: %s", link)`） |
| 代理/解析/请求失败 | `Error` | `emby.go:118`（`logs.Error("代理异常: %v", err)`）、`redirect.go:226,231`（checkErr 内）、`openlist/api.go:40,66`、`playbackinfo.go:313,368` |
| 可降级失败（回退原始链接/辅助请求失败） | `Warn` | `redirect.go:261`（`logs.Warn("内部重定向失败: %v", err)` 后返回原始链接）、`playing.go:107,111`（`logs.Warn("辅助发送 Progress 进度记录失败: %v", err)`）、`playbackinfo.go:447`、`webproxy/webproxy.go:23,32`（代理地址解析失败仅告警并跳过） |
| 调试/提示性信息（路径转换过程、缓存淘汰） | `Tip` | `path/path.go:48`（`logs.Tip("embyPath 转换路径: %s", pathRoutes.String())`）、`m3u8/m3u8.go:207,270`（playlist 淘汰提示）、`playing.go:105` |
| 后台任务进度 | `Progress` | `m3u8/m3u8.go:228`（`logs.Progress("当前正在维护的 playlist 个数: ...")`）、`m3u8/info.go:220`（`logs.Progress("更新 playlist, ...")`）、`localtree/synchronizer.go:103` |
| 配置初始化流程 | `Info` + `Success` 配对 | `web/route.go:20,97`（"正在初始化路由规则..." → "路由规则初始化完成"） |

### 2.2 localtree 包的私有 `logf` 前缀模式

`internal/service/openlist/localtree/localtree.go:76-79` 定义了模块级日志助手：

```go
// logf 带上前缀的日志输出
func logf(c colors.C, format string, v ...any) {
	s := fmt.Sprintf(format, v...)
	logs.Raw("%s%s\n", logs.Now(), colors.WrapColor(c, "[openlist 目录树]: "+s))
}
```

- 用 `logs.Raw` 绕过级别函数，手动拼接时间戳 + 模块前缀 `[openlist 目录树]: ` + 自选颜色（`logf(colors.Blue/Green/Red/Yellow/Purple/Gray, ...)`，全包 14 处）。
- 失败用 `logf(colors.Red, "同步失败: %v", err)`（localtree.go:63,70；localtree/api.go:66），成功用 `logf(colors.Green, "同步完成, ...")`（localtree.go:56）——这是全项目**唯一**用红色标记错误的地方。

### 2.3 消息格式约定（从调用点归纳）

- 消息为中文短语 + 冒号 + `%v`/`%s` 承载 err 或关键变量，如 `"代理接口失败: %v"`、`"FsList 请求失败: %v"`。
- 失败消息普遍带上下文参数：`"路由正则编译失败, pattern: %v, error: %v"`（web/handler.go:102）、`"请求远程地址失败, url: %s, err: %v"`（m3u8/info.go:95）、`"playlist 更新失败, path: %s, template: %s, err: %v"`（m3u8/m3u8.go:90）。
- 面向用户的提示常以 `"请检查日志"` 结尾：`"代理接口失败, 请检查日志"`（redirect.go:227）、`"代理 m3u8 失败, 请检查日志"`（m3u8/proxy.go:43）。
- 个别不一致：`m3u8/proxy.go:42` 写成 `logs.Error("代理 m3u8 失败: %v", err.Error())`——先 `.Error()` 再传 `%v`，与其余直接传 `err` 的写法不同。

---

## 三、错误创建与包装

### 3.1 `fmt.Errorf` 为主（116 处），动词混用

- `%v`：54 处，如 `internal/util/https/request.go:140`（`fmt.Errorf("读取请求体失败: %v", err)`）。
- `%s`：51 处，如 `internal/service/openlist/api.go:194`（`fmt.Errorf("Fetch 请求响应状态异常: %d, 消息: %s", res.Code, res.Message)`）。
- `%w`：29 处，**集中在较新的模块**：`localtree/synchronizer.go`（10 处，如 :78 `"初始化快照异常: %w"`）、`localtree/task.go`（6 处）、`internal/util/files/files.go`（3 处）、`openlist/walk.go:68`、`music/write.go`、`ffmpeg/auto_download.go:141`、`config/openlist.go:25,84`。
- 全项目**没有**任何 `errors.Is/As/Unwrap/Join` 调用（grep 零命中）——即 `err == openlist.ErrWalkEOF`（synchronizer.go:196,214）和 `err == ErrBreakRange`（jsons/item.go:63,127）都是**直接 `==` 比较**，`%w` 的包装链从未被解开过。

### 3.2 `errors.New`（27 处）的用途

1. 参数校验：`errors.New("参数为空")`（util/https/web.go:78,99）、`errors.New("仅支持 GET")`（m3u8/proxy.go:21）、`errors.New("参数不足")`（m3u8/proxy.go:32）、`errors.New("参数 c 不能为空")`（emby/media.go:425）。
2. 把非 error 的失败状态转成 error 传给 `checkErr`：`errors.New(res.Msg)`（emby/api.go:24、playbackinfo.go:73）、`errors.New("获取不到 MediaSources 属性")`（playbackinfo.go:81）、`errors.New(resp.Status)`（episode.go:46）、`errors.New("请求 openlist 失败: " + res.Msg)`（m3u8/info.go:226）。
3. 哨兵错误（见 3.3）。

### 3.3 哨兵错误清单

| 哨兵 | 定义处 | 用途 | 比较方式 |
|---|---|---|---|
| `openlist.ErrWalkEOF` | `openlist/walk.go:13`（`errors.New("walk EOF")`） | 分页遍历结束信号 | `err == openlist.ErrWalkEOF`（synchronizer.go:196,214） |
| `jsons.ErrBreakRange` | `util/jsons/item.go:19`（`errors.New("break arr or obj range")`） | `RangeArr/RangeObj` 回调中提前终止遍历 | `err == ErrBreakRange`（item.go:63,127）；调用方返回它终止遍历（download.go:58,81,94,98；playbackinfo.go:327） |
| `haveReturned` | `playbackinfo.go:90`、`playbackinfo.go:201` **各自现场 `errors.New("have returned")`** | `RangeArr` 回调中标记"已直接回源响应客户端"，外层 `if err == haveReturned { return }`（playbackinfo.go:157）与 `return err == haveReturned`（:224） | 局部 `==` |

注意 `haveReturned` 不是包级变量，两处各自创建、各自比较，属于「用 error 做控制流」的内部约定。

### 3.4 无第三方错误库、无自定义错误类型

go.mod（项目根）无 pkg/errors 等依赖；grep 全仓库没有实现 `Error() string` 方法的自定义错误结构体；错误分类（该不该回源、该不该重试）全部依赖 `model.HttpRes.Code`（int）或字符串判断。

### 3.5 `internal/util/trys` —— 同步重试工具

`internal/util/trys/trys.go:8-22` 完整实现：

```go
func Try(fn func() (err error), tryNum int, interval time.Duration) error {
	if tryNum <= 0 {
		return nil
	}
	var err error
	for range tryNum {
		if err = fn(); err == nil {
			return nil
		}
		time.Sleep(interval)
	}
	return err
}
```

- 语义：最多尝试 `tryNum` 次，两次之间 `time.Sleep(interval)`；全部失败返回**最后一次**错误。
- 边界：`tryNum <= 0` 直接返回 `nil`（调用方无法区分「成功」与「根本没试」）；最后一次失败后也会多 sleep 一个 interval。
- 真实调用点（4 个文件 6 处）：
  - `emby/redirect.go:249-258`：内部重定向探测，3 次 / 2s，失败 `logs.Warn` 后降级返回原始链接。
  - `localtree/task.go:113,190,202`：ffmpeg 探测/元数据提取，3 次 / 1s。
  - `localtree/task.go:241-268`：RawWriter 下载源文件，3 次 / 5s。
  - `localtree/synchronizer.go:194-201,212-219`：openlist 分页遍历，3 次 / 5s（并在回调里把 `ErrWalkEOF` 归一化为 nil）。

---

## 四、错误传播路径

### 4.1 service 层两种并行的错误返回风格

**风格 A：Go 惯用 `error` 返回值**（util 层与多数内部函数）：
- `https.RequestHolder.execute() (string, *http.Response, error)`（util/https/request.go:128-183），内部逐层 `fmt.Errorf("读取请求体失败: %v", err)` / `"创建请求失败: %v"` 包装。
- `https.ProxyPass(r, w, remote) error`（util/https/web.go:97-118）。
- `resolveItemInfo(c, routeType) (ItemInfo, error)`（emby/media.go:423-469）。
- `fetchFullPlaybackInfo(itemInfo) (*jsons.Item, error)`（playbackinfo.go:454-483）。
- `jsons.New(rawJson) (*Item, error)`（util/jsons/jsons.go:122-155）。

**风格 B：`model.HttpRes[T]` 结构体**（`internal/model/http.go:4-8`：`Code int / Data T / Msg string`）：
- openlist 侧：`FetchResource`（openlist/api.go:20-82）、`FetchFsList/FetchFsGet/FetchFsOther`（api.go:87-151）——内部先走风格 A 的 `Fetch(...) error`（api.go:154-206，逐项 `fmt.Errorf("Fetch 请求失败: %v", err)` 等 6 种包装），再转成 `HttpRes{Code: 500, Msg: fmt.Sprintf("FsGet 请求失败: %v", err)}`。
- emby 侧：`RawFetch(uri, method, header, body) (model.HttpRes[*jsons.Item], http.Header)`（emby/api.go:37-60）——请求失败/解析失败统一折叠为 `Code: http.StatusBadRequest, Msg: "请求发送失败: " + err.Error()`（api.go:50,57）。
- 下游消费方式是判 `res.Code != http.StatusOK`（如 redirect.go:139、playbackinfo.go:72、m3u8/info.go:225）。

### 4.2 web 层统一错误出口：`checkErr`

`internal/service/emby/redirect.go:212-234`：

```go
func checkErr(c *gin.Context, err error) bool {
	if err == nil || c == nil {
		return false
	}
	// 异常接口, 不缓存
	c.Header(cache.HeaderKeyExpired, "-1")

	// 采用拒绝策略, 直接返回错误
	if config.C.Emby.ProxyErrorStrategy == config.PeStrategyReject {
		logs.Error("代理接口失败: %v", err)
		c.String(http.StatusInternalServerError, "代理接口失败, 请检查日志")
		return true
	}

	logs.Error("代理接口失败: %v, 回源处理", err)
	ProxyOrigin(c)
	return true
}
```

- 返回 `true` 表示请求已被处理，handler 立即 `return`——这是全部 emby handler 的标准写法（`if checkErr(c, err) { return }`，全项目 20+ 处）。
- 两种策略：`reject` → 500 纯文本；否则 → 记 Error 日志后 `ProxyOrigin(c)` 回源兜底（`ProxyOrigin` 内部失败再 `logs.Error("代理异常: %v", err)`，emby.go:117-119，即一次请求可能输出两条 Error）。
- 附带副作用：设置响应头 `Expired: -1` 禁用缓存中间件缓存该响应。
- 亦有内联调用形态：`checkErr(c, https.ProxyPass(...))`（episode.go:24）。

### 4.3 客户端错误响应格式（三种并存的约定）

1. **代理类接口**（emby/m3u8 资源链路）——纯文本 + 固定文案，不回传 err：
   - 500：`c.String(http.StatusInternalServerError, "代理接口失败, 请检查日志")`（redirect.go:227）；`"代理字幕失败, 请检查日志"`（m3u8/proxy.go:128,139）；`"查无缓存, 请稍后尝试重新播放"`（playbackinfo.go:369）。
   - 400：`c.String(http.StatusBadRequest, "代理 m3u8 失败, 请检查日志")`（m3u8/proxy.go:43,76,113）、`"获取不到播放列表, 请检查日志"`（proxy.go:68）。
   - 401：`c.String(http.StatusUnauthorized, "鉴权失败")` + `c.Abort()`（emby/auth.go:135-137）。
   - 403：`c.String(http.StatusForbidden, "下载接口已禁用")`（emby/download.go:135）。
2. **管理类接口**（ge2o 自身 API）——HTTP 一律 200 + JSON body 表达成败，结构为 `model.Response{Success bool `json:"success"`; Message string `json:"message"`}`（`internal/model/gin.go:4-7`）：
   - `service/service.go:15,21,27,34`（ValidateApiSecret）、`service/log.go:82`、`localtree/api.go:29,35,42,48,55,78,86`。
   - 失败详情放 Message：`model.Response{Message: "同步失败: " + err.Error()}`（localtree/api.go:78）——**这里会把 err 原文返回给客户端**，与代理类接口「不暴露 err」的做法相反。
3. **透传源站状态**：`episode.go:45-47` 把 emby 源站非 200 状态转成 `checkErr(c, errors.New(resp.Status))`。

### 4.4 一条完整链路示例（资源重定向）

`Redirect2OpenlistLink`（redirect.go:59-178）：
`resolveItemInfo`(error) → `checkErr` → `getEmbyFileLocalPath`(error) → `checkErr` → `path.Emby2Openlist` 返回 `OpenlistPathRes{Success, Path, Range func() ([]string, error)}`（path/path.go:15-25，惰性错误）→ `openlist.FetchResource` 返回 `HttpRes`（失败时把多个 path 的错误累积进 `allErrors strings.Builder`，redirect.go:132-140）→ 全部失败后 `checkErr(c, fmt.Errorf("获取直链失败: %s", allErrors.String()))`（redirect.go:177）。

---

## 五、panic / recover 使用情况

| 位置 | 内容 |
|---|---|
| `internal/web/web.go:57`、`web.go:74` | `r.Use(gin.Recovery())`——HTTP/HTTPS 两个 engine 各挂一份，是全局 panic 兜底（engine 用 `gin.New()` 创建，:56/:73） |
| `internal/util/jsons/jsons.go:123-127` | 全项目唯一手写 recover：`jsons.New` 中 `defer func() { if rec := recover(); rec != nil { err = fmt.Errorf("内部转换异常: %v", rec) } }()`，把深层 panic 归一化为 error |
| `internal/util/jsons/jsons.go:57` | `panic("不支持的 map 类型")`——反射遇到非 string key 的 map |
| `internal/util/jsons/jsons.go:116` | `log.Panicf("无效的数据类型, kind: %v, name: %v", ...)`——`FromValue` 遇到不支持的类型 |
| `internal/service/emby/emby.go:31` | `panic("转换 emby host 异常: " + err.Error())`——`ProxySocket` 的 `sync.Once` 初始化中 `url.Parse(config.C.Emby.Host)` 失败；发生在请求 goroutine 内，实际由 `gin.Recovery()` 捕获（首个触发请求 500，代理器保持未初始化，后续请求会再次尝试） |
| `main.go:31,38,44` | 启动失败用标准库 `log.Fatal`（不是 panic，直接 os.Exit(1)） |

没有自定义 Recovery 中间件、没有在 goroutine 里普遍加 defer recover（goroutine panic 未 recover 时将导致整个进程崩溃，当前依赖「goroutine 内不主动 panic」的编码约定）。

---

## 六、goroutine 中的错误处理

按场景分四类（启动点共 40+ 处 `go ...`）：

1. **fire-and-forget + 日志兜底**（最常见）：
   - `go sendPlayingProgress(kType, kName, apiKey, body)`（emby/playing.go:57）：函数内部对两个内部请求逐个 `logs.Warn("辅助发送 Progress 进度记录失败: %v", err)` 后 return（playing.go:106-113），成功则 `logs.Success`（:114）。
   - `go sendOpenStreamPlaybackInfoReqToOrigin(itemInfo)`（redirect.go:103、playbackinfo.go:142）。
   - `main.go:26` `go func() { http.ListenAndServe(":60360", nil) }()`（pprof）——**错误被完全丢弃**，连日志都没有。
2. **channel 回传结果（错误被折叠为 nil）**：
   - `resChan := make(chan []*jsons.Item, 1); go findVideoPreviewInfos(source, itemInfo.ApiKey, resChan)`（playbackinfo.go:151-153）：goroutine 内部失败时 `logs.Error` + `resChan <- nil`（media.go:169-171,184-186），主流程 `previewInfos := <-resChan` 仅判空（playbackinfo.go:170-175）——错误细节只存在于日志。
3. **errgroup 聚合错误**（`golang.org/x/sync/errgroup`，go.mod 已声明）：
   - `localtree/synchronizer.go:84`（`errgroup.WithContext`）：子任务错误经 `s.eg.Go(func() error {...})` 聚合，`s.eg.Wait()` 的 err 再被 `fmt.Errorf("同步异常: %w", err)` 上抛（synchronizer.go:117-119）；`ctx.Done()` 用于取消（:290,328）。
   - `emby/custom_cssjs.go:110-118,128-160`：裸 `new(errgroup.Group)` 并发读文件，`g.Wait()` 错误上抛后由调用方 `logs.Error("加载自定义脚本异常: %v", err)`（custom_cssjs.go:169-172）。
   - `util/jsons/jsons.go:190-214,237-260`：并行构造 JSON 子项，错误经 result channel 汇聚后直接返回首个 err。
4. **常驻后台循环（单 goroutine 维护内存状态）**：
   - `m3u8/m3u8.go:71-288` `loopMaintainPlaylist`：统一用闭包 `printErr(info, err)` 输出 `logs.Error("playlist 更新失败, path: %s, template: %s, err: %v", ...)`（m3u8.go:89-91），更新失败即从内存淘汰（:219-224,246-249）。
   - `web/cache/holder.go:54-102` `loopMaintainCache`：纯状态维护，无错误路径。
   - `localtree/localtree.go:61-73` `startSync`：定时同步，失败 `logf(colors.Red, "同步失败: %v", err)` 后继续下一轮（不中断循环）。
   - `localtree/api.go:61-75`：手动同步接口用 `errChan := make(chan error)` + `select` + 2s timer——超时则不等待结果（err 为 nil 时直接返回"调用成功"提示，实际任务仍在后台跑）。
5. **服务启动错误通道**：`web/web.go:21-37` 两个 `chan error`（容量 1）接收 HTTP/HTTPS `r.Run`/`ListenAndServeTLS` 的返回错误，`select` 任一到达即 `log.Fatal` 整体退出。

---

## 七、反模式记录（如实，供 spec 撰写时决策）

1. **忽略 `resolveItemInfo` 的错误**：`emby/items.go:129` `itemInfo, _ := resolveItemInfo(c, RouteItems)`——仅用 `itemInfo.ApiKey` 拼缓存 key，出错时静默使用零值。同类：`episode.go:62` `bytes, _ := json.Marshal(ih)`（序列化失败则 Content-Length 头缺失/为 0）。
2. **`url.Parse` 错误大面积忽略（约 11 处）**：`redirect.go:49,77,154`、`subtitles.go:25`、`media.go:218,290,387`、`m3u8/info.go:127,184,197`、`download.go:91`——理由是输入为程序内构造的常量模板（如 `MasterM3U8UrlTemplate`），但该假设散落在各处、无注释说明。
3. **鉴权失败不写响应体**：`emby/auth.go:119-124` 源服务器请求失败时只 `logs.Error` + `c.Abort()`，客户端会收到 200 状态码 + 空 body（对比 :134-137 正常 401 路径有 `c.String(401, "鉴权失败")`）。
4. **`RawFetch` 错误分类折叠**：`emby/api.go:50,57` 把网络错误与响应解析错误统一映射为 `Code: http.StatusBadRequest`，下游无法区分错误种类；`Msg` 用字符串拼接 `"请求发送失败: " + err.Error()` 而非 `fmt.Errorf`。
5. **`trys.Try` 的 `tryNum <= 0` 返回 `nil`**（trys.go:9-11）：调用方无法区分「成功」与「未尝试」；且最后一次失败后仍多 sleep 一个 interval。
6. **错误文案质量参差**：`m3u8/info.go:222` `errors.New("参数为设置, 无法更新")`（"为设置"系"未设置"笔误）；`localtree.go:27` `fmt.Errorf("不可重复初始化")`、`synchronizer.go:72` `fmt.Errorf("当前正在执行同步任务")`——用 `fmt.Errorf` 却无格式化参数（应为 `errors.New`）；`util/https/web.go:78,99` `errors.New("参数为空")` 不指明是哪个参数。
7. **`logs.Error` 注释与实现不符**（颜色为 Gray 而非 Red，见 1.1），以及 `m3u8/proxy.go:42` 的 `err.Error()` 传入 `%v` 的风格不一致。
8. **敏感信息进日志**：`logs.Info("解析到的 itemInfo: %v", itemInfo)`（redirect.go:71、playbackinfo.go:49、download.go:26）会整体打印 `ItemInfo` 结构体，其中包含 `ApiKey` 字段（media.go:423-469 可见该结构含 ApiKey/ApiKeyType 等），api_key 明文进入日志。
9. **管理接口把 err 原文返回客户端**：`localtree/api.go:78` `model.Response{Message: "同步失败: " + err.Error()}`——与代理类接口"固定文案 + 看日志"的约定相反（内部路径、底层错误细节直接暴露）。
10. **`haveReturned` 哨兵每次现场创建**（playbackinfo.go:90,201）：语义相同的信号量重复构造，且用 error 承载控制流（对比包级 `ErrWalkEOF/ErrBreakRange` 的做法）。

---

## 八、附录：核心文件清单

| 文件 | 与本主题的关系 |
|---|---|
| `internal/util/logs/logs.go` | 7 个级别函数 + writeLog 广播 |
| `internal/util/logs/logger.go` | Logger 接口 / DefaultLogger / RegisterLogger |
| `internal/util/logs/colors/colors.go` | ANSI 真彩色常量 + Enabler 开关 |
| `internal/config/log.go` | 唯一日志配置 disable-color |
| `internal/service/log.go` | wsLogger（WebSocket 日志推送）+ SyncServerLog |
| `internal/web/log.go` | CustomLogger 访问日志中间件 |
| `internal/web/web.go` | gin.Recovery 挂载 / errChan 启动错误 / log.Fatal |
| `internal/service/emby/redirect.go` | checkErr 统一错误出口 / trys.Try 用例 |
| `internal/service/emby/api.go` | RawFetch/Fetch 的 HttpRes 错误通道 |
| `internal/service/openlist/api.go` | HttpRes 风格 + Fetch 的 6 类 fmt.Errorf 包装 |
| `internal/service/openlist/walk.go` | 哨兵 ErrWalkEOF |
| `internal/util/jsons/item.go` | 哨兵 ErrBreakRange |
| `internal/util/jsons/jsons.go` | 唯一 defer recover / 两处 panic |
| `internal/util/trys/trys.go` | 同步重试工具 |
| `internal/model/http.go`、`internal/model/gin.go` | HttpRes[T] / Response 响应结构 |
| `internal/service/openlist/localtree/*.go` | logf 前缀日志 / errgroup / %w 集中地 |
| `internal/service/m3u8/m3u8.go`、`m3u8/proxy.go` | 后台循环错误日志 / handler 错误响应 |
| `internal/service/emby/auth.go`、`download.go`、`playing.go`、`media.go`、`playbackinfo.go`、`items.go`、`episode.go` | handler 层错误处理样本 |
| `internal/service/lib/ffmpeg/auto_download.go` | 重试下载 + fmt.Print 进度条 |
| `main.go` | log.Fatal 启动失败处理 |

## Caveats / Not Found

- 未发现任何日志落盘、轮转、级别开关的代码或配置；README 中关于日志的运维说明未纳入本次研究范围（仅以代码为准）。
- `cmd/fake_mp3_1`、`cmd/fake_mp4` 为独立工具程序，未纳入统计。
- 测试文件（`*_test.go`）中的错误处理模式未纳入统计（任务聚焦生产代码）。
- `getEmbyFileLocalPath`、`sendOpenStreamPlaybackInfoReqToOrigin` 等个别函数体未逐行展开，但其签名与调用侧错误处理均已核实。
