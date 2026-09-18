# Research: strm 直链代理播放的现状、实测证据与风险

- **Query**: strm 内容为配置域名开头的 http 地址时, 让本项目代理播放而不是 302 的可行性与实现约束
- **Scope**: internal/service/emby 播放链路 + internal/web/cache 中间件 + internal/util/https 出站客户端; 含 2 次真实上游探测与 1 次本地 Go 复现
- **Date**: 2026-09-16
- **代码基线**: main 分支, HEAD = d697fec (v2.8.2 之后)

---

## 关键结论速览

1. **用户描述的上游"返回一个直链"并未发生**：实测 `http://vault.bjyt.de:7811/redirect?...` 返回的是 `200 OK` 并直接把文件流吐回来, 没有 `Location`。该端点自身就是源, 不存在"解析出直链"这一步。协议上仍按"兼容 3xx 与 2xx"实现。
2. **上游完整支持 Range**：带 `Range: bytes=0-1023` 请求返回 `206 Partial Content` + `content-range: bytes 0-1023/655902790`, 拖动进度可用。不带 Range 时响应头**没有** `Accept-Ranges`。
3. **URL 编码是真 bug, 不是理论风险**：strm 原文含裸空格与中文, 直接交给 `http.NewRequest` 会生成**畸形的 HTTP 请求行**（请求目标被第一个空格截断），且 Go 不报错。现有 `strm.internal-redirect-enable` 功能因此对这类 URL 已经失效。
4. **响应缓存中间件会把整个响应体缓冲进内存**：`respCacheWriter.Write` 无条件 `rcw.body.Write(b)`; 而 `Reg_ResourceStream` 在可缓存白名单内。改成代理 625MB 播放会直接打爆内存 —— 这是本功能的**前置 blocker**, 已单独立为子任务。
5. 现有"直链缓存 10 分钟"并不是一个独立的直链缓存模块, 而是**靠响应缓存中间件缓存那个空 body 的 302 响应**实现的。代理模式下该机制不再适用, 需要独立的、只缓存 URL 的小缓存。

---

## 1. 现状链路（改前行为）

播放请求的入口是 `internal/service/emby/redirect.go:59 Redirect2OpenlistLink`, 由 `internal/web/route.go` 的规则表挂到两条正则上:

| 正则 | 常量 | 值 |
|---|---|---|
| `/Videos/{id}/stream`、`/universal` | `Reg_ResourceStream` | `(?i)^/.*(videos\|audio)/.*/(stream\|universal)(\.\w+)?\??` |
| `/Items/{id}/Download` | `Reg_ItemDownload` | `(?i)^/.*items/\d+/download($\|\?)` |

处理分支（`redirect.go:59-178`）:

1. `resolveItemInfo` → 拿到 `itemInfo`（含 api key、PlaybackInfoUri）
2. 转码资源 → 重定向到本地 m3u8 代理（本任务不涉及）
3. `getEmbyFileLocalPath` → 请求 Emby PlaybackInfo 取 `MediaSources[].Path`
4. **`urls.IsHttpRemote(embyPath)` 为真（strm）** → `redirect.go:95-106`:
   - `config.C.Emby.Strm.MapPath(embyPath)` 做路径片段替换
   - `getFinalRedirectLink(finalPath, header)`（`redirect.go:241`）: 仅当 `strm.internal-redirect-enable` 为真时，用 `https.Get(link).Header(header).DoRedirect()` 内部请求一次拿最终地址；失败则回退原地址
   - `c.Header(cache.HeaderKeyExpired, cache.Duration(time.Minute*10))` + `c.Redirect(307, finalPath)` → **客户端被甩到目标地址**
5. 本地媒体 → 同源 307 回 Emby / 不同源代理回源（`ProxyOrigin` → `https.ProxyPass`）
6. OpenList 路径 → `/api/fs/get` 拿直链后 307（本任务后将移除）
7. 任何一步 `checkErr` 失败 → 按 `emby.proxy-error-strategy` 回源或返回 500

### 与需求的关系

需求要的"本项目代理字节流"正好是第 4 步的终点从 `c.Redirect` 换成"流式回写"。**改动点集中的程度比预期高**，但第 4 步之外还有三个隐藏前提（下面第 3、4、5 节）。

### "直链缓存 10 分钟"的真实实现

`redirect.go:99` 只是设置了一个响应头。真正缓存的是 `internal/web/cache` 中间件:

