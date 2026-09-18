# 技术设计: strm 直链代理播放

> 父任务共享契约。各子任务实现时以本文件为准; 若实现中发现契约有误, 回到本文件修订后再继续。

## 1. 术语

| 术语 | 含义 |
|---|---|
| strm 地址 | Emby `MediaSources[].Path` 返回的、strm 文件内的原始文本（一个 http 地址） |
| 代理域名 / 前缀 | 配置中列出的 URL 前缀, 命中则进入代理模式 |
| 上游 | strm 地址指向的远端服务（本例为 `vault.bjyt.de:7811` 上的自建网关） |
| 直链 | 上游最终返回媒体字节流的地址。注意实测中它**就等于 strm 地址本身**（E1）, 跟随 3xx 后才不同 |
| 代理模式 | 本项目请求上游并把字节流转发给客户端, 对客户端不产生 302 |

## 2. 决策流程

`Redirect2OpenlistLink`（`internal/service/emby/redirect.go`）中 strm 分支的改后逻辑:

```
embyPath 是 http 远程地址 (urls.IsHttpRemote)
│
├─ 1. path-map 映射 (保持现有行为)
│
├─ 2. 代理开关是否开启?
│     └─ 否 ─────────────────────────────────► 现有 302 流程 (含 internal-redirect-enable)
│
├─ 3. strm 地址是否命中配置前缀?
│     └─ 否 ─────────────────────────────────► 现有 302 流程
│
└─ 4. 代理模式
      ├─ 4.0 URL 归一化 (见 §3) —— 失败则记 Error 并回退现有 302 流程
      ├─ 4.1 获取并发槽位 (见 §11) —— 满则等待, 客户端断开即释放并放弃
      ├─ 4.2 查直链缓存 (见 §6)
      ├─ 4.3 请求上游 (见 §4)
      │     ├─ 3xx → 跟随至最终地址, 写入缓存, 用最终地址重新发起带 Range 的请求
      │     └─ 2xx → 直接进入 4.4
      ├─ 4.4 响应状态属于"直链失效"集合? → 清缓存, 重试一次 (仅一次)
      ├─ 4.5 回写响应头 + 流式回写响应体 (见 §5)
      └─ 4.6 按日志契约输出 (见 §7)
```

**不需要 handler 调用任何缓存旁路接口**: 字节流路由已不在响应缓存白名单内（见 §9），`c.Writer` 不会被包装。

**关键约束**: 第 4 步一旦开始写响应体, 就不再有任何回退可能。回退（`checkErr` → `ProxyOrigin` / 500）只在 §4.3 尚未拿到可回写响应之前允许。

## 3. URL 归一化契约（对齐 E2）

输入: strm 原文（可能含裸空格、中文全角标点、未编码的 `&`）。

输出: 可直接交给 `https.Request` 的安全 URL。

规则:

1. `url.Parse` 原文。失败 → 返回错误, 由调用方回退 302 流程。
2. 若 `RawQuery` 非空:
   - `url.ParseQuery(RawQuery)`。失败 → 返回错误（不做静默降级, 因为被截断的请求会以更难排查的方式失败）。
   - `q.Encode()` 重新编码，再 `strings.ReplaceAll(encoded, "+", "%20")`。
     `Encode()` 会把字面 `+` 编码为 `%2B`，把空格编码为 `+`；替换后空格变成 `%20`，对任何服务端实现都无歧义。
   - 写回 `u.RawQuery`。
3. 路径部分由 `u.String()` 经 `EscapedPath()` 自动编码，不需要手工处理。
4. 归一化后的地址与原地址不一致时, 日志同时打印两者（便于排查）。

**已知取舍**: `url.ParseQuery` 会丢弃无法解析的片段并且 `Encode()` 会按键排序。对该上游（uvicorn/FastAPI）无影响。若未来遇到不接受重排序的上游, 需要改成"只对需要编码的字符做最小转义"的实现 —— 届时在本文件记录。

