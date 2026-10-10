# implement：hub 模式（agent 二进制）顺序清单

> `agent/` 模块；`export PATH=$PATH:/usr/local/go/bin`；不 commit。
> 前置：v0.3.2 发布冻结（同一工作区避免发布混入半成品）。

- [ ] S1 角色/配置脚手架：`ROLE`、`HUB_PORT`、`HUB_ALLOW_IPS`、`DISK_CACHE_DIR`、`DISK_BUDGET_GB`、`CACHE_MAX_AGE_MINUTES`(2880)、warm 默认值；enroll 携带 role；hub 不挂客户端路由
- [ ] S2 磁盘块存储 diskcache：布局/temp+rename/identity/惰性 TTL+周期清扫/LRU 上限/atime 节流/启动恢复/单飞 + 单测（含崩溃复用、并发）
- [ ] S3 `/f/` 三态服务：复用 serveCached 语义（抽出共用或镜像实现，**不得改动 node 的 serveCached 行为**）；409 not_warmed；命中不出网 + 字节一致性矩阵
- [ ] S4 控制指令：`/warm`（区域集三条抓取：头单流切片/尾 Range/续播点 Range；幂等；单飞）、`/cancel`；3 分钟规则（可注入时钟）+ 播放转全量续取
- [ ] S5 回源加固：多地址快速失败拨号（单地址 ~1.5s）+ 401/403 换链续取；单测（坏地址跳过、全败报错）
- [ ] S6 node 侧微调核查：hub 上游 Authorization 行为取证；需要则加"hub host 跳过 auth"（含测试），预计零改动
- [ ] S7 测试/负向/e2e：三态矩阵全绿、时间线断言、请求数断言、假 Google 全链（命中/部分/空/断网命中）；`-race`、`-count=3` 无 flake；gofmt/vet
- [ ] S8 回归对照：node 模式全量对照 v0.3.2 基线（5 个既有失败包按对照式口径不动）；实现报告记录偏差与遗留风险

## 回滚点

- 全部为新增文件 + 新配置默认值（`ROLE` 缺省=node）；回滚=不启用 hub 角色，node 行为与 v0.3.2 一致。发布随 agent v0.4.0（由部署子任务统一做）。
