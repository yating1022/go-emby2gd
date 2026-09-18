# Journal - fruits (Part 1)

> AI development session journal
> Started: 2026-09-16

---

## 2026-09-16: Bootstrap project guidelines (00-bootstrap-guidelines)

**Goal**: Before secondary development (二开), fully analyze the codebase and populate `.trellis/spec/backend/` with real conventions.

**Approach**: Dispatched 3 parallel `trellis-research` agents (architecture / error+logging / quality+state), each persisting findings to `.trellis/tasks/00-bootstrap-guidelines/research/`. Wrote the 5 spec files from their evidence; spot-checked key references against real code (all passed).

**Deliverables**:
- `research/architecture.md`, `research/error-logging.md`, `research/quality-state.md` (~300 lines each, Chinese, with file:line evidence)
- 5 filled spec files (English): directory-structure, error-handling, logging-guidelines, quality-guidelines, database-guidelines (repurposed as Configuration & State Management — project has no DB)
- Updated `spec/backend/index.md` statuses

**Key facts learned about the codebase**:
- Routing is a regex rule table (`[][2]any` in `internal/web/route.go`), NOT gin native routes; new endpoints need constant + rules-table row
- Config loads via reflection + `Initializer.Init()`; global `config.C` is read-only after startup, no hot reload
- Errors: dual channels (`error` + `model.HttpRes[T]`), unified web exit `checkErr` (redirect.go:216), no `errors.Is/As` anywhere
- Logging: in-house `logs` package, 7 level functions, Chinese messages, no structured logging
- Signature concurrency pattern: single maintainer goroutine + pre-buffered channel FIFO eviction (web/cache + m3u8)
- Clean checkout cannot `go build ./...` until frontend is built (`web/embed.go:5` needs `web/dist`)
- CI runs no tests/vet/lint — quality is convention-enforced

**Known quirks documented as reality** (not fixed, out of scope for docs task):
`logs.Error` is gray not red; `%w` wrapped but never unwrapped; itemInfo logging leaks ApiKey; 4 reverse dependencies (config→webport, config→ffmpeg, util/https→webproxy, emby→web/cache); pprof exposed unconditionally on :60360.

**Next**: user reviews specs → `task.py finish` + `task.py archive 00-bootstrap-guidelines`.
