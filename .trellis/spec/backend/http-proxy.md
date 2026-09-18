# Outbound HTTP Proxying

> Contracts for code that fetches a remote URL **on the server's behalf** and streams the
> response to a client — `internal/service/streamproxy/` today, and anything similar later.
> Load this before writing or reviewing proxy/streaming code.

---

## 1. Scope / Trigger

Load this spec when a change involves any of:

- A handler that proxies or relays bytes from a remote service to a client
- Building an outbound request whose headers come from a client request
- Streaming a response body (media, files, archives) rather than a small JSON payload
- Anything under `internal/service/streamproxy/`

The contracts below are not stylistic. Every one of them was violated at least once during
development and caught only by a test or an independent review.

## 2. Signatures

```go
// internal/service/streamproxy/streamproxy.go

// Proxy 将 rawURL 指向的媒体字节流代理给客户端
//
// written 为 true 表示响应已经开始写入, 调用方不得再做任何回退;
// written 为 false 且 err 非空表示尚未写入任何响应, 调用方可按既有策略回退。
func Proxy(w http.ResponseWriter, r *http.Request, rawURL string) (written bool, err error)

// internal/service/streamproxy/urls.go
func NormalizeURL(rawURL string) (string, error)
func MatchDomain(rawURL string) (prefix string, ok bool)
```

Outbound requests **must** go through `internal/util/https` (`RequestHolder`), never a bare
`http.Get` / `http.NewRequest`. Use `DoSingle()` (no auto-redirect) so redirect hops can be
logged and bounded.

## 3. Contracts

### 3.1 Request headers: whitelist, then force

Build the upstream header set by **whitelisting**, never by copying the client's headers and
deleting the bad ones — a blacklist silently misses any header added later.

| Group | Headers |
|---|---|
| copied from the client (only if present) | `Range`, `Accept` |
| `User-Agent` | config value > client value > unset |
| must be **stripped** | `Host`, `Connection`, `Keep-Alive`, `Transfer-Encoding`, `Trailer`, `Upgrade`, `TE`, `Authorization`, `Cookie`, prefixes `proxy-`, `x-emby-`, `x-mediabrowser-`, `if-` |
| must be **forced** | `Accept-Encoding: identity` (see §3.3) |

Config-supplied `request-header` entries are layered on top, then the strip pass runs again —
a credential must not reach upstream just because it was written into the config.

### 3.2 Header names are canonicalised — match prefixes case-insensitively

```go
// WRONG — never matches: Header.Set stores "X-Mediabrowser-Token" (lowercase b)
if strings.HasPrefix(key, "X-MediaBrowser-") { header.Del(key) }

// CORRECT
if strings.HasPrefix(strings.ToLower(key), "x-mediabrowser-") { header.Del(key) }
```

`http.Header` keys pass through `textproto.CanonicalMIMEHeaderKey`, which upper-cases only the
first letter after each hyphen. `X-MediaBrowser-Token` is stored as **`X-Mediabrowser-Token`**.

This was a real, shipped-in-development defect: an Emby token configured as
`request-header: {X-MediaBrowser-Token: ...}` was forwarded to the upstream in cleartext,
because the literal prefix could never match. **Keep the prefix list lowercase and always
`ToLower` the key before comparing.** (`header.Del("TE")` works fine — `Del` canonicalises too.)

### 3.3 `Accept-Encoding` must be set to `identity`, not merely omitted

Omitting the header is **not** enough. `net/http/transport.go`, on `DisableCompression`:

> prevents the Transport from requesting compression with an "Accept-Encoding: gzip" request
> header **when the Request contains no existing Accept-Encoding value**. If the Transport
> requests gzip on its own and gets a gzipped response, it's transparently decoded in the
> Response.Body.

So a request with no `Accept-Encoding` gets `gzip` added and the response transparently
decompressed. Two consequences:

1. A gateway that gzips a non-Range media response makes us burn CPU decompressing it — on a
   memory-constrained host this is not free.
2. **Transparent decompression strips `Content-Encoding` and `Content-Length` from the response
   headers**, so we cannot relay the total size and the player cannot show it.

