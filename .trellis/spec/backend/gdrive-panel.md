# GD Panel Direct Link

> Contracts for `internal/service/gdrive/` — the code that turns a Drive path into a media
> response by asking the **GD 管理面板** for a download URL plus request headers.
> Load this before touching `gdrive`, or before "fixing" anything that looks like a redirect
> problem on this chain.

---

## 1. Scope / Trigger

Load this spec when a change involves any of:

- `internal/service/gdrive/`
- `gdrive.*` config keys (`api-base` / `api-token` / `mount-prefix` / `enable`)
- The `MatchMountPath` branch in `internal/service/emby/redirect.go`
- `streamproxy.ProxyGDrive`
- Anything about "the direct link stopped working" / "playback breaks after an hour"

## 2. Data flow

```
strm content                      一次替换                 面板 /api/dl
/home/googleDrive/影视库/x.mkv ─▶ /影视库/x.mkv ─▶ url + headers
                                                        │
                    客户端 ◀── 本项目流式代理 ◀── 带 headers 请求 url
```

The project server proxies the bytes. **Neither the Emby server nor the panel forwards them.**

Public surface is exactly three functions — keep it that way:

```go
func IsEnabled() bool
func MatchMountPath(strmContent string) (gdPath string, ok bool)
func FetchStream(ctx context.Context, gdPath, clientRange string) (*http.Response, error)
```

## 3. Contracts

### 3.1 The cache margin must stay under the panel's refresh-ahead window

```
panelTokenRefreshAhead = 60s   // 服务端行为: 面板提前 60 秒才开始发新令牌
linkCacheSafetyMargin  = 30s   // 本项目: 缓存提前 30 秒作废
```

`linkCacheSafetyMargin` **must be strictly less than** `panelTokenRefreshAhead`.
`cache_internal_test.go` asserts the inequality — do not "tune it up".

If the margin is ≥ 60s there is a window where this project has declared the cache dead but the
panel is still handing out **the same old token with the same `expires_at`**. Every refresh
recomputes an unusable TTL, so **every Range request re-hits the panel** for the whole window.
At a 5-minute margin that window is 240 seconds, each request paying 150–500 ms — seeking
stutters, and "~3–4 panel calls per 3-hour movie" stops being true.

Do not set it to 0 either: that means trusting the credential up to the instant it expires.

### 3.2 The token is account-level — one copy, globally

`data.headers.Authorization` is an **account-level** Google credential, valid for every file in
the shared drive. It is cached **once, globally** (`tokenSlot`), never per path; the invalidation
point is therefore singular.

The download URL is per path and long-lived, so it is cached per path in `urlCache` with no
expiry of its own — it is refreshed only when a fetch fails.

Three rules, non-negotiable:

- Never write it back to the client (the `passthroughResponseHeaders` whitelist already blocks it)
- Never log it — no code path may print the headers map, and `api-base` rejects userinfo so a
  credential can never ride along in the panel URL and surface in a request error
- Never persist it to disk

### 3.3 This chain does **not** redirect — do not "fix" it

Measured 2026-09-18 on 10 cached files (9 videos + 1 jpg, largest 9.3 GB), requesting the real
`/api/dl` URL with its `Authorization`, reading headers only:

**10/10 returned 200 with no `Location`, `Content-Type` immediately `video/x-matroska`.**
An authenticated `files.get?alt=media` on a shared drive serves bytes directly.

⚠️ **Never write "Google will 302" / "cross-host redirect drops Authorization" into a comment,
a doc, or a spec.** That is `drive.google.com/uc?export=download` behaviour (303 to
`drive.usercontent.google.com`, plus a virus-scan interstitial for large files) — a different
endpoint that this project never calls. Writing it down sends the next maintainer to fix a
problem that does not exist, which is far more expensive than the code it would have justified.

`GD_DIRECT_LINK_API.md` §6.2 makes the same claim and is **also wrong for this chain**. Its
"实测" table actually tests a synthetic host pair (`127.0.0.1` → `localhost`); the
`googleapis.com` → `googleusercontent.com` sentence is an extrapolation that was never verified
end to end.

Status-code policy is pure defence, unrelated to redirects: accept **200/206 only**, treat
everything else as a failure and fall back.

We use `DoSingle()` (no auto-redirect) because `/api/dl` is a synchronous JSON endpoint — not
because of redirects. Do **not** switch to `Do()`: `RequestHolder.execute`'s hand-written
redirect loop never closes the intermediate 3xx body, so the connection stays out of the pool
until GC.

### 3.4 Retry status codes are local to this package

```go
var panelRetryStatusCodes = []int{401, 403, 404, 410}
```

**401 must be in the list.** Google returns 401 (`Invalid Credentials`) for an expired
credential, not 403; omitting it sends the one failure that most needs self-healing straight to
the fallback branch.

Do **not** reuse `emby.strm.proxy.retry-status-codes`. That knob describes gateway link expiry;
this set is a property of the Google API and is independent of how the gateway is configured.
Reusing it creates hidden coupling where editing strm-proxy config silently changes GD behaviour.

### 3.5 Retry once, and share the refresh

`FetchStream` retries **at most once** (`attempt == 0` is the only retry condition) — no loop, no
backoff. Retries happen before anything is written, so fallback stays available.

Concurrent failures must share one refresh: `ensureTarget` runs the panel call inside a
`singleflight.Group` keyed by path. A wave of 401s from a player's parallel Range requests
therefore produces **one** panel call, not N.

