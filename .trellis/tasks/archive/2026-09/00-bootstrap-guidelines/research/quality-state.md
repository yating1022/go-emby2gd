# Research: 代码质量惯例、测试规范、并发模式与状态管理

- **Query**: 梳理 go-emby2openlist 项目的代码质量惯例、测试规范、并发模式与状态/存储管理，为 `quality-guidelines.md` 与 `database-guidelines.md`（改写为"配置与状态管理规范"）提供素材
- **Scope**: internal（全部 Go 源码 + CI + 构建脚本）
- **Date**: 2026-09-16

---

## 关键结论速览

1. **CI 完全不跑测试**：`.github/workflows/build.yml` 只执行 `./build.sh`（交叉编译 13 个平台），无 lint、无 vet、无 test；项目无 `.golangci.yml`、无 Makefile。
2. **测试是"开发者手动验证脚本"而非自动化资产**：13 个 `*_test.go` 中约半数没有任何断言，只 `log.Println` 打印结果靠肉眼确认；多个测试依赖仓库根目录的 `config.yml`（已被 .gitignore）和本地活服务（openlist `0.0.0.0:12345`、ffmpeg、`/Users/ambitious/...` 本地路径）。
3. **无任何 mock/断言框架**：不用 testify/gomock，测试中甚至不用 `httptest`；集成测试直接打真实远端。
4. **util 包设计规律**：`internal/util` 下按"复数名词"切分微型包（strs/slices/maps/bytess/trys…），每包 1~5 个导出函数、中文 godoc、失败时返回原值而非 error（fail-soft）；泛型极少（全库仅 4 处）。
5. **并发三板斧**：`parallels.SliceChunk`（按 CPU 数分块）+ `sync.WaitGroup` + 结果 channel；后台状态用"单一维护 goroutine + 预缓冲 channel（FIFO 淘汰）"模式（web/cache 与 m3u8 两处同构实现）；errgroup 仅 2 处。
6. **无数据库、无持久化存储**：状态全部在内存（`sync.Map` / 单 goroutine 私有 map）；唯一落盘的是"本地目录树"生成的媒体占位文件与自动下载的 ffmpeg 二进制；配置只在启动时读一次，**无热更新**。
7. **配置 = 全局单例 `config.C` + 反射驱动初始化**：yaml 解析后用反射给 nil 指针字段补零值，逐字段调用 `Initializer.Init()` 做校验和默认值填充，校验失败返回中文错误。
8. **注释 91% 为中文**（1098 行注释中 1004 行含中文），导出标识符全有 godoc 风格中文注释；错误消息也是中文，格式统一为 `"动作失败: %v"`。
9. **代码用 `any` 不用 `interface{}`**（0 处 interface{}），Go 1.26 新特性在用（`for range N`、`min()`、`sync.OnceFunc`）。
10. **路由不走 gin 原生路由**：单一 catch-all handler + `constant` 包正则规则表顺序匹配（`internal/web/route.go`、`internal/web/handler.go`）。

---

## 1. 测试规范

### 1.1 测试文件清单与分类

