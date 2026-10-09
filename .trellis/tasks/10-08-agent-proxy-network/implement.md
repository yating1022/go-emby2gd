# implement：master/agent 代理网络（执行计划）

> 技术依据 = 同目录 `design.md`（协议落点）；需求与验收 = `prd.md`。
> 本文件是父任务的执行总表：子任务拆分、顺序、验证命令、评审门与回滚点。
> E2E 集成矩阵由父任务持有（§4）。

## 0. 前置与总览

- Go 工具链不在 PATH：**每条 go 命令前先 `export PATH=/usr/local/go/bin:$PATH`**。
- 本工作区（go-emby2openlist）**不产生任何 git 提交**（用户规则）；唯一例外是 §5 发布执行
  在 `yating1022/go-emby2gd` 的**独立检出**里操作。
- 参考资产（只读）：`/home/debian/project/gd/agent/`、`deploy/agent-install.sh`、
  `.github/workflows/release-agent.yml`、`.trellis/spec/backend/agent-network.md`。
- 执行顺序：**A ∥ C → B → 父任务 E2E → 发布（D-e）→ 收尾（§6）**。

```bash
# 创建子任务（父 = 10-08-agent-proxy-network）
python3 ./.trellis/scripts/task.py create "网关 master 侧（agent 端点/调度/注册表/播放入口）" --slug agent-master-side --parent 10-08-agent-proxy-network
python3 ./.trellis/scripts/task.py create "agent 侧（搬迁/余量修正/发布流水线）" --slug agent-node --parent 10-08-agent-proxy-network
python3 ./.trellis/scripts/task.py create "Web 节点页（列表/启停/复制安装命令）" --slug agent-web-page --parent 10-08-agent-proxy-network
```

评审门（已过：用户 2026-10-09 批准开始实现）：按 **C → A → B 串行**执行——原计划「A ∥ C」
改为串行（单一工作区避免同树并发编辑，且 A8 依赖 C 落位的 `installshell/agent-install.sh`）；
**父任务保持 planning**，到 §4 E2E 阶段再启动。

## 1. 子任务 A：网关 master 侧

顺序清单（每步后备验证命令）：

- [ ] A1 配置段 `agent-network`：`internal/config/agentnetwork.go`（struct + Init，惯例照
      `gdrive.go`）；`config-example.yml` 新增段；`config_example_test.go` 增加断言；
      `docker-compose.yml` 卷列表追加状态目录；根 `.gitignore` 追加。
      验证：`go test ./internal/config/`
- [ ] A2 `internal/util/cryptos`：`RandomHex(nBytes)`（crypto/rand）+ `Equal(a,b)`（常数时间）+ 单测。
      验证：`go test ./internal/util/cryptos/`
- [ ] A3 `internal/service/agentnet/` 骨架：`log.go`（前缀 `[agent 网络] `，照 streamproxy/log.go 模式）、
      `registry.go`（`map[id]` + `map[machineID]id` + RWMutex）、`persist.go`
      （`<BasePath>/agent-network/agents.json`，0600、temp+rename、损坏 fail-fast、变更即存）。
      验证：单测（round-trip、原子写、损坏文件启动失败、变更触发落盘）
- [ ] A4 `sign.go` + `schedule.go`：`PickAndSign(gdPath)`（选点 → `v1\n<token>\n<e>` HMAC → URL）、
      file token base64url 编解码、地址推导（`net.JoinHostPort`）。
      验证：单测含**跨实现固定向量**（独立脚本/Python 重算 HMAC 对比）+ `e == now` 边界 + 平局随机
- [ ] A5 agent 端点：`POST /api/agent/enroll`（幂等/轮换/置空 last_seen/写盘成功才回）、
      `POST /api/agent/heartbeat`（更新规则非空才覆盖）、`GET /api/agent/download-link`
      （解码 token → gdrive.ResolveTarget → 透传/错误映射）；`constant.go` 常量 + `route.go` 注册
      （`Reg_All` 之前）。
      验证：httptest 全矩阵（无真实 agent）：幂等 ×2、密钥轮换后旧 secret 401、同文案枚举防护、
      端口/base url 校验、download-link 200/400/401/502
