# implement：agent IPv6 客户端接入（顺序清单）

> 主模块为主；**agent 侧零代码改动**（只可能改安装脚本）。`export PATH=$PATH:/usr/local/go/bin`；不 commit。
> ⚠️ 文件边界：不要碰 `agent/internal/proxy/handler.go` 与 `handler_cache_test.go`（并行任务 v2 正在那里工作）。
> 范围修订（2026-10-10）：取消 UPSTREAM_* 配置键与拨号改造（出站保持现状）。

- [ ] S1 master 侧补测（生产代码零改动）：`parsePublicBaseURL` v6（带端口/无端口/非法）；
      `agentBaseURL` v6 `LastIP`；`sign.go` v6 基址签名全链
- [ ] S2 `internal/service/agentnet/installshell/agent-install.sh`：幂等路径 `--public-url`
      锚定替换 `PUBLIC_BASE_URL`（失败不改文件）→ 重启 → 打印；帮助文本补 v6 示例；沙盒用例
- [ ] S3 测试对照：`go test ./internal/...`（既有 5 失败包原样）+ `-race` 相关包 + gofmt/vet
- [ ] S4（随发布）真机：v6 节点设置 → 节点页 v6 地址 → 客户端 307 v6 → 拉流成功；spec 补 v6 小节

## 回滚点

- 生产代码无改动；脚本替换单行可逆（改回旧值或删行即回默认推导）；随 v0.3.1 发布。