| 测试文件 | 测试对象 | 类型 | 断言情况 |
|---|---|---|---|
| `internal/util/urls/urls_test.go` | `urls.IsRemote`/`Unescape` | 纯单元 | 表驱动 + `t.Errorf`（L22-66） |
| `internal/util/parallels/paralllels_test.go` | `parallels.SliceChunk` | 纯单元 | 表驱动 + `reflect.DeepEqual`（L10-28） |
| `internal/service/openlist/localtree/snapshot_test.go` | `Snapshot.Check` | 纯单元 | 表驱动 + `t.Errorf`（L9-38） |
| `internal/util/mp4s/mp4s_test.go` | `mp4s.GenWithDuration` | 单元 | **无断言**，写 `test.mp4` 到当前目录（L11-15） |
| `internal/util/jsons/jsons_test.go` | `jsons` 序列化 | 单元 | **无断言**，全部 `log.Println`（L12-71） |
| `internal/util/https/https_test.go` | （实际未测 https 包） | 无效测试 | 用 `log.Fatalf`，只测了 stdlib 的 `http.NewRequest`（L11-21） |
| `internal/service/emby/media_test.go` | 正则（**复制进测试**，非引用生产代码） | 无效测试 | `log.Println`（L10-18） |
| `internal/service/path/path_test.go` | `path.SplitFromSecondSlash` | 单元 | **无断言**（L10-13） |
| `internal/service/openlist/api_test.go` | `openlist.Fetch` | 集成 | 依赖 `../../../config.yml` + 活 openlist（L12-31） |
| `internal/service/openlist/walk_test.go` | `openlist.WalkFsList` | 集成 | 同上（L11-28） |
| `internal/service/m3u8/m3u8_test.go` | playlist 缓存 | 集成 | 依赖 `config.yml`（L11-37） |
| `internal/service/m3u8/info_test.go` | `NewByContent` 等 | 混合 | 内嵌 427 行 `TestContent` 常量；`TestNewByRemote` 打真实 OSS URL（L396-403） |
| `internal/service/lib/ffmpeg/ffmpeg_test.go` | ffmpeg 探测 | 集成 | 依赖本地路径 `/Users/ambitious/Downloads/test.mp4`（L21）、活服务 `http://0.0.0.0:12345`，写 `cover.jpg`（L15-70） |
| `internal/service/music/write_test.go` | `music.WriteFakeMP3` | 集成 | 下载 ffmpeg + 活服务 + 写文件到 `../../../openlist-local-tree`（L13-36） |
| `internal/service/openlist/localtree/synchronizer_test.go` | `InitSnapshot` | 半集成 | 表驱动外壳，实际扫描本地磁盘 `../../../../openlist-local-tree`（L18） |

### 1.2 归纳出的实际惯例

**命名与组织**
- 测试包一律用外部测试包 `package xxx_test`（黑盒），全部 13 个文件无一例外。
- 函数命名 `Test + 被测函数名`（`TestIsRemote`、`TestSliceChunk`）；localtree 包用 `Test类型_方法`（`TestSnapshot_Check`、`TestSynchronizer_InitSnapshot`）。
- 表驱动测试是标准写法，结构固定：

```go
// internal/util/urls/urls_test.go:22-44
type args struct { path string }
tests := []struct {
    name string
    args args
    want bool
}{ {name: "rtp", args: args{path: "rtp://1.2.3.4:9999"}, want: true}, ... }
for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
        if got := urls.IsRemote(tt.args.path); got != tt.want {
            t.Errorf("IsRemote() = %v, want %v", got, tt.want)
        }
    })
}
```

**断言方式**
- 只用标准库 `t.Errorf` / `t.Fatal` / `t.Error`，无第三方断言库。
- 错误消息格式：`"FuncName() = %v, want %v"`。
- 大量测试**没有断言**，靠 `log.Println` 肉眼验证（jsons、path、media、mp4s）。

**Mock 方式**
- **不存在任何 mock**。无 gomock/testify/httptest。HTTP 层的"可测性"靠 `https.RequestHolder` 的链式构造器隐式提供，但没人用它写测试。
- 集成测试直接依赖：仓库根 `config.yml`（gitignored）、`http://0.0.0.0:12345` 的 openlist、真实公网 OSS URL、macOS 本地路径。

**测试覆盖的层与空白**
- 有测试：util 层（urls/parallels/jsons/mp4s/https 名义上）、localtree 的 Snapshot、少数 service 函数（集成形态）。
- **完全没有测试的层**：`internal/web`（cache/handler/route/referer/webproxy）、`internal/service/emby` 的全部 handler（media_test.go 不算）、`internal/config`（校验逻辑无测试）、`internal/model`、`main.go`。
- CI 不跑测试，所以这些测试的存活状态无人保障。

**反模式（如实记录）**
- 测试内使用 `log.Fatal` / `log.Fatalf`（`https_test.go:14`、`m3u8/info_test.go:391`）会杀掉整个测试进程，应使用 `t.Fatal`。
- 测试向仓库写文件（`test.mp4`、`cover.jpg`）。
- `media_test.go:11` 把生产代码里的正则复制到测试里，正则改了测试也不会发现。
- `ffmpeg_test.go:21` 硬编码开发者个人机器路径。

---

## 2. util 包设计模式

### 2.1 包清单（internal/util/）