- `CacheableRouteMarker()` 白名单包含 `Reg_ResourceStream` / `Reg_ItemDownload`（`cache.go:50-57`）
- 不在白名单的路由会被打上 `Expired: -1`，`RequestCacher` 第一行据此跳过
- 在白名单内的请求，响应（含那个空 body 的 307 和 `Location` 头）被存进 `cacheMap`，10 分钟内同样的 cache key 直接重放

缓存 key 由 `calcCacheKey` 计算，`Range` 在 `CacheKeyIgnoreParams` 里被忽略（`cache.go:33`），所以不同 Range 请求共享同一个缓存条目 —— 这在 302 场景下没问题。

---

## 2. 上游实测证据（2 次探测, 用户授权上限 3 次）

探测目标: `http://vault.bjyt.de:7811/redirect?path=/影视库/最新电影/世界第一初恋：求婚篇 (2020)/世界第一初恋：求婚篇 (2020).mkv&pickcode=18Vozg_HhL8lpxkYprYE0rtXPjkbzRn7l&storage=googledrive-1`

### 探测 1 —— 不带 Range

```
HTTP/1.1 200 OK
date: Wed, 16 Sep 2026 03:49:43 GMT
server: uvicorn
content-type: video/x-matroska
content-length: 655902790
```

### 探测 2 —— 带 `Range: bytes=0-1023`

```
HTTP/1.1 206 Partial Content
date: Wed, 16 Sep 2026 03:50:10 GMT
server: uvicorn
content-type: video/x-matroska
content-range: bytes 0-1023/655902790
content-length: 1024
```

实际落盘 1024 字节。

### 结论

| 观察 | 对设计的影响 |
|---|---|
| 无 `Location`, 直接 200 流式 | 不存在"解析直链"步骤; 但为了兼容用户其他链接, 协议上仍支持 3xx 跟随 |
| 服务端 `uvicorn`（Python/FastAPI 系） | 上游是自建网关, 大概率有 IP 白名单等访问控制 —— 与"客户端直连不了、必须过本项目"的前提一致 |
| 完整支持 Range / 206 | 可以直接透传客户端的 `Range`, 拖动进度不需要额外适配 |
| 200 响应无 `Accept-Ranges` | 代理回写时应主动补 `Accept-Ranges: bytes`，帮助播放器识别可拖动 |
| `content-length` 是全量大小 | 上游在首响应就给出总长度, 适合直接透传 |

**已消耗 2/3 次配额, 保留 1 次用于实现阶段验证。**

---

## 3. 风险 A：URL 编码 —— 沿用了原始 strm 文本会生成畸形请求

strm 内容里的 `path` 参数含裸空格、中文与全角冒号。本地 Go 1.26.3 复现（`url.Parse` → `http.NewRequest` → `req.Write`）:

```
1) NewRequest ok, 实际请求行 =
"GET /redirect?path=/影视库/最新电影/世界第一初恋：求婚篇 (2020)/世界第一初恋：求婚篇 (2020).mkv&pickcode=...&storage=googledrive-1 HTTP/1.1"
   Write err=<nil>
```

HTTP 请求目标到第一个空格就结束, 所以上游实际只会收到 `path=/影视库/最新电影/世界第一初恋：求婚篇`，其余被当成协议垃圾；后续的中文也是裸 UTF-8 字节。**`http.NewRequest` 与 `req.Write` 全程不报错**，属于静默失败，排查成本很高。

对照 `url.ParseQuery` + `url.Values.Encode()` 归一化后的请求行:

```
"GET /redirect?path=%2F%E5%BD%B1%E8%A7%86%E5%BA%93%2F...%EF%BC%9A%E6%B1%82%E5%A9%9A%E7%AF%87+%282020%29%2F...mkv&pickcode=...&storage=googledrive-1 HTTP/1.1"
```

注意 `Encode()` 把空格编码成 `+`。查询串语境下 `+` 即空格, 语义正确; 但为了对任何实现都无歧义, 建议编码后再做一次 `strings.ReplaceAll(q, "+", "%20")`（`Encode()` 会把原文里的字面 `+` 编码成 `%2B`, 所以这个替换是无损的）。

### 已有影响

`getFinalRedirectLink`（`redirect.go:251`）走的是 `https.Get(originLink)` → `RequestHolder.execute` → `http.NewRequestWithContext`，同样不编码。所以 **`strm.internal-redirect-enable` 对这类 URL 现在就是坏的**。本任务顺带修复。

