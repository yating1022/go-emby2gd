# implement：网关 master 侧（A1–A10 顺序清单）

> 上下文顺序：本任务 `prd.md` → `design.md` → 父任务 `design.md`（全文）。
> 硬性约束：每条 go 命令前 `export PATH=/usr/local/go/bin:$PATH`；**禁止 git 提交**；
> 只改动本任务范围文件；协议细节一律回查父 design，不即兴发挥。

- [ ] A1 配置段 `agent-network`：`internal/config/agentnetwork.go`（struct + Init）；
      `config-example.yml` 新增段；`config_example_test.go` 增加断言；`docker-compose.yml` 卷；根 `.gitignore`。
      验证：`go test ./internal/config/`
- [ ] A2 `internal/util/cryptos`：`RandomHex` + `Equal` + 单测。验证：`go test ./internal/util/cryptos/`
- [ ] A3 `agentnet` 骨架：`log.go`、`registry.go`（map[id] + map[machineID]id + RWMutex）、
      `persist.go`（0600、temp+rename、损坏 fail-fast、变更即存）。
      验证：单测（round-trip、原子写、损坏启动失败、变更落盘、并发读写 -race）
- [ ] A4 `sign.go` + `schedule.go`：token base64url 编解码；`v1\n<token>\n<e>` HMAC（e 十进制、s 小写 hex）；
      选点（最少 active_streams 平局随机）；地址推导（`net.JoinHostPort`）；`PickAndSign` + 哨兵错误。
      验证：固定向量跨实现重算 + `e == now` 边界 + 平局随机 + 地址推导（IPv6 括号）
- [ ] A5 三个 agent 端点 + 路由常量/注册（`Reg_All` 之前）：enroll（幂等/轮换/置空 last_seen/写盘成功才回）、
      heartbeat（非空才覆盖）、download-link（解码 → ResolveTarget → 400/401/502 映射）。
      验证：httptest 全矩阵（幂等 ×2、轮换后旧 secret 401、同文案枚举防护、端口/base url 校验）
- [ ] A6 `gdrive.ResolveTarget` 导出 + 测试（沿用 gdrive 包设施；确认 headers 语义不变共享同一 map）。
      验证：`go test ./internal/service/gdrive/`
- [ ] A7 `redirect.go` 接入：`MatchMountPath` 分支内、`ProxyGDrive` 之前；三类结果分级；
      302 响应头 `cache.HeaderKeyExpired` 10min；日志不落 s/完整 URL。
      验证：扩展 `redirect_gdrive_test.go` 模式（有节点 302 且签名可重算 / 无节点回退 / 开关关闭零变化）
- [ ] A8 `/install.sh`：`installshell/` embed + handler（GET/HEAD 同头、HEAD 无 body）。
      **前置：等待子任务 C 落位 `installshell/agent-install.sh`。**
      验证：httptest GET/HEAD 一致；`curl -I` 手测
- [ ] A9 admin API `/ge2o/agent-network/*`（列表脱敏 / 启停 / 删除 / install-command）。
      验证：httptest——断言响应序列化不含 secret/sign_key 原值；secret 校验
- [ ] A10 全量门（基线对比式）：`go build ./...` 通过；`go vet ./internal/...` 干净；
      `gofmt -l` 对本任务改动文件干净；本任务相关包 `go test` 全绿
      （config / cryptos / agentnet / gdrive / emby / streamproxy / web/cache）。
      **不得新增失败**——基线既有失败包（lib/ffmpeg、m3u8、music、openlist、util/jsons）与既有未格式化文件
      （`internal/service/emby/emby.go`）一律不碰。httptest 是允许的（仓库大量使用，见
      `internal/service/gdrive/helper_internal_test.go`）。

## 回滚点

默认 `enable:false`：A 完成即「可部署但零行为变化」；回滚 = 改配置重启 / 按需去掉接入点。
