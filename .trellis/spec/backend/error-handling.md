# Error Handling

> How errors are handled in this project.

---

## Overview

This project uses **plain Go errors** — no third-party error libraries (no pkg/errors), no custom error types implementing `Error() string`, and no `errors.Is/As/Unwrap/Join` anywhere in the codebase.

Error handling has two parallel channels:

1. **Go `error` return values** — used by util layer and most internal functions (`internal/util/https`, `internal/util/jsons`, `internal/service/emby/media.go` `resolveItemInfo`, etc.)
2. **`model.HttpRes[T]` struct** (`internal/model/http.go:4-8`, fields `Code int / Data T / Msg string`) — used by remote-fetch wrappers (`openlist.FetchResource`, `emby.RawFetch`); callers check `res.Code != http.StatusOK`

The web layer funnels all handler errors through one exit point: **`checkErr(c, err)`** (`internal/service/emby/redirect.go:212-234`).

**All error messages are written in Chinese** (e.g. `"读取请求体失败: %v"`). New code must keep this convention.

---

## Error Types

No custom error types exist. Only three kinds of sentinel errors, all compared with `==`:

| Sentinel | Defined at | Purpose |
|---|---|---|
| `openlist.ErrWalkEOF` | `internal/service/openlist/walk.go:13` | End of paginated walk |
| `jsons.ErrBreakRange` | `internal/util/jsons/item.go:19` | Early-terminate `RangeArr`/`RangeObj` traversal |
| `haveReturned` | created inline in `playbackinfo.go:90,201` | Control-flow signal: "client already answered from origin" |

Package-level sentinels (`ErrWalkEOF`, `ErrBreakRange`) are the preferred pattern for new sentinels. The inline `haveReturned` pattern is legacy — do not replicate it.

---

## Error Handling Patterns

### Creating errors

- `fmt.Errorf` (dominant, ~116 uses) for contextual errors; `errors.New` (~27 uses) for static validation messages.
- Message shape: **Chinese phrase + `: ` + values**, e.g. `fmt.Errorf("路由正则编译失败, pattern: %v, error: %v", pattern, err)` (`internal/web/handler.go:102`).
- Wrap with `%w` in newer modules (`localtree/`, `util/files`, `config/openlist.go`); older code mixes `%v`/`%s`. **For new code prefer `%w`** when wrapping another error, `%v` is acceptable when just embedding it in a message.
- Do not use `fmt.Errorf` without format verbs (e.g. `fmt.Errorf("不可重复初始化")`) — use `errors.New` for constant strings.

### Propagating to the web layer: `checkErr`

Every emby handler follows this shape:

```go
if checkErr(c, err) {
    return
}
```

`checkErr` (`internal/service/emby/redirect.go:212-234`):

- Sets `cache.HeaderKeyExpired: "-1"` so the failing response is never cached.
- Under `config.PeStrategyReject`: logs `logs.Error("代理接口失败: %v", err)` and returns `500` fixed text.
- Otherwise: logs and falls back to `ProxyOrigin(c)` (proxy the origin server as a safety net).

Inline form is also accepted: `checkErr(c, https.ProxyPass(w, r, remote))` (`internal/service/emby/episode.go:24`).

### Retrying

Use `trys.Try(fn, tryNum, interval)` (`internal/util/trys/trys.go`) for synchronous retries. Real usages: 3×/2s origin probe (`emby/redirect.go:249-258`), 3×/1s ffmpeg probing (`localtree/task.go`), 3×/5s downloads. Caveat: `tryNum <= 0` returns `nil` — always pass a positive count.

### Goroutines

- Fire-and-forget helpers log failures with `logs.Warn` and return (`emby/playing.go:106-113`).
- Concurrent collection: `errgroup` (`golang.org/x/sync/errgroup`) in `localtree/synchronizer.go:84` and `emby/custom_cssjs.go`; or a buffered channel where `nil` means failure (`playbackinfo.go:151-175`).
- **Never let a goroutine panic** — there is no per-goroutine recover; only `gin.Recovery()` (request goroutines) and one `defer recover` in `jsons.New` (`internal/util/jsons/jsons.go:123-127`) exist. A panic in a background goroutine kills the whole process.

### panic policy

- Startup failures: `log.Fatal` in `main.go` (process exit).
- Inside request handling: rely on `gin.Recovery()` (`internal/web/web.go:57,74`); avoid deliberate panics — `emby.go:31` (`ProxySocket` init) is a known exception, not a pattern to copy.
- Deep reflection code may convert panics to errors via `defer recover` (see `jsons.New`).

---

## API Error Responses

Three coexisting formats — pick by API family:

1. **Proxy/resource endpoints** (emby, m3u8, subtitles, downloads): plain-text **fixed messages**, never leak `err` details to the client. Status codes: 500 `"代理接口失败, 请检查日志"`, 400 `"代理 m3u8 失败, 请检查日志"`, 401 `"鉴权失败"` + `c.Abort()`, 403 `"下载接口已禁用"`.
2. **Management endpoints** (ge2o's own API, e.g. `internal/service/openlist/localtree/api.go`): always HTTP 200 + JSON `model.Response{Success bool, Message string}` (`internal/model/gin.go:4-7`). Note: current code puts raw `err.Error()` into `Message` — acceptable here, but prefer sanitized messages for new endpoints.
3. **Origin passthrough**: non-200 origin status becomes `checkErr(c, errors.New(resp.Status))` (`emby/episode.go:45-47`).

---

## Common Mistakes

Known pitfalls in the current codebase — do not replicate in new code:

1. **Ignoring errors silently**: `itemInfo, _ := resolveItemInfo(...)` (`emby/items.go:129`), ~11 ignored `url.Parse` errors. If you must ignore (e.g. constant URL templates), add a comment saying why the input is trusted.
2. **Aborting without a response body**: `emby/auth.go:119-124` aborts after a failed origin request, leaving the client with 200 + empty body. Always write a status + body before `c.Abort()`.
3. **`trys.Try` with `tryNum <= 0`** returns `nil`, indistinguishable from success.
4. **Typos in messages**: `"参数为设置"` (should be 未设置) — review Chinese messages.
5. **Logging secrets**: `logs.Info("解析到的 itemInfo: %v", itemInfo)` prints `ApiKey` in cleartext (`redirect.go:71`). Never log api keys/tokens.
