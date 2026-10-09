# PRD：master/agent 代理网络（网关为 master）

> 状态：规划完成，待评审（评审通过后 `task.py start` 子任务）。
> 技术设计 = `design.md`；执行计划与 E2E 矩阵 = `implement.md`。
> 来源交接稿：仓库根 `AGENT_NETWORK_HANDOFF.md`（临时文件，内容已全部吸收，收尾时删除）。

## 1. 目标与价值

把面板仓库里已验证的 master/agent 代理网络搬进本项目：**本网关（自述名 go-emby2gd）充当 master**，
负责 agent 注册 / 心跳 / 调度 / 签名；播放入口把客户端 **302 到 agent 直连拉流**——
数据面字节不再经过网关，也不经过 Emby。价值：

1. 直链串流的带宽从网关机器卸载到 agent 节点（玩家的流量不再压网关出口）；
2. 客户端永远拿不到 Google 账号级凭据——凭据只下发到持 `agent_secret` 的受信节点（D3）。

## 2. 背景与已确认事实（证据）

### 2.1 冻结协议（沿用，不重新设计）

- enroll：`POST /api/agent/enroll`，幂等键 `machine_id`，重注册=复用行+轮换 secret/sign_key，
  并把 `last_seen` 置空防签给旧进程。
- 心跳：`POST /api/agent/heartbeat`（Bearer `agent_secret` + `X-Agent-Id`），间隔 15s，
  离线判定 45s（**必须大于心跳周期**——参考稿实测教训）。
- 客户端 URL：`http://<agent>/dl/<file_id>?e=<unix>&s=<hex>`，
  `s = HMAC-SHA256(key=sign_key, msg="v1\n<file_id>\n<e>").hex()`，默认时效 24h；
  agent 本地常数时间验签，失败统一 403（防签名预言机）。已有跨语言固定测试向量。
- download-link：`GET /api/agent/download-link?file_id=`（agent→master），返回 `{url, headers, expires_at}`。
- 调度 D9：最少活跃连接（心跳上报 `active_streams`），平局随机；无节点按开关回退。
- 凭据规则：`agent_secret`/`sign_key` 明文入库为已接受取舍（参考稿 R2）；Google 凭据绝不落日志/不写回客户端/不落盘。

### 2.2 本项目现状（已核实，带锚点）

- 播放入口：`internal/service/emby/redirect.go:101-123`（命中 `MatchMountPath` → `streamproxy.ProxyGDrive`
  本机字节代理，失败回源）；`:133-161` strm 远程代理分支（`streamproxy.Proxy`）。
  两条分支都遵守 `written bool`「首字节后不得回退」契约。
- `internal/service/gdrive/`：已是「路径换直链」的面板客户端（`panel.go` `/api/dl`；
  `type.go` 响应已含 `file.id`；`fetch.go` 只收 200/206，重试集 401/403/404/410）。
- **实测：该链路不重定向**——`gdrive-panel.md` §3.3（2026-09-18，10/10 返回 200 无 Location）；
  参考稿「跟随 302/丢 Authorization」清单对本链路不适用，设计稿不得照抄该警告。
- 令牌链余量不变式：面板 refresh-ahead 60s > 本项目 `linkCacheSafetyMargin` 30s
  （`cache_internal_test.go` 断言）；直链 URL 按路径长缓存，仅在取流失败时刷新（`gdrive-panel.md` §3.2）。
- 路由：`internal/web/route.go` 有序正则表 + `Reg_All` catch-all；自有 API 区为 `/ge2o` 前缀
  （单一 `ge2o.api-secret`，明文比较，无会话）；中间件 `ApiKeyChecker`/`DownloadStrategyChecker`
  按各自正则过滤，不会拦截新路由（`internal/service/emby/auth.go:67-77`）。
- Web 前端：React Router 7 SPA（`web/src`，`build_web.sh` → `web/dist`，`web/embed.go` 嵌入），
  路由表 `web/src/app/routes.ts`，导航 `web/src/app/routes/layout.tsx:50-66`。
- **无数据库**：状态全部内存化且「可重建」（`database-guidelines.md`）；磁盘仅放可重建产物
  （`ssl/`、`custom-js/`、`openlist-local-tree/`、`lib/ffmpeg/`）。配置无写回、无热重载。
- 无 crypto 工具（全仓库无 hmac/crypto-rand/subtle）；`randoms.RandomHex` 是 math/rand，不可用于密钥。
- 无信任代理链：gin 默认信任所有代理，`c.ClientIP()` 可被 XFF 伪造（不能作为唯一鉴权因子）。