**替代方案（未采用）**: 直接对原文做 `url.PathEscape` 或手工替换空格。理由: 无法同时正确处理中文、全角标点与 `&`/`=`/`+` 的语义边界，容易引入新的截断。`ParseQuery` + `Encode` 是唯一能保证"解码-再编码"无损的路径。

## 4. 上游请求契约

- 一律通过 `internal/util/https` 的 `RequestHolder`（C1）。
- 用 `DoSingle()` 而非 `Do()`：`Do()` 会自动跟随重定向, 我们就拿不到中间跳转信息、也无法在跟随前后分别打日志。跟随由本功能自己的循环实现, 上限 `max-redirect-depth`（默认 5）。
- **必须**通过 `.Context(c.Request.Context())` 传递客户端请求上下文，否则客户端断连后上游仍会继续拉取（违反 R6）。
- 客户端上下文不可用时（例如异步场景）退回 `context.Background()`, 但当前设计只在同步请求路径中使用。

### 请求头策略

透传白名单（仅在客户端确实携带时转发）:

| 头 | 说明 |
|---|---|
| `Range` | 拖动进度的关键 |
| `Accept` | 部分网关按 Accept 决定响应类型 |

`User-Agent` 取值优先级: 配置的固定 UA > 客户端 UA > 不设置。

**禁止透传**（显式剔除）:

| 头 | 原因 |
|---|---|
| `Host` | 必须由目标 URL 决定 |
| `Connection` / `Keep-Alive` / `Transfer-Encoding` / `Trailer` / `Upgrade` / `TE` / `Proxy-*` | 逐跳头, 跨代理转发非法 |
| `Accept-Encoding` | 见下方"必须显式设置 identity", 不能只是剔除 |
| `Authorization` / `Cookie` / `Proxy-Authorization` / `X-Emby-*` / `X-MediaBrowser-*` | Emby 凭据, 不得泄漏给第三方上游 |
| `If-*` | 条件请求语义不适用于代理链路 |

> **按前缀剔除请求头时必须大小写不敏感。** `http.Header` 的键会被
> `textproto.CanonicalMIMEHeaderKey` 规范化 —— 连字符后只大写首字母, 所以
> `X-MediaBrowser-Token` 实际存成 **`X-Mediabrowser-Token`**。用字面量 `X-MediaBrowser-`
> 做 `strings.HasPrefix` 判断永远匹配不上, 该条剔除会静默失效, 凭据照常发给上游。
> 这是实测复现过的缺陷, 前缀判断一律先 `strings.ToLower` 再比较。

**已知行为（有意为之, 记录以免后续误判为 bug）**: 剔除发生在配置头叠加**之后**, 因此在 `request-header` 里配置 `Authorization` / `Cookie` 会被**静默丢弃**, 且 `Init()` 不会报错。这是"宁可漏配也不漏凭据"的取舍。若上游将来需要 Basic Auth 之类的鉴权, 应改用自定义头名（如 `X-Gateway-Auth`）, 而不是放开这份清单。使用者从日志看不出被丢弃的原因, 这是该取舍的已知代价。

**必须显式设置 `Accept-Encoding: identity`, 而不是仅仅剔除该头。**

Go 的 `http.Transport` 在"请求里没有 `Accept-Encoding` 值时"会自行加上 `Accept-Encoding: gzip`, 并**透明解压**响应（`net/http/transport.go` 中 `DisableCompression` 的说明: *prevents the Transport from requesting compression ... when the Request contains no existing Accept-Encoding value*）。后果:

1. 若上游对非 Range 的媒体响应做了 gzip, 我们拿到的是解压后的字节流 —— 在一台内存不足 1GB 的机器上白白烧 CPU;
2. 透明解压时 Go 会**从响应头里删掉 `Content-Encoding` 与 `Content-Length`**, 于是我们回写不出总长度, 播放器拿不到文件大小。

显式给请求设置 `Accept-Encoding: identity` 后, Transport 既不会追加 gzip, 也不会透明解压, 上述两点都不成立。**这是本包内即可完成的修正, 不需要改动 `internal/util/https` 的共享 client。**（带 `Range` 的请求本就不会被自动 gzip, 但非 Range 的首次请求会。）

