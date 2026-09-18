# Response Cache Middleware

> Executable contracts for `internal/web/cache` — the gin middleware that caches whole HTTP
> responses in memory. Load this before touching the cache middleware, the route whitelist,
> or any handler that streams bytes to the client.

---

## 1. Scope / Trigger

Load this spec when a change involves any of:

- Adding/removing a route in `internal/web/route.go` that writes a response body
- `CacheableRouteMarker()` / `RequestCacher()` / `respCacheWriter`
- Any handler whose response body can be large (media, files, archives)
- Writing tests for anything that goes through the cache middleware

The cache is load-bearing: it also implements the project's "direct link cache" (see §3.3).

## 2. Signatures

```go
// internal/web/cache/cache.go
func CacheableRouteMarker() gin.HandlerFunc   // marks NON-whitelisted routes with `Expired: -1`
func RequestCacher() gin.HandlerFunc          // wraps c.Writer, buffers, stores
func Duration(d time.Duration) string         // -> absolute UnixMilli timestamp string

// internal/web/cache/holder.go
const HeaderKeyExpired  = "Expired"     // override TTL; "-1" disables caching
const HeaderKeySpace    = "Space"       // cache-space name
const HeaderKeySpaceKey = "Space-Key"   // key inside that space
const MaxCacheSize      int64 = 100 * 1024 * 1024  // total cache budget, enforced by eviction only
const MaxBufferedRespSize int64 = 32 * 1024 * 1024 // per-response buffer cap (backstop)

// internal/web/cache/space.go
func GetSpaceCache(space, spaceKey string) (RespCache, bool)
```

Registration (`internal/web/web.go`) — both are mounted **only** when `config.C.Cache.Enable`:

```go
r.Use(cache.CacheableRouteMarker())
r.Use(cache.RequestCacher())
```

## 3. Contracts

### 3.1 The whitelist is the only gate

`CacheableRouteMarker()` holds an ordered list of `*regexp.Regexp`. Each request is matched
against `c.Request.RequestURI`:

- **match** → middleware returns; `Expired` is left unset; `RequestCacher` proceeds to wrap `c.Writer`
- **no match** → `c.Header("Expired", "-1")`; `RequestCacher` returns at step 1 and never wraps

Measured membership:

| Route constant | Cached? | Rationale |
|---|---|---|
| `Reg_PlaybackInfo` | **yes** | small JSON, expensive to compute (~2-3s) |
| `Reg_VideoSubtitles` | **yes** | subtitle text, KB-MB |
| `Reg_UserItemsRandomWithLimit` | **yes** | items JSON |
| `Reg_ResourceStream` | no | media bytes |
| `Reg_ItemDownload` | no | file bytes |
| `Reg_ItemSyncDownload` | no | file bytes |

Routes that were **never** whitelisted and must stay that way: `Reg_ResourceOriginal`,
`Reg_ProxyTs`, `Reg_ProxyPlaylist`, `Reg_ProxySubtitle`, `Reg_Images`, `Reg_ResourceMaster`,
`Reg_ResourceMain`, `Reg_All` (`ProxyOrigin`).

### 3.2 Never whitelist a byte-stream route

```go
// internal/web/cache/type.go
func (rcw *respCacheWriter) Write(b []byte) (int, error) {
	if rcw.canBuffer(len(b)) {
		rcw.body.Write(b)
	}
	return rcw.ResponseWriter.Write(b)
}
```

Every byte the handler writes is **also** copied into `rcw.body`. `MaxCacheSize` (100MB) is
enforced **only** by the background eviction loop (`loopMaintainCache`, 10s ticker) — it cannot
stop a single request from buffering its whole response. `putCache` then does a second full copy:

```go
go putCache(cacheKey, c, append([]byte(nil), customWriter.body.Bytes()...), respHeader)
```

So a whitelisted 625MB stream response costs ~1.25GB. **A whitelisted byte-stream route is an
OOM, not a performance regression.**

### 3.3 The "direct link cache" is not a module

There is no direct-link cache. `Redirect2OpenlistLink` sets a TTL header and returns an
**empty-bodied 307**; the response cache is what actually stores it:

```go
c.Header(cache.HeaderKeyExpired, cache.Duration(time.Minute*10))
c.Redirect(http.StatusTemporaryRedirect, finalPath)
```

Consequence: if you remove a 302-emitting route from the whitelist, you **lose that route's
link caching**. That is an accepted trade-off for byte-stream routes (the client follows the
307 and leaves, so the proxy sees one request per playback) but must be a conscious decision
elsewhere.

### 3.4 Buffer cap (backstop)

`canBuffer(n)` returns false once `body.Len()+n > MaxBufferedRespSize`, at which point it sets
`disabled = true` and **discards the buffer reference** (`rcw.body = bytes.NewBuffer(nil)` —
not `Reset()`, which would keep the ~32MB backing array alive until the request ends).

`body` therefore has exactly two states: **complete** or **empty**. There is no "non-empty
prefix", so a truncated body can never be cached.

### 3.5 Internal headers leak on two paths (known, unfixed)

`Expired` / `Space` / `Space-Key` are internal signals and should not reach clients.
`RequestCacher` deletes them, but only on paths that reach step 6. Two paths bypass it:

1. **Step 1 early return** (`Expired == "-1"`) — every non-whitelisted route sends `Expired: -1`
   to the client. Long-standing behaviour.
2. **Cache-hit replay** — `https.CloneHeader(c.Writer, rc.header.header)` restores the cached
   header map, including `Expired` (a timestamp) and `Space-Key` (an internal cache key).

