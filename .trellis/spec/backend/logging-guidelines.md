# Logging Guidelines

> How logging is done in this project.

---

## Overview

Logging goes through the **in-house package `internal/util/logs`** — not slog/zap/logrus (none are used). It is printf-style, writes to stdout, and broadcasts every line to registered `Logger` implementations (currently: stdout `defaultLogger` + a WebSocket pusher `wsLogger` in `internal/service/log.go` for the live-log web page).

**What does NOT exist** (do not assume otherwise): log level filtering, dynamic level switches, file output, rotation, structured fields, caller file/line info. The only log-related config is `disable-color` (`internal/config/log.go`).

**All log messages are written in Chinese.** New code must keep this.

---

## Log Levels

Seven level functions in `internal/util/logs/logs.go`. Each emits `timestamp (YYYY-MM-DD HH:MM:SS) + colored prefix + formatted message`:

| Function | Prefix | Color | When to use | Example call site |
|---|---|---|---|---|
| `logs.Info` | `[INFO]` | Blue | Key request-flow nodes (parsed itemInfo, init steps) | `redirect.go:71`, `web/route.go:20` |
| `logs.Success` | `[SUCCESS]` | Green | Redirect/proxy succeeded | `redirect.go:83`, `m3u8/proxy.go:87` |
| `logs.Warn` | `[WARN]` | Yellow | Degradable failures (fallback to original link, auxiliary request failed) | `redirect.go:261`, `playing.go:107` |
| `logs.Error` | `[ERROR]` | **Gray** (not red — known quirk, comment in `logs.go:47` says red) | Proxy/parse/request failures that fail a request | `emby.go:118`, `redirect.go:226` |
| `logs.Tip` | (none) | Gray | Debug-ish hints: path conversion details, cache eviction | `path/path.go:48` |
| `logs.Progress` | (none) | Purple | Background task progress (playlist maintenance count) | `m3u8/m3u8.go:228` |
| `logs.Raw` | (none) | none | Access log lines and module-prefixed custom output | `web/log.go`, `localtree` |

Level mapping used in practice: request node → `Info`; success → `Success`; failure that breaks the request → `Error`; failure with a fallback → `Warn`; internal detail → `Tip`; background loop status → `Progress`.

---

## Structured Logging

None. Conventions instead:

- **Message shape**: Chinese phrase + `: ` + values, e.g. `logs.Error("代理接口失败: %v", err)`.
- **Always carry context in failure messages** — the failing URL/path/template and the error: `logs.Error("playlist 更新失败, path: %s, template: %s, err: %v", path, template, err)` (`m3u8/m3u8.go:90`).
- User-facing failure messages may end with `, 请检查日志` (see `checkErr` in `redirect.go:227`).
- Pass the error value directly (`%v`, err); do not pre-call `err.Error()`.

### Module-prefixed logging

For a module that wants its own prefix and colors, follow the `localtree` pattern — a private helper over `logs.Raw`:

```go
// internal/service/openlist/localtree/localtree.go:76-79
func logf(c colors.C, format string, v ...any) {
    s := fmt.Sprintf(format, v...)
    logs.Raw("%s%s\n", logs.Now(), colors.WrapColor(c, "[openlist 目录树]: "+s))
}
```

### Access log

`internal/web/log.go` `CustomLogger(port)` middleware emits one `logs.Raw` line per request: version header, time, colored status code, latency, ClientIP, port, matched route, method, URI. Mounted right after `gin.Recovery()` on both HTTP and HTTPS engines (`web/web.go:58,75`).

### Startup / fatal output

Startup failures use stdlib `log.Fatal` / `log.Fatalf` (`main.go:31,38,44,65,71`), sometimes with `colors.ToRed` for emphasis. This is the accepted boundary: **in-house `logs` for runtime, stdlib `log` for fatal startup errors.**

---

## What to Log

- Parsed request identity (itemInfo) at redirect entry points.
- Every redirect/proxy success with the target URL.
- Every handler failure (via `checkErr`) — with the triggering error.
- Degradations: fallback to origin, skipped proxy address, failed auxiliary progress reports.
- Background loop lifecycle: playlist maintenance, localtree sync start/finish/failure.
- Config/route initialization steps (start + done pairs, `web/route.go:20,97`).

## What NOT to Log

- **API keys / tokens** — known violation: `logs.Info("解析到的 itemInfo: %v", itemInfo)` prints the `ApiKey` field (`redirect.go:71`, `playbackinfo.go:49`, `download.go:26`). For new code, log selected fields (ItemId, Path) instead of the whole struct.
- Full request/response bodies (volume; they also may contain credentials).
- High-frequency per-chunk messages in hot proxy paths — keep the access log as the only per-request line.

---

## Extension point

`logs.RegisterLogger(logger)` (`internal/util/logs/logger.go:27-36`) adds a broadcast sink; the `Logger` interface only receives the final formatted string (no level metadata). WebSocket live logs (`internal/service/log.go`) use it with a non-blocking channel (drops lines when full — acceptable for logs, never for business data).