| 包 | 文件数 | 导出内容 | 职责 |
|---|---|---|---|
| `bytess` | 1 | `Buffer`、`CommonFixedBuffer`、`CommonBufferSize` | `sync.Pool` 缓冲区复用（32KB） |
| `encrypts` | 1 | `Md5Hash` | md5 哈希 |
| `files` | 1 | `ReleasePath` | 删除文件/目录 |
| `https` | 3 | `Request/Get/Post/...`、`RequestHolder`、`IsRedirectCode` 等 | HTTP 客户端封装（单例 client） |
| `jsons` | 5 | `Item`、`New`、`FromObject` 等 | 自研 JSON 树（可变更、延迟序列化） |
| `logs`(+`colors`) | 3 | `Info/Success/Warn/Error/Tip/Progress/Raw` | 彩色日志 |
| `maps` | 1 | `Keys[K,V]` | 泛型取 key |
| `mp4s` | 1 | `GenWithDuration` | 生成指定时长 mp4 box |
| `parallels` | 1 | `SliceChunk`、`Range` | 按 CPU 分块 |
| `randoms` | 1 | `RandomHex` | 随机 hex 串 |
| `slices` | 1 | `Copy[T]` | 泛型拷贝 |
| `strs` | 1 | `AllNotEmpty/AnyEmpty/Sort` | 字符串判断 |
| `structs` | 1 | `String/IsStruct` | 反射打印结构体 |
| `trys` | 1 | `Try` | 重试器 |
| `urls` | 1 | `IsRemote/Unescape/AppendArgs` 等 | URL 处理 |

### 2.2 设计规律

- **包粒度**：一个包 = 一个主题的少量纯函数，多数单文件。`https` 和 `jsons` 是仅有的"重"包（3/5 个文件）。
- **命名**：包名一律**复数名词**，与 stdlib 冲突时变形避让——`bytess`（避开 `bytes`）、`trys`（非标准拼写）；文件 `paralllels.go` 有三个 l 的笔误（`internal/util/parallels/paralllels.go`）。
- **导出函数风格**：动词开头 PascalCase（`ReleasePath`、`AnyEmpty`、`Try`）；可变参数接收字符串对（`urls.AppendArgs(rawUrl, kvs ...string)`、`urls.ReplaceAll(rawUrl, oldNews ...string)`）。
- **fail-soft 哲学**：util 函数失败时**返回原值/零值而不返回 error**——`urls.IsRemote` 解析失败返回 false（`urls.go:13-19`）、`urls.Unescape` 失败返回原串（`urls.go:94-101`）、`urls.AppendArgs` 异常返回 `rawUrl`（`urls.go:74-91`）。只有 IO 类（`files.ReleasePath`）返回 error。
- **泛型极少**：全库仅 4 处泛型声明——`maps.Keys`、`slices.Copy`、`model.HttpRes[T any]`（`internal/model/http.go:4`）、`openlist.Walker[T any]`（`internal/service/openlist/walk.go:16`）。其余 util 都用具体类型或 `any`。
- **依赖方向**：小 util 之间几乎无依赖（`urls→strs`、`jsons→maps/parallels/strs`），无环。
- **注释**：每个导出标识符都有中文 godoc，多行注释解释"为什么"（如 `bytess.go:14`、`trys.go:5-7`）。

---

## 3. 并发模式

### 3.1 parallels：CPU 分块原语

`internal/util/parallels/paralllels.go:9-28`——`SliceChunk(size)` 返回 `[]Range`（左闭右开），块数 = `min(runtime.NumCPU(), size)`，块大小向上取整。这是项目自定义的并行骨架，不依赖 errgroup。

### 3.2 WaitGroup + 结果 channel（jsons 并行解析）

`internal/util/jsons/jsons.go:171-216`（makeObject，makeArray 同构 L218-262）：
- `parallels.SliceChunk` 分块 → 每块一个 goroutine（`wg.Add(1)` + `go func(r parallels.Range)`）；
- 结果写入带缓冲 channel（`make(chan result, runtime.NumCPU()*2)`）；
- **单独 goroutine 执行 `wg.Wait(); close(results)`**，主协程 `for r := range results` 收集。

### 3.3 单一维护 goroutine + 预缓冲 channel（核心状态模式）

两个同构实现，是本项目管理可变共享状态的招牌模式：

**web/cache**（`internal/web/cache/holder.go:37-102`）：
- `cacheMap sync.Map` 存缓存；`preCacheChan = make(chan *respCache, MaxCacheNum)`（L44）；
- `init()` 启动 `loopMaintainCache()`（L49-51），**只有这个 goroutine 写 cacheMap**；
- 写入方 `putCache`（L113-154）只往 channel 塞；channel 满时从头部淘汰（FIFO，L145-153），用 `sync.OnceFunc(cacheHandleWaitGroup.Done)` 保证 WaitGroup 配平；
- 每 10 秒 `time.NewTicker` 清洗过期/超量缓存（L91-99）。

