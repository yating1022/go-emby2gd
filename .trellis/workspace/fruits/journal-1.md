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


## Session 2: v0.3.2 发布上线 + hub（大盘鸡缓存中心）开工

**Date**: 2026-10-10
**Task**: v0.3.2 发布上线 + hub（大盘鸡缓存中心）开工
**Branch**: `main`

### Summary

v0.3.2 全链闭环（发布/升级/真链复测 requests=2、探测 0.36s）；hub 任务树激活、双实现代理进行中；后缀区间绕缓存登记为可选增强

### Main Changes

## v0.3.2（预取流式化与让路）闭环

- 检查通过：另发现并修复 1 个字节一致性边界（非块对齐起点弃流），补 2 个回归测试；变异验证 8/8 目标用例全红（测试真实有效）。
- 发布：fork 快照 `dda7e73` → tag `agent-v0.3.2` → Actions（vet + go test -race 门）通过 → Release 三资产。（注：首次误推 tag 指向旧 commit，即刻删 tag，`--verify-tag` 挡住误发布，无污染。）
- 四节点（VIMESS/酷网云/家人云/Zouter）升级 0.3.2，服务 active。
- 真链复测（NAS vantage，E01/E02 实测）：
  - **requests=2**（头单流 + 尾一条）；首块就绪约 2.1s（含让路等待）；让路/恢复精确（客户端在途即暂停、结束即恢复）。
  - 热探测 TTFB 0.36–0.39s（与 v0.3.1 下限持平）；冷探测 2.6s（面板解析 + Google 初始化）。
  - 发现：后缀区间 `bytes=-N` 不经缓存（`parseByteRange` 明确不接受，走纯透传）。**真实播放器（iPhone/Lenna）从不发后缀形态**（实测其探测 = `0-65535`、`0-`、中段偏移开区间），影响为零；登记为可选增强。
  - 尾部块落盘正确（blocks 计数与区间算术吻合；开区间形态可命中）。
- 探测方法沉淀：用 agents.json 的 sign_key + 冻结签名格式本机铸 URL（零转写：中转文件服务 → NAS `$(curl)` 取用）——后续真链测量复用。
- 归档 6 个任务：prefetch-stream-yield、gateway-preheat、agent-readahead-cache、mixed-serve-prefix-first、agent-ipv6-support、父任务 playback-startup-boost。

## hub（大盘鸡缓存中心）开工

- 讨论收敛 + 用户全权授权（规划→开发→部署→测试，直推到「用户可手动播放测试」；无需逐段授权）。部署硬性要求：缓存目录必须在 250G 数据盘 vdb1（/home），systemd `RequiresMountsFor=/home`。
- 任务树 `10-10-cache-hub-center` 建好并激活（hub-agent-mode / hub-master-integration / hub-deploy-verify），设计冻结：`/f/<fileID>` 三态、`/warm`+`/cancel`、播放探测全集预热（头+尾+续播点）、48h TTL + LRU 上限、多 A 记录快速失败拨号、role 多实例/多 hub 预留、hub 不可达回退直连。
- 两个实现代理并行进行中（agent 侧 hub 模式；master 侧接入）。


### Git Commits

(No commits - planning session)

### Testing

- [OK] (Add test results)

### Status

[OK] **Completed**

### Next Steps

- None - task complete