### 2.3 可复用资产（面板仓库 /home/debian/project/gd，只读参考）

- `agent/`：4173 行、零第三方依赖、模块 `github.com/yating1022/go-emby2gd/agent`，
  7 个测试文件 + 全链路 e2e；enroll/serve/version 子命令；`internal/mockmaster` 手工联调工具。
- `deploy/agent-install.sh`：GitHub Releases 下载（sha256 校验）、systemd（`gd-agent` 用户/服务、8790）、
  chown 关键点、幂等重跑、沙箱钩子（可本地整树测试）。
- `.github/workflows/release-agent.yml`：tag `agent-v*` → vet+test → amd64/arm64 静态构建 → checksums → Release。
- `.trellis/spec/backend/agent-network.md`：协议契约 + 实测坑清单（含「expires_at 带微秒」「re-enroll 置空 last_seen」
  「安装脚本 chown」等），搬迁后并入本项目 spec。
- E2E 矩阵 12 项（`notes.md`），含「数据面不经过 master」的证明方法：
  驱动流量后检查 master 日志路径分布，`/dl` 出现次数 = 0；强化版：下载一次让直链入缓存后停掉 master，
  同一文件仍可持续下载且 master 无字节流量。
- 已发布 `agent-v0.1.0` tag 曾存在、已随误做回滚删除；发布仓库 `yating1022/go-emby2gd`（Public，
  匿名可下载 Release 资产），本机部署密钥 `~/.ssh/github_go_emby2gd`。

## 3. 需求（含全部已确认决策）

- **R1 master 端点**：`POST /api/agent/enroll`、`POST /api/agent/heartbeat`、
  `GET /api/agent/download-link`，形状与错误码沿用冻结稿 §2 的**字面形状**（agent 客户端两种信封都
  接受，选字面 = 零 agent 改动）。鉴权：enroll_token（新 `internal/util/cryptos` 常数时间比较）
  与 `Bearer agent_secret` + `X-Agent-Id`；agent 不存在与 secret 错误同状态同文案（防枚举）。
  细节 = design §2.1 / §2.7。
- **R2 调度**：候选 = `enabled` 且未离线（`offline-seconds`，默认 45，**必须 > 心跳周期 15s**）；
  取最少 `active_streams`，平局随机。无可用节点按开关回退本机直出（`fallback-to-local`，默认 true），
  关 → 503 + 中文原因。地址推导 `public_base_url` 优先，否则源 IP + 监听端口（NAT 机器必须
  `--public-url`）。细节 = design §2.5。
- **R3 播放入口决策**（范围已定）：**仅** GD 挂载路径分支（`redirect.go:101-123` 的 `MatchMountPath`）；
  strm 远程代理分支（`:133-161`）**不接入**（留扩展点，见 §5）。语义：功能开启且有可用节点 →
  302 到 `<agent_base>/dl/<token>?e=<exp>&s=<sig>`（file token = `base64url(gdPath)` 无状态令牌；
  签名消息 `v1\n<token>\n<e>` 格式不变）；无可用节点 → 回退开关控制；任何内部故障 → 记 WARN
  走现行为，绝不因新功能让播放挂掉。前置条件：`agent-network.enable` 且 `gdrive.IsEnabled()`。
  细节 = design §2.2 / §2.4。
- **R4 直链来源**：master 自己调 GD 面板换直链（复用 `internal/service/gdrive/` 内核，新增第 4 个
  公共函数 `ResolveTarget`）；download-link 只向持 `agent_secret` 的 agent 下发。
  **余量链修正**：agent 25s < 网关 30s < 面板 refresh-ahead 60s——参考稿 agent 默认 5min 必须改，
  否则出现「余量窗口内每次请求重拉」（病灶同 `gdrive-panel.md` §3.1）。细节 = design §2.3。
- **R5 agent 侧**：搬面板仓库 `agent/`（零第三方依赖，module 路径 `github.com/yating1022/go-emby2gd/agent`
  不变）；按 R4 修正缓存余量并加断言测试；mockmaster 保留；`go test -race ./...` 全绿。
- **R6 注册表持久化**（已确认）：`<BasePath>/agent-network/agents.json`（0600、原子写、
  损坏 fail-fast、变更即存）。参考稿「全部在 MySQL」等同落为「全部在状态文件」：网关重启不掉节点、
  已签发 URL（签名密钥未变）继续有效。`last_seen`/`active_streams` 不落盘，重启后至多 15s
  由首个心跳补全。enroll 幂等键 `machine_id`，重注册 = 复用 + 轮换 + 置空 `last_seen`。