**m3u8**（`internal/service/m3u8/m3u8.go:22-66, 71-288`）：
- `init()` 启动 `loopMaintainPlaylist()`，goroutine 内私有 `infoMap`/`infoArr`（无锁）；
- 对外 API 是**函数变量** `GetPlaylist/GetTsLink/GetSubtitleLink`（L27-33），在维护 goroutine 内部赋值（L145-176），实现"数据不动、函数闭包动"的无锁暴露；
- `preMaintainInfoChan`（cap 1000）+ `preChanHandlingGroup` 与 cache 完全同构。

### 3.4 errgroup（仅 2 处）

- `internal/service/openlist/localtree/synchronizer.go:84`：`errgroup.WithContext(context.Background())` 驱动 BFS 同步；`threadsSem chan struct{}`（容量=配置线程数，L85）做信号量；任务循环里 `select { case s.threadsSem <- struct{}{}: case <-s.ctx.Done(): }`（L327-331）；`atomic.AddInt32(&s.activeTaskCount, -1) == 0` 时关闭任务 channel 终止 BFS（L337-341）。
- `internal/service/emby/custom_cssjs.go:110,118`：两个裸 `new(errgroup.Group)` 并发读文件。

### 3.5 锁与原子操作

| 场景 | 位置 | 方式 |
|---|---|---|
| 读多写少共享 map | `cache/holder.go:37`、`cache/space.go:23`、`emby/auth.go:30`（validApiKeys）、`logs/logger.go:24` | `sync.Map` |
| 缓存对象读写 | `cache/type.go:43` | `sync.RWMutex`（读方法全部 `RLock/defer RUnlock`） |
| 串行化外部进程 | `lib/ffmpeg/ffmpeg.go:17` | 包级 `sync.Mutex`（ffmpeg 逐个执行） |
| 防重入 | `localtree/synchronizer.go:57` | 结构体**内嵌** `sync.Mutex` + `TryLock()`（L71-74） |
| 条件变量优先级 | `openlist/walk.go:26-45` | `sync.NewCond`：客户端主 API 请求时 `walkWaiter.Wait()` 暂停后台遍历，`removeMainApiRunner` 中 `Broadcast`（`api.go:216-225`） |
| 懒初始化 | `emby/emby.go:24-50` | `sync.Once` 包裹 ReverseProxy 构造 |
| 进度计数 | `synchronizer.go:209,317` | `atomic.AddInt32/AddInt64` |

### 3.6 goroutine 启动惯例

- **fire-and-forget 很常见**：`go putCache(...)`（`cache/cache.go:123`）、`go sendPlayingProgress(...)`（`playing.go:57`）、`go sendOpenStreamPlaybackInfoReqToOrigin(...)`（`redirect.go:103`、`playbackinfo.go:142`）、`go runtime.GC()`（`items.go:166`）。
- **带缓冲 resChan 收集异步结果**：`playbackinfo.go:151-175`——`resChan := make(chan []*jsons.Item, 1)` 先收集后统一 `<-resChan`。
- **循环变量捕获**：老式写法 `idx, transcode := idx, transcode`（`media.go:194`）在 Go 1.22+ 已不必要但保留。
- **context 传递**：`https.RequestHolder.Context(ctx)`（`request.go:100-103`）+ `http.NewRequestWithContext`（L147）；`https.ProxyRequest` 透传 `r.Context()`（`web.go:92`）；errgroup 场景用 `ctx.Done()` 检查（`synchronizer.go:290,329,366`）。**大量后台 goroutine 不带 context**（fire-and-forget 类）。
- `main.go:26` 无条件启动 pprof：`go func() { http.ListenAndServe(":60360", nil) }()`，`_ "net/http/pprof"` 匿名导入。
- `internal/service/emby/items.go:166` 在响应后手动 `go runtime.GC()`（大 JSON 处理后的显式回收）。

---

## 4. 配置与状态管理（替代数据库章节）

### 4.1 internal/config：yaml 配置

**定义**（`internal/config/config.go:13-30`）：根 `Config` 结构体由 8 个**指针子结构**组成（Emby/Openlist/VideoPreview/Path/Cache/Ssl/Log/Ge2o），字段带 `yaml:"kebab-case"` 标签，每个字段上方一行中文注释。

