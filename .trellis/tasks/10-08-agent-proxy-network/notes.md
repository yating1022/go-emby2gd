# notes：父任务 E2E 集成矩阵（`internal/e2e/`，全自动跨进程）

日期：2026-10-09。环境：本机（Linux amd64，Go 1.26.3，`export PATH=/usr/local/go/bin:$PATH`）。
参与方：**进程内真实网关**（真实 gin 路由 + agent 三端点 + 全部管理接口 + `/install.sh`，
处理器就是生产 handler）+ **真实 agent 二进制**（测试内 `cd agent && go build`，版本号用
`-ldflags "-X main.version=e2e-0.0.1"` 注入后 exec）+ 假面板 `/api/dl` + 假 Google
（`http.ServeContent`，真实 Range/206 语义，1 MiB 逐字节可复算的媒体）+ 假 Emby 源。

播放入口按 `redirect_gdrive_test.go` 的既有模式直调 `emby.Redirect2OpenlistLink`，
不经 gin 路由表与缓存中间件（路由表本身由 `internal/web/route_internal_test.go` 覆盖）。

## 运行方式（可复现）

```bash
export PATH=/usr/local/go/bin:$PATH
go test ./internal/e2e/ -count=2      # 验收口径：106.5s，14 项两轮全绿
go test -race ./internal/e2e/         # 54.9s，零 DATA RACE
go test ./internal/e2e/ -short        # 21.7s（2026-10-09 复核实测），跳过 E9/E10 与 E13（时间型）；
                                      # 注意 E12 的 20s 停机等待不在跳过之列，短模式仍会真跑
gofmt -l internal/e2e/ ; go vet ./internal/e2e/
```

## 结果矩阵（全部实测）

| # | 检查 | 结果 |
|---|---|---|
| E1 | enroll ×2 幂等 | 两次**同一 agent_id**；`secret`/`sign_key` 均轮换；`agents.json` 恒 1 行且落的是轮换后凭据；旧 `agent_secret` 心跳 401、新凭据 200；响应为裸对象（无 `ok`/`data`） |
| E2 | 心跳入库 | admin 列表 `online=true, enabled=true`；`version=e2e-0.0.1`（ldflags 注入值）、`last_ip=127.0.0.1`、`listen_port=<动态端口>`、`address=http://127.0.0.1:<端口>`（未配 public_base_url 时推导）、`last_seen_at` 在 2 分钟窗口内 |
| E3 | 播放入口 302 | 挂载路径 → 302 `http://127.0.0.1:<端口>/dl/<token>?e&s`；`file_id` 解码 = gdPath；**签名由测试独立重算**（HMAC-SHA256(key=agents.json 里的 sign_key, msg=`v1\n<file_id>\n<e>`) 小写 hex）逐字匹配；`e ≈ now+24h`；`Expired ≈ now+10min`；调度阶段面板 0 次、媒体 0 次 |
| E4 | 客户端直连 agent Range 0-99 | **206** + `Content-Range: bytes 0-99/1048576` + 前 100 字节与源文件逐字节一致；假 Google 侧看到透传的 `Range: bytes=0-99`，凭据错误数 0 |
| E5 | 篡改 `s` | **403** + 统一文案「链接无效或已过期」；`download-link` 计数不增（验签在取直链之前） |
| E6 | 篡改 `e`（签名不再匹配） | **403**（同上） |
| E7 | 禁用节点（回退开） | 无 `Location`；**200 + 完整 1 MiB 媒体字节**（本机代理转发）；面板调用 +1；停用状态落盘（重启后仍禁用）；不回源 |
| E8 | 重新启用 | 下个请求**立即恢复 302**（地址 = 节点地址） |
| E9 | 离线判定 + 回退关 | 真停 agent → **真实等满 16s**（`offline-seconds: 16`，本项目允许的最小合法值，必须 > 心跳 15s）判为离线 → 播放 **503** + 中文原因「无可用 agent 节点」；面板 0 次、回源 0 次 |
| E10 | agent 重启 | 复用同一份 `agent.env` 重启（不重新注册，`agents.json` 仍 1 行）→ **实测 53ms** 回归可调度（启动即发首次心跳）→ 302 → 客户端 Range 直取得 206 + 字节精确 |
| E11 | 回退关 + 无节点 | 从未注册过任何节点的实例 → **503** 中文原因；面板 0 次、回源 0 次；状态目录里连 `agents.json` 都不存在 |
| E12 | **数据面不经过网关** | (上) 一次 256 KiB 拉取期间：网关只收到**控制面**请求（`/api/agent/*`、`/ge2o/*`），`/dl/*` 计数 **0**；网关共写出 **873 字节**，同期客户端从节点直取 **262244 字节**；(下) **网关停机 20s（>1 个心跳周期）后**同文件续传 Range 1000-1099 仍 **206** + 字节精确，且无任何新的控制面请求 |
| E13 | 网关重启 | 停机 → 把真实跑出来的 `agents.json` 原样搬入新 BasePath → 冷加载 `agentnet.Init()`（生产冷启动路径）→ 同地址重新监听：重启后**注册表 1 条记录不变**、节点"记录在但不在线"（运行时字段不持久化）→ 未心跳前播放回退本机代理（无 302）→ **下一次心跳即回归（实测 14.882s，上界 = 心跳周期 15s）**，恢复 302 且可直接服务媒体；全过程 `agents.json` 字节未变（心跳不写盘） |
| E14 | 开关关闭对照 | 先证明"开 + 在线 = 302"这一前提；停 agent（节点仍在 45s 新鲜窗口内）后切配置：`enable:false` 与**整段未配置（nil）**两种形态均 **200 + 完整 1 MiB**，与"功能关闭"逐字节一致（状态码/`Location`/`Content-Type`/`Content-Length`/响应体全等），`download-link` 0 次 |

