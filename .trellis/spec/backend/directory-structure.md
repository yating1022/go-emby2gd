# Directory Structure

> How backend code is organized in this project.

---

## Overview

Single-binary Go application (module `github.com/AmbitiousJun/go-emby2openlist/v2`, Go 1.26, gin v1.10.0). **All business code lives under `internal/`** — there is no `pkg/` and no `api/` directory. ~105 Go files, ~10.7k lines. The frontend (`web/`) is a React Router 7 SPA embedded into the binary via `go:embed`.

The service is a reverse-proxy middleware: it sits in front of an Emby server, intercepts playback requests, rewrites Emby mount paths to OpenList paths, and 302-redirects to direct drive links (or proxies transcoded m3u8).

**Build prerequisite**: `web/embed.go` uses `//go:embed all:dist` but `web/dist` is gitignored — a clean checkout fails `go build ./...` until `./build_web.sh` (or a manual `mkdir web/dist`) has run.

---

## Directory Layout

```
go-emby2openlist/
├── main.go                  # Entry point (93 lines): pprof → parseFlag → config → localtree.Init → web.Listen
├── config-example.yml       # Config template (8 top-level sections)
├── internal/
│   ├── config/              # Config structs + loader (one file per domain)
│   ├── constant/            # Version + ALL route regexes / route constants (single file)
│   ├── model/               # Cross-package types: HttpRes[T], Response
│   ├── service/
│   │   ├── emby/            # Core business: proxy, auth, redirect, items (16 files)
│   │   ├── agentnet/        # Master side of the agent proxy network: registry, signing, endpoints, install shell
│   │   ├── openlist/        # OpenList API client
│   │   │   └── localtree/   # Local directory-tree sync feature
│   │   ├── m3u8/            # Transcoded playlist in-memory cache + proxy
│   │   ├── music/           # Fake music file generation (ID3v2)
│   │   ├── path/            # emby→openlist path conversion
│   │   ├── lib/ffmpeg/      # ffmpeg binary wrapper + auto-download
│   │   ├── service.go / log.go / model.go   # ge2o's own endpoints (secret validate, WS log sync)
│   ├── util/                # 17 small utility subpackages (all plural-named)
│   ├── e2e/                 # Test-only package: cross-process E2E for the agent proxy network
│   └── web/                 # gin server, route table, middleware
│       ├── cache/           # Response cache middleware
│       ├── webport/         # Global port variables
│       └── webproxy/        # HTTP_PROXY/HTTPS_PROXY parsing
├── agent/                   # Nested Go module: gd-agent (proxy-network node). Own go.mod; root ./... skips it
├── cmd/                     # Dev-time standalone tools (fake_mp4, fake_mp3_1) — not shipped
├── web/                     # Frontend: embed.go + src/ (React Router 7)
├── build.sh / build_web.sh  # 14-platform cross-compile / frontend build
├── Dockerfile               # 3-stage: node → golang → alpine
└── .github/workflows/       # build.yml (release binaries), docker.yml (tag images), release-agent.yml (gd-agent releases)
```

---

## Module Organization

### Routing: regex rule table, NOT gin native routes

gin registers a single catch-all `r.Any("/*vars", globalDftHandler)` (`internal/web/route.go:102`). The real routing is a package-level ordered slice of `{regex, handler}` pairs in `internal/web/route.go:19-98` (`rules [][2]any`), compiled by `initRulePatterns()` and matched linearly per request in `globalDftHandler` (`internal/web/handler.go:20-40`). All regex/route constants live in `internal/constant/constant.go:8-66`.

