# design：网关预热

## 1. 组件与落点

新文件 **`internal/service/emby/preheat.go`**（放在 emby 包内，而不是 agentnet——依赖方向：`emby → agentnet` 已存在（redirect.go 用 `agentnet.PickAndSign`），反向会成环）。

```go
// TryFire 异步预热：条目 → gdrive 逻辑路径 → 签名地址 → 小 Range 打一枪。
// 不阻塞、不返回错误；一切失败 WARN 后忽略。
func TryFire(c *gin.Context, itemId string)
```

执行序（全部在守卫之后）：

1. 守卫：`config.C.AgentNetwork.IsEnabled() && config.C.AgentNetwork.PreheatEnabled() && gdrive.IsEnabled()`，否则立即 return（零开销）。
2. 解析：复用本包的取路径口（`resolveItemInfo` + `getEmbyFileLocalPath` + `gdrive.MatchMountPath`）得到 `gdPath`；**若调用方 handler 已持有 `itemInfo` 则直接传入**，省一次 Emby 往返。
3. 去重：`map[string]time.Time` + 互斥 + TTL 10min + 容量上限裁剪（写法对齐本项目既有内存缓存，见 spec「配置与状态管理」）。
4. 触发：`go func(){ url, err := agentnet.PickAndSign(gdPath); GET url, Range: bytes=0-65535, timeout 8s, body discard }`。
   - 调度与播放同一条路径（priority 策略下通常同节点）；同优先级随机轮换时预热门可能≠播放节点——**收益降级但不劣化**（已知限制）。

## 2. 挂载点

- `internal/service/emby/redirect.go` / `items.go` 里的 `TransferPlaybackInfo`（web/route.go:27）与 `LoadCacheItems`（web/route.go:35）两 handler **主逻辑完成后**调用 `TryFire(c, itemId)`（`go` 异步语义封装在 TryFire 内更稳）。
- itemId 提取方式与各 handler 内现有解析保持一致。

## 3. 配置

- `internal/config/agentnetwork.go`：新增 `PreheatEnable *bool yaml:"preheat-enable"`（默认 true）；
  **该文件的 UnmarshalYAML 是显式字段列表，需同时登记**；新增获取函数 `PreheatEnabled()`（nil → true）。
- `config-example.yml` / `config_example_test.go` 同步补键。

## 4. 错误与降级矩阵

| 条件 | 行为 |
|---|---|
| 开关关 / gdrive 关 / agent 网络关 | 不触发（零开销） |
| 条目非 gdrive 挂载路径（MatchMountPath 未命中） | 不触发 |
| 无可用节点（`ErrNoAgent`）/ 签名失败 | WARN 记录，忽略 |
| 打枪请求失败 / 超时 | WARN 记录，忽略 |
| TTL 内同一路径 | 跳过（不打日志或 DEBUG 级一行） |

## 5. 测试

- 单测（httptest 假 agent）：触发一次且 Range/路径正确；TTL 内不重复；开关关零请求；
  假 agent 挂起时 handler 响应耗时与 baseline 无差；解析失败/无节点不 panic 不阻断。
- 既有测试对照全绿（含 config 样例测试与 route 相关测试）。

## 6. 风险与备注

- 预热门被其他文件挤出 LRU / 与播放节点轮换不一致：收益降级，可接受。
- 预热请求占 agent 一个并发名额毫秒级：可接受。
- 预热产生的日志频率与 PlaybackInfo 频率同阶（用户浏览行为级别），不构成刷屏；不为此新增静音。
