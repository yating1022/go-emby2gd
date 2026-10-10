# agent 直连大盘鸡（签名 URL v2，去稳态回访）

> 父任务：`10-10-cache-hub-center`。用户定调（2026-10-10）：**大盘鸡健康且已预热时，307 之后 agent 应直接透传大盘鸡的文件、零回访 master**；只有大盘鸡失败时才回访 master 一次拿 Google 直链去自己拉流。随 v0.4.2 发布。

## Background

- 现状：agent 在每次需要上游时回访 master `download-link` 拿 `{url, headers}`（链接缓存 ~1h 级）。在大盘鸡路径下，那个 `url` 只是 `http://154.19.43.32:8791/f/<driveID>`、`headers` 为空——**纯路由信息、无凭据**，稳态下这次回访没有存在价值。
- 用户模型（目标形态）：
  - 正常：`客户端 → 307 → agent →(直接)→ 大盘鸡 /f/`，master 完全不参与；
  - 大盘鸡宕机/不可服务：agent 回访 master 一次 → 拿 Google 直链 → 自己拉流透传（保留 v0.4.1 的兜底，仅失败时触发）。
- 顺带修复 R1：hub 重启后 `409 not_warmed`（未缓存区段）目前要等接受标记过期（≤30min）才自愈；agent 回访时若带"陈旧"提示，master 可**当场重新预热**再应答，重试即命中（秒级自愈）。
- 签名格式本就预留了版本演进（v1 前缀 + 文中注“将来换算法时新旧 URL 可区分”），v2 是既定演进路径。

## Requirements

- **N1 签名 URL v2（版本化，v1 原样保留）**：
  - hub 模式：`s = HMAC-SHA256(sign_key, "v2\n<token>\n<e>\n<u>\n<f>")`（小写 hex）；URL 形如 `/dl/<token>?e=<unix>&u=<urlencode(hubBase)>&f=<driveID>&s=<hex>`；
  - **`u`/`f` 必须进签名**——否则合法 URL 会被改造成"任意文件代理"（签名 URL 的防滥用前提）；
  - agent 侧 `u` 校验：http(s)、有主机、无 userinfo/query/fragment（对齐既有 parsePublicBaseURL 风格），非法 → 视为校验失败。
- **N2 master（生成侧）**：307 时若「该文件 hub 已接受（标记有效）且 hub 健康」→ 签 v2（`u`=hubFor(fileID) 基址 + 端口，`f`=Drive 文件 ID）；否则维持 v1（现行为）。**仅对心跳版本 ≥0.4.2 的 agent 签 v2**；旧版本 agent 一律 v1（滚动兼容）。
- **N3 agent（消费侧）**：URL 携带 `u`/`f` → 校验 v2 → 上游 = `u + /f/ + f`，**直接透传，不询问 master**；出现**连接级失败或 409** → 恰好一次换链重试（连接类失败沿用 v0.4.1 语义；409 场景换链请求附带 `stale=1`）；仍失败 → 502（现语义）。无 `u`/`f` 的 v1 URL 行为完全不变。
- **N4 master（download-link 侧）**：收到 `stale=1`（或等价陈旧提示）→ **先同步重发一次 `/warm`**（短超时）再应答 hub 上游（hub 健康时）；标记缺失/失效路径的既有"首播同步补发预热"语义不变。目的：R1 现场自愈。
- **N5 回归与兼容**：v1 全链零变化（旧 agent 继续工作）；v1/v2 校验共存；两版测试向量独立重算（不调用被测函数生成期望值）。
- **N6 发布节奏**：随 v0.4.2；**master 先行**（生成侧先支持但不启用对旧 agent 的 v2），节点后升级。master 侧带开关 `agent-network.hub-direct-v2`（默认开；关闭 = 全部签 v1，秒级回滚路径）。

## Acceptance Criteria

- [ ] **A1 单测**：v2 校验矩阵（篡改 `u`/`f`/`e`、跨 agent 密钥、非法 `u` 形态、过期）全按失败处理；v1 回归全绿（既有签名向量与用例不动）；master 签 v2 的门槛用例（标记有/无 × hub 健康/不健康 × agent 版本 ≥/<0.4.2）。
- [ ] **A2 真链（核心指标）**：已预热文件的正常播放全程，**master 日志零 `download-link` 调用**（客户端 307 → agent → 大盘鸡直连）；人为停大盘鸡 → agent 回访一次 → 走 Google 直链续播（2–3s 内恢复）。
- [ ] **A3 R1 自愈**：重启大盘鸡后播未缓存区段 → agent 得 409 → 回访（带提示）→ master 重预热 → 重试命中（秒级，不再等 30 分钟）。
- [ ] **A4 滚动兼容**：留一台不升级的节点，其播放仍按 v1 路径工作（master 对其签 v1）。
- [ ] **A5** `-race`、`-count=3` 无 flake；对照式口径；不 commit。

## Out of Scope

- 多播机制（多 hub 时 `u` 填具体一台，由 hubFor 决定，已兼容）；v1 的移除（保留）；节点侧其它重构。
