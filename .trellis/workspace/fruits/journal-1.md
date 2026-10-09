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


## Session 1: agent 代理网络上线：master 落网关、E2E 全过、Release agent-v0.2.0

**Date**: 2026-10-09
**Task**: agent 代理网络上线：master 落网关、E2E 全过、Release agent-v0.2.0
**Branch**: `main`

### Summary

按 Trellis 走完规划→实现→收尾：子任务 C（agent 模块搬迁+余量 25s 修正）→ A（网关 master 侧：端点/调度/签名/agents.json 持久化/播放入口/install.sh/admin API）→ B（Web 节点页）全部交付并经独立检查；父任务 E2E 矩阵 E1–E14 全绿（-count=2、-race 零竞争；数据面不经过网关有量化证据 873B vs 262244B）。spec：新增 backend/agent-network.md + 修订 gdrive-panel/database-guidelines/directory-structure。发布：yating1022/go-emby2gd@92d4ca0 + tag agent-v0.2.0，匿名下载+sha256+version 验证通过。本仓库零提交（工作区即交付）；交接稿已删。待用户：真机冒烟（P6：安装命令 + 真实播放）。

### Main Changes

(Add details)

### Git Commits

(No commits - planning session)

### Testing

- [OK] (Add test results)

### Status

[OK] **Completed**

### Next Steps

- None - task complete
