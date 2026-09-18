# 技术设计: streamproxy 包

> 父任务 `design.md` 定义契约; 本文件把它落到具体的包结构、类型与函数上。

## 1. 包结构

```
internal/service/streamproxy/
├── streamproxy.go        # Proxy 入口: 回写响应头 + 流式回写响应体
├── link.go               # 上游请求、3xx 跟随、直链缓存、失效重试
├── urls.go               # NormalizeURL, MatchDomain
├── config.go             # 从 config.C 读取并缓存代理配置的访问器
├── type.go               # 内部类型 (linkEntry 等)
├── log.go                # [直链代理] 前缀日志封装
├── urls_test.go          # package streamproxy_test
├── link_internal_test.go # package streamproxy — 缓存内部状态
└── streamproxy_test.go   # package streamproxy_test — Proxy 行为 (假上游)
```

`type.go` / `log.go` 的拆分沿用项目惯例（`directory-structure.md`: 包内 `type.go` 放纯类型, `log.go` 放日志适配）。

## 2. 配置

`internal/config/emby.go` 中 `Strm` 增加字段:

```go
// Strm strm 配置
type Strm struct {
	PathMap                []string   `yaml:"path-map"`
	InternalRedirectEnable bool       `yaml:"internal-redirect-enable"`
	Proxy                  *StrmProxy `yaml:"proxy"` // 新增

	pathMap [][2]string
}

// StrmProxy strm 直链代理播放配置
type StrmProxy struct {
	Enable               bool              `yaml:"enable"`
	Domains              []string          `yaml:"domains"`
	LinkCacheExpired     string            `yaml:"link-cache-expired"`
	MaxRedirectDepth     int               `yaml:"max-redirect-depth"`
	RetryStatusCodes     []int             `yaml:"retry-status-codes"`
	MaxConcurrentStreams int               `yaml:"max-concurrent-streams"`
	RequestHeader        map[string]string `yaml:"request-header"`

	// 初始化后派生
	domains        []string      // 去掉 scheme 大小写差异与尾部斜杠
	linkCacheExpire time.Duration
	retryCodes     map[int]struct{}
	requestHeader  http.Header
}
```

`Strm.Init()` 在 `PathMap` 处理之后调用 `Proxy.Init()`; `Proxy` 为 nil 时按零值构造（等价于 `enable: false`）。

默认值: `MaxRedirectDepth` 未配置取 5; `LinkCacheExpired` 未配置取 `10m`; `RetryStatusCodes` 未配置取 `[403, 404, 410]`; `MaxConcurrentStreams` 未配置取 16。

`MaxConcurrentStreams` 的取值语义: `0` 表示不限制。注意 yaml 中"未配置"与"显式配置 0"都以零值呈现, 二者都按"不限制"处理会造成默认值失效 —— 因此 `Enable` 为真且该字段为 0 时, 按**显式关闭限制**处理, 默认值 16 只在配置项**完全缺省**（`proxy` 段中不出现该键）时生效。实现方式: 在 `Init()` 中先用 16 初始化, 再用 `yaml` 解码后的值覆盖。若该细节在实现中不易表达, 退化为"0 = 不限制, 默认 16", 并在 `config-example.yml` 中显式写出 `max-concurrent-streams: 16` 避免歧义。

**导出给 `streamproxy` 包的访问器**（放在 `internal/config/emby.go`, 与 `IsLocalMediaPath` 同风格）:

```go
// StrmProxyEnabled 判断 strm 直链代理播放是否开启
func (e *Emby) StrmProxyEnabled() bool

// StrmProxyConfig 获取 strm 直链代理配置 (未配置时返回零值, 不返回 nil)
func (e *Emby) StrmProxyConfig() StrmProxy
```

`streamproxy` 包通过这两个访问器读取配置, 不直接触碰未导出字段。

## 3. `NormalizeURL`

```go
// NormalizeURL 归一化 strm 地址
//
// 修复 strm 原文中的裸空格、未编码中文与全角标点:
// 直接把它们交给 http.NewRequest 会生成请求目标被空格截断的畸形请求行,
// 且 Go 不会报错, 属于静默失败。
func NormalizeURL(rawURL string) (string, error)
```

步骤（对应父 `design.md` §3）:

1. `strs.AnyEmpty(rawURL)` → `errors.New("strm 地址为空")`
2. `u, err := url.Parse(rawURL)`; `err != nil` → `fmt.Errorf("解析 strm 地址失败: %w", err)`
3. `u.RawQuery != ""`:
   - `q, err := url.ParseQuery(u.RawQuery)`; 出错 → `fmt.Errorf("解析 strm 地址查询参数失败: %w", err)`
   - `encoded := strings.ReplaceAll(q.Encode(), "+", "%20")`
   - `u.RawQuery = encoded`