Explicitly setting `Accept-Encoding: identity` disables both behaviours. It must be written
**after** the config headers so that config cannot override it back to `gzip`.

(Range requests are not auto-gzipped, but the initial non-Range request is — so this matters.)

### 3.4 Response headers: a fixed passthrough list

`Content-Type`, `Content-Length`, `Content-Range`, `Accept-Ranges`, `Last-Modified`, `ETag`,
**`Location`**.

`Location` is required: the follow-the-redirect predicate (`https.IsRedirectCode`) covers only
301/302/307/308. An upstream `300`/`303` is therefore relayed **as a status code** — and by then
`written == true`, so the caller has lost its fallback. Without `Location` the client receives a
redirect with nowhere to go.

Backfill `Accept-Ranges: bytes` when the upstream returns 206 without it — the gateway measured
during development did exactly that, and players use the header to decide whether seeking works.

### 3.5 Streaming

- Use a fixed, pooled buffer: `bytess.CommonFixedBuffer()` + `defer buf.PutBack()`.
- **Never** buffer the whole body. Memory must be independent of file size — a 32KB buffer
  serving a 62GB file. See [Response Cache Middleware](./response-cache.md) §3.2 for the
  middleware that will happily break this invariant if a byte-stream route is whitelisted.
- Bind the upstream request to the caller's context (`.Context(r.Context())`); otherwise a
  client that disconnects leaves the upstream connection pulling the whole file.
- Close `resp.Body` on every path, including errors.

### 3.6 `RequestHolder.Header()` stores the map by reference — pass it a copy

```go
// internal/util/https/request.go:82
func (r *RequestHolder) Header(header http.Header) *RequestHolder {
	r.header = header // 直接赋值, 不 clone
	return r
}
```

`execute()` then does `req.Header = header`, and `net/http` **writes into that map** on the way
out (it adds `User-Agent`, and `Accept-Encoding` when you have not set one). So handing a
long-lived or shared map to `Header()` means every concurrent request mutates the same map — a
data race, and cached headers silently growing fields nobody set.

```go
// Wrong — hands the cached map straight to the transport
https.Get(u).Header(cachedHeaders)

// Correct — copy first, then add per-request fields
header := make(http.Header, len(cachedHeaders)+2)
for k, v := range cachedHeaders {
	header.Set(k, v)
}
header.Set("Range", r)
https.Get(u).Header(header)
```

`AddHeader()` is safe — it creates the map on first use. The trap is specific to `Header()`.
`fetch_internal_test.go` in `internal/service/gdrive/` pins this with a "shared map was not
mutated" assertion; copy that pattern when caching header maps elsewhere.

## 4. Validation & Error Matrix

| Condition | Expected |
|---|---|
| URL contains raw spaces / non-ASCII | normalise before requesting; a raw URL produces a truncated request line and Go reports **no error** |
| URL cannot be parsed | return an error; do **not** silently pass the raw value through |
| upstream returns 3xx in the follow set | follow, bounded by `max-redirect-depth`; log each hop |
| upstream returns 300/303 | do not follow; relay status + `Location` |
| upstream returns a configured "link expired" code | drop the cached link and retry **once** |
| request fails before any response bytes are written | return `written=false` + error so the caller may fall back |
| failure after the first byte | return `written=true`; fallback is no longer possible |
| client disconnects mid-transfer | log `Warn` with bytes sent; release the upstream body |

## 5. Good / Base / Bad Cases

- **Good** — a strm URL with raw spaces and CJK is normalised, requested with
  `Accept-Encoding: identity`, and streamed to the client through a 32KB pooled buffer.
- **Base** — a non-matching URL never reaches the proxy at all (zero upstream requests).
- **Bad** — the raw strm URL is handed straight to `http.NewRequest`. It does not error; it
  sends `GET /redirect?path=/影视库/... 最新电影/... HTTP/1.1`, and the request target is
  truncated at the first space. Verified: the request line contains 5 fields instead of 3.

## 6. Tests Required