**加载与初始化**（`config.go:44-78` `ReadFromFile`）：
1. `os.ReadFile` + `yaml.Unmarshal` 到全局 `C`；
2. **反射遍历**：nil 指针字段 `reflect.New` 补零值（L63-67）——保证子配置永远非 nil，业务代码不需要判空；
3. 每个实现 `Initializer` 接口（`Init() error`，L38-41）的字段依次调用 `Init()` 做校验+默认值。

**校验/默认值惯例**（以 `emby.go:66-112` 为例）：
- 必填项为空 → 返回中文 error（`errors.New("emby.host 配置不能为空")`）；
- 零值 → 就地填默认（`ImagesQuality = 70`、`Threads = 8`、缓存过期 `24h`）；
- 枚举值 → 包级 `validXxx map[Xxx]struct{}` 白名单校验（`emby.go:31-38`）；
- **派生私有字段**：Init 时把原始串转成便于查询的结构（`Strm.pathMap [][2]string`、`LocalTreeGen.virtualContainers map[string]struct{}`，`emby.go:141-157`、`openlist.go:115-140`）；
- 复杂字符串格式校验（`path-map` 的 `=>` 分割，`emby.go:148-156`）。

**全局单例**：`config.C`（`config.go:33`）与 `config.BasePath`（L36）是包级全局变量，全项目直接 `config.C.Emby.Host` 访问，**启动后只读，无锁**。

**热更新：不存在**。全库无 fsnotify/文件 watch；配置只在 `main.go:30` 启动时读取一次。`config.C == nil` 有防御（`config.go:83-85`）。

### 4.2 internal/web/cache：内存响应缓存

- **纯内存**，无磁盘层。核心结构：`cacheMap sync.Map`（key=md5）+ 二级 `spaceMap sync.Map`（`space.go:23`，`map[space]*sync.Map` 实现命名缓存空间）。
- **失效策略**（`holder.go:14-26, 54-102`）：容量上限 `MaxCacheSize = 100MB`、条数上限 `MaxCacheNum = 8092`；每 10s ticker 清洗过期（UnixMilli 时间戳比较）；预缓冲 channel FIFO 先进先淘汰（不看过期时间）。
- **过期时间可覆盖**：handler 通过响应头 `Expired`（`cache.Duration(...)` 生成毫秒时间戳，`cache.go:127-131`）覆盖默认值，如 playbackinfo 缓存 12h（`playbackinfo.go:163`）。
- **缓存 key**：method+uri+排序后的(query+body+header) 做 md5（`cache.go:142-187`），忽略清单 `CacheKeyIgnoreParams`（L25-45）。
- **公开 API**：`RespCache` 接口（`public.go:11-46`）+ `RWMutex` 保护的 `respCache` 实现（`type.go`），支持 `Update` 原地改缓存。
- **写路径异步**：`RequestCacher` 中间件用自定义 `respCacheWriter` 包装 gin Writer 边写边拷贝，`go putCache(...)` 异步入缓存（`cache.go:100-124`）。

### 4.3 其他内存状态

- **m3u8 playlist 缓存**：单 goroutine 私有 map + 函数变量 API（见 3.3）；`MaxPlaylistNum = 10` 按 LastRead 淘汰（`m3u8.go:259-271`）。
- **emby 鉴权缓存**：`validApiKeys sync.Map`，注释明确说明不设上限的理由（`auth.go:26-30`）。
- **custom css/js 缓存**：包级切片 + `customCacheOpMutex` + 5 秒最小刷新间隔（`custom_cssjs.go:22-40`）。

### 4.4 持久化：没有数据库，只有"生成物"

- 无 sql/bbolt/嵌入式 KV（grep 无结果）。
- 落盘的都是**派生产物**：本地目录树 `openlist-local-tree/`（strm/mp4 占位文件，`localtree/task.go:109-160` 的 `os.WriteFile`）、自动下载的 ffmpeg 可执行文件（`lib/ffmpeg/auto_download.go:116`）、音乐假 MP3（`music/write.go:38,99`）。这些不是"状态存储"，重启后可由远程 openlist 全量重建。
- `.gitignore` 排除 `config.yml`、`openlist-local-tree`、`custom-js/*.js` 等运行时产物。

### 4.5 全局单例/变量清单

