# Quality Guidelines

> Code quality standards for backend development.

---

## Overview

Reality check: **CI runs no tests, no vet, no lint** (`.github/workflows/build.yml` only cross-compiles via `./build.sh` on release). There is no `.golangci.yml`, no Makefile, no pre-commit. Quality is enforced by convention, not tooling.

Minimum local gate before submitting changes (verified working):

```bash
./build_web.sh          # or: mkdir -p web/dist  — REQUIRED, go:embed fails otherwise
go vet ./internal/...
go build ./...
go test ./internal/util/...   # only util-layer tests run without live services
```

**Build prerequisite**: `web/embed.go` uses `//go:embed all:dist` but `web/dist` is gitignored — a clean checkout cannot compile Go until the frontend is built (or an empty `web/dist` is created).

---

## Forbidden Patterns

The project deliberately avoids these (verified across the whole codebase):

1. **No third-party test/assert/mock libraries** — no testify, no gomock, not even `httptest`. Stdlib only (`t.Errorf`, `t.Run`).
2. **No `interface{}`** — always `any` (0 occurrences of `interface{}` vs 41 of `any`).
3. **No database / ORM / embedded KV storage** — state is in-memory (see [Configuration & State Management](./database-guidelines.md)).
4. **No structured logging libraries** — no zap/logrus/slog; always the in-house `internal/util/logs` package.
5. **No gin native route tree** — routes go through the catch-all + regex rule table (`internal/web/route.go`); do not call `r.GET(...)` etc.
6. **No dependency injection framework** — package-level singletons + `init()` are the established pattern.
7. **No English comments/messages** — Chinese is the de-facto standard (91% of comments; all error and log messages).
8. **No gratuitous interfaces** — the whole codebase has only ~4 (`logs.Logger`, `cache.RespCache`, `config.Initializer`, plus localtree's writer convention). Don't add interface layers without a second consumer.

---

## Required Patterns

- **Comments**: godoc-style Chinese comment on every exported identifier (`// FuncName 中文说明`); multi-line "why" comments for non-obvious logic; numbered step comments (`// 1 xxx`) in long functions (see `util/https/request.go:135-161`).
- **Struct tags**: `yaml:"kebab-case"` for config, `json:"snake_case"` for DTOs, no space before the tag.
- **Named constants for thresholds** with a comment: `MaxCacheSize`, `MaxPlaylistNum`, `MaxRedirectDepth` (`internal/web/cache/holder.go:16-23`). Use underscore separators in numeric literals (`5_000`).
- **Modern Go**: `for range N`, builtin `min()`, `sync.OnceFunc` are in use — target Go 1.26.
- **util functions are fail-soft**: on failure return the original/zero value, not an error (`urls.Unescape` returns the input on failure, `urls.go:94-101`). Errors surface at the service layer. Only IO-ish utils return `error`.
- **util package granularity**: one plural-named micro-package per topic (1–5 exported functions, mostly single-file). Generics are rare (only `maps.Keys`, `slices.Copy`, `model.HttpRes[T]`, `openlist.Walker[T]`) — prefer concrete types.
- **All outbound HTTP goes through `internal/util/https`** (global client + chain-style `RequestHolder`), never a raw `http.Get`.

---

## Testing Requirements

Current state: tests are developer verification scripts, not CI assets. For **new code**, write tests that actually run:

- **External test package**: `package xxx_test` (all 13 existing files use this).
- **Naming**: `Test + FuncName`, or `TestType_Method` in localtree.
- **Table-driven with stdlib assertions** — the only sanctioned pattern:

```go
// internal/util/urls/urls_test.go:22-44
tests := []struct {
    name string
    args args
    want bool
}{...}
for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
        if got := urls.IsRemote(tt.args.path); got != tt.want {
            t.Errorf("IsRemote() = %v, want %v", got, tt.want)
        }
    })
}
```

- **Never** use `log.Fatal`/`log.Fatalf` in tests (kills the whole test run) — use `t.Fatal`.
- **Never** write files into the repo from tests (`test.mp4`, `cover.jpg` are existing violations); use `t.TempDir()`.
- **Never** copy production regexes/constants into tests (see `media_test.go:11`) — import them.
- Integration tests requiring live services/config are tolerated (existing ones depend on a gitignored root `config.yml`), but new unit-testable logic must be testable without them.

---

## Code Review Checklist

- [ ] `go build ./...` passes (frontend built / `web/dist` present)
- [ ] `go vet ./internal/...` clean
- [ ] New endpoints: constant in `internal/constant/constant.go` + row in `internal/web/route.go` rules table
- [ ] New config: field + yaml tag + `Init()` validation + `config-example.yml` updated
- [ ] Errors: Chinese message with context, `checkErr` in handlers, no leaked `err.Error()` to proxy-endpoint clients
- [ ] Logs: correct level function, no API keys/tokens logged
- [ ] Goroutines: no panic path inside (no recover exists there), fire-and-forget must log failures
- [ ] Comments in Chinese, godoc on exported identifiers
- [ ] No new reverse dependencies between layers (config→service, util→web, service→web — see directory-structure.md)
- [ ] Tests: table-driven, stdlib assertions, no repo file writes
- [ ] Cache: no media/file byte-stream route added to `CacheableRouteMarker`'s whitelist — the response cache buffers whole bodies in memory and will OOM (see [Response Cache Middleware](./response-cache.md))
- [ ] Proxy: outbound header handling follows [Outbound HTTP Proxying](./http-proxy.md) — whitelist not blacklist, case-insensitive prefix stripping, `Accept-Encoding: identity`, caller context bound, fixed streaming buffer