- [ ] A6 `gdrive.ResolveTarget` 导出（ensureTarget 内核复用）+ 复用测试（沿用 gdrive 包测试设施）。
      验证：`go test ./internal/service/gdrive/`
- [ ] A7 播放入口接入 `redirect.go`：`MatchMountPath` 分支内、`ProxyGDrive` 之前插入决策；
      回退开关语义（无节点 ≠ 内部故障）；日志不落 `s`/完整 URL。
      验证：扩展 `redirect_gdrive_test.go` 模式——有节点 302 + 签名可重算；无节点回退本机代理；
      开关关闭零变化
- [ ] A8 `/install.sh`：`installshell/agent-install.sh`（从参考 `deploy/` 搬）+ `go:embed` +
      handler（GET/HEAD 同头、HEAD 无 body、显式 Content-Length）。
      验证：httptest GET/HEAD 字节一致；curl -I 手测
- [ ] A9 admin API `/ge2o/agent-network/*`（列表脱敏 / 启停 / 删除 / install-command）+
      `model.Response` 增加可选 `data,omitempty`（或本地结构，评审定）。
      验证：httptest——**断言响应序列化结果不含 secret/sign_key 原值**；secret 校验
- [ ] A10 全量门：`gofmt -l .` 干净 + `go vet ./...` + `go test ./...` 全绿。
      回滚点：默认 `enable:false`，A 完成即处于「可部署但零行为变化」状态

## 2. 子任务 C：agent 侧

- [ ] C1 拷贝 `/home/debian/project/gd/agent` → 本仓库 `./agent/`（嵌套独立 module，
      路径 `github.com/yating1022/go-emby2gd/agent` 不变；根 module 的 `./...` 自动跳过嵌套 module）。
- [ ] C2 **余量修正**：`internal/proxy/linkcache.go` 刷新余量 5min → **25s**
      （注释引用 design.md §2.3 的余量链 25<30<60）+ 断言测试（镜像
      `internal/service/gdrive/cache_internal_test.go` 的不等式断言）。
- [ ] C3 `cd agent && gofmt -l . && go vet ./... && go test -race ./...` 全绿。
- [ ] C4 mockmaster 链路实测一次（端点路径不改，协议保真）：
      `go run ./internal/mockmaster` + `gd-agent enroll/serve` + curl Range 206。
- [ ] C5 安装脚本落位：`internal/service/agentnet/installshell/agent-install.sh`（供 A8 embed）；
      与参考稿 `deploy/agent-install.sh` diff 检查（预期近零差异）。
- [ ] C6 `.github/workflows/release-agent.yml` 落位；与安装脚本**互查资产名**
      （`gd-agent-linux-{amd64,arm64}` + `checksums.txt`）。
- [ ] C7 本地构建验证：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=0.2.0" -o /tmp/gd-agent ./` → `version` 输出正确。
      回滚点：agent 在发布前不影响任何现有部署（纯新增目录）

## 3. 子任务 B：Web 节点页（依赖 A9 的接口契约）

- [ ] B1 `web/src/app/routes.ts` 注册新路由 + `routes/agent_network/index.tsx`
      + `layout.tsx:50-66` 导航项。
- [ ] B2 节点表格（名称/ID/在线/版本/last_seen/active_streams/地址）+ 启停/删除 +
      「复制安装命令」；沿用 `settings_modal` 的 localStorage `api_secret` 与 fetch 模式。
- [ ] B3 `./build_web.sh` 构建通过；本地起网关手测页面全流程。
      回滚点：纯新增页面，不影响既有页面

## 4. 父任务：E2E 集成矩阵（A/B/C 完成后执行）

Harness 建议（可微调，验收项不可减）：Go 测试为主 —— mock Emby + mock 面板 + mock Google
（`http.ServeContent` 提供真实 Range 语义）+ **agent 二进制子进程**（测试内 `go build` 后 exec）；
可复用/扩展 `internal/service/emby/redirect_gdrive_test.go` 的既有假 Emby 设施。

