# implement：起播加速（父任务 = 集成与上线）

> 状态（2026-10-10）：**I1–I6 全部完成，直通部署。** 发布物 = **agent v0.3.1**
> （读前缓存 + 网关预热 + 混合前缀先行 + IPv6 客户端接入）。
> 实测：NAS 探针序列 TTFB 全部压到 RTT 地板（0.36–0.39s @ 180ms 线路，对照 v0.3.0 的 1.6–5.9s）；
> Zouter 配置 v6 对外地址（其 v4 从家庭网络不通；v6 直连实测 12MB/s）；四台节点 0.3.1；master 重建完成。

- [x] I1 前置：子任务各自完成 implement + check
      （agent-readahead-cache / gateway-preheat / mixed-serve-prefix-first / agent-ipv6-support）
- [x] I2 发布 agent v0.3.1：tag → Actions（vet + race 为发布门）→ 匿名验证（sha256 OK、`gd-agent 0.3.1`）
- [x] I3 升级节点（经 OneSSH）：VIMESS → 酷网云/家人云/Zouter，全部 0.3.1、零重新注册
- [x] I4 网关重建：ge2o:master 重建 + force-recreate（预热 + 新版安装脚本上线）
- [x] I5 A2 端到端（NAS）：预热 → 探测序列 TTFB：0.359 / 0.375 / 0.388 / 0.381 / 0.366s
      （对照今晨 v0.3.0：3.27 / 0.35 / 0.37 / 2.41 / —）
- [x] I6 A3 真机：v2 达 TTFB 地板 ✓；IPv6：Zouter 设 `PUBLIC_BASE_URL=http://[2a0e:97c0:3f0:1::1b01]:8790`
      → 心跳生效 → master 组装 Address 为 v6 ✓ → v6 直连实测 12MB/s ✓
- [ ] I7 收尾：spec 已更新（agent-network.md §3.8/§3.10/§6）；任务归档待办

## 回滚点

- agent：v0.3.0 资产保留可重装；`CACHE_BUDGET_MB=0` 关闭缓存。
- Zouter v6 地址回退：重跑安装脚本带 `--public-url 'http://155.117.82.69:8790'`（非空覆盖语义）。
- 网关：上一镜像 tag 保留，同参数重建即回退；预热可独立关闭。