4. 返回 `u.String()`

**测试断言要点**: 归一化结果中不得出现裸空格与裸非 ASCII 字节（`strings.ContainsAny` / 逐字节 `> 0x7F` 检查）。

## 4. `MatchDomain`

```go
// MatchDomain 判断地址是否命中配置的代理前缀
//
// 返回命中的前缀; 未开启代理或未命中时 ok 为 false
func MatchDomain(rawURL string) (prefix string, ok bool)
```

- 归一化两侧: `strs.TrimSpace` + scheme 部分小写（只处理开头的 `http://` / `https://` 段, 不动路径大小写）。
- 配置项尾部 `/` 去掉。
- 按配置顺序线性匹配, 第一个命中即返回。
- **边界校验**: 命中的前缀在原地址中结束位置的下一字符必须是 `/`、`?`、`#` 或字符串结尾。不满足则视为未命中, 继续匹配下一个前缀。

拒绝方案: 用 `net/url` 解析出 host 再比较。理由: 用户需要"锁定 host:port"与"锁定到路径前缀"两种粒度, 前缀匹配天然同时支持, host 比较做不到后者。

## 5. 上游请求与 3xx 跟随

```go
// resolveLink 请求上游并解析出可直接流式读取的最终地址
//
// 返回的 resp 已经带有本次请求的 Range 头, 调用方负责关闭 Body
func resolveLink(ctx context.Context, finalURL string, clientRange string) (*http.Response, error)
```

实现要点:

1. 查缓存（见 §6）。命中则直接用缓存地址发请求, 并打 L4。
2. 未命中:
   - `hook := https.Get(targetURL).Header(buildUpstreamHeader(clientRange)).Context(ctx)`
   - `resp, err := hook.DoSingle()`
   - `err != nil` → 打 L12, 返回错误（`written=false`）
   - `https.IsRedirectCode(resp.StatusCode)`:
     - `resp.Body.Close()`
     - `loc := resp.Header.Get("Location")`; 为空 → 报错
     - 处理相对跳转（复用 `RequestHolder` 相对地址拼接的语义: `http` 前缀直接用 / `/` 开头补 scheme+host / 其他用当前路径目录拼接）
     - 打 L6, 累加跳数; 超过 `MaxRedirectDepth` → 报错
     - 用新地址重新请求（回到步骤 2, 且**带上 Range**）
   - 非重定向:
     - `written` 置 true 之前先判断状态码; 命中 `RetryStatusCodes` 且未重试过 → 打 L10, 清缓存, 重试一次
     - 最终地址与原地址不同 → 写入缓存, 打 L7
3. 返回 `resp`。

`buildUpstreamHeader(clientRange string) http.Header`:

- 依次尝试从客户端请求头复制 `Range`、`Accept`（仅当存在）。
- `User-Agent`: 配置的固定头 > 客户端 UA > 不设置。
- 叠加配置 `request-header` 中的全部键值（覆盖同名透传头）。
- 不复制任何其他客户端头（白名单策略, 而非"全量复制后删除"——后者容易漏掉新增的敏感头）。

## 6. 直链缓存

```go
// linkEntry 直链缓存条目
type linkEntry struct {
	finalURL string
	expireAt time.Time
}

var linkCache sync.Map // map[string]linkEntry
```

- key: `NormalizeURL` 之后的地址。
- 读: 命中且未过期 → 返回; 已过期 → `Delete` 并视为未命中。
- 写: 仅当发生过跳转且最终地址 != 归一化地址。
- 清: 命中 `retry-status-codes` 时 `Delete`。
- 过期条目采用**读时惰性清理**。不引入后台清理 goroutine: 条目数量等于被访问过的不同 strm 地址数量, 量级很小, 无需额外机制。

**缓存不存响应体**, 只存地址字符串, 因此不存在内存风险。

## 7. `Proxy`

```go
// Proxy 将 rawURL 指向的媒体字节流代理给客户端
func Proxy(w http.ResponseWriter, r *http.Request, rawURL string) (written bool, err error)
```

流程:

1. `normalized, err := NormalizeURL(rawURL)` → 失败: 打 L11, 返回 `false, err`。
2. `resolveLink(r.Context(), normalized, r.Header.Get("Range"))` → 失败: 返回 `false, err`（尚未写响应, 调用方可回退）。
3. 响应头回写:
   - 过滤逐跳头后, 复制 `Content-Type` / `Content-Length` / `Content-Range` / `Accept-Ranges` / `Last-Modified` / `ETag`。
   - `resp.StatusCode == http.StatusPartialContent` 且上游未给 `Accept-Ranges` → 补 `Accept-Ranges: bytes`。
   - `w.WriteHeader(resp.StatusCode)`。
   - `w.(http.Flusher).Flush()`（若实现了 `http.Flusher`）—— 用类型断言, 断言失败不影响后续。