配置可另外指定一组固定请求头（`request-header`），用于上游需要特定 UA/Referer 的场景。固定头与透传头冲突时以固定头为准。

### 响应头回写

- 逐跳头不写回客户端（同上表）。
- 透传: `Content-Type`、`Content-Length`、`Content-Range`、`Accept-Ranges`、`Last-Modified`、`ETag`、`Location`。
  - `Location` 必须包含在内: 跟随重定向的判定只覆盖 301/302/307/308（`https.IsRedirectCode`）。上游若返回
    **300 / 303** 这类同样带 `Location` 但不在跟随集合内的状态码, 不透传 `Location` 就会给客户端一个
    "没有 Location 的重定向", 而此时响应已开始写入（`written=true`）, 调用方已失去回退机会。
    把 `Location` 透传出去可以让客户端自行处理这种少见情况。
- **若上游返回 206 但未携带 `Accept-Ranges`，补写 `Accept-Ranges: bytes`**（实测 E1 中 200 响应没有该头）。
- 状态码原样回写（200 / 206）。
- 不使用 `https.CloneHeader`（它无条件全量复制）, 需要带过滤的克隆。

## 5. 流式回写契约

- 用 `io.CopyBuffer` + `bytess.CommonFixedBuffer()` 复用缓冲（与现有 `ProxyPass` 一致）。
- **不得整体缓冲**（C 系约束 + E3）：不得写入会缓存响应体的 writer。
- 写响应头后立即 `Flush()` 一次，让首字节尽快到达客户端；后续由 `net/http` 自身的缓冲在写满时 flush。
- 传输结束（正常或中断）都必须 `resp.Body.Close()`。
- 客户端中途断开 → `io.CopyBuffer` 返回错误 → 记 `Warn`（含已传输字节数），不算请求失败。
- 上游 body 读取错误 → 记 `Error`（含已传输字节数）。

## 6. 缓存契约

**不使用**响应缓存中间件（`internal/web/cache`）来缓存直链。原因:

1. 它的语义是缓存**响应**（含 body），而我们需要缓存的只是一个 URL；
2. 播放流量必须先旁路该中间件（见子任务 `09-16-cache-stream-passthrough`）。

本功能自建一个轻量内存缓存，位于代理包内部:

- 结构: `sync.Map[string]entry`, key = 归一化后的 strm 地址, value = `{finalUrl string, expireAt time.Time}`。
- TTL: 配置项 `link-cache-expired`, 默认 `10m`（复用 `internal/config/cache.go` 的 `d/h/m/s` 单位解析风格）。
- **只在发生过 3xx 跟随、且最终地址与原地址不同的情况下写入缓存**。上游直接 2xx 时不写 —— 此时"直链"就是原地址本身, 缓存没有意义，写进去只会白白占内存。
- 失效重试: 上游响应状态码命中配置的失效集合（默认 `403, 404, 410`）时, 删除该 key 并**重试一次**（重新请求上游）。仅重试一次, 避免与上游异常形成循环。
- 无单飞（single-flight）要求: 并发 Range 请求各自独立请求上游是可接受的；若实现阶段观察到上游并发压力, 再引入 `golang.org/x/sync/singleflight`（`x/sync` 已在 go.mod 中, 无需新增依赖）。

**对用户"缓存 + 失效自动重解析"选择的说明**: 由于实测 E1 不存在 302, 在直连场景下"缓存直链"退化为"没有可缓存的对象", 缓存只在 3xx 场景生效；而"失效自动重解析"在直连场景下表现为"命中失效状态码后重试一次上游请求"。两条能力都已实现, 只是触发条件不同。

## 7. 日志契约

采用**标准级别函数 + 固定前缀 `[直链代理]`** 的方式（不采用 `localtree` 的 `logs.Raw` 自定义前缀模式）, 理由: 保留 `Info/Success/Warn/Error` 的级别语义（符合 `logging-guidelines.md` 的级别映射）, 同时前缀保证可 grep。

修复 `logging-guidelines.md` 中记录的既有违规: 本功能**不得**打印整个 `itemInfo`（会泄漏 api key）, 只打印需要的字段。