- **R7 Web 端**（范围已定）：`/ge2o/web` 新增节点页（列表**脱敏** / 启停 / 删除 / 复制安装命令）+
  `/ge2o/agent-network/*` admin API（沿用现有 `/ge2o` 惯例：body `secret` + `model.Response`，恒 200；
  列表数据用 `model.Response` 可选 `data` 字段或本地结构）。作为**独立子任务**（依赖 A 的接口契约）。
  细节 = design §2.9。
- **R8 安装**：网关新增 `GET/HEAD /install.sh`（嵌入安装脚本、GET/HEAD 同头、注册在 `Reg_All` 之前）；
  安装脚本默认下载源 = `yating1022/go-emby2gd` Releases（已 Public，匿名可下载）。
- **R9 发布**（已授权，执行前再确认）：实现与 E2E 通过后，在 `yating1022/go-emby2gd` 的独立检出里
  推送快照（含 agent/ + workflow + 安装脚本）、打 `agent-v0.2.0` tag（可调）、验证 Release
  （匿名下载 + sha256 + `version`）。**本工作区不产生任何提交**。
- **R10 配置**：新增 `agent-network` 段（`enable` 默认 false / `enroll-token` + env 覆盖
  `AGENT_ENROLL_TOKEN` / `offline-seconds=45` / `url-ttl=24h` / `fallback-to-local=true`），
  Init 惯例照 `gdrive.go`（错误不回显凭据值）；`config-example.yml` + `config_example_test.go` 同步；
  `docker-compose.yml:14-20` 卷列表与根 `.gitignore` 更新。

## 4. 验收标准

- [ ] A1 单测全绿：master 侧（enroll 幂等/轮换、验签含 `e == now` 边界与跨 agent 密钥拒绝、
      调度选择、回退分支、`/dl` 决策、常数时间比较）、agent 侧 `go test -race ./...`。
- [ ] A2 本地跨进程 E2E：`implement.md` §4 矩阵 **E1–E14** 全过（mock 面板 + mock Google +
      真实 agent 二进制），含 E12「数据面不经过网关」（网关日志 `/dl` 计数 0 + 停网关续播 ≥5min）
      与 E13 网关重启不掉节点。
- [ ] A3 网关重启后 agent 不下线：重启后心跳继续被接受、节点仍可被调度（= E13）。
- [ ] A4 真机冒烟（用户真实环境）：复制安装命令一键接入 → 网页在线 → 播放流量走 agent →
      禁用/删除立即生效（`implement.md` §5 P6）。
- [ ] A5 无行为回退：`enable=false`（或未配置本段）时播放行为与现状**字节级一致**
      （E14 + 现有 `redirect_gdrive_test` 全绿）。

## 5. 明确不做（MVP 外）

- 非 GD 挂载路径（strm 远程代理分支，redirect.go:133-161）的 agent 卸载——该分支面向 vault 等
  非 GD 上游，与面板直链/agent 网络无关；后续可扩展（design §1 留扩展点）。
- 转码 / m3u8 链路；agent 自动升级（重跑安装脚本即升级）；已发 URL 的吊销清单（时效封顶 24h）；
  节点权重调度；客户端↔agent TLS（用户自行反代，D2）。
- openlist 相关任何新增依赖。

## 6. 任务树（本任务为父任务，持有任务地图）

| 任务 | 范围 | 独立验收 |
|---|---|---|
| 父任务（本任务） | 协议落点（design.md）、E2E 集成矩阵、发布执行（R9）、spec 更新与收尾 | implement.md §4 E1–E14 |
| 子任务 A `10-08-agent-master-side` | 网关 master 侧全部 Go 实现 + 部署清单 | 无真实 agent 全链路可测；`go test ./...` 全绿；开关关闭零变化 |
| 子任务 C `10-08-agent-node` | agent 搬迁 / 余量修正 / mockmaster / 安装脚本 / 发布流水线 | `go test -race ./...` 全绿；mock 链路可跑 |
| 子任务 B `10-08-agent-web-page` | Web 节点页（**依赖 A 的 admin API**） | 页面全流程可用；构建通过 |

执行顺序：**A ∥ C → B → 父任务 E2E → 发布（R9）→ 收尾**。跨任务依赖写在各子任务 artifact 内
（不靠树位置隐含）；父任务保持 planning，到 E2E 阶段再启动。
