# 执行计划: 响应缓存中间件不再缓冲媒体字节流响应

## 涉及文件

| 文件 | 改动 |
|---|---|
| `internal/web/cache/cache.go` | `CacheableRouteMarker()` 白名单移除三条字节流路由 |
| `internal/web/cache/holder.go` | 新增具名常量 `MaxBufferedRespSize` |
| `internal/web/cache/type.go` | `respCacheWriter` 增加 `disabled` 字段与 `canBuffer`; `Write` 按需缓冲 |
| `internal/web/cache/cache_internal_test.go` | 新增内部测试 |

**不涉及** `internal/service/emby` —— handler 侧零改动（R5）。

## Step 1 摘除白名单条目

```go
// CacheableRouteMarker 缓存白名单
// 只有匹配上正则表达式的路由才会被缓存
//
// 注意: 媒体/文件字节流类路由 (stream / download / sync download) 不在白名单内。
// 响应缓存会把整个响应体缓冲进内存, 而这类响应体可能是数 GB 的媒体数据,
// 必须由 handler 直接流式写回客户端。
func CacheableRouteMarker() gin.HandlerFunc {
	cacheablePatterns := []*regexp.Regexp{
		regexp.MustCompile(constant.Reg_PlaybackInfo),
		regexp.MustCompile(constant.Reg_VideoSubtitles),
		regexp.MustCompile(constant.Reg_UserItemsRandomWithLimit),
	}
	...
```

删掉的三行: `Reg_ResourceStream`、`Reg_ItemDownload`、`Reg_ItemSyncDownload`。

**验证方式**: 起服务后请求一次 `/Videos/{id}/stream`, 在 `RequestCacher` 第一行的跳过分支打一个临时断点或临时日志, 确认命中。验完删掉临时日志。

## Step 2 缓冲上限加固

`holder.go` 常量块内:

```go
	// MaxBufferedRespSize 单个响应可进入内存缓冲的最大字节数
	//
	// 超过该值的响应不再参与缓存, 转为直通写回客户端。
	//
	// 摘除字节流路由后, 剩余可缓存响应以元数据 (PlaybackInfo / 字幕 / 列表 JSON) 为主,
	// 正常体积远小于该值; 该阈值是纵深防御, 避免出现"某个响应悄悄吃掉整个缓存预算"的路径。
	// 取值 32MB 远小于 MaxCacheSize (100MB), 因此不会影响任何正常可缓存的响应。
	MaxBufferedRespSize int64 = 32 * 1024 * 1024
```

`type.go`:

```go
// respCacheWriter 自定义的请求响应器
type respCacheWriter struct {
	gin.ResponseWriter               // gin 原始的响应器
	body               *bytes.Buffer // gin 回写响应时, 同步缓存
	disabled           bool          // 是否已关闭缓冲 (响应体超过上限)
}

// Write 写入响应体
//
// 未关闭缓冲且未超过 MaxBufferedRespSize 时同步缓存到内存;
// 超过上限时关闭缓冲并丢弃已缓存内容, 之后所有字节直通客户端
func (rcw *respCacheWriter) Write(b []byte) (int, error) {
	if rcw.canBuffer(len(b)) {
		rcw.body.Write(b)
	}
	return rcw.ResponseWriter.Write(b)
}

// canBuffer 判断再写入 n 字节是否仍在缓冲上限内
//
// 超过上限时会将 disabled 置为 true 并清空已缓冲内容,
// 确保该响应不会被写入缓存 (避免缓存到被截断的响应体)
func (rcw *respCacheWriter) canBuffer(n int) bool {
	if rcw.disabled {
		return false
	}
	if int64(rcw.body.Len()+n) > MaxBufferedRespSize {
		rcw.disabled = true
		rcw.body.Reset()
		return false
	}
	return true
}
```

**不要为 `WriteString` 增加覆写**（见 prd "Out of Scope"）。

`cache.go` 的 `RequestCacher` 增加提前返回, 并**把三个内部响应头的清理提到所有提前返回之前**:

