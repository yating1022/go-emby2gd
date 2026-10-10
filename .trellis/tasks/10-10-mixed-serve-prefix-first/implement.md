# implement：混合服务前缀先行（顺序清单）

> `agent/` 内；`export PATH=$PATH:/usr/local/go/bin`；不 commit。
> 状态（2026-10-10）：S1–S4 + 独立检查完成。`serveCached` 重写为「先写 206 头 + 缓存前缀 + Flush →
> 再开上游续传」；失败分支 WARN + `panic(http.ErrAbortHandler)`（标准库文档化断连机制，经 net/http
> 源码核对）；身份不符断连前补 `Observe` 保自愈；前缀为空窄窗口保留完整回退。
> **检查自修 1 个语义缺口**：新顺序丢失了「上游 401/403 → 重拉直链重试一次」（旧路径经回退透传继承）——
> 已补回（前缀已刷出后才触发，TTFB 不受影响；二次失败才断连），+2 回归用例（修前必败）。
> 负向验证、-race、`-count=3`、mockmaster e2e（预热后 `bytes=0-` TTFB 6.6ms）全部完成。
> 待办：随 v0.3.1 发布后做 A3 真链实测。

- [x] S1 `handler.go serveCached`：混合态「先写 206 头 + 缓存前缀 + Flush → 再打开上游续传」；
      上游失败/校验不过 → WARN + 断连；前缀为空路径保持完整回退
- [x] S2 测试更新：字节一致性矩阵、1 次上游断言保留；三个"回退"用例改断言（前缀已写 + 连接断开 + WARN）；
      新增 TTFB 用例与「前缀为空回退」钉住用例；`-count=3` 无 flake
- [x] S3 `go test -race ./...` + gofmt + vet 全绿；既有矩阵不回归
- [x] S4 本机 e2e：mockmaster 复现 `bytes=0-` 与 `635914-`，前缀先达 + 混合路径日志确认

## 回滚点

- 改动集中在 `serveCached` 一个函数 + 测试；随 v0.3.1 发布，回滚=重装 v0.3.0。