4. 打 L8（含客户端 Range）。
5. `io.CopyBuffer(w, resp.Body, buf)`，buf 来自 `bytess.CommonFixedBuffer()`（用完 `PutBack()`，与 `https.ProxyPass` 一致的资源复用方式）。**此时 `written` 已为 true**, 后续任何错误都不再回退。
6. 收尾:
   - 正常结束 → 打 L9（字节数 + 耗时）。
   - `err != nil` 且 `errors.Is(err, context.Canceled)`（或客户端断连特征）→ 打 L9'（Warn）。
   - 其他错误 → 打 Error（含已传输字节数）。
   - 返回 `true, err`。

**关于"客户端断连"的识别**: `io.CopyBuffer` 写向 `w` 失败时返回的错误通常是 `net/http` 的连接错误; 上游读失败则来自 `resp.Body`。两者都只需按 Warn/Error 分别记录, 不需要精确判别, 因此不做字符串匹配判断——只区分"是否已写出过响应体"。实现时如果发现难以区分, 记录原始错误即可, 不要在错误文本上做脆弱的字符串匹配。

## 8. 并发控制与观测

### 信号量

```go
// streamSlots 并发传输信号量
//
// 容量为 max-concurrent-streams; 上限为 0 时该 channel 为 nil, 不参与控制
var streamSlots chan struct{}

// activeStreams 当前活跃传输数, 仅用于日志观测
var activeStreams atomic.Int64

// acquireSlot 获取一个传输槽位
//
// 上限为 0 时立即返回;
// 等待期间客户端断开则返回错误, 调用方放弃本次请求
func acquireSlot(ctx context.Context) error

// releaseSlot 释放传输槽位
func releaseSlot()
```

要点:

- 信号量用**带缓冲的 channel**, 容量即上限; 上限为 0 时不创建 channel（保持 `nil`）, `acquireSlot` 直接返回 —— 避免"检查上限是否为 0"的分支散落在各处。
- `acquireSlot` 用 `select` 同时监听槽位 channel 与 `ctx.Done()`, **等待而不是拒绝**。满载时记 L13, 拿到槽位后记 L14。
- `releaseSlot` 必须在 `Proxy` 中用 `defer` 调用, 覆盖正常结束、上游出错、客户端中断三条路径（AC13）。
- `activeStreams` 用 `atomic.Int64`, 只用于日志, 不参与控制逻辑。

### 为什么默认值是 16

每路代理流约 128KB（父 `design.md` §11）, 16 路合计约 2MB, 在小内存机器上可忽略。
16 的依据是 Emby 客户端单次播放通常开 1-4 条连接, 该值提供了足够裕量, 正常使用不会触发等待。

**不要以省内存为由调低该值** —— 内存不是它的目的。需要调整时的依据是 L14 记录的实际并发峰值, 或上游出现连接拒绝。

## 9. 日志封装

```go
// log.go
func logInfof(format string, v ...any)    { logs.Info("[直链代理] "+format, v...) }
func logSuccessf(format string, v ...any) { logs.Success("[直链代理] "+format, v...) }
func logWarnf(format string, v ...any)    { logs.Warn("[直链代理] "+format, v...) }
func logErrorf(format string, v ...any)   { logs.Error("[直链代理] "+format, v...) }
```

不采用 `localtree` 的 `logs.Raw` + 自定义颜色前缀模式: 那样会丢失日志级别语义, 而 `logging-guidelines.md` 明确要求按级别映射选择函数。

## 10. 错误处理

- 所有错误用 `fmt.Errorf` + `%w` 包装, 中文消息（`error-handling.md`: 新代码优先 `%w`）。
- 不定义自定义错误类型, 不引入哨兵错误。
- `written` 布尔返回值是唯一的"是否可回退"信号。

## 11. 已排除的实现方案

| 方案 | 排除理由 |
|---|---|
| `httputil.ReverseProxy` / `http.Redirect` 复用 | 与 C1 冲突, 且需要自行重写 Director 才能满足请求头白名单 |
| 用 `https.ProxyPass` | 它是 `remote + RequestURI` 拼接模型, 无法处理"整条绝对 URL + 归一化"; 且无条件全量复制头 |
| 用 `RequestHolder.Do()` 自动跟随重定向 | 拿不到中间跳转, 无法实现 L6 日志与跳数上限控制 |
| 复制客户端全部请求头后再删除敏感项 | 白名单策略更安全（黑名单会随客户端新增头而出现遗漏） |
| 缓存整个上游响应 | 违反 R6, 且需要无限内存 |
