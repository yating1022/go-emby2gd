# design：hub 部署与真链验证

## 1. 部署拓扑

```
客户端 → 边缘节点(node, v0.4.0) → VPS.Twon(hub:8791, 明文/白名单) → Google
                     ↑                        ↑
                 master(ge2o-master, hub-enable=true) ── /warm /cancel（仅 master IP）
```

- **（用户指定，硬性）缓存目录必须在 250G 数据盘上**：`/dev/vdb1` → 挂载点 `/home`（xfs，249G 空闲）；部署时以 `findmnt /home` + `df -h /home` 验证；systemd 单元加 `RequiresMountsFor=/home`（挂载缺失则服务不启，**绝不让 20G 根盘承接缓存**）；确认 fstab 开机自动挂载（重启演练内验证）。
- Twon：`/opt/ge2o-agent`（同现有节点安装布局）；`config.env` 关键项：
  `ROLE=hub`、`HUB_PORT=8791`、`HUB_ALLOW_IPS=<master_ip>,<v1..v4>,<twon 自身>`、
  `DISK_CACHE_DIR=/home/ge2o-hub-cache`、`DISK_BUDGET_GB=200`、`CACHE_MAX_AGE_MINUTES=2880`；
  systemd 单元与节点同款（开自启/重启）。
- master：`config.yml` → `agent-network.hub-enable: true`、`hub-port: 8791`；compose rebuild + force-recreate。
- 白名单验证：从 NAS（非白名单）curl 8791 → 拒绝；从 master/节点 → 通。

## 2. 验证矩阵（判据）

| # | 场景 | 方法 | 判据 |
|---|---|---|---|
| 1 | 浏览→播放 | 预览后 10–30s 点播（NAS/手机） | 探测 TTFB 毫秒级；hub 日志 full/partial 命中；节点侧无 Google 请求 |
| 2 | 秒点播放 | 浏览后 ≤2s 点播 | 起播 ≤5s（对照 v0.3.2 基线再降） |
| 3 | 3 分钟规则 | 只浏览不播放 | hub 停在头段，流量增量 ≈ 头+尾+续播区 |
| 4 | 冷播放 | 未预热直接播放 | 走 hub 透传+落盘；起播不低于现状 |
| 5 | 断网命中 | hub 断 googleapis（iptables 临时）后播放已缓存文件 | 正常播放（命中不出网） |
| 6 | 故障演练 | `systemctl stop` hub 后播放 | master 回退 Google 直链，播放无感；恢复后 hub 恢复参与 |
| 7 | TTL/LRU | 观察项：48h 过期重拉日志；写入压满预算后 LRU 淘汰日志 | 盘余量稳定在预算内 |
| 8 | 流量 | Twon 流量统计（观测） | 月内 3T 内 |

## 3. 发布与升级

- agent v0.4.0：sync fork → tag `agent-v0.4.0` → Actions（vet+test gate）→ Release；hub 与四节点均升 v0.4.0（node 模式回归对照为发版门槛）。
- 若 v0.3.2 尚未上线，则跳过中间版本直接 v0.4.0（同二进制含全部修复）。

## 4. 回滚

- 一级：master `hub-enable=false`（回现状，秒级）。
- 二级：节点/hub 回装前版本（既有安装器路径）。
- 三级：主人的"没 hub"状态即 v0.3.x 全量——所有 hub 代码在 node 模式默认路径外。

## 5. 数据落档

- 日志摘录（脱敏：无签名 URL）存任务目录 `research/`；数字对照表（TTFB/起播/请求数 vs v0.3.1/v0.3.2）。