Harmless in practice (no client or CDN reads a header named `Expired`; the standard one is
`Expires`), but do not describe the cleanup as covering "all paths" — it does not.

Tracked as `.trellis/tasks/09-16-fix-cache-header-leak` (P3, not yet done). The fix is to move
the three `defer header.Del(...)` calls above the step-1 early return so every return path
registers them. Update this section when that lands.

## 4. Validation & Error Matrix

| Condition | Behaviour |
|---|---|
| `cache.enable: false` | neither middleware is mounted; `c.Writer` is never wrapped |
| route not whitelisted | step 1 early return; no wrapping, no buffering, `Expired: -1` sent to client |
| `calcCacheKey` fails | logs `Warn`, skips cache read **and** write; request proceeds normally |
| cache hit, stored status is 3xx | `c.Redirect(rc.code, rc.header.header.Get("Location"))`, then `Abort` |
| cache hit, other status | `c.Status` + `CloneHeader` + body write, then `Abort` |
| handler writes > `MaxBufferedRespSize` | `disabled = true`, buffer discarded, body streams through intact, **not cached** |
| response status is 4xx/5xx | not cached (`https.IsErrorStatus` guard) |
| `MaxCacheSize` exceeded | eviction by the background loop only — **not** a per-request limit |

## 5. Good / Base / Bad Cases

- **Good** — a `PlaybackInfo` JSON response (~200KB) is buffered, cached for 12h, replayed on
  the second request without hitting Emby.
- **Base** — `/Videos/1/stream` is not whitelisted: `RequestCacher` returns at step 1, the
  handler streams straight to the client, memory is independent of file size.
- **Bad** — `Reg_ResourceStream` added back to the whitelist: a single 625MB playback buffers
  625MB in `rcw.body`, then `putCache` copies it again. On the target deployment (<1GB RAM)
  this OOMs on the first play.

## 6. Tests Required

Assertion points for any change to the cache middleware:

| Test | Assertion |
|---|---|
| route whitelist | for each byte-stream route constant, `CacheableRouteMarker` sets `Expired: -1` (i.e. does not match) |
| no wrapping | inside the handler, `c.Writer.(*respCacheWriter)` must be **false** for byte-stream routes |
| heap independence | writing N bytes through a byte-stream route leaves `HeapAlloc` growth independent of N |
| buffer boundary | `canBuffer` at exactly `MaxBufferedRespSize` → true; `+1` → false; already-disabled → false |
| oversize not cached | whitelisted route writing `MaxBufferedRespSize+1` bytes → a second identical request re-enters the handler |
| closure safety | deleting the `disabled` early return must not change observable cache state |

Use the "delete the code under test — does the test go red?" check for every new test.
`TestRequestCacher_OversizeResponseNotCached` documents one case where this check **cannot**
pass: the `disabled` guard is redundant with `putCache`'s `respBody == nil` early return, so no
observation can distinguish them. That is recorded rather than papered over with a test seam.

> **Warning — test-environment landmine.** `putCache` calls `DefaultExpired()`, which
> dereferences `config.C` (`internal/config/config.go:33` is `var C *Config`, initialised only
> by `ReadFromFile`). In a test binary that never loads config, **any response that actually
> reaches `putCache` panics in the `go putCache(...)` goroutine and kills the test binary** —
> `config.C == nil`.
>
> Mitigations: either assign a `config.C` in the test, or make the tested response exceed
> `MaxBufferedRespSize` so `putCache` is never reached. Do not add sleeps or polling to work
> around this; the panic is in a background goroutine whose timing you cannot control.

> **Warning — gin v1.10.0 response-writer details.**
> `responseWriter.WriteString` (`gin@v1.10.0/response_writer.go:88`) calls
> `io.WriteString(w.ResponseWriter, s)` directly — it does **not** go through `Write`, so an
> embedded `gin.ResponseWriter` inherits a `WriteString` that **bypasses** any `Write` override.
> Conversely, `responseWriter` does **not** implement `io.ReaderFrom`, so `io.Copy` /
> `io.CopyBuffer` into a wrapped writer *do* reach the overridden `Write`. Both facts matter
> when reasoning about what gets buffered. Overriding `WriteString` to "restore consistency"
> would change existing `c.String(...)` caching behaviour — that is a regression, not a fix.

## 7. Wrong vs Correct

### Wrong — gate byte-stream routes by handler opt-out

```go
// A handler-level API is forgettable: the next handler added to this route OOMs.
c.Writer = wrap(c.Writer)
c.Next()
// handler: if streaming { cache.DisableResponseCache(c) }   ← one omission = OOM
```

### Correct — gate them structurally, in the whitelist

```go
var cacheablePatterns = []*regexp.Regexp{
	regexp.MustCompile(constant.Reg_PlaybackInfo),
	regexp.MustCompile(constant.Reg_VideoSubtitles),
	regexp.MustCompile(constant.Reg_UserItemsRandomWithLimit),
	// byte-stream routes deliberately absent — see response-cache.md §3.2
}
```

Removing a route from the whitelist makes "this response is never buffered" a property of the
route table, which cannot be forgotten by a future handler.

### Wrong — assume `MaxCacheSize` bounds a single response

```go
// "the cache is capped at 100MB, so we're fine"  ← false for one large response
```

### Correct — treat the per-response cap as the only single-request bound

```go
// holder.go: MaxBufferedRespSize is the per-response bound;
// MaxCacheSize only bounds the total across requests, enforced by eviction.
```
