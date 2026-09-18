# 修复响应缓存内部响应头泄漏给客户端

> 父任务: `09-16-strm-proxy-play`。轻量任务, PRD-only。
> 来源: `09-16-cache-stream-passthrough` 的质量检查报告（W1 + N2）。

## Goal

`internal/web/cache` 的三个内部控制字段 `Expired` / `Space` / `Space-Key` 在两条返回路径上会被发给客户端。把清理时机上移, 让它们不再出网。

## Background

`RequestCacher` 的闭包里, 响应头清理注册在 `c.Next()` **之后**（`cache.go` 第 6 步），因此只有走到那一步之后才生效。两条路径绕过它:

### W1 —— 第 1 步提前返回

```go
// cache.go 第 1 步
if c.Writer.Header().Get(HeaderKeyExpired) == "-1" {
    return        // ← 清理的 defer 还没注册
}
```

`CacheableRouteMarker` 会给**所有不在白名单**的路由打上 `Expired: -1`。于是这些路由的每个响应都带着 `Expired: -1` 出网。

**实测对比**（同一测试 harness，只换被测代码）:

| 用例 | 改动前 | 改动后 |
|---|---|---|
| `/emby/Videos/123/stream`, handler 不设 `Expired` | 客户端未收到 `Expired` | 收到 `Expired = "-1"` |
| handler 自设 `Expired` 的路径 | 未收到任何内部头 | 收到 `Expired = "<时间戳>"` |

对 stream / download / sync-download 三条路由而言, 这是 `09-16-cache-stream-passthrough` 把它们移出白名单后**新出现**的行为; 对其余非白名单路由（图片、首页、m3u8、回源等）则是项目长期行为。

### N2 —— 缓存命中回放

```go
// cache.go 第 3 步（缓存命中）
https.CloneHeader(c.Writer, rc.header.header)   // ← 整份回放, 含内部头
```

被缓存过的响应在重放时, 连同 `Expired`（时间戳）、`Space`、`Space-Key`（内部缓存键）一起回写。**实测**: 第二次请求 `PlaybackInfo` 收到 `Expired = "1789537903017"`、`Space = "CheckSpace"`、`Space-Key = "g1"`。改动前后逐字节相同, 属既有问题。

## Requirements

- **R1** 把三个 `defer header.Del(...)` 上移到 `RequestCacher` 闭包中**第一个 `return` 之前**（即第 1 步的判断之前）, 使全部返回路径都被覆盖。
- **R2** 不得改变清理之外的行为: 缓存的读写时机、`-1` 的跳过语义、`calcCacheKey`、`putCache` 均不变。
- **R3** 不得依赖 `defer` 的执行时机与位置的耦合关系被误读 —— 在注释中写明"defer 在函数返回时执行, 上移是为了让所有返回路径都完成注册, 而不是为了提前执行"。

## Acceptance Criteria

- [ ] **AC1** 非白名单路由的响应中不再出现 `Expired` / `Space` / `Space-Key`。
- [ ] **AC2** 缓存命中的响应中不再出现这三个头。
- [ ] **AC3** 白名单路由（`PlaybackInfo` / 字幕 / 随机列表）的缓存读写行为不变: 第二次请求仍命中缓存、不进入 handler。
- [ ] **AC4** 直链缓存（366 空 body 的 302）仍生效。
- [ ] **AC5** `go build ./...`、`go vet ./internal/...`、`go test ./internal/web/cache/...` 通过。
- [ ] **AC6** 每个新增断言都用"删掉被测改动必须变红"验证过。

## Constraints

- 改动限于 `internal/web/cache`。
- 若手上还有 `09-16-cache-stream-passthrough` 的未提交改动, 本任务应在其之后进行 —— 两者改同一批函数, 分开验证更容易定位问题。
- **不做**其它缓存中间件的重构（把 header 信号换成 context key 之类的设计变更不在范围内）。

## Out of Scope

- 用 gin context key 取代 `Expired` 头作为"不缓存"的信号（那是设计变更, 会牵动两个中间件的契约）。
- `expires` 之外的其它 cache 语义。

## Notes

- 实际影响评估: 这三个是本项目自造的头, Emby 客户端与任何 CDN 都不会读取（HTTP 标准头是 `Expires`, 带 s）。实际风险约等于零。本任务的价值在于**消除内部实现细节的外泄**（`Space-Key` 暴露了内部缓存键）与**让注释与实际一致**, 而不是修复用户可见的故障。因此优先级定为 P3。
- 参考: `.trellis/spec/backend/response-cache.md` §3.5 已把当前行为记为已知项; 本任务完成后需同步更新该节。