| # | 时机 | 级别 | 消息模板 |
|---|---|---|---|
| L1 | 识别出 strm 远程地址 | Info | `[直链代理] 检测到 strm 远程地址: %s` |
| L2 | 命中配置前缀 | Info | `[直链代理] 命中代理前缀: %s` |
| L2' | 未命中任一前缀 | Info | `[直链代理] 未命中任何代理前缀, 走原有 302 流程` |
| L3 | 归一化结果与原地址不同 | Info | `[直链代理] 归一化地址: %s (原始: %s)` |
| L4 | 缓存命中 / 未命中 | Info | `[直链代理] 直链缓存命中: %s` / `[直链代理] 直链缓存未命中, 开始请求上游` |
| L5 | 上游返回响应 | Info | `[直链代理] 上游响应: status=%d, content-type=%s, content-length=%s, accept-ranges=%q` |
| L6 | 每次跟随跳转 | Info | `[直链代理] 跟随重定向 (第 %d 跳): %s -> %s` |
| L7 | 解析出最终地址并写入缓存 | Success | `[直链代理] 解析到直链: %s, 已缓存 %s` |
| L8 | 开始传输（含客户端 Range） | Info | `[直链代理] 开始传输: %s, 客户端 Range: %q`（无 Range 时打印 `""`） |
| L9 | 传输完成 | Success | `[直链代理] 传输完成: 已发送 %d 字节, 耗时 %s` |
| L9' | 传输被客户端中断 | Warn | `[直链代理] 传输中断: 已发送 %d 字节, err: %v` |
| L10 | 直链失效重试 | Warn | `[直链代理] 上游返回 %d, 判定直链失效, 清除缓存并重试一次` |
| L11 | 代理失败（可回退阶段） | Error | `[直链代理] 代理失败: %v` |
| L12 | 上游请求建连失败 | Error | `[直链代理] 请求上游失败: %s, err: %v` |
| L13 | 并发槽位已满, 开始等待 | Warn | `[直链代理] 并发传输已达上限 %d, 等待槽位, 当前活跃: %d` |
| L14 | 拿到槽位并进入传输 | Info | `[直链代理] 当前活跃传输: %d/%d`（上限为 0 时只打印活跃数） |

L9/L9' 的完成日志同时充当**内存与负载观测点**: 每条传输结束时都会输出实际发送字节数与耗时, 结合 L14 的活跃数即可还原任意时刻的并发规模, 用于在小内存机器上判断是否需要调低 `max-concurrent-streams`。

排查一次播放时, `grep '\[直链代理\]'` 应能还原完整的 L1→L9 序列。

### 日志打印完整地址（含查询串），不做脱敏

**决策: 日志按 L3/L4/L6/L7/L8/L12 的模板原样打印完整地址, 包括 `pickcode` 等查询参数。**
**连带决定: 本包返回给调用方的错误也不做脱敏**（`strm-proxy-integration` 会用 `checkErr` 打印它）。

曾考虑对敏感查询参数脱敏, 理由与放弃理由记录如下, 以免后续重复讨论:

- **曾提出的理由**: strm 地址中的 `pickcode` 是网盘取件码, 性质接近凭据; 日志既进 stdout 也经 WebSocket 推到 web 日志页, 贴日志求助会泄漏。
- **放弃的理由**: 上游网关只对部署本项目的服务器开放（本功能存在的前提本身）。取件码单独拿出去在别的 IP 上无法使用, 因此它不构成独立的凭据泄漏面。同时, 现有代码本来就原样打印含查询串的地址（如 `redirect.go` 的 `logs.Success("重定向 strm: %s", finalPath)`）, 脱敏会与本项目既有行为不一致。
- **诊断价值上的权衡**: 完整地址对排查"是否命中前缀""归一化是否正确""上游收到的是什么"是最直接的一手信息, 不脱敏可以让排查更省事。

**注意**: `logging-guidelines.md` 中"禁止打印 api key / token"针对的是 **Emby 的凭据**（`itemInfo` 里的 `ApiKey`、`X-Emby-Token` 等）。这些**仍然严格禁止**出现在任何日志中, 与本条决策不冲突。

