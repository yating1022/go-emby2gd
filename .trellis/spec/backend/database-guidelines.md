# Configuration & State Management

> This project has **no database** — this file documents config loading and in-memory state management instead.

---

## Overview

There is no database, ORM, or embedded KV storage (no sql/bbolt/lumberjack anywhere). Almost all state is **in-memory**; the things written to disk are *derived artifacts* that can be fully rebuilt from the remote OpenList on restart:

- `openlist-local-tree/` generated media placeholder files (strm / fake mp4 / fake mp3 / NFO — `localtree/task.go:109-160`)
- the auto-downloaded ffmpeg binary (`lib/ffmpeg/auto_download.go:116`)

**One sanctioned state-file exception (2026-10-09):** `<BasePath>/agent-network/agents.json` — the
agent-network registry (id / machine_id / secret / sign_key / enabled / …). Enrollment secrets are
**not rebuildable** (regenerating them strands every registered agent), so this state must survive
restart: 0600, same-dir temp+rename atomic write, loaded at startup, corrupt file = fail-fast.
Volatile fields (`last_seen` / `active_streams`) deliberately stay in memory. Any *further* state
file still requires the same "discuss first" as before — see agent-network.md.

`config.yml` (gitignored) is read **once at startup** — there is no hot reload, no fsnotify, no file watching. After `main.go:30` completes, `config.C` is treated as read-only without locks.

---

## Configuration (`internal/config`)

### Structure conventions

- Root `Config` struct (`config.go:13-30`) is composed of **pointer sub-structs**, one per domain, one file each: `emby.go`, `openlist.go`, `cache.go`, `ssl.go`, `log.go`, `ge2o.go`, `path.go`, `video_preview.go`.
- Fields carry `yaml:"kebab-case"` tags with a one-line Chinese comment above each.
- Global singleton: `config.C` + `config.BasePath` (`config.go:33,36`). Business code reads `config.C.Emby.Host` directly — no getters, no locks.

### Loading mechanism (`ReadFromFile`, `config.go:44-78`)

`os.ReadFile` → `yaml.Unmarshal` into `C` → **reflection walk**: nil pointer fields are allocated (so sub-configs are never nil downstream), then every field implementing `Initializer` (`Init() error`, `config.go:38-41`) is called for validation and derived data.

**To add a config section**: add the field + `Init()` method — the loader needs no changes.

### `Init()` conventions (see `emby.go:66-112` for the reference implementation)

- Missing required value → Chinese error: `errors.New("emby.host 配置不能为空")`
- Zero value → fill default in place (`ImagesQuality = 70`, `Threads = 8`)
- Enum validation → package-level whitelist `validXxx map[Xxx]struct{}` (`emby.go:31-38`)
- Derived private fields built at Init time (e.g. `Strm.pathMap [][2]string`, `LocalTreeGen.virtualContainers`)

---

## In-Memory State Patterns

### Pattern 1: `sync.Map` for read-mostly shared maps

Used for: response cache (`cache/holder.go:37`), cache spaces (`cache/space.go:23`), validated API keys (`emby/auth.go:30`), registered loggers (`logs/logger.go:24`).

### Pattern 2: single maintainer goroutine + pre-buffered channel (the signature pattern)

Two isomorphic implementations — **only the maintainer goroutine writes the state map**; producers push into a pre-buffered channel; when full, the head is evicted FIFO (regardless of expiry); a ticker sweeps expired entries periodically.

- Response cache: `internal/web/cache/holder.go:37-102` — `cacheMap sync.Map`, `preCacheChan` (cap `MaxCacheNum=8092`), `loopMaintainCache` started in `init()`, 10s sweep ticker, `MaxCacheSize=100MB`.
- m3u8 playlist cache: `internal/service/m3u8/m3u8.go:22-66` — goroutine-private `infoMap` (no locks), exposed via **function variables** (`GetPlaylist`/`GetTsLink`/`GetSubtitleLink`, `m3u8.go:27-33`) that the maintainer goroutine assigns (`m3u8.go:145-176`); `MaxPlaylistNum=10` LRU by LastRead.

For new background-maintained state, copy this shape (cache/holder.go is the cleaner reference).

### Pattern 3: locks

| Need | Tool | Example |
|---|---|---|
| Read-heavy object access | `sync.RWMutex` | `cache/type.go:43` |
| Serialize an external process | package-level `sync.Mutex` | `lib/ffmpeg/ffmpeg.go:17` |
| Re-entry guard | embedded `sync.Mutex` + `TryLock()` | `localtree/synchronizer.go:57,71-74` |
| Lazy init | `sync.Once` | `emby/emby.go:24-50` |
| Priority signaling | `sync.NewCond` (background walk pauses while a main-API request runs) | `openlist/walk.go:26-45` |
| Counters | `atomic.AddInt32/Int64` | `synchronizer.go:209,317` |

### Concurrency primitives

- CPU-chunked parallelism: `parallels.SliceChunk(size)` → per-chunk goroutines + `sync.WaitGroup` + buffered result channel, closed by a separate `wg.Wait()` goroutine (`util/jsons/jsons.go:171-216`).
- `errgroup` only where error aggregation matters: `localtree/synchronizer.go:84` (with context + semaphore channel), `emby/custom_cssjs.go:110`.
- Fire-and-forget goroutines are common (`go putCache(...)`, `go sendPlayingProgress(...)`) — they must log failures and must never panic (no recover exists outside `gin.Recovery()`).

---

## Global Singletons (know them before adding more)

| Global | Location | Notes |
|---|---|---|
| `config.C` / `config.BasePath` | `config/config.go:33,36` | Read-only after startup |
| `https.client` | `util/https/https.go:25` | Built in `init()`; redirects disabled; proxy-aware |
| `webport.HTTP/HTTPS` | `web/webport/webport.go` | Set from CLI flags, not config |
| `webproxy.HttpUrl/HttpsUrl` | `web/webproxy/webproxy.go:10` | From `HTTP_PROXY`/`HTTPS_PROXY` env |
| `localtree.synchronizer` | `localtree/localtree.go:17` | Guarded against double `Init()` |
| `m3u8.GetPlaylist` etc. | `m3u8/m3u8.go:27-33` | Function variables assigned by maintainer goroutine |
| `route.rules` | `web/route.go:17` | Regex route table |

`init()` functions exist in 4 packages (webproxy, https, cache, m3u8) — the last three use them to start background maintainer goroutines. That is the accepted way to launch maintainers.

---

## Common Mistakes

1. **Assuming config can change at runtime** — it can't. Don't write to `config.C`; there is no reload mechanism.
2. **Adding disk persistence for state** — state is rebuildable by design; if you think you need a DB, reconsider (or discuss first — it would be an architectural change). The single sanctioned exception today is `agent-network/agents.json` (credentials are not rebuildable — agent-network.md); anything else still needs that discussion.
3. **Writing to shared maps from multiple goroutines** — route writes through the maintainer-goroutine pattern or use `sync.Map`; don't add ad-hoc mutexes around plain maps.
4. **Forgetting `config.C == nil` guard** in code paths that might run before `ReadFromFile` (the guard exists at `config.go:83-85`).
5. **Unbounded caches** — every cache in this project has explicit capacity/eviction (`MaxCacheNum`, `MaxPlaylistNum`, pre-buffered channel). New caches must too.