| # | 验收项 | 判据 |
|---|---|---|
| E1 | enroll ×2 幂等 | 两次同 `agent_id`；`agents.json` 仍 1 行 |
| E2 | 心跳入库 | `last_ip/last_seen/version` 更新；admin 列表显示在线 |
| E3 | 播放入口 302 | 打 `/videos/.../stream`（挂载路径场景）→ 302 到 `http://<agent>/dl/<token>?e&s`；测试用独立实现重算 HMAC 校验 |
| E4 | 客户端直连 agent | Range 0-99 → 206 + `Content-Range` + 字节精确 |
| E5 | 篡改 `s` | 403（统一文案，不泄露原因） |
| E6 | 篡改 `e` | 403 |
| E7 | 禁用节点（回退开） | 播放走现本机代理路径（无 302；字节由网关转发） |
| E8 | 重新启用 | 下个请求立即回到 302 |
| E9 | 离线判定 + 回退关 | 停 agent 超 `offline-seconds` → 503 中文原因 |
| E10 | agent 重启 | 下个心跳自动回归可调度 |
| E11 | 回退关 + 无节点 | 503 |
| E12 | **数据面不经过网关** | 网关日志 `/dl` 计数 = 0；强化版：入缓存后停网关，同文件续下 ≥5min |
| E13 | 网关重启（D-d） | 重启后 agent 不掉线：心跳继续被接受、节点仍可调度（A3 落地） |
| E14 | 开关关闭对照 | `enable:false` 播放行为与现状字节级一致 |

- 证据记录：本任务目录 `notes.md`（命令 + 关键日志片段），格式沿用参考稿 notes.md。
- 通过后：进入 §5 发布。

## 5. 发布执行（D-e；用户已授权，P1 再确认一次）

- [ ] P1 向用户确认执行（含 tag 版本号，默认 `agent-v0.2.0`）。
- [ ] P2 独立检出 `yating1022/go-emby2gd`（`core.sshCommand` 指向 `~/.ssh/github_go_emby2gd`）。
- [ ] P3 同步本工作区快照（rsync 排除 `.git`/`config.yml`/`openlist-local-tree`/`lib`）→ commit → push main。
- [ ] P4 tag `agent-v0.2.0` → push tag → 观察 Actions（vet + go test -race 是发布门）。
- [ ] P5 Release 验证：匿名下载 asset + `sha256sum -c` + `./gd-agent version`。
- [ ] P6 真机冒烟（需要用户环境）：`curl <网关>/install.sh | sudo bash -s -- --master <网关> --token <tok>`
      → 网页在线 → 真实播放走 agent（A4）。
      回滚点：tag/Release 可删；仓库 main 可强推回滚（操作前记录旧 SHA）

## 6. 收尾（Phase 3）

- [ ] S1 spec 更新：新建 `.trellis/spec/backend/agent-network.md`（移植参考稿 + 本项目落点 +
      正确版「无 302」事实 + 余量链 + 状态文件例外）；更新 `gdrive-panel.md`（公共面 3→4 函数、
      redirect 集成点）、`database-guidelines.md`（首个写盘状态及理由）、`directory-structure.md`（新包）。
      另记两处子任务 A 检查遗留：① 心跳对 `last_ip`/`version` 采用「非空才覆盖」（较父 design §2.5
      措辞更保守，防清空已知信息——在 spec 里登记为既定语义）；② gdrive `resetCache` 未导出，
      跨包测试需以唯一路径规避（测试设施备忘）。
- [ ] S2 `README.md` 增补功能段 + R1 警示（注册 Token ≈ 共享盘访问权）。
- [ ] S3 `AGENT_NETWORK_HANDOFF.md` 去留：内容已全部吸收进本任务文档 → 建议删除（执行前问用户）。
- [ ] S4 任务树归档（父任务 + 子任务）。

## 附录：验证命令速查

```bash
export PATH=/usr/local/go/bin:$PATH
gofmt -l . && go vet ./... && go test ./...          # 网关侧
(cd agent && gofmt -l . && go vet ./... && go test -race ./...)   # agent 侧
./build_web.sh                                        # 前端构建（web/dist）
# 本地 E2E（示意）：go test ./internal/service/agentnet/... -run TestE2E -v
```
