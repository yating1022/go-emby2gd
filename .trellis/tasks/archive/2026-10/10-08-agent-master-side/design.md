# design：网关 master 侧（实现落点）

> 技术设计 = 父任务 `design.md` **全文适用**（§2 协议落地 / §4 风险 / §6 陷阱）；
> 本文件只补**实现落点**，不重复协议内容；两者冲突时以父 design 为准（改协议 = 先改父 design）。

## 模块清单

| 落点 | 内容 |
|---|---|
| `internal/config/agentnetwork.go` | `AgentNetwork` 配置段 + Init（惯例照 `gdrive.go`；env 覆盖 `AGENT_ENROLL_TOKEN`） |
| `internal/util/cryptos/` | `RandomHex(nBytes)`（crypto/rand）、`Equal(a,b)`（subtle 常数时间） + 单测 |
| `internal/service/agentnet/` | `log.go`（前缀 `[agent 网络] `）/ `registry.go`（内存表+RWMutex）/ `persist.go`（agents.json 原子写）/ `schedule.go`（选点+地址推导）/ `sign.go`（token 编解码 + HMAC + PickAndSign）/ `api.go`（三个 agent 端点）/ `admin.go`（/ge2o API）/ `installshell/`（embed + handler） |
| `internal/service/gdrive/` | 新增导出 `ResolveTarget`（第 4 公共函数；ensureTarget 内核复用；gdrive-panel.md §2 收尾时更新） |
| `internal/service/emby/redirect.go` | `MatchMountPath` 分支接入（`ProxyGDrive` 之前） |
| `main.go` | 启动链路调用 `agentnet.Init()`（注册表加载；损坏文件 fail-fast 由 §2.5/父 design R6 约束） |
| `internal/constant/constant.go` + `internal/web/route.go` | `Reg_Agent*` / `Route_AgentNetwork*` 常量与规则注册（`Reg_All` 之前） |
| 配置/部署 | `config-example.yml`、`config_example_test.go`、`docker-compose.yml`、根 `.gitignore` |

## 关键内部契约（实现约束）

- `PickAndSign(gdPath)`：内部完成 开关 → 候选筛选 → 平局随机 → HMAC 签名；**ok=false 不产生副作用**。
  调用侧必须能区分三类结果：未启用 / 无可用节点 / 内部故障（父 design §2.4 语义；建议
  `(url string, err error)` + 哨兵错误 `ErrDisabled` / `ErrNoAgent`，其余错误一律按内部故障处理）。
- 常数时间比较一律 `cryptos.Equal`；密钥一律 `cryptos.RandomHex(32)`（32 字节 → 64 hex，
  agent 侧硬校验）。
- 日志：前缀 `[agent 网络] `；任何路径不得打印 secret/sign_key/Google 凭据/`s` 参数/完整签名 URL。
- 写盘 = 同目录 temp + rename；失败时 enroll 返回 500（**不得**先回 200 再写盘）。
- `/api/agent/*`、`/install.sh`、`/ge2o/agent-network/*` 不参与响应缓存（不在缓存白名单，天然满足；勿加入）。
- `model.Response` 若无 `data` 承载列表：优先加可选 `data,omitempty`；担心动共享模型则用 agentnet 本地结构。

## 测试落点

- 端点：`httptest` + 假 agent client（无真实网络）；
- 签名：**跨实现固定向量**（独立小脚本/Python hmac 重算对照）+ `e == now` 边界 + 跨 agent 密钥拒绝；
- 播放入口：沿用 `internal/service/emby/redirect_gdrive_test.go` 的假 Emby 设施扩展；
- 全量门：`export PATH=/usr/local/go/bin:$PATH && gofmt -l . && go vet ./... && go test ./...`。