## 8. 错误与回退契约

| 阶段 | 失败情形 | 处理 |
|---|---|---|
| 归一化 | URL 无法解析 | 记 L11, 回退现有 302 流程 |
| 配置 | 开关未开 / 未命中前缀 | 完全走现有流程, 不产生任何代理日志噪音（只打 L2'） |
| 建连 | 请求上游失败 | 记 L12, 回退现有 302 流程 |
| 首响应 | 上游返回 5xx | 记 L11, 回退现有 302 流程 |
| 跟随 | 超过 `max-redirect-depth` | 记 L11 + 实际跳数, 回退现有 302 流程 |
| 传输中 | 上游 body 读取出错 | 记 Error, 中断连接（此时无法回退, 客户端表现为播放中断） |
| 传输中 | 客户端断开 | 记 Warn, 关闭上游 body |

**为何代理失败不再叠一层 `checkErr`**: 代理失败时客户端尚未收到任何响应, 此时有两种可选回退 —— 回退 302（改动前的既定行为）或 `checkErr`（回源 / 500）。选择回退 302, 因为:

1. 它是改动前该分支的**既定行为**, 因此"不引入比改动前更差的结果"这一目标是自动成立的;
2. `checkErr` 的回源路径会把播放请求转给 Emby, 由 Emby 自行解析 strm —— 而 Emby 未必能访问上游, 该路径是否可行无法保证;
3. 两层回退叠加后, 客户端最终收到的是 302 还是 Emby 的响应将取决于运行时状态, 排查时无法从日志一眼判定 —— 这与 R7 的可排查性目标直接冲突。

**关于"回退 302 是否有效"**: 由于上游通常只对本项目服务器开放（PRD Background）, 回退 302 大概率仍会播放失败。保留它仅为"不引入比改动前更差的行为", 而不是期望它可用。这一点需在 `09-16-strm-proxy-integration` 的验收中向开发者确认。

## 9. 模块落点与分层

新增包 `internal/service/streamproxy/`:

| 文件 | 职责 |
|---|---|
| `streamproxy.go` | 对外入口: 判断是否命中、执行代理、回写响应 |
| `link.go` | 上游请求、3xx 跟随、直链缓存与失效重试 |
| `urls.go` | strm 地址归一化、前缀匹配 |
| `type.go` | 纯类型定义（与项目 `type.go` 命名惯例一致） |
| `*_test.go` | 外部测试包 `streamproxy_test`, 表驱动 |

**分层理由**:

- 放独立包而不是塞进 `internal/service/emby/`, 因为该包已有 16 个文件, 且此能力是"把一条绝对 URL 的字节流代理给客户端", 与 Emby 无耦合。
- 该包只依赖 `config` / `util/https` / `util/logs` / `util/bytess` / `util/strs`，**不依赖 gin, 不依赖 `web`**，不新增反向依赖（C6）。
- **响应缓存的旁路不在本任务内**：字节流路由（`Reg_ResourceStream` / `Reg_ItemDownload` / `Reg_ItemSyncDownload`）已由子任务 `09-16-cache-stream-passthrough` 从缓存白名单中移除，因此到达 handler 时 `c.Writer` 就是原始 writer，不需要任何显式旁路调用。`streamproxy` 包对 `web/cache` 完全无感知。

**配置落点**: 复用 `internal/config/emby.go` 的 `Strm` 结构体（已有 `path-map` / `internal-redirect-enable`），新增 `proxy` 子结构。理由: 语义上同属 strm 远程地址处理, 新增独立顶层配置段会割裂。

```yaml
emby:
  strm:
    path-map: [...]
    internal-redirect-enable: false
    proxy:                      # 新增; 缺省整个段时行为与改动前一致
      enable: false             # 总开关
      domains:                  # 命中任一前缀即进入代理模式
        - http://vault.bjyt.de:7811
      link-cache-expired: 10m   # 直链缓存时长, 单位 s/m/h/d
      max-redirect-depth: 5     # 上游重定向最大跳数
      retry-status-codes: [403, 404, 410]   # 判定直链失效的状态码
      max-concurrent-streams: 16  # 并发代理传输上限, 0 表示不限制
      request-header:           # 覆盖/补充到上游请求的固定请求头
        User-Agent: ""          # 为空则不设置
```

