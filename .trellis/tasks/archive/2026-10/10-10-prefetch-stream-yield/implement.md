# implement：预取流式化与让路（顺序清单）

> `agent/` + 网关预热小改；`export PATH=$PATH:/usr/local/go/bin`；不 commit。

- [ ] S1 `agent/internal/proxy/prefetch.go`：头预取改单条流 + 流式切片（凑满即 Put；`首块就绪` 日志；
      短读/非 0 起点处理）+ 触发条件改期望块集缺失 + 续取（只补缺失、连续缺失单流）
- [ ] S2 让路：`Prefetcher.ClientBegin/ClientEnd` + `handler.go serve()` 登记/注销 + 读取循环让路检查
      （注意与 v2 文件边界：本任务允许动 handler.go 的 serve 登记点，但不得改动 `serveCached` 的混合逻辑）
- [ ] S3 网关 `internal/service/emby/preheat.go`：响应体直接关闭；超时语义分级（未送达=WARN / 送达后=INFO）
- [ ] S4 测试：块 0 早熟（慢速滴流）、让路（时间线断言）、续取（请求数精确）、短读/200 边界、
      既有预取请求数断言更新（10→2 量级）、字节一致性矩阵全绿、网关超时语义用例
- [ ] S5 `go test -race ./...`（agent）+ 主模块相关包对照基线 + gofmt/vet；`-count=3` 无 flake
- [ ] S6 本机 e2e：mockmaster + 慢速假上游，前后对比（上游请求数、块 0 就绪耗时）

## 回滚点

- 改动集中在 prefetch.go（+serve 登记点 + 网关一处）；随 v0.3.2 发布，回滚=重装 v0.3.1。
