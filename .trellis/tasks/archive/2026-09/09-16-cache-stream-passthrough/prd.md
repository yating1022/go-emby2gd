# 响应缓存中间件不再缓冲媒体字节流响应

> 父任务: `09-16-strm-proxy-play`。本子任务是该功能的**前置 blocker**, 必须最先完成。
> 证据见父任务 `research/strm-proxy-findings.md` §4。

## Goal

让**媒体/文件字节流**类响应不再进入响应缓存中间件的内存缓冲, 使本项目可以把上游直链的内容流式转发给客户端, 而不会把整个响应体驻留在内存里。

部署机器的内存小于 1GB, 因此这不是"优化", 而是硬性约束。

## Background

### 缓冲是怎么发生的（既有代码, 非本次引入）

```go
// internal/web/cache/type.go:16
func (rcw *respCacheWriter) Write(b []byte) (int, error) {
	rcw.body.Write(b)                       // 无条件全量缓冲进 bytes.Buffer
	return rcw.ResponseWriter.Write(b)
}
```

```go
// internal/web/cache/cache.go:73,100-101
if c.Writer.Header().Get(HeaderKeyExpired) == "-1" { return }   // 判断在包装之前
c.Writer = &respCacheWriter{...}                                 // handler 拿到的已是缓冲 writer
go putCache(cacheKey, c, append([]byte(nil), customWriter.body.Bytes()...), respHeader)  // 再复制一份
```

`MaxCacheSize`（100MB）只在 `loopMaintainCache` 的**事后淘汰**里生效, 拦不住单次请求的缓冲。

### 为什么今天没暴露

`Reg_ResourceStream` 命中白名单（`cache.go:53`）, 但该分支当前总是以一个**空 body 的 307** 结束（"直链缓存 10 分钟"就是靠缓存这个 307 响应实现的）。缓冲区里只装了 0 字节。

一旦改成代理播放, 同一路径会写入 625MB 的媒体字节流 → 单请求 ~625MB 常驻, `putCache` 再复制一份 → 在小内存机器上必然 OOM。

**同一个坑今天已存在于 `checkErr` 回退路径**（默认 `proxy-error-strategy: origin`）: 错误回退时 `ProxyOrigin` 代理回源的响应体走的是同一条缓冲。只是 Emby 的 `/stream` 通常回一个空体重定向, 所以一直没触发。

### 旁路方式的选择

| 方案 | 结论 |
|---|---|
| **摘白名单（采用）** | 把字节流类路由从 `CacheableRouteMarker` 白名单移除 |
| 显式旁路接口 | 在中间件上开放一个 handler 可调用的关闭方法。改动更大, 且**更容易出错**: 以后在这几条路由上新增 handler 时忘记调用就会重新 OOM |
| 全局关闭响应缓存 | 会连带丢掉 `PlaybackInfo`（2-3 秒 → 12 小时）、字幕（30 天）等最有价值的缓存, 代价远大于收益 |

采用摘白名单的核心理由是**结构性安全**: "媒体字节流的响应不进内存缓存"从此是路由层面的不变量, 而不是依赖每个 handler 记得调用一个方法。

### 代价（开发者已知悉并接受）

未命中代理前缀的 strm 地址仍会走 302, 但 302 响应不再被缓存。实测代价很小: 客户端拿到 302 后即离开本项目直连上游, 一次播放只经过本项目一次, 该缓存节省的仅是一次 Emby PlaybackInfo 查询。

## Requirements

- **R1 移除白名单条目**: 从 `CacheableRouteMarker()` 的 `cacheablePatterns` 中移除 `Reg_ResourceStream`、`Reg_ItemDownload`、`Reg_ItemSyncDownload`。移除后这三条路由的请求会被打上 `Expired: -1`, `RequestCacher` 在第一行就返回, `c.Writer` 不会被包装。
- **R2 缓冲上限加固**: 为 `respCacheWriter` 增加单响应缓冲上限, 超过则放弃缓存并转为直通（不写缓存, 也不丢字节）。这是纵深防御 —— 剩余可缓存路由以元数据（JSON / 字幕）为主, 正常情况下远达不到上限, 但不应存在"某个响应悄悄吃掉整个缓存预算"的路径。
- **R3 保留剩余缓存能力**: `Reg_PlaybackInfo`、`Reg_VideoSubtitles`、`Reg_UserItemsRandomWithLimit` 三条路由的缓存行为与改动前**完全一致**。
- **R4 不改动中间件其它语义**: `Expired` / `Space` / `Space-Key` 三个内部响应头的设置与清理时机、`HeaderKeyExpired = "-1"` 的跳过语义、缓存空间机制均保持不变。
- **R5 handler 无感知**: `internal/service/emby` 侧不需要任何改动, 不引入新 API 供 handler 调用。

## Acceptance Criteria

- [ ] **AC1** 请求 `/Videos/{id}/stream` 与 `/Items/{id}/Download` 时, `c.Writer` 不再是缓冲 writer（`RequestCacher` 在包装前返回）。
- [ ] **AC2** 在这两条路由上写入一个远超上限的响应体时, 进程堆内存增量与响应体大小无关（用 `runtime.ReadMemStats` 断言）。
- [ ] **AC3** `PlaybackInfo` 的缓存仍然生效: 同一请求第二次不再回源 Emby（可通过日志或假源计数验证）。
- [ ] **AC4** 字幕路由的缓存仍然生效。
- [ ] **AC5** 未命中代理前缀的 strm 播放请求仍返回 307 到目标地址（行为不变, 仅不再被缓存）。
- [ ] **AC6** R2 的上限生效: 超过上限的响应不会被写入缓存, 且客户端收到的响应体**完整无损**。
- [ ] **AC7** `go build ./...`、`go vet ./internal/...`、`go test ./internal/...` 通过。
- [ ] **AC8** 新增逻辑有表驱动单元测试（覆盖 R2 的两条路径）。

## Constraints

- 改动留在 `internal/web/cache` 包内, 不新增反向依赖。
- 不引入新依赖; 测试只用标准库。
- 新常量用中文注释说明取值理由。
- 不改变 `cache.CacheKeyIgnoreParams`、`calcCacheKey`、`putCache` 的既有逻辑。

## Out of Scope

- 重构 `internal/web/cache` 同时承担"响应缓存"与"直链缓存"两个职责的现状。
- 为 `WriteString` 补齐缓冲一致性 —— gin v1.10.0 的 `responseWriter.WriteString` 直接 `io.WriteString` 到下层, 本来就不经过 `Write`; 补齐它会**改变**既有 `c.String(...)` 响应的缓存行为, 属于回归风险。
- 为 handler 提供显式的缓存旁路 API（本方案已由路由层面结构性解决, 不加无人调用的接口）。

## Notes

- 本子任务独立提交。回滚后 `internal/web/cache` 回到"字节流响应会被整体缓冲"的状态, **此时必须同时关闭 `emby.strm.proxy.enable`**, 否则代理播放会在小内存机器上 OOM。
- 摘白名单会顺带修掉 `checkErr` 回退路径上那个同源的既有隐患。
