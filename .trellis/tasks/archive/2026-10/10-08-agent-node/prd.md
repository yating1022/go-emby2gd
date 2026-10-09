# agent 侧（搬迁/余量修正/发布流水线）

> 来源：父任务 `10-08-agent-proxy-network`（用户 2026-10-09 批准开始实现）。
> 本任务第一个执行（C → A → B 串行；C 的 installshell 产物供 A8 使用）。

## Goal

把面板仓库中已验证的 Go agent（纯标准库、零第三方依赖、含完整测试）搬进本仓库成为正式模块，
按本项目实测契约修正直链缓存余量；备好安装脚本与 Release 流水线，使其可发布分发。

## Requirements

- C-R1 拷贝 `/home/debian/project/gd/agent/` → `./agent/`：嵌套独立 module，路径
  `github.com/yating1022/go-emby2gd/agent` **不变**；根 module 的 `./...` 自动跳过嵌套 module。
- C-R2 **余量修正**（唯一行为变更，父 design §2.3 为唯一依据）：`internal/proxy/linkcache.go`
  刷新余量默认 5min → **25s**；必须小于 master 侧 30s，链 = **25 < 30 < 60**（面板 refresh-ahead）。
  5min 会在 `expires_at−5min` 窗口内每次请求重拉而 master 返回同一 token（病灶同 gdrive-panel.md §3.1）。
  加断言测试（余量 >0 且 <30s）；修正任何依赖旧值的既有测试。
- C-R3 **协议面零改动**：端点路径、签名格式 `v1\n<file_id>\n<e>`、信封解析（bare + `{ok,data}`
  双兼容）、CLI、config.env 键名、Release 资产名——全部保持参考稿原样。
- C-R4 安装脚本落位 `internal/service/agentnet/installshell/agent-install.sh`
  （供子任务 A 的 `/install.sh` go:embed；与参考稿 `deploy/agent-install.sh` 近零差异）。
- C-R5 `.github/workflows/release-agent.yml` 落位；与安装脚本**互查**资产名
  （`gd-agent-linux-{amd64,arm64}` + `checksums.txt`）。
- C-R6 mockmaster 链路可跑；本地交叉构建验证。

## Acceptance Criteria

- [ ] `cd agent && gofmt -l . && go vet ./... && go test -race ./...` 全绿
- [ ] 余量断言测试存在且通过（<30s）
- [ ] mockmaster + enroll/serve + `curl -r 0-99` 得 206（或 e2e_test 全绿 + 说明替代）
- [ ] 本地构建 `-X main.version=0.2.0` → `/tmp/gd-agent version` 输出正确
- [ ] 工作流资产名与安装脚本一致

## Notes

- 禁止任何 git 提交/暂存（本仓库规则，改动只留工作区）；参考资产只读。
- 只允许改动：`./agent/**`、`internal/service/agentnet/installshell/**`、`.github/workflows/release-agent.yml`。
- 详细步骤见 `implement.md`；技术依据 = 父任务 `design.md` §2.3 / §2.8；本任务 `design.md` 为落点说明。
