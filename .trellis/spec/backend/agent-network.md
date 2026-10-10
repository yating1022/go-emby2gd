# Agent Proxy Network

> Contracts for the master/agent proxy network: **this gateway is the master**; lightweight Go agents
> (the nested `agent/` module, released as `gd-agent` binaries) serve the data plane.
> Load this before touching `internal/service/agentnet/`, the `agent/` module, the `/api/agent/*`
> or `/ge2o/agent-network/*` endpoints, `/install.sh`, or the `agent-network` config section.
>
> Protocol changes are **freeze-first**: edit this file, then the gateway side, the agent side and
> both fixed-vector tests — in the same change.

## 1. Scope / Trigger

Load this spec when a change involves any of:

- `internal/service/agentnet/` — registry, scheduling, signing, endpoints, install shell, admin API
- The nested Go module `agent/` (enroll/serve CLI, link cache, proxy handler), the install script
  `internal/service/agentnet/installshell/agent-install.sh`, or `.github/workflows/release-agent.yml`
- `/api/agent/enroll` / `/api/agent/heartbeat` / `/api/agent/download-link` / `GET|HEAD /install.sh`
- `/ge2o/agent-network/*` admin endpoints
- `agent-network.*` config keys or the `AGENT_ENROLL_TOKEN` env override
- The play-entry dispatch in `internal/service/emby/redirect.go` (the `MatchMountPath` branch)
- `gdrive.ResolveTarget` — the 4th public function of `internal/service/gdrive`
- Release assets `gd-agent-linux-{amd64,arm64}` / `checksums.txt`, or the `agent-v*` tag flow

## 2. Signatures

### 2.1 HTTP endpoints

Agent-facing (registered in the `rules` table before `Reg_All`):

| Endpoint | Method | Auth |
|---|---|---|
| `/api/agent/enroll` | POST | body `enroll_token`, constant-time compared |
| `/api/agent/heartbeat` | POST | `Authorization: Bearer <agent_secret>` + `X-Agent-Id` |
| `/api/agent/download-link?file_id=<token>` | GET | same as heartbeat |
| `/install.sh` | GET / HEAD | none — the script carries no secrets |

Admin (`/ge2o` conventions: POST JSON + body `secret` + `model.Response` envelope, always HTTP 200):
`/ge2o/agent-network/agents` (list), `/agents/update` `{secret,id,enabled}`, `/agents/delete` `{secret,id}`,
`/agents/edit` `{secret,id,name,priority}` (full profile update: name 1–64 runes, priority 0–9999,
Chinese `message` on rejection), `/install-command` `{secret}` → `data:{command,master_url}`.
**Route order**: `/agents/update`, `/agents/delete` and `/agents/edit` must be registered **before**
`/agents` (first match wins in the rule table).

### 2.2 Go surface

```go
// internal/service/agentnet
func Init() error                                          // main.go startup; registry load, fail-fast
func PickAndSign(gdPath string) (string, error)            // sentinels: ErrDisabled / ErrNoAgent
func Enroll(c *gin.Context)                                // + Heartbeat / DownloadLink / InstallScript
func AdminListAgents(c *gin.Context)                       // + AdminUpdateAgent / AdminDeleteAgent / AdminEditAgent / AdminInstallCommand

// internal/service/gdrive — the 4th public function
func ResolveTarget(ctx context.Context, gdPath string) (url string, headers map[string]string, expiresAt string, err error)
```

`ErrDisabled` = feature off (caller should not have called); `ErrNoAgent` = no schedulable node —
**the only result that maps to the `fallback-to-local` branch**; every other error = internal fault
(WARN + existing behavior, never fail playback).

### 2.3 Agent CLI (frozen)

```
gd-agent enroll --master <url> --token <tok> [--public-url <url>] [--port 8790] [--config /etc/gd-agent/config.env]
gd-agent serve --config /etc/gd-agent/config.env
gd-agent version
```

config.env keys: `MASTER_URL AGENT_ID AGENT_SECRET SIGN_KEY LISTEN_PORT PUBLIC_BASE_URL MAX_CONCURRENT`
(alias `AGENT_MAX_CONCURRENT`; canonical wins). All protocol constants — endpoint paths, signature
format, envelope parsing (bare object **and** `{ok,data}`), asset names — are frozen; do not "improve" them.

### 2.4 Config section + state file