**To add an endpoint**: add a `Reg_*` (or `Route_*` for ge2o's own `/ge2o`-prefixed routes) constant in `internal/constant/constant.go`, add one row to the `rules` table in `internal/web/route.go`, and implement the handler in the matching service package. Order matters — first match wins.

### Config: reflection-driven loading

`internal/config/config.go` — global `config.C`, one file per config domain (`emby.go`, `openlist.go`, `cache.go`, ...). `ReadFromFile` unmarshals YAML then reflectively walks `Config` fields: nil pointers get allocated, and any field implementing `Initializer` gets its `Init()` called for validation/derived data.

**To add a config option**: add the yaml-tagged field to the domain struct (+ `Init()` if it needs validation), update `config-example.yml`. No loader changes needed.

### Dependency direction

Nominal layering: `constant`/`model` → `config` → `util` → `service` → `web`. Four known reverse dependencies exist (documented reality — avoid adding new ones):

- `internal/config/config.go:9` → `web/webport` (reads ports for internal request host)
- `internal/config/openlist.go:7` → `service/lib/ffmpeg` (auto-downloads ffmpeg in Init)
- `internal/util/https/https.go:16` → `web/webproxy` (client proxy callback)
- `internal/service/emby` (4 files) → `web/cache` (business layer reads/writes cache spaces)

Root cause: `webport`/`webproxy`/`cache` are really infrastructure/global-state packages but physically sit under `internal/web/`.

### Startup flow (`main.go:25-46`)

1. pprof listener on :60360 (unconditional — known issue)
2. `parseFlag()` — `-p`/`-ps` ports (default 8095/8094), `-dr` data root; ports go to `webport` globals, **not** config
3. `config.ReadFromFile(<dataRoot>/config.yml)`
4. `localtree.Init()` — no-op unless `openlist.local-tree-gen.enable`
5. `agentnet.Init()` — loads the agent registry from `<BasePath>/agent-network/agents.json` (fatal on corrupt file; `main.go:43`)
6. `web.Listen()` — middleware chain: `referrerPolicySetter` → `emby.ApiKeyChecker()` → `emby.DownloadStrategyChecker()` → optional `cache.*`; then the catch-all route

### Nested module + test-only package (2026-10)

- `agent/` is a **separate Go module** (module path `github.com/yating1022/go-emby2gd/agent`, zero
  third-party dependencies). The root module's `./...` patterns and `build.sh` skip it automatically;
  its checks are `cd agent && gofmt -l . && go vet ./... && go test -race ./...`. It is released
  independently via tag `agent-v*` (agent-network.md §3.8).
- `internal/e2e/` is a **test-only package** (no non-test files) holding the agent-network E2E matrix
  (real agent subprocess + real gateway server + mock upstreams). It may import production packages;
  production code must never import it.

---

## Naming Conventions

- **util subpackages are plural**: `strs`, `maps`, `jsons`, `logs`, `urls`, `trys`, `bytess`, ... (author style; even `logs/colors`). Most are single-file packages where file name = package name.
- **Recurring file names inside packages**:
  - `type.go` — pure type definitions (emby, openlist, m3u8, localtree, ffmpeg, cache)
  - `model.go` — request/response DTOs
  - `api.go` — HTTP wrappers for external systems (emby/api.go, openlist/api.go)
  - `log.go`, `service.go`, `handler.go`, `route.go`, `web.go` — web/entry concerns
- **Business packages split one feature per file** (see `internal/service/emby/`: `auth.go`, `media.go`, `redirect.go`, `items.go`, `episode.go`, `download.go`, `subtitles.go`, `playing.go`, `cors.go`, `custom_cssjs.go`)
- **Tests sit next to the code, same base name** (`media_test.go`, `walk_test.go`).

---

## Examples

Git-verified patterns for where new code lands:

| Change type | Files touched (real commits) |
|---|---|
| Small feature (config switch) | `config-example.yml` + `internal/config/<domain>.go` + `internal/service/emby/<feature>.go` (commit d5a5c11 images-original: 3 files) |
| New endpoint | `internal/constant/constant.go` + `internal/web/route.go` (one row) + handler in the matching service package |
| Big feature | new `internal/service/<domain>/<subpkg>/` (with `localtree.go` entry, `type.go`, `model.go`, tests) + `main.go` mounts `Init()` + new `internal/util/` subpackage if needed (commit 1fb2ce9 localtree) |
| Frontend page | `web/src/app/routes.ts` + `routes/<path>/index.tsx` + components in `components/` |

Well-organized reference modules: `internal/service/openlist/localtree/` (newest, has `type.go`/`model.go` split and tests) and `internal/util/https/` (chain-style `RequestHolder`, the single exit point for all outbound HTTP).

---

## Known Tech Debt (context, not endorsement)

Route table uses `[][2]any` with type assertions and O(n) matching; global mutable state is widespread (`config.C`, `webport`, `emby.validApiKeys`, m3u8 `infoMap`); `TestProxyUri` test residue in `emby/emby.go:124`; `internal/util/parallels` is misspelled (three l's); `cmd/fake_mp4` duplicates `util/mp4s` logic; `slices`/`structs` util packages currently have zero importers.
