# implement：agent 读前缓存（顺序清单）

> 仓库 = go-emby2openlist；agent 是嵌套 module（`agent/`），命令在 `agent/` 内跑。
> Go 工具链在 /usr/local/go/bin（先 `export PATH=$PATH:/usr/local/go/bin`）。测试：`go test -race ./...`。
> **2026-10-09 变更：不做 tee——缓存只含首触预取块**（design.md 为准）。

- [ ] S1 `agent/internal/proxy/blockcache.go`：块键/预算/LRU/Get/Put/Prefix/SetEtag/Enabled
      + 单测（块数学、LRU 淘汰、etag 失效、Prefix 连续性）
- [ ] S2 `agent/internal/proxy/prefetch.go`：首触触发（无块且在途标记）、头预取（32MB）、
      size 已知后的尾预取（4MB）、单飞、全局并发上限（2 文件）、失败 WARN 放弃 + 单测（httptest 假上游）
- [ ] S3 `agent/internal/proxy/handler.go` 服务三态：Range 解析器、纯本地命中、混合（先开上游再写首字节）、
      **未命中纯透传（不缓存）**、白名单头本地拼装、HEAD 直通
      + 单测：**同 Range 缓存开/关响应 sha256 相等**、边界（块中/跨块/尾不足一块/超 size）、上游失败回退
- [ ] S4 `agent/internal/config/config.go`：CACHE_BUDGET_MB(256) / PREFETCH_HEAD_MB(32) / PREFETCH_TAIL_MB(4)，
      0=关闭；`config.env` 样例与测试同步
- [ ] S5 日志：预取开始/完成、缓存命中、混合服务（INFO/WARN），确认不漏令牌/签名
- [ ] S6 全量 `go test -race ./...` + `gofmt -l` / `go vet` 对照全绿；既有签名/重定向矩阵不回归
- [ ] S7 本机 e2e：mockmaster + agent，curl 复现探测序列（`0-` → 尾探 → 偏移重启），
      确认除首请求外全部本地命中（日志 duration 两位数 ms）

## 回滚点

- 新增文件 + handler 内缓存分支；`CACHE_BUDGET_MB=0` 运行时即回退 v0.2 行为（对照测试钉死）。
- 本仓库不 commit；发布走 tag `agent-v0.3.0`（父任务 I2）。