| Test | Assertion |
|---|---|
| URL normalisation | output has no raw space / raw non-ASCII; **idempotent**; all query params preserved after decode |
| prefix match boundary | a configured `host:7811` must **not** match `host:78111`, `host:7811.evil.com`, `user@host:7811`, or a prefix embedded in a fragment |
| excluded headers | inject every credential/hop-by-hop name via config and assert **neither name nor value** reaches an echoing upstream; check values too, because canonicalisation hides the name |
| `Accept-Encoding` | assert the upstream observes `identity` while the client sent `gzip, deflate` |
| `written` contract | spy on `WriteHeader` and assert `written` matches "response started" for every exit path |
| streaming | large body → client bytes complete, heap growth independent of size |
| client disconnect | upstream request cancelled promptly |

Use the "delete the code under test — does the test go red?" check on every new test. During
development, deleting the entire header-strip function left the suite **green**, which is how
the canonicalisation bug survived; the regression test now names the canonicalised form
explicitly.

## 7. Wrong vs Correct

### Wrong — blacklist the client's headers

```go
header := r.Header.Clone()
delete(header, "Authorization")   // forgets X-Emby-*, Proxy-Authorization, the next one added
```

### Correct — whitelist what is wanted, then force invariants

```go
header := make(http.Header, 4)
copyIfPresent(header, clientHeader, "Range")
copyIfPresent(header, clientHeader, "Accept")
stripExcludedHeaders(header)          // case-insensitive prefixes
header.Set("Accept-Encoding", "identity")   // last, so config cannot override
```

### Wrong — treat "no Accept-Encoding" as "no compression"

```go
// Transport adds gzip for us and decompresses; Content-Length disappears.
```

### Correct — state it

```go
header.Set("Accept-Encoding", "identity")
```

## 8. Integration point — wiring the proxy into a handler

The proxy is called from `internal/service/emby/redirect.go`, in the strm branch of
`Redirect2OpenlistLink`, **after** `config.C.Emby.Strm.MapPath` and **before** the existing
`getFinalRedirectLink` / 302 path.

### 8.1 `written` decides the control flow, and one side is unrecoverable

```go
written, proxyErr := streamproxy.Proxy(c.Writer, c.Request, finalPath)
if written {
    return          // ← MUST return: the response body is already flowing
}
// only reachable when nothing has been written yet
logs.Error("[直链代理] 代理失败, 回退原有 302 流程: %v", proxyErr)
// ... falls through to the pre-existing 302 path
```

- `written == true` → the status line is on the wire. Falling through would append a `Location`
  header and a second status to a response that has already streamed megabytes. **Always return.**
- `written == false` → nothing was written; the caller may safely fall back.
- The fallback is the **pre-existing 302 path**, deliberately *not* `checkErr`. Stacking two
  fallbacks makes "what did the client actually receive" depend on runtime state, which
  defeats the point of having greppable logs.

### 8.2 Anything that must happen at request time goes **before** `Proxy`, not after

`Proxy` blocks until the whole transfer finishes. A fire-and-forget call placed after it fires
only when playback has already ended.

```go
// CORRECT — the probe must reach Emby while playback is starting
go sendOpenStreamPlaybackInfoReqToOrigin(itemInfo)
written, err := streamproxy.Proxy(...)
```

`sendOpenStreamPlaybackInfoReqToOrigin` sends Emby a PlaybackInfo request with
`IsPlayback=true&AutoOpenLiveStream=true`, which makes Emby actually open and probe the remote
stream for duration/codec. `playbackinfo.go` fires the same function for `IsRemote` sources, so
this dependency is pre-existing and still applies when we are the ones streaming the bytes.

### 8.3 Config kill switch

`emby.strm.proxy.enable: false` (the default) makes the whole branch inert — no upstream
requests, no `[直链代理]` log lines. Use it as the first rollback step: it restores the previous
behaviour without touching code.

Note the switch does **not** restore the response cache for byte-stream routes; that removal is
unconditional and belongs to the cache layer (see [Response Cache Middleware](./response-cache.md)).

