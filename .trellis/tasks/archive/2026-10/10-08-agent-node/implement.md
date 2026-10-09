# implement：agent 侧（C1–C7 顺序清单）

> 上下文顺序：本任务 `prd.md` → `design.md` → 父任务 `design.md` §2.3/§2.8。
> 硬性约束：每条 go 命令前 `export PATH=/usr/local/go/bin:$PATH`；**禁止 git 提交**；
> 参考资产只读（`/home/debian/project/gd/...`）；只改动 `./agent/**`、`installshell/**`、workflow 文件。

- [x] C1 拷贝：`cp -r /home/debian/project/gd/agent ./agent`（参考目录已无构建产物；
      保留其 `.gitignore`、`go.mod`、全部测试）——`diff -r` 逐字节一致
- [x] C2 余量修正：`agent/internal/proxy/linkcache.go` 5min → **25s**（新命名常量 `defaultMargin`，
      注释含余量链 25<30<60、病灶说明、依据引用）；新增断言测试 `TestDefaultMarginInSafeChain`
      （>0 且 <30s）；既有 `TestLinkRefetchesInsideMargin` 的 4min 用例改为 20s（旧值依赖）
- [x] C3 `cd agent && gofmt -l . && go vet ./... && go test -race ./...` 全绿（6 包全 ok）
- [x] C4 mockmaster 冒烟（带超时的短命进程，注意清理）：`go run ./internal/mockmaster`（默认
      127.0.0.1:18990）→ `gd-agent enroll --master ... --token dev-token --config /tmp/...` →
      `gd-agent serve`（短时）→ `curl -r 0-99` 得 206 + `Content-Range: bytes 0-99/1048576`（已实测通过）
- [x] C5 `cp /home/debian/project/gd/deploy/agent-install.sh internal/service/agentnet/installshell/agent-install.sh`
      （`mkdir -p` 目录）+ `diff` 检查（逐字节一致，零差异）
- [x] C6 `cp /home/debian/project/gd/.github/workflows/release-agent.yml .github/workflows/` +
      与 C5 脚本互查资产名（gd-agent-linux-{amd64,arm64} + checksums.txt，两侧一致；YAML 解析通过）
- [x] C7 本地构建：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=0.2.0" -o /tmp/gd-agent ./`
      → `/tmp/gd-agent version` 输出 `gd-agent 0.2.0`（静态链接、stripped）

## 回滚点

纯新增目录/文件（`agent/`、`installshell/`、workflow），对现有功能零影响；回滚 = 删除新增内容。

## 收口补记（2026-10-09，主会话，行为零变化）

检查通过后做的注释收口（复核：`go test -race ./...` 全绿；与参考稿 `diff -r` 的非注释差异
仅剩余量修正本体 `defaultMargin` + 断言测试）：

- **陈旧路径修正**：`release-agent.yml` 头注释 2 处（`deploy/agent-install.sh` → 本仓库
  `internal/service/agentnet/installshell/agent-install.sh`；任务编号纠正）；`agent-install.sh`
  头部 5 处（`app/api/install.py`、`design §8`→`§2.8`、`§9-8`→`§6-3`、两处 `tests/test_install_sh.py`
  → 注明本仓库未移植）。脚本 `bash -n` 与 workflow YAML 解析复核通过。
- **注释称谓重绑**：agent 模块内 41 处「父 design / 父任务 design §X」→「冻结稿 §X」
  （原编号即冻结稿编号；「冻结稿」是任务 design.md 定义的术语）——避免在本仓库解析到错误小节。
- **内容修正 3 处**：`enroll.go` 注册 Token 来源（面板 setting 表 → 本网关配置段
  `agent-network.enroll-token`）；`upstream.go` 重定向注释按「本链路实测无重定向」改写
  （引用 gdrive-panel.md §3.3，保留「不要手动补 Authorization」约定）；`linkcache` 两处余量注释
  改指本项目 design §2.3。
