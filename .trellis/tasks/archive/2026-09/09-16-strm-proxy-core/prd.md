# strm 代理核心: 域名匹配 + 上游解析 + 流式代理

> 父任务: `09-16-strm-proxy-play`。技术契约以父任务 `design.md` 为准, 本文件只写本子任务的范围、交付与验收。
> 前置: `09-16-cache-stream-passthrough` 必须先完成。

## Goal

提供一个**与 gin / Emby 解耦**的能力: 给定一条绝对 URL, 在本机请求它（必要时跟随重定向）, 并把响应体以流式方式写回客户端。配套提供 `emby.strm.proxy` 配置段与校验。

包 `internal/service/streamproxy/` 只依赖 `config` / `util/https` / `util/logs` / `util/bytess` / `util/strs`, **不 import gin, 不 import `web`**。

## Requirements

- **R1 配置**: 在 `emby.strm` 下新增 `proxy` 子结构, 字段与默认值见父 `design.md` §9。缺省整段时行为与改动前完全一致。`Init()` 需校验:
  - `enable` 为真时 `domains` 不得为空
  - `link-cache-expired` 单位合法（`s/m/h/d`）、数值 ≥ 1
  - `max-redirect-depth` ≥ 1, 小于等于 `https.MaxRedirectDepth`
  - `retry-status-codes` 元素为合法状态码（100-599）
  - `max-concurrent-streams` ≥ 0（0 表示不限制）
  - `domains` 元素非空且形如 http/https 地址
  - 同步更新 `config-example.yml`
- **R2 URL 归一化**: 按父 `design.md` §3 实现。解码-再编码必须无损; 归一化失败返回错误而不是静默降级。
- **R3 前缀匹配**: 按父 `design.md` §9 实现, 含"边界成立"校验（避免 `host:7811` 误命中 `host:78111`）。按配置顺序取第一个命中。
- **R4 上游请求与 3xx 跟随**: 按父 `design.md` §4 实现。使用 `RequestHolder.DoSingle()` 手动跟随, 上限 `max-redirect-depth`。请求头按白名单透传、黑名单剔除, 叠加配置的固定请求头。
- **R5 直链缓存与失效重试**: 按父 `design.md` §6 实现。仅当发生过跳转且最终地址与原地址不同时写入缓存; 命中 `retry-status-codes` 时清缓存并重试一次。
- **R6 流式回写**: 按父 `design.md` §5 实现。全程不整体缓冲; 传递 `Range`; 206/Content-Range/Content-Length 正确透传; 上游 206 缺少 `Accept-Ranges` 时补写。
- **R7 客户端上下文**: 上游请求必须绑定调用方传入的 `*http.Request` 的 Context, 保证客户端断连后上游连接释放。
- **R8 日志**: 实现父 `design.md` §7 中属于本包职责的条目（L3-L10、L12-L14）, 统一 `[直链代理]` 前缀。不得打印 api key / token。
- **R9 并发控制**: 按父 `design.md` §11 实现 `max-concurrent-streams`（默认 16, `0` 为不限）。满载时**等待**而非拒绝, 等待过程中必须响应客户端 Context 取消。槽位在传输结束时释放。
- **R10 观测**: 每次进入传输输出当前活跃数（L14）; 每次传输结束输出字节数与耗时（L9/L9'）。这两项是小内存机器上判断并发默认值是否需要调整的唯一数据来源。
- **R11 测试**: 覆盖归一化、前缀匹配、缓存与失效重试、Range 透传与状态码回写、并发槽位的获取与释放。

## Interface

```go
// Proxy 将 rawURL 指向的媒体字节流代理给客户端
//
// written 为 true 表示响应已经开始写入, 调用方不得再做任何回退;
// written 为 false 且 err 非空表示尚未写入任何响应, 调用方可按既有策略回退。
func Proxy(w http.ResponseWriter, r *http.Request, rawURL string) (written bool, err error)

// NormalizeURL 归一化 strm 地址
func NormalizeURL(rawURL string) (string, error)

// MatchDomain 判断地址是否命中配置的代理前缀, 返回命中的前缀
func MatchDomain(rawURL string) (string, bool)
```

