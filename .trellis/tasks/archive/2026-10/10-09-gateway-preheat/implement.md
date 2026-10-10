# implement：网关预热（顺序清单）

> 仓库 = go-emby2openlist（主 module）。`export PATH=$PATH:/usr/local/go/bin`；测试 `go test ./internal/...`。
> 本仓库不 commit；改动只留工作区。
> 状态（2026-10-09）：S1–S5 完成（实现 + 独立检查）。检查自修 1 个日志脱敏缺陷
> （失败场景错误文本含完整签名地址 → `redactSignedURL`，含变异测试证明）并补 4 用例
> （gdrive 关、配置引脚、解析失败、详情页耗时）；-race 全绿、`internal/e2e` ok、既有 5 失败包原样。
> 真机部分（A3）随父任务验收。

- [x] S1 `internal/service/emby/preheat.go`：TryFire（守卫 → 解析 → 去重 → 异步打枪）
      + 去重 map（TTL 10min/有界/裁剪）
- [x] S2 挂点：`TransferPlaybackInfo`（defer，覆盖各提前 return）与 `LoadCacheItems`
      （前置 `preheatReady()` 守卫 + 类型校验）调用 TryFire；共享 `buildPlaybackInfoUri` 避免重复解析
- [x] S3 配置：`agentnetwork.go` 加 `preheat-enable`（默认 true、显式列表同步）+ 样例与样例测试同步
- [x] S4 单测补齐（httptest 假 agent：触发/去重/开关/无阻塞/降级）+ 全量 `go test ./internal/...` 对照基线
- [x] S5 本机 e2e：本地起 ge2o + 假 agent，请求 PlaybackInfo 与详情页各一次，
      确认触发一次、响应耗时不变、日志正确

## 回滚点

- 新增文件 + 两处 handler 挂点 + 一个配置键；`preheat-enable: false` 即回退。