```yaml
agent-network:
  enable: false            # whole-feature rollback switch; false = byte-identical prior behavior
  enroll-token: ""         # env override AGENT_ENROLL_TOKEN (non-empty wins)
  offline-seconds: 45      # MUST be > heartbeat interval (15s); validated at Init
  url-ttl: 24h             # signed client URL TTL
  fallback-to-local: true  # explicit false ≠ absent (UnmarshalYAML distinguishes; same trick as strm max-concurrent-streams)
  schedule-strategy: least-active  # least-active (default) | priority; invalid → startup error, independent of enable
```

`UnmarshalYAML` decodes through an **explicit field list** (`plainAgentNetwork`): a new key missing
from that list is silently dropped — keep it in sync (pinned by `config` tests).

State file `<BasePath>/agent-network/agents.json` — **the project's only written state file** (the
sanctioned exception in database-guidelines.md): 0600, same-dir temp+rename atomic, loaded at startup
(`agentnet.Init()`), corrupt file = **startup failure** (fail-fast — silently rebuilding would strand
every agent's rotated credentials). Written on enroll/rotate/enable/disable/delete/edit only;
`last_seen`/`active_streams` are volatile and stay in memory. `name`/`priority` are persisted admin
state: set at row creation (name = reported hostname, priority = 0) and by `/agents/edit`; a re-enroll
of an existing row **never overwrites** them (`priority` uses `omitempty` — 0 = unset = default, and
old files without the key load as 0).

## 3. Contracts

### 3.1 enroll

Request: `{enroll_token, machine_id, hostname, version, listen_port, public_base_url?}`.
Idempotency key = `machine_id` (agent falls back to hostname when `/etc/machine-id` is unreadable).
Re-enroll = reuse row + **rotate** `agent_secret`/`sign_key` + clear `last_seen` (so nothing is
scheduled to a process still holding the old keys — that process gets 401 on heartbeat, backs off,
and is replaced by systemd) + refresh reported fields — but **`name` and `priority` are admin state
and are never touched** (nodes re-run the install script on every upgrade; applying the reported
hostname there would erase custom names). Response (bare object): `{agent_id, agent_secret, sign_key,
heartbeat_interval_seconds: 15}`; secret/sign_key are 32 bytes → 64 hex. The response is written
**only after the state-file write succeeds** (disk failure → 500; never a success response for
unpersisted credentials).

### 3.2 heartbeat

Request: `{active_streams, version, uptime_seconds, listen_port, public_base_url?}` (15s interval;
`public_base_url` nullable). Response: `{ok:true, heartbeat_interval_seconds:15, enabled:<bool>}`.
A 401 (unknown agent **or** wrong secret — same text 「凭证无效」, no enumeration) makes the agent
back off exponentially without exiting; recovery = re-run the install script.

Update semantics: `last_seen`/`active_streams` every beat; **`last_ip`, `version`, `public_base_url`
and `listen_port` are overwritten only when non-empty** ("非空才覆盖"). This is deliberately
conservative vs a literal "overwrite every beat": a degraded agent report must not erase known-good
values (an empty `last_ip` would make the node unschedulable). Keep this semantic.

### 3.3 download-link

`file_id` is **this project's opaque file token**: `base64.RawURLEncoding(gdPath)` (no padding;
alphabet `[A-Za-z0-9_-]` — no `/`, `%`, `=`). The agent treats it as opaque; the gateway decodes it
back to the Drive path. No server-side file-id map, no per-file state — a gateway restart does not
invalidate any in-flight client URL.

- decode failure → 400 `AGENT_TOKEN_INVALID` (Chinese message)
- success → `{url, headers, expires_at}` (RFC3339; the panel's raw string — Go parses fractional
  seconds natively)
- panel failure → 502 `AGENT_LINK_UNAVAILABLE` + the panel's Chinese `message` verbatim (through the
  existing redaction); resolution goes through `gdrive.ResolveTarget` → the same `ensureTarget` core
  (global token slot, per-path URL cache, singleflight, generation retry — see gdrive-panel.md)
- `headers` carry the **account-level Google credential**: never logged, never returned to clients,
  never persisted
- No check that the token was issued for *this* agent — agents are trusted infrastructure (accepted risk)

### 3.4 Signed client URL (frozen format)

```
http://<agent_base>/dl/<token>?e=<unix>&s=<hex>
s = HMAC-SHA256(key = bytes.fromhex(sign_key), msg = "v1\n<token>\n<e>").hex()   # lowercase hex
```