签名细节与内部类型见本子任务 `design.md`。

**不使用哨兵错误表达"已写响应"**。`error-handling.md` 指出内联哨兵（`haveReturned`）是遗留模式, 不应复制; 这里用布尔返回值显式表达。

## Acceptance Criteria

- [ ] **AC1** 命中前缀的地址进入代理; 未命中前缀时 `MatchDomain` 返回 false, 且不发起任何上游请求。
- [ ] **AC2** 含裸空格、中文、全角标点的地址经 `NormalizeURL` 后生成的请求行合法（本地验证: 归一化结果中不含裸空格与裸非 ASCII 字节）。
- [ ] **AC3** 客户端带 `Range` 时, 上游收到同样的 `Range`, 客户端收到 206 与正确的 `Content-Range`/`Content-Length`。
- [ ] **AC4** 上游返回 3xx 时能跟随到最终地址并完成代理; 跳数超限时报错并回退。
- [ ] **AC5** 上游返回 `retry-status-codes` 中的状态码时, 清除缓存并重试一次; 重试仍失败则报错。
- [ ] **AC6** 代理大响应时内存不随响应体大小增长（用本地假上游返回 > 上限的响应体验证）。
- [ ] **AC7** 客户端中途断开时, 上游请求被取消（假上游可观测到连接关闭）, 且日志出现 L9'。
- [ ] **AC8** `enable: false` 或未配置 `proxy` 段时, 本包不产生任何上游请求。
- [ ] **AC9** `go build ./...`、`go vet ./internal/...`、`go test ./internal/service/streamproxy/...` 全部通过。
- [ ] **AC10** 配置校验覆盖 R1 列出的每条规则, 非法配置在启动时报中文错误并拒绝启动。
- [ ] **AC11** 并发上限生效: 上限为 N 时, 同时进行的传输不超过 N 条; 客户端在等待槽位期间断开时, 等待立即结束且不产生传输（不泄漏槽位）。
- [ ] **AC12** `max-concurrent-streams: 0` 时不做任何限制, 且等待逻辑不参与（不产生 L13 日志）。
- [ ] **AC13** 传输结束（正常/出错/客户端中断）时槽位一定被释放: 连续发起超过上限数量的请求不会因为槽位泄漏而永久阻塞。

## Constraints

- 出站请求一律走 `internal/util/https`（父 PRD C1）。
- 不得 import gin, 不得 import `internal/web/...`。
- 测试只用标准库; 表驱动 + `t.Errorf`; 不写文件到仓库; 不用 `log.Fatal`。
- 具名常量承载阈值（重定向深度、缓冲大小等）, 中文注释。

## Out of Scope

- 接入 `Redirect2OpenlistLink` 决策分支与回退语义的落地 —— 属于 `09-16-strm-proxy-integration`。
- 端到端真实环境验证 —— 同上。
- 单飞（single-flight）合并并发解析。若实现阶段观测到上游并发压力再评估, `golang.org/x/sync` 已在 go.mod 中。

## Notes

- 部署机器内存小于 1GB（父 PRD C9）, 因此本包**不得**引入任何按响应体大小增长的常驻内存。自建的直链缓存只存 URL 字符串（见父 `design.md` §6）, 不存响应体。
- **并发上限的目的不是内存**: 每路流约 128KB（父 `design.md` §11）, 20 路并发也只有 2.5MB。不要以"省内存"为由调低默认值 16; 需要调整时应依据 L14 的实测并发数, 或上游出现连接拒绝时。
- 实测上游 `vault.bjyt.de:7811` 直接返回 2xx 而非 3xx（父 research §2）, 因此**默认路径是 2xx 直连代理**; 3xx 跟随是兼容性分支, 不要因为"实测没走到"就省略。
- 测试中的假上游用 `httptest.NewServer`（标准库）。项目此前未使用 `httptest`, 但对"流式代理 + Range + 断连"这类行为, 它是唯一能不依赖真实服务完成验证的方式。这是一处**有意偏离既有约定**的决策, 已在 `quality-guidelines.md` 的"当前状态"之外单独记录, 评审时请留意。