| 全局变量 | 位置 | 说明 |
|---|---|---|
| `config.C` / `config.BasePath` | `config/config.go:33,36` | 全局配置 |
| `https.client` | `util/https/https.go:25` | init() 构造的单例 HTTP 客户端（InsecureSkipVerify、禁自动重定向、支持 http/https 代理） |
| `webport.HTTP/HTTPS` | `web/webport/webport.go` | 端口字符串 |
| `webproxy.HttpUrl/HttpsUrl` | `web/webproxy/webproxy.go:10` | 环境变量代理 |
| `localtree.synchronizer` | `localtree/localtree.go:17` | 唯一同步器，`Init()` 防重复初始化（L26-28） |
| `m3u8.GetPlaylist` 等函数变量 | `m3u8/m3u8.go:27-33` | 维护 goroutine 赋值 |
| `route.rules` | `web/route.go:17` | 正则路由表 |

`init()` 共 4 处：webproxy、https、cache、m3u8——后三者用于**启动后台维护 goroutine**。

---

## 5. 代码风格

### 5.1 注释

- **中文注释占绝对主导**：非测试代码注释行 1098，其中 1004 行含中文（91.4%）。英文只出现在 pprof、正则、常量值等非注释处。
- **godoc 风格**：每个导出函数/类型/字段都有 `// Name 中文说明`，复杂逻辑用多行注释解释"为什么"（如 `cache/holder.go:39-43` 解释 FIFO 淘汰规则）。
- **结构体字段行内注释**：`Cache struct`（`config/cache.go:18-21`）字段与注释同行。
- **步骤注释**：长函数用 `// 1 xxx` `// 2 xxx` 编号（`request.go:135-161`、`api.go:161-197`、`cache.go:71-123`）。
- 个别 Java 风格 `@param` 注释残留（`structs/structs.go:11-13`）。

### 5.2 结构体标签

- 配置：`yaml:"kebab-case"`（`emby.host`、`episodes-unplay-prior`）。
- DTO：`json:"snake_case"`（`openlist/type.go:26-53`），个别 `json:",omitempty"`（`emby/type.go:58`）。
- 标签统一贴在类型后，不加空格。

### 5.3 魔法数字

- 关键阈值**具名常量 + 注释**：`MaxCacheSize/MaxCacheNum`（`holder.go:16-23`）、`MaxPlaylistNum/PreChanSize`（`m3u8.go:14-19`）、`MaxRedirectDepth = 10`（`https.go:22`）、`CommonBufferSize = 32*1024`（`bytess.go:5`）、`customUpdateInterval = 5_000`（`custom_cssjs.go:25`，用下划线分隔数字字面量）。
- 仍有内联魔法数字：`playbackinfo.go:163` 的 `time.Hour*12`、`holder.go:22` 的 `8092`（与 MaxCacheNum 重复字面量）、`1<<3`/`1<<6` 容量提示（`holder.go:60`、`synchronizer.go:383`）。

### 5.4 错误处理

- 错误消息**中文**，格式高度统一：`fmt.Errorf("动作失败/异常 [上下文]: %v", err)`。
- 包装不一致：`%w` 29 处 vs `%v` 76 处（`%v` 占多数，`errors.Is/As` 基本不可用）。
- 哨兵错误用包级 `var ErrXxx = errors.New(...)`（`walk.go:13` ErrWalkEOF、`jsons/item.go:19` ErrBreakRange），后者作为"回调中断"信号。
- `panic` 仅 2 处（`jsons.go:57` 不支持的 map 类型、`emby.go:31` 懒初始化解析失败），`jsons.New` 用 `defer recover` 兜底转 error（`jsons.go:122-127`）。
- `main.go` 用标准 `log.Fatal` 终止；业务代码用自研 `logs` 包。

### 5.5 其他

- **`any` 而非 `interface{}`**：全库 0 处 interface{}，41 处 any。
- Go 1.26 语法在用：`for range chunkNum`（int range，`paralllels.go:22`）、`min()` 内置（`paralllels.go:15,24`）、`sync.OnceFunc`（`holder.go:144`）。
- 日志 API：`logs.Info/Success/Warn/Error/Tip/Progress/Raw`（`logs/logs.go`），带颜色和时间戳；`Error` 用灰色（L53）。
- 路由常量集中在 `internal/constant/constant.go`（正则 `Reg_*` + 路径 `Route_*`）。
- `cmd/fake_mp4`、`cmd/fake_mp3_1` 是独立 main 的开发工具（生成测试媒体），不属于生产代码。