`e` = decimal unix seconds (master: `strconv.FormatInt(t.Unix(), 10)` — the agent hashes the raw
query string, so both sides must agree on the decimal form). Default TTL `url-ttl` (24h). Agent-side
verification is constant-time (`hmac.Equal`); failures collapse into one 403 text 「链接无效或已过期」
— do not distinguish reasons (a distinguishing response is a free signature oracle). Fixed vectors
exist on both sides; when touching the format, recompute them with an **independent** implementation
(the e2e tests do — never with the function under test).

### 3.5 Cache margin chain (NEVER break)

```
agent link cache 25s  <  gateway gdrive margin 30s  <  panel refresh-ahead 60s
```

The panel only starts issuing new tokens within 60s of expiry; the gateway declares its cache dead at
`expires_at−30s`, the agent at `expires_at−25s`. If a lower level uses a **larger** margin than the
level above it (e.g. the reference agent's original 5min), the window between them refetches the same
token on every Range request — the exact pathology documented in gdrive-panel.md §3.1. Assertions:
agent `TestDefaultMarginInSafeChain` (<30s) + master-side `cache_internal_test.go`. Do not raise any
level without re-reading gdrive-panel.md §3.1 and this chain.

### 3.6 Scheduling + address derivation

Candidates = `enabled && now−last_seen ≤ offline-seconds && address derivable` — **identical for both
strategies** (disabled is permanent; a stale heartbeat drops the node from the pool, which is exactly
how the next priority takes over; the next heartbeat puts it back). Selection by
`agent-network.schedule-strategy` (math/rand tiebreak — not security-relevant):

- `least-active` (**default**; absent config = byte-identical legacy behavior): minimum
  `active_streams`, random tiebreak — load spreads across nodes;
- `priority`: minimum `priority` (0 = unset = highest), random tiebreak, **`active_streams` ignored
  entirely** — the best-priority online node takes every new playback, even when busy and other
  nodes are idle, until its heartbeat stops. Recovery is automatic (no session migration: only *new*
  playbacks are scheduled).

`priority` is per-record admin state (0–9999, set via `/agents/edit`), persisted (`priority,omitempty`;
old files → 0) and preserved across re-enroll — same as `name`. `active_streams` still comes from
heartbeats only (≤15s stale — accepted scheduling precision; no real-time channel). No automatic
eviction: offline nodes just stop being scheduled; the next heartbeat restores them.

Address = `public_base_url` (http/https only, trailing slash trimmed) else
`"http://" + net.JoinHostPort(last_ip, strconv.Itoa(listen_port))` (IPv6 gets brackets for free).

### 3.7 Play entry (the only integration point)

Inside the `MatchMountPath` branch of `internal/service/emby/redirect.go`, **before** `ProxyGDrive`;
requires `agent-network.enable && gdrive.IsEnabled()` (the direct link source is the panel).
Result grading for `PickAndSign`:

| Result | Behavior |
|---|---|
| `nil` | 302 to the signed URL (`cache.HeaderKeyExpired` 10min, same as the strm 302 branch) |
| `ErrDisabled` | existing behavior (should be unreachable — guarded by the enable check) |
| `ErrNoAgent` | `fallback-to-local` true → existing local proxy; false → 503 + Chinese reason |
| any other error | WARN log + existing behavior — **never** let the new feature break playback |

Logs must never contain `s` or the full signed URL (node name/id + token prefix at most).

### 3.8 Install + release contract

`GET/HEAD /install.sh` serves the embedded `internal/service/agentnet/installshell/agent-install.sh`.
GET and HEAD return identical headers (`Content-Type: text/x-shellscript`, explicit `Content-Length`,
`Cache-Control: no-cache`); HEAD has an empty body — `globalDftHandler` in `internal/web/handler.go`
short-circuits HEAD to an empty 200 for everything **except** `constant.Route_InstallScript`; keep
that exemption scoped to this path.

Release (`.github/workflows/release-agent.yml`): tag `agent-v*` → `go vet` + `go test -race` gate →
`CGO_ENABLED=0` linux amd64/arm64 builds → `checksums.txt` → GitHub Release. Asset names
`gd-agent-linux-{amd64,arm64}` + `checksums.txt` must match the script's `ASSET_PREFIX`/`CHECKSUMS_ASSET`
usage byte-for-byte — grep both sides when changing.

Install-script invariants: re-run = binary replace + restart, **no re-enroll** (re-enrolling rotates
keys out from under the live process) unless `--force-enroll`; after enroll, `chown gd-agent:gd-agent`
+ 0600 on the config — otherwise `serve` gets EACCES and the node is "installed but forever offline".
On a re-run, `--public-url` does **not** re-enroll: the script anchors a replacement of the
`PUBLIC_BASE_URL=` line in config.env (any failure leaves the file byte-identical) and the follow-up
restart applies it; only `--force-enroll` rotates credentials. The script's `--public-url` pre-check
refuses values that would break the node at the next heartbeat: userinfo, query/fragment and
unclosed / dangling / empty IPv6 brackets (the master rejects these with 400 — the heartbeat stalls
and the node drops out of the scheduling pool), plus, deliberately stricter than the master, any
whitespace and an unbracketed v6 literal (`url.Parse` tolerates a space inside a path and the
unbracketed literal, but the resulting client address is not usable).

### 3.9 Error shape (agent-facing)

`{"ok":false,"error":{"code":"...","message":"中文"}}` + a meaningful status code. The agent client
accepts bare-object and `{ok,data}` envelopes — never invent a third shape.

### 3.10 IPv6 client-side access (client → agent)

The client→node leg supports IPv6 with **zero protocol change**: set the node's
`PUBLIC_BASE_URL=http://[2408:xxxx::1]:8790` (install script:
`--public-url 'http://[2408:xxxx::1]:8790'`). The heartbeat reports it, the master stores it
(§3.2 non-empty overwrite), and `agentBaseURL` uses the bracketed literal verbatim as the base of the
307 Location / signed URL. Without `public_base_url` the master derives the address from `last_ip`
via `net.JoinHostPort`, which brackets a v6 literal automatically. No new field, no new config key.

Formula check (pinned by tests): `parsePublicBaseURL` accepts `http://[v6]:port` and `https://[v6]`
(bracketed, with or without port); an unclosed bracket (`http://[2001:db8::1`) is rejected. Known
boundary: an *unbracketed* v6 literal passes `url.Parse` untouched — operators must write the
brackets (the `--public-url` help text shows the form; the install script rejects the bare form and
pre-checks whitespace / userinfo / query / fragment / bracket shapes, but does not fully parse the
host).

Deployment boundaries:

- A v6 base URL only reaches clients that have IPv6; leave `--public-url` unset for v4-only
  audiences (the derived address then depends on what the master sees).
- The node listens dual-stack (`net.Listen(":8790")` → `::` with `v6only=0` on Linux); the node
  firewall still has to allow the v6 port.
- Master-side preheat is issued by the master itself: a v4-only master cannot reach a pure-v6 node,
  so preheat degrades silently (WARN) — playback is unaffected, only that warm-up path is lost.
- Node → Google egress has **no** address-family control (plain Go dual-stack dial, by decision);
  node → master is unchanged (use a v6-shaped `MASTER_URL` if that leg ever needs v6).

## 4. Validation & Error Matrix

| Situation | Status | Code / behavior |
|---|---|---|
| enroll_token wrong/missing | 401 | `ENROLL_TOKEN_INVALID` |
| enroll params invalid (port range, base-url shape, empty machine_id) | 400 | Chinese message |
| enroll state-file write fails | 500 | Chinese message (never a success first) |
| heartbeat / download-link: unknown agent or bad secret | 401 | 「凭证无效」 for both (no enumeration) |
| download-link token decode failure | 400 | `AGENT_TOKEN_INVALID` |
| download-link panel failure | 502 | `AGENT_LINK_UNAVAILABLE` + panel message |
| agent: bad/expired/tampered signature | 403 | unified 「链接无效或已过期」 |
| agent: concurrency gate full | 503 | + `Retry-After: 5` |
| agent: link fetch failure | 502 | Chinese reason passed through |
| play entry: no node + fallback disabled | 503 | Chinese reason |
| play entry: any internal fault | — | WARN + existing local behavior |

## 5. Good / Base / Bad Cases

- **Good**: the client receives only the 302; bytes flow client→agent→Google. Gateway request
  accounting shows only control-plane paths and ~1KB of its own writes (e2e E12: 4 requests / 873
  bytes vs 262244 bytes fetched directly).
- **Base**: no node online → `fallback-to-local` keeps playback working through the gateway
  (byte-identical to pre-feature behavior; e2e E7/E14).
- **Bad**: an A-node URL replayed on B / expired / tampered → 403, same text for every cause.
  Redaction is part of the contract: neither the admin list nor any log line may contain
  `secret`/`sign_key`/Google headers (e2e asserts serialization + log absence).

## 6. Tests Required (assertion points)

- `internal/service/agentnet/` — enroll idempotency (same `agent_id`, file stays 1 row, old secret
  401 / new secret 200); signature **fixed vector recomputed independently** (python) + `e == now`
  boundary + cross-agent key rejection; scheduling + `JoinHostPort` address derivation; download-link
  branches (200/400/401/502); admin list serialization must not contain secret/sign_key.
- `internal/service/emby/redirect_agent_test.go` — 302 URL signature recomputed independently (never
  call the function under test); three-way grading; log hygiene (`s`, full URL, sign_key absent).
- v6 client access — `parsePublicBaseURL` (`http://[v6]:port`, bracketed v6 without port, unclosed
  bracket rejected), `agentBaseURL` (v6 `last_ip` → `http://[v6]:port`; v6 `public_base_url` used
  verbatim), `signClientURL` / `PickAndSign` full chain with a v6 base (signature recomputed
  independently), and the install-script sandbox cases (`--public-url` on a re-run replaces exactly
  one `PUBLIC_BASE_URL` line, is idempotent, sed metacharacters / `$( )` land literally, values the
  master would 400 (userinfo / query / fragment / bad brackets) and bare v6 are refused, a run
  without `--public-url` leaves the file byte-identical and the same file (`os.SameFile`), and every
  failure path leaves config.env byte-identical).
- `internal/web/route_internal_test.go` — rules registered before `Reg_All`; HEAD exemption applies
  only to `/install.sh` (normal paths keep the empty-200 HEAD).
- `internal/e2e/` (real agent subprocess + real gateway server + mock panel/Google/Emby) — matrix
  E1–E14: idempotency, heartbeat fields, 302 + independent signature, Range 206 byte-exact, tamper 403
  with **no** download-link triggered, disable→local fallback, re-enable→302, offline after a real
  `offline-seconds` wait, agent restart self-heals ≤1 heartbeat, fallback-off 503, data-plane
  isolation (control-plane-only traffic + post-stop resume), gateway cold reload keeps nodes,
  `enable:false` byte-identical. Runs `-count=2 -race`; use unique gdPaths (the gdrive cache is
  process-global).
- `agent/` module — `go test -race ./...`: margin assertion, signature rejection matrix (incl.
  `e == now`), handler 503 gate, link-cache singleflight, two-hop header test, disconnect cancels upstream.

## 7. Wrong vs Correct

#### Wrong — a 5-minute (or any ≥ next-level) agent margin

```go
cfg.Margin = 5 * time.Minute // ≥ master 30s: every Range request in the window refetches the same token
```

#### Correct — under the level above

```go
const defaultMargin = 25 * time.Second // 25s < 30s (master) < 60s (panel), asserted in tests
```

#### Wrong — documenting that the chain redirects

```go
// "Google will 302 and Go strips Authorization cross-host" — unverified for this chain, and it misleads
```

#### Correct — measured reality, default behavior kept as inert defense

Measured 2026-09-18 (gdrive-panel.md §3.3): 10/10 authenticated `alt=media` responses were **200 with
no Location**. Keep Go's default redirect handling (never re-add Authorization by hand) but do not
write "the chain redirects" claims anywhere.

#### Wrong — persisting volatile fields / silently rebuilding the registry

```go
// write agents.json on every heartbeat; on corrupt file start with an empty registry
```

#### Correct — persist enrollment facts only, fail fast on corruption

`last_seen`/`active_streams` stay in memory (a restart costs ≤15s of "offline" display); a corrupt
`agents.json` **fails startup** with a Chinese fix hint — silently rebuilding would strand every
agent's rotated credentials.

#### Wrong — trusting raw client IP, comparing secrets with `==`, or logging them

```go
lastIP := r.Header.Get("X-Forwarded-For")   // no trusted-proxy chain in this project
if secret == want { ... }                    // timing side channel
logs.Info("enroll: %s", agentSecret)         // credential in logs
```

#### Correct — documented posture + constant time + redaction

`c.ClientIP()` is the project's actual (spoofable) view — enroll/heartbeat both require credentials,
so spoofing can only misdirect one's *own* node address; NAT nodes must pass `--public-url`.
Compare with `cryptos.Equal`; generate with `cryptos.RandomHex(32)`; credentials and signed URLs
never reach logs (logging-guidelines.md).