---

## 4. 风险 B：响应缓存中间件会缓冲整个响应体（前置 blocker）

```go
// internal/web/cache/type.go:16
func (rcw *respCacheWriter) Write(b []byte) (int, error) {
	rcw.body.Write(b)                       // ← 无条件全量缓冲
	return rcw.ResponseWriter.Write(b)
}
```

```go
// internal/web/cache/cache.go:70-101
func RequestCacher() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Writer.Header().Get(HeaderKeyExpired) == "-1" { return }   // ← 判断在包装之前
		...
		customWriter := &respCacheWriter{body: &bytes.Buffer{}, ResponseWriter: c.Writer}
		c.Writer = customWriter                                          // ← handler 拿到的已是缓冲 writer
		c.Next()
		...
		go putCache(cacheKey, c, append([]byte(nil), customWriter.body.Bytes()...), respHeader)  // ← 再复制一份
	}
}
```

`MaxCacheSize`（100MB, `holder.go`）只在 `loopMaintainCache` 的事后淘汰里生效，**拦不住单次请求的缓冲**。

### 影响面

| 场景 | 是否被包装 | 后果 |
|---|---|---|
| `Reg_ResourceStream` / `Reg_ItemDownload` 命中白名单 | 是 | 代理 625MB 播放 = 单请求 ~625MB 常驻 + `putCache` 再复制一份 → OOM |
| `checkErr` 回退到 `ProxyOrigin`（`proxy-error-strategy: origin`, 默认值） | 是（若命中上述路由） | **既有潜伏 bug**：错误回退时代理回源的媒体体也走同一缓冲 |
| `Reg_ResourceOriginal`（`ProxyOriginalResource` 的代理回源路径） | 否（不在白名单） | 安全，这是 README 里"不会内存暴涨"那句的由来 |

所以本功能必须在此之前先解决缓冲问题，已拆为子任务 `09-16-cache-stream-passthrough`。

---

## 5. 风险 C：HEAD 请求被全局短路

```go
// internal/web/handler.go:20-23
func globalDftHandler(c *gin.Context) {
	if c.Request.Method == http.MethodHead {
		c.String(http.StatusOK, "")
		return
	}
```

所有 HEAD 请求在进入规则表之前就返回空 200，永远到不了代理处理器。这是全局既有行为，本任务**不改动**，但需要在验收时留意：若某客户端靠 HEAD 探测文件大小/是否支持 Range，它拿不到真实信息。列为开放问题。

---

## 6. 可复用的既有实现

| 能力 | 现有实现 | 能否复用 |
|---|---|---|
| 出站 HTTP 客户端 | `internal/util/https`（全局 `client`, 链式 `RequestHolder`, `CheckRedirect: ErrUseLastResponse`, `ResponseHeaderTimeout: 5min`） | **必须**复用（规范要求所有出站请求走这里）；`DoSingle()` 不自动重定向, 正好用来手动处理 3xx |
| 流式代理 | `https.ProxyPass`（`util/https/web.go`）: `CloneHeader` + `io.CopyBuffer` + `bytess.CommonFixedBuffer()` 缓冲池复用 | 结构可借鉴, 但它的 `ProxyRequest` 是 `remote + r.RequestURI` 拼接, 不适用于"整条绝对 URL + 需要归一化", 需要新的代理函数 |
| 上游 ctx 传递 | `ProxyRequest` 里 `.Context(r.Context())` | 必须照做, 否则客户端断开后上游连接不释放, 大文件会持续拉取 |
| 重试 | `trys.Try(fn, tryNum, interval)` | 可用于上游建连重试；注意 `tryNum <= 0` 会返回 nil |
| 带前缀的日志 | `localtree` 的 `logf` + `logs.Raw` 模式 | 仅借鉴"前缀"思路；本任务改用标准级别函数 + 固定前缀, 保留级别语义（见 design.md） |

---

## 7. 待确认 / 未验证

1. 用户其他 strm 链接是否为同一网关、是否会有 `302` 分支（当前按两种都兼容实现）。
2. 上游网关是否存在并发连接数限制（播放器会并发多条 Range 连接）。
3. 代理后客户端 IP 变为本项目服务器 IP；若上游做 IP 白名单，需要把本项目服务器 IP 加白。