## 自动化适配（两处，均为验收口径内的等价替换）

1. **E12 的"≥5min 长拉流"** → 自动化版为「停机 20s（跨过一个完整心跳周期，期间至少一次心跳
   失败）+ 立即续传 Range → 206」，并附「网关零数据面字节」的量化证据（873 B vs 262244 B）。
   5 分钟人工长拉流仍留在用户环境验收清单（见下）。
2. **E9 的等待时长** → `offline-seconds` 被 `AgentNetwork.Init` 约束为**严格大于心跳周期 15s**，
   因此取合法下界 **16s** 真等（实测 16s 判离线），不伪造内部状态；同时该用例在 `-short`
   下跳过并在报告中标注。

## 保真度与简化（如实记录）

- **网关是真 HTTP 服务但监听器由用例自己持有**（不是 `httptest.NewServer`）：E13 需要在**同一
  地址**上重启，而 `httptest` 关闭后无法同址重开。停机走 `http.Server.Shutdown`（先关监听器与
  空闲连接，再等在途请求回到空闲）。
- **配置注入**：直接写 `config.C` / `config.BasePath`（生产由配置加载器在启动时写一次）。
  为避免与网关处理器的并发访问，改写一律发生在静默点：先停网关并等在途请求退出 → 写 → 同地址
  重启；`newHarness` 也改为「先写全局配置、再启动网关」。
- **E13 的"重启"**：不是重启真实进程，而是「停机 + 把真实产生的状态文件搬进新的 BasePath +
  冷加载 + 同址重监听」。它覆盖的是冷启动装载路径与"运行时状态不持久化"两条契约；真实进程级
  重启（`systemctl restart`）留给用户环境。
- **凭据是假的**：面板下发的 Google 凭据是固定串，假 Google 校验它；真实 Google 一跳不在本矩阵。

## 暴露的问题与处理

1. **`-count=2` 下的直链缓存串扰（测试设施坑，已修）**：gdrive 的直链缓存是**进程级全局**、
   按路径为键且条目不主动过期。首版把路径序号做成"场景级"，第二轮复用了第一轮的路径 → agent
   拿到指向**上一轮已关闭的假服务器**的死直链 → E4 得到 502
   （`connect: connection refused`）。修复：路径序号提到进程级（与
   `redirect_agent_test.go` 的 `gdPathSeq` 同一手法）。这条同时印证了 spec 里
   「gdrive 缓存未导出，跨包测试须以唯一路径规避」的备忘。
2. **用例换配置与网关在途处理器的数据竞争（测试设施坑，已修）**：首版 `-race` 报 5 处竞争
   （`agentnet.Enroll` 处理器读 `config.C` vs `newHarness` 写 `config.C`）。
   产品侧无问题（生产只在启动时写一次），但用例必须中途换配置 → 改为「优雅停机 + 等在途请求
   退出 → 写配置 → 同址重启」，并把 `newHarness` 的写配置提前到网关启动之前。
   修复后 `-race`（含 `-count=2`）**零报告**。全部改动都落在测试设施内，未触碰产品代码。
3. 除此之外**没有发现产品缺陷**：14 项验收项在两轮执行中全部一次通过，未出现协议/结构性问题，
   因此没有触发"停下上报"的条件（`./agent/**` 一行未改）。

## 未覆盖（留给用户环境验收）

1. E12 人工版：网关停机后**持续 ≥5min** 的同文件长拉流/拖拽（自动化版为 20s 等价验证）。
2. 真实 Google 一跳：真凭据 + 真实文件（假 Google 只保真 Range 语义与凭据校验）。
3. 真实播放器行为：多次 Range、断点续传、并发多流下的 `active_streams` 调度（本矩阵客户端是
   Go http client）。
4. `/install.sh` 真机安装（systemd 分支）与网页端交互（子任务 B 的页面 + 一键安装命令）。
5. 真实进程级网关重启（本矩阵是冷加载 + 同址重监听）。

## 收尾门（2026-10-09，均通过）

```bash
go build ./...            # OK
go vet ./internal/...     # OK
go test ./internal/...    # 失败包与基线一致：lib/ffmpeg、m3u8、music、openlist、util/jsons
(cd agent && go test ./...)   # 全绿（agent 侧未改动）
```

工具/资源清理：`t.Cleanup` 覆盖 agent 子进程（SIGTERM → 10s 兜底 KILL）、网关停机、假服务器
关闭、二进制临时目录删除；实测无 `gd-agent` 进程残留、无 `/tmp/gd-agent-e2e-*` 残留；端口一律
动态获取（网关 `127.0.0.1:0`、agent 端口探测空闲后使用，E13 复用同一动态端口）。