```go
		// 5 执行请求处理器
		c.Next()

		// 6 响应头清理必须在任何提前返回之前
		//
		// 这三个是缓存中间件的内部控制字段, 不能泄漏给客户端
		header := c.Writer.Header()
		defer header.Del(HeaderKeyExpired)
		defer header.Del(HeaderKeySpace)
		defer header.Del(HeaderKeySpaceKey)

		// 7 响应体超过缓冲上限, 已放弃缓存, 直接返回
		if customWriter.disabled {
			return
		}

		// 8 不缓存错误请求
		if https.IsErrorStatus(c.Writer.Status()) {
			return
		}

		// 9 刷新缓存
		respHeader := respHeader{
			expired:  header.Get(HeaderKeyExpired),
			space:    header.Get(HeaderKeySpace),
			spaceKey: header.Get(HeaderKeySpaceKey),
			header:   header.Clone(),
		}
		go putCache(cacheKey, c, append([]byte(nil), customWriter.body.Bytes()...), respHeader)
```

**为什么要调整响应头清理的顺序**: 原代码在 `putCache` 之后用 `defer` 清理。若在第 7 步直接 `return`, 那些 `defer` 不会被注册, `Expired` / `Space` / `Space-Key` 会被发给客户端。提到前面既能修掉这个隐患, 也覆盖了新增的第 7 步返回路径。

## Step 3 测试

新增 `internal/web/cache/cache_internal_test.go`, 使用 `package cache`。

**偏离既有约定并记录理由**: `quality-guidelines.md` 要求测试用外部测试包 `package xxx_test`, 但要验证的是**未导出的** `respCacheWriter` / `canBuffer`, 外部包无法构造带 `disabled` / `body` 字段的实例。这是本任务唯一一处有意偏离。

其他约定不放宽: 表驱动、标准库断言、不写文件到仓库、不用 `log.Fatal`。

| 用例 | 验证 |
|---|---|
| `TestRespCacheWriter_BufferWithinLimit` | 未超限时 `body` 累积内容与写入一致 |
| `TestRespCacheWriter_ExceedLimitDisablesCache` | 超限后 `disabled` 为真、`body` 被清空, 底层 writer 仍收到**完整**数据 |
| `TestRespCacheWriter_AlreadyDisabled` | 已 `disabled` 时连续 `Write`, `body.Len()` 保持 0 |

为避免构造 32MB 数据, 用例直接构造**已填充接近上限**的 `body`（`bytes.NewBuffer(make([]byte, MaxBufferedRespSize-1))`），只需再写 2 字节即触发上限。注意这会占用约 32MB 内存；若在低内存环境跑测试有问题, 把上限比较抽成一个接受阈值参数的纯函数再按小阈值测试。

`gin.ResponseWriter` 的构造: 用 `gin.CreateTestContext(httptest.NewRecorder())` 取 `c.Writer`, 再包一层 `respCacheWriter`。`httptest` 是标准库, 不违反"禁止第三方测试库"。

## Step 4 验证

```bash
export PATH=$PATH:/usr/local/go/bin
mkdir -p web/dist

gofmt -l internal/web/cache/
go vet ./internal/web/cache/...
go build ./...
go test ./internal/web/cache/... -v
go test ./internal/...
```

手工验证 AC5（需要真实环境）:

1. 播放一个**未命中**代理前缀的 strm 地址 → 应仍收到 307, 目标地址正确。
2. 重复请求同一地址 → 日志中仍应看到解析过程（说明不再命中响应缓存）。
3. 请求 `PlaybackInfo` 两次 → 第二次应命中缓存, **不**回源 Emby（AC3）。

## Review Gate

- [ ] 白名单中已无 `Reg_ResourceStream` / `Reg_ItemDownload` / `Reg_ItemSyncDownload`
- [ ] 剩余三条白名单正则**逐字符未变**
- [ ] 三个内部响应头在任何返回路径上都会被清理
- [ ] `respCacheWriter` 未超限时的行为与原实现逐字节等价（原实现无条件 `body.Write`）
- [ ] 没有为 `WriteString` 增加覆写
- [ ] 没有给 handler 新增任何缓存旁路 API
- [ ] `internal/service/emby` 下零改动
- [ ] 新常量有中文注释与取值理由
- [ ] 临时验证日志/断点已删除

## 回滚点

本子任务独立提交一个 commit。回滚后字节流响应会重新被整体缓冲, **必须同时关闭 `emby.strm.proxy.enable`**。
