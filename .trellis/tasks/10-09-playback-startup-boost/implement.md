# implement：起播加速（父任务 = 集成与上线）

> 顺序：子任务 1 → 子任务 2 → 本清单。每个上线动作含验证与回滚点。

- [ ] I1 前置：两个子任务各自完成 implement + check（见各自任务）
- [ ] I2 发布 agent v0.3.0：打 tag `agent-v0.3.0` 走 `.github/workflows/release-agent.yml`；
      校验 release 资产（gd-agent-linux-amd64 / arm64 / checksums.txt）可匿名下载且与本地构建一致
- [ ] I3 升级节点（经 OneSSH）：**先升一台（Vimess）** → 跑 A2 探针确认收益 → 再升 酷网云 / 家人云 / zouter；
      升级后确认心跳注册 version=0.3.0（master agents.json）
- [ ] I4 网关重建：`/dpanel/compose/ge2o-master` 重建 ge2o:master（docker build + force-recreate）；
      healthz 通过；节点列表/调度正常
- [ ] I5 A2 端到端：NAS 跑探测序列（`-r 0-` → `-r <尾偏移>` → `-r <尾偏移2>` → `-r <偏移>-`），
      记录各请求 duration 与序列总时长（目标 <3s；把 2026-10-09 的 ~19s 基线一并附上对照）
- [ ] I6 A3 真机：用户手机实测两档（直接播放 / 经详情页预热）并记录起播秒数
- [ ] I7 收尾：更新 spec（agent-network.md 增补「读前缓存」契约与配置键）、归档任务

## 回滚点

- agent：v0.2.0 资产保留可直接重装（安装脚本同一路径）；`CACHE_BUDGET_MB=0` 即关闭缓存。
- 网关：上一镜像 tag 保留，同参数重建即回退；预热可独立关闭（`agent-network.preheat-enable=false`）。
