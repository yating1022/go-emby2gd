# 网关 master 侧（agent 端点/调度/注册表/播放入口）

> 来源：父任务 `10-08-agent-proxy-network`（用户 2026-10-09 批准开始实现）。
> 技术依据 = 父任务 `design.md`（全文适用）；顺序清单 = 父任务 `implement.md` §1（A1–A10）。

## Goal

让本网关成为 master：管理 agent 注册表（JSON 持久化）、提供 enroll/心跳/download-link 三个
agent 端点、调度选点与 HMAC 签名、在 GD 挂载路径播放入口把客户端 302 到 agent、提供 `/install.sh`
与 `/ge2o` 管理接口；直链由网关自己调 GD 面板换取（复用 gdrive 包内核）。

## Requirements（细节见父 design 对应节，此处只列范围与硬约束）

- A-R1 配置段 `agent-network`（§2.6）：`enable=false` 默认 / `enroll-token`(+env `AGENT_ENROLL_TOKEN`) /
  `offline-seconds=45`(校验 >15) / `url-ttl=24h` / `fallback-to-local=true`；Init 惯例照
  `gdrive.go`（错误不回显凭据值）；`config-example.yml` + `config_example_test.go`;
  `docker-compose.yml:14-20` 卷列表 + 根 `.gitignore`。
- A-R2 `internal/util/cryptos`（§2.7）：`RandomHex`（crypto/rand）+ `Equal`（subtle 常数时间）。
- A-R3 `internal/service/agentnet/` 注册表与持久化（§2.5）：内存 map + RWMutex；
  `<BasePath>/agent-network/agents.json`（0600、temp+rename、损坏 fail-fast、变更即存）；
  心跳只改内存；`last_seen`/`active_streams` 不落盘。
- A-R4 签名与调度（§2.2/§2.5）：file token = `base64url(gdPath)`；消息 `v1\n<token>\n<e>`
  （e 十进制、s 小写 hex）；选点 = 最少 `active_streams` 平局随机；地址推导
  `public_base_url` 优先，否则 `net.JoinHostPort(last_ip, listen_port)`。
- A-R5 三个 agent 端点（§2.1）+ 路由注册（`Reg_All` 之前）：enroll 幂等/轮换/置空 last_seen/
  写盘成功才回响应；heartbeat 更新规则（非空才覆盖）；download-link = 解码 token →
  `gdrive.ResolveTarget` → 错误映射（400/401/502，中文 message 透传）。
- A-R6 gdrive 新增第 4 个公共函数 `ResolveTarget(ctx, gdPath)`（§2.3）：ensureTarget 内核复用；
  headers 绝不进日志/不外泄给客户端。
- A-R7 `redirect.go` 播放入口接入（§2.4）：`MatchMountPath` 分支内、`ProxyGDrive` 之前；
  「无可用节点」与「内部故障」分级（内部故障一律 WARN + 现行为）；日志不落 `s`/完整 URL。
- A-R8 `/install.sh`（§2.8）：go:embed `installshell/agent-install.sh`（**由子任务 C 提供，
  C 完成后才开始本项**）；GET/HEAD 同头、HEAD 无 body、显式 Content-Length。
- A-R9 admin API `/ge2o/agent-network/*`（§2.9）：列表**脱敏**（测试断言响应不含 secret/sign_key
  原值）/ 启停 / 删除 / install-command；沿用 `/ge2o` 惯例（body `secret` + `model.Response` 恒 200；
  列表数据用 `model.Response` 可选 `data` 字段或本地结构）。
- A-R10 全量门：`gofmt -l .` 干净 + `go vet ./...` + `go test ./...` 全绿。

## Acceptance Criteria

- [ ] 无真实 agent 下：enroll 幂等/轮换、鉴权枚举防护（同状态同文案）、调度平局、签名固定向量
      （独立实现重算）、download-link 全分支、回退开关语义可测
- [ ] 播放入口：有节点 302（签名可重算）；无节点回退本机代理；`enable=false` 字节级现行为
- [ ] admin 列表响应序列化结果不含任何 secret/sign_key 原值
- [ ] `go build ./...` 通过、`go vet ./internal/...` 干净、**本任务相关包**测试全绿
      （对照基线不新增失败；基线既有失败：`lib/ffmpeg`、`m3u8`、`music`、`openlist`、`util/jsons`
      ——环境依赖/既有问题，**不属于本任务**，不得改动这些包；`internal/service/emby/emby.go`
      为既有未 gofmt 文件，勿动）

## Dependencies

- 与子任务 C 依赖上无先后（只靠协议），但 **A-R8 需要 C 先落位 installshell/agent-install.sh**；
  实际执行序 C → A（同一工作区串行）。
- 需求全集 = 父任务 `prd.md` R1–R4/R6/R8/R10；细节一律以父 `design.md` 为准。
