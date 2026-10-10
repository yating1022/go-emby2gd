# implement：agent 直连大盘鸡（v2）顺序清单

> `agent/` + 主模块 `internal/service/agentnet`（生成侧）；`export PATH=$PATH:/usr/local/go/bin`；不 commit。

- [ ] S1 agent `sign.go`：v2 消息与校验（`v2\n<token>\n<e>\n<u>\n<f>`）；v1 原样保留；独立测试向量
- [ ] S2 agent `handler.go`：解析 `u`/`f`（含 url-decode 与形态校验）→ hub 直连上游；**连接级失败或 409 → 恰一次换链重试**（409 附加 `stale=1`）；v1 路径零变化
- [ ] S3 master 生成侧（`agentnet`）：307/PickAndSign 门槛判定（标记有效 × hub 健康 × agent 版本 ≥0.4.2）→ 签 v2；否则 v1；版本读取自心跳记录
- [ ] S4 master download-link 侧：识别 `stale=1` → 同步重发 `/warm`（短超时）后应答 hub 上游（R1 自愈）
- [ ] S5 测试：agent 侧 v2 矩阵 + v1 回归 + 直连/失败/409 重试请求计数；master 侧门槛矩阵 + stale 重预热用例；两模块 `-race`、新增用例 `-count=3`
- [ ] S6 实现报告（偏差/证据/遗留）；主模块 5 个既有失败包对照式口径

## 回滚点

- 随 v0.4.2；master 侧先上（对旧 agent 仍签 v1，回滚=回装旧镜像）；agent 侧回滚=装回 v0.4.1（此时 master 需回退或继续只签 v1——回滚前先把 master 的 v2 开关关掉，若有开关则并入设计）。