### 前缀匹配规则

- 匹配前对配置项与候选地址统一做: 去首尾空白 + scheme 大小写归一（`HTTP://` → `http://`）。
- 配置项尾部多余的 `/` 去掉后比较。
- 按配置顺序线性匹配, **第一个命中即生效**（与现有 `path-map` 的"自上而下第一个匹配"一致）。
- 命中后要求**边界成立**: 地址在该前缀之后的第一个字符必须是 `/`、`?`、`#` 或字符串结束。否则 `http://vault.bjyt.de:7811` 会错误命中 `http://vault.bjyt.de:78111.evil.com`。
- 不做 host 解析匹配。用户通过把前缀写成 `http://host:port`（含端口）来锁定目标; 需要锁路径时写成 `http://host:port/prefix`。

## 10. 已排除的方案

| 方案 | 排除理由 |
|---|---|
| 继续用 302, 只把 `internal-redirect-enable` 修好 | 不满足核心需求: 客户端仍会直连上游, 而上游只对本项目服务器开放 |
| 用 `httputil.ReverseProxy` 标准库代理 | 与项目"所有出站 HTTP 走 `internal/util/https`"的约束冲突（C1）; 且它自带连接池与重试策略, 与项目全局 client 的代理/超时设置重复 |
| 复用 `https.ProxyPass` | 它是 `remote + r.RequestURI` 的拼接模型, 无法表达"一条需要归一化的绝对 URL"; 且它无条件 `CloneHeader` 全量复制请求头/响应头, 不符合 §4 的过滤要求 |
| 让响应缓存中间件原样缓存代理响应 | 需要把 625MB 响应体放进内存, 直接违反 R4（见 E3） |
| 先删 OpenList 再做代理 | 会让代理功能的 diff 与大规模删除的 diff 混在一起, 出问题难以定位; 且删除本身可能引入编译期连带影响, 干扰新功能的验证 |
| 用 `strm.internal-redirect-enable` 复用为代理开关 | 两者语义不同（前者只影响 302 的目标地址, 后者改变是否 302）, 复用会让配置含义变得难以理解 |

## 11. 并发与内存

### 内存实际占用

| 项 | 量级 |
|---|---|
| 每路代理流的缓冲 | 32KB（`bytess.CommonBufferSize`, 从 `sync.Pool` 复用） |
| 每路代理流的 goroutine 栈 | 数 KB |
| 每路代理流的 TCP 连接 | 2 条（客户端侧 + 上游侧） |

**合计约 128KB/路**。20 路并发约 2.5MB, 相对部署机器不足 1GB 的内存而言可忽略。

**结论: 内存不是并发限制的动因。** 引入 `max-concurrent-streams` 的真实目的是另外两条:

1. 上游网关可能存在并发连接数限制（父 PRD Q3）, 打满会表现为上游拒绝连接;
2. 小带宽机器上, 并发流会互相争抢带宽。

### 并发控制语义

- 配置 `max-concurrent-streams`, 默认 **16**, 配置 `0` 表示不限制。
- 实现方式: 带缓冲的 channel 作为信号量, 容量即上限。
- **满载时的行为是等待, 不是拒绝**: 用 `select` 同时监听槽位 channel 与客户端请求的 Context。客户端断开时立即放弃等待并释放, 避免连接堆积。
- 槽位在传输结束时释放（`defer`），包括出错与客户端中断路径。
- 默认值 16 的依据: Emby 客户端单次播放通常开 1-4 条连接, 16 提供了足够裕量, 正常使用不会触发等待。

### 观测

- L14 在每次进入传输时输出当前活跃数;
- L9 / L9' 在每次传输结束时输出字节数与耗时。

两者结合即可从日志还原并发曲线, 用于判断默认值是否需要调整, 数据来源不依赖任何外部监控。

