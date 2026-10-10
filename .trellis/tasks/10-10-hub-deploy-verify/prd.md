# hub 部署与真链验证

> 父任务：`10-10-cache-hub-center`。设计见 `design.md`，执行见 `implement.md`。
> 依赖：`10-10-hub-agent-mode` 与 `10-10-hub-master-integration` 完成且通过检查。
> 前置：v0.3.2 已全量上线（四节点）。

## Goal

发布 agent v0.4.0（含 hub 模式）；在 VPS.Twon 部署 hub（ROLE=hub、白名单、systemd）；master 开启 hub 接入；按父任务 A1–A7 做真链验证与故障演练，数据落档。

## Requirements

- **N1 发布**：agent v0.4.0（node 模式零回归是发版门槛）；hub 与 node 同二进制。
- **N2 Twon 部署**：安装目录/二进制、config.env（ROLE=hub、DISK_CACHE_DIR=/home/ge2o-hub-cache、DISK_BUDGET_GB=200、CACHE_MAX_AGE_MINUTES=2880、HUB_ALLOW_IPS=master+四节点）、systemd 服务、端口（8791）与白名单生效验证。
- **N3 master 配置**：`hub-enable=true`、`hub-port=8791`；compose rebuild + force-recreate（既有流程）；hub 在注册表显示 role=hub 且不参与客户端调度。
- **N4 真链验证矩阵**（家庭 vantage：NAS + 手机）：
  - 浏览→播放：探测 TTFB（毫秒级）、起播耗时（目标 ≤5s 再降）；
  - 秒点播放（浏览后 ≤2s 点播）；
  - 命中验证：hub 日志 full/partial 命中、边缘不触 Google；
  - 3 分钟规则：浏览不播放→hub 停在头段（流量不涨）；
  - 断网命中：hub→Google 不通时已缓存文件仍可播；
  - **故障演练**：hub 停机→播放自动回退直连（无感）；
  - 观察项：48h TTL、LRU 上限、Twon 月流量。
- **N5 数据落档**：对照 v0.3.1/v0.3.2 基线的数字（起播、TTFB、请求数）；日志摘录（脱敏）。

## Acceptance Criteria

- [ ] **A1** 发布 v0.4.0 且四节点按既定计划升级（v0.3.2 或 v0.4.0，二者取当时最新）；节点零回归。
- [ ] **A2** hub 部署就绪：`/f/` 与 `/warm` 白名单生效（未授权 IP 拒绝，实测）；systemd 开机自启。
- [ ] **A3** 真链矩阵全过：浏览→播放 TTFB 毫秒级、秒点起播达标、3 分钟规则、断网命中、故障演练回退无感。
- [ ] **A4** 父任务 A1–A7 逐条有实测数据/日志支撑（含多 hub 预留为代码+测试层证据）。
- [ ] **A5** 回滚步骤已演练/确认：关 `hub-enable` → 行为回到现状。

## Out of Scope

- 多 hub 实际部署；hub 高可用；第二台大盘鸡。

## Notes

- hub 控制口/拉流口的公网暴露面只限白名单；部署时用 `curl` 从非白名单机器验证拒绝。