---

## 6. 质量门槛（CI / 构建）

### 6.1 `.github/workflows/build.yml`
- 触发条件：**仅 release published**（不是 push/PR）。
- 步骤：checkout → Node 24（缓存 npm）→ Go 1.26.3 → `./build.sh` → 上传 `dist/**` 到 release。
- **没有 test / vet / lint / coverage 任何一步**。

### 6.2 `.github/workflows/docker.yml`
- 触发：推送 `v*.*.*` 标签；buildx 多架构（amd64/arm64）推 Docker Hub。同样无测试。

### 6.3 `build.sh` / `build_web.sh`
- `build.sh`：先 `build_web.sh`（`npm ci && npm run build`，产物移到 `web/dist`）→ 删旧 dist → 循环交叉编译 13 个平台，统一参数 `CGO_ENABLED=0 go build -tags=goexperiment.jsonv2 -ldflags="-X main.ginMode=release"`（`build.sh:35`）。
- **注意**：`web/embed.go:5` 的 `//go:embed all:dist` 导致**未构建前端时整个项目 `go build ./...` 失败**（本地实测报 `pattern all:dist: no matching files found`）。Go 侧开发必须先跑前端构建或手动创建 `web/dist`。

### 6.4 本地验证现状
- 无 `.golangci.yml`、无 Makefile、无 pre-commit。
- `go vet ./internal/util/...` 本地实测通过（无输出）。
- Go 版本 1.26.3（`go.mod:3`），依赖极简：gin、uuid、gorilla/websocket、`golang.org/x/sync`、`gopkg.in/yaml.v3`、`bogem/id3v2`。

---

## 7. 明显的禁忌模式（从代码归纳）

**项目明显避免/不用的写法：**
1. 不用第三方测试/断言/mock 库（testify、gomock 均未出现在 go.mod）。
2. 不用 `interface{}`，一律 `any`。
3. 不引入数据库/ORM/嵌入式存储。
4. 不用结构化日志库（zap/logrus/slog），坚持自研 `logs` 包。
5. 不用 gin 原生路由树，坚持"catch-all + 正则规则表"。
6. 不用依赖注入框架，坚持包级单例 + `init()`。
7. 配置启动后不修改（无热更新机制），业务代码直接读 `config.C` 不加锁。
8. util 工具函数不返回 error（fail-soft 返回原值），错误只在 service 层出现。
9. 注释不用英文（中文是事实标准）。
10. 不写裸 `interface` 抽象——全库接口极少（`logs.Logger`、`cache.RespCache`、`config.Initializer`、`localtree` 的 writer 隐式约定）。

**存在的反模式（如实记录，供规范"避免清单"参考）：**
- 测试无断言、依赖活服务与 gitignored 配置、`log.Fatal` 滥用、测试写脏仓库文件。
- 错误包装 `%v` 多于 `%w`，无法 `errors.Is`。
- 文件名拼写错误长期存在（`paralllels.go`）。
- `media_test.go` 复制生产正则而非引用。
- 少量内联魔法数字（12h、8092 重复字面量）。
- fire-and-forget goroutine 无 panic 防护、无 context 取消。

---

## 8. 与待写规范文件的映射建议

- `quality-guidelines.md` 素材：第 1 节（测试惯例）、第 5 节（代码风格）、第 6 节（质量门槛现状）、第 7 节（禁忌模式）。
- `database-guidelines.md`（改写为"配置与状态管理规范"）素材：第 4 节全部 + 第 3.3 节（单 goroutine 状态模式）。
- 相关既有 spec：`.trellis/spec/backend/` 下 6 个文件均为待填模板（index.md / directory-structure.md / database-guidelines.md / error-handling.md / logging-guidelines.md / quality-guidelines.md）。

## Caveats / Not Found

- 未运行完整 `go test ./...`（多数测试需要活服务与本地 config.yml，必然失败）；仅验证了 `go vet ./internal/util/...` 通过、`go build` 因缺 `web/dist` 失败。
- 未统计精确测试覆盖率（无 coverage 工具运行）；"哪些层没测试"基于测试文件清单与 import 关系判断。
- `internal/service/emby` 其余文件（episode.go、download.go、subtitles.go 等）与 `internal/web/referer.go`、`internal/service/log.go` 只做了模式级浏览，未逐行精读；其风格与已读文件一致（中文注释、logs 包、https 链式调用）。