The generation guard is what makes this deterministic — and it must cover **both** caches:

- the **token slot** carries the generation it was written at
- each **URL cache entry** carries the generation of the refresh that wrote it
- a retry asks for a generation newer than the one that just failed, and `cachedTarget` requires
  *both* to be newer

Checking only the token is a real bug (fixed 2026-09-18): if an unrelated concurrent request
happens to refresh the token between the failure and the retry, the token looks "new enough" and
the retry silently reuses this path's **stale direct link**, hits the same 404, and falls back to
Emby for nothing. `TestFetchStream_RetryRefreshesStaleDirectLink` pins this.

Checking both does *not* cost extra panel calls: the first refresh advances the token generation
and this path's URL generation together, so every other concurrent request for the same path
satisfies both and reuses the same result.

`putToken` advances the generation **even when `expires_at` is unusable** — otherwise the
"has anyone refreshed already?" check silently breaks and N concurrent calls reappear.

### 3.6 Ungovernable expiry means no caching

`expires_at` is parsed from a **string**, not a `time.Time` field: a malformed timestamp must not
fail the whole response. Unparseable, missing, or too-near expiry all mean *immediately expired* —
every request re-asks the panel. Never guess a lifetime for a credential of unknown provenance.

## 4. Validation & Error Matrix

| Situation | Behaviour |
|---|---|
| Panel 401/404/400/422/502 | Log the panel's Chinese `message` verbatim, then fall back. No per-code branching. |
| Panel 200 with `ok: false` | Treat the body as authoritative, not the status code |
| `expires_at` malformed | Still usable for this request; not cached |
| Google 200/206 | Proxy the bytes |
| Google 401/403/404/410 | Require a token **and** a URL newer than the failed pair, refresh once, retry; second failure → fall back |
| Anything else | Fall back to `ProxyOrigin` (Emby reads the mount) |

**Never re-invent the panel's wording.** Its `message` is purpose-written; printing it verbatim
is what makes `PATH_NOT_IN_CACHE` vs `PATH_NOT_FOUND` distinguishable during an incident.

**Do not run panel text through an ASCII character whitelist.** The old package had one, built for
Google's OAuth error pages; applied to Chinese messages it turns the whole sentence into `?` and
destroys the diagnostic value. Only credential substitution is applied (`redactSecret`).

## 5. Tests Required

- `cache_internal_test.go` — asserts the margin/refresh-ahead inequality; TTL derivation edges
- `longplay_internal_test.go` — the long-playback contract (below)
- `panel_internal_test.go` — `path` URL-encoding round-trip; every error code keeps its Chinese
  message; token redaction
- `fetch_internal_test.go` — headers and `Range` forwarded; the shared headers map is **not**
  mutated; failures carry a status code
- `redirect_gdrive_test.go` (`internal/service/emby/`) — bytes reach the client with status 200
  and **no `Location`**; the Emby origin receives no fallback request on success

### The long-playback contract

A credential lives ~1 hour; a single playback can run for hours. **Playback must not fail because
of expiry.** `longplay_internal_test.go` pins three layers:

1. Expired cache entry → the panel is called again, and the fetch succeeds
2. Google 401 while the cache still believes the credential is good → refresh, retry, succeed
3. Persistent 401 → exactly two download attempts and one refresh, then return an error

Plus: a concurrent-401 test asserting the panel call count is **1**.

### Unverified, deliberate

Whether Google cuts an **in-flight** stream that crosses the token boundary has never been
measured — it needs a real >1h playback. The spec therefore does not assert either way. If it
does cut, the player re-requests by range and layers 1–3 resume transparently: a brief stall, not
a failure. Verify once on a real deployment (see the task's `implement.md` smoke list).

## 6. Wrong vs Correct

### Wrong — cache the token per path

```go
// N copies of the same account-level credential, N invalidation points
targetCache.Store(gdPath, entry{url: u, headers: h, expireAt: exp})
```

### Correct — split by lifetime

```go
tokenSlot // one entry, account-level credential, TTL from expires_at - 30s
urlCache  // per path, long-lived, refreshed only on failure
```

### Wrong — a five-minute safety margin

```go
const linkCacheSafetyMargin = 5 * time.Minute // ≥ the panel's 60s refresh-ahead
```

### Correct — under the panel's window

```go
const panelTokenRefreshAhead = 60 * time.Second
const linkCacheSafetyMargin  = 30 * time.Second // asserted < the above
```

### Wrong — replicate the panel's error text

```go
return fmt.Errorf("文件还没有被缓存, 请稍后重试") // invented wording
```

### Correct — pass it through

```go
return fmt.Errorf("取直链失败 [%s] %s", envelope.Error.Code, redactConfigSecrets(envelope.Error.Message))
```

## 7. Integration points

- `internal/service/emby/redirect.go` — `MatchMountPath` is checked **outside and before** the
  `urls.IsHttpRemote` branch: a mount path is a local filesystem path, not an HTTP URL, so it can
  never match inside that branch. Do not move it.
- `internal/service/streamproxy/` — owns byte relay, response-header write-back, the concurrency
  slot and fallback semantics. `gdrive` must not reimplement any of it.
- The concurrency slot is acquired **before** the link is resolved, so a request that queued a
  long time never carries a credential fetched before the wait.
