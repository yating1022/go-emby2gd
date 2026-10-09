# implement：agent 节点优先级调度（顺序清单）

> 上下文顺序：本任务 `prd.md` → `design.md` → `.trellis/spec/backend/agent-network.md`。
> 硬性约束：每条 go 命令前 `export PATH=/usr/local/go/bin:$PATH`；**禁止 git 提交**；
> Go 只动 design 列出的落点文件；前端只动 `web/src/**`；不动 `./agent/**` 与线协议。

- [ ] P1 配置：`config/agentnetwork.go` 加 `ScheduleStrategy` 字段 + **`UnmarshalYAML` 显式清单同步**
      + `Init`（缺省 least-active / 非法值中文报错）+ 导出常量与 getter；
      `config-example.yml` 加注释行；`agentnetwork_test.go` 与 `config_example_test.go` 加断言。
      验证：`go test ./internal/config/`
- [ ] P2 节点字段与持久化：`type.go`（`Priority int` + 双向落盘字段）、`persist.go`
      （`Priority int \`json:"priority,omitempty"\``）、`enroll` 新建置 0 / **复用分支移除对 Name 的
      覆盖且不动 Priority**（改名不乱、升级不丢名）、`setProfile`（name+priority 一次落盘，仿 setEnabled）。
      验证：`go test ./internal/service/agentnet/`（含新用例：旧文件缺省→0、有值保留、re-enroll 不重置、落盘往返）
- [ ] P3 调度：`schedule.go` 加策略分派（least-active 原样；priority 取 Priority 最小→平局随机，
      忽略 ActiveStreams；抽 `minKey` 助手保持"最小→并列→随机"同形状）；`sign.go:96` 传
      `cfg.ScheduleStrategy()`；调度日志附优先级。
      验证：schedule 新用例（下推/回归/平局/禁用/忽略活跃流）+ 既有 least-active 用例原样全过
- [ ] P4 管理接口：`constant.go` 常量 + `route.go` 规则行（**在 `/agents` 之前**）+ `admin.go`
      `AdminEditAgent`（name 1–64 + priority 0–9999 校验、中文 message）+ `agentView.Priority`；
      `route_internal_test.go`、`admin_internal_test.go` 同步。
      验证：`go test ./internal/web/ ./internal/service/agentnet/`
- [ ] P5 前端：`types.ts` += priority；`agents_table.tsx` 加「优先级」列 + 「编辑」弹窗（名称+优先级）；
      `index.tsx` 加 editAgent 处理器（照 onToggle 模式）。
      验证：`./build_web.sh` 通过
- [ ] P6 全量门与文档：`gofmt -l`（改动文件）、`go vet ./internal/...`、相关包测试全绿
      （对照基线不新增失败）；spec `agent-network.md` §2.4/§3.6 更新；README 段更新。
- [ ] P7 部署与生产验证：`sudo docker build -t ge2o:master /home/debian/project/go-emby2openlist` →
      `docker compose up -d --force-recreate`；config 切 `schedule-strategy: priority`；
      实机验证：设「家人云」优先级 → 播放走它（日志含优先级）→ 停其心跳（`systemctl stop gd-agent`）→
      顺位/503 → 恢复心跳 → 回归。

## 回滚点

任何阶段出问题：`schedule-strategy` 缺省（或切回 `least-active`）即回到现状行为；
代码改动只留工作区，按文件 checkout 即可逐文件回滚（本仓库不提交）。
