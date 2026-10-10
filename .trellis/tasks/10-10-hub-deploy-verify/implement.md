# implement：hub 部署与真链验证 顺序清单

> 前置：两个实现子任务完成并通过检查；v0.3.2 已全量（否则直接以 v0.4.0 覆盖）。
> 变更一律先读现状确认，再动手；破坏性操作（重启/防火墙）逐一确认。

- [ ] S1 发布 agent v0.4.0：sync fork → tag → Actions → Release（node 零回归对照为门槛）
- [ ] S2 Twon 装 hub：二进制 + config.env（ROLE=hub/白名单/磁盘/48h）+ systemd；白名单实测（NAS 拒绝、master/节点通）；**250G 落盘验证：`findmnt /home`=/dev/vdb1、`df -h /home` 容量、systemd `RequiresMountsFor=/home`、重启演练确认 fstab 自动挂载**；**hub 注册链路（部署前联调发现）**：安装脚本需透传 `--role hub` + `--port 8791`（`agent-install.sh` 若未支持则先小改并跑其沙盒冒烟测试；`--port` 与 master 的 `hub-port` 必须一致——master 按 LastIP:hub-port 拼内网基址）；enroll token 用 master config.yml 的 `agent-network.enroll-token`。**预检已确认（2026-10-10）**：Twon 无主机防火墙（应用层白名单 fail-closed 即唯一门禁，保持）；/etc/fstab 已持久挂载 `vdb1 → /home`；8791 空闲；systemd 257
- [ ] S3 master 开启：`hub-enable: true` + rebuild/force-recreate；注册表确认 role=hub 且不入客户端调度
- [ ] S4 联调冒烟：浏览→播放一次全链；hub 日志：warm 接受→头/尾/续播区抓取→/f/ 命中
- [ ] S5 真链矩阵（见 design §2 表）：逐场景跑并记录数字与日志（NAS + 手机 vantage）
- [ ] S6 故障演练：stop hub→播放回退直链无感→恢复；**hub 重启后立即播放已预热文件（R1 窗口，如实记录）**；断网命中；`hub-enable=false` 回滚确认
- [ ] S7 数据落档：对照表 + 脱敏日志入 `research/`；父任务 A1–A7 逐条勾稽；观察项（TTL/LRU/流量）列入后续跟踪

## 回滚点

- 每个演练场景前记录当前状态；回滚顺序：`hub-enable=false` → 停 hub →（必要时）装回上一版二进制。
