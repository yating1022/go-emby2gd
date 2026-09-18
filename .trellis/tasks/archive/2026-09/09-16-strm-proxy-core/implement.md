# 执行计划: streamproxy 核心

## 前置检查

```bash
export PATH=$PATH:/usr/local/go/bin
mkdir -p web/dist          # C7: go:embed all:dist 需要该目录存在
go build ./...
```

确认 `09-16-cache-stream-passthrough` 已完成（`internal/web/cache` 的白名单中已不含 `Reg_ResourceStream` / `Reg_ItemDownload` / `Reg_ItemSyncDownload`）。

本子任务**不需要**调用任何缓存旁路接口: 字节流路由已不在白名单内, handler 拿到的 `c.Writer` 就是原始 writer。

## Step 1 配置层

1. `internal/config/emby.go`: 新增 `StrmProxy` 类型与 `Strm.Proxy` 字段; `Strm.Init()` 串联 `Proxy.Init()`; 补 `StrmProxyEnabled()` / `StrmProxyConfig()` 两个访问器。
2. 校验规则按子任务 `prd.md` R1 实现, 错误消息中文并指出具体配置项名。
3. `max-concurrent-streams` 的默认值处理见设计 §2 末尾 —— 注意区分"未配置"与"显式配置 0", 实现时在 `config-example.yml` 中显式写出 `max-concurrent-streams: 16`, 避免使用者误以为 0 是默认值。
3. `config-example.yml`: 在 `emby.strm` 段内补 `proxy` 子段, 每项带中文注释说明默认值与取值范围, 且**默认 `enable: false`**。

验证: 故意写错配置（如 `link-cache-expired: 10x`）应启动报错并给出中文提示。

## Step 2 `urls.go`

1. `NormalizeURL`（设计 §3）。
2. `MatchDomain`（设计 §4）。
3. 先写 `urls_test.go` 的表驱动用例, 覆盖:
   - 裸空格 / 中文 / 全角冒号的地址 → 结果不含裸空格与裸非 ASCII
   - 已是规范编码的地址 → 归一化结果稳定（幂等: 归一化两次结果相同）
   - 空地址、无法解析的地址 → 返回错误
   - 命中 `http://vault.bjyt.de:7811`; **不命中** `http://vault.bjyt.de:78111`
   - scheme 大小写差异、配置项尾部斜杠、命中路径前缀
   - 未开启代理时 `MatchDomain` 恒为 false

## Step 3 `link.go`

1. `linkEntry` / `linkCache`（设计 §6）。
2. `buildUpstreamHeader`（设计 §5）。
3. `resolveLink`（设计 §5）: 缓存查询 → 上游请求 → 3xx 跟随 → 失效重试 → 写缓存。
4. 日志按父 `design.md` §7 的 L4/L6/L7/L10/L12 打点。

## Step 4 `streamproxy.go`

1. `Proxy`（设计 §7）。
2. 日志 L8/L9/L9'/L11 打点。
3. 确认 `bytess.CommonFixedBuffer()` 的 `PutBack()` 用 `defer` 保证执行; 确认 `resp.Body.Close()` 在所有路径上都被调用。
4. 并发槽位（设计 §8）: 在 `Proxy` 中, **归一化之后、请求上游之前**调用 `acquireSlot(r.Context())`; 失败（客户端在等待中断开）直接返回 `false, err`, 此时尚未写响应。`defer releaseSlot()` 紧跟其后注册。
5. 满员等待时打 L13, 拿到槽位后打 L14（含活跃数与上限）。上限为 0 时 `streamSlots` 保持 `nil`, `acquireSlot` 立即返回且**不打** L13/L14 之外的额外日志——L14 此时只打印活跃数。
6. 槽位释放必须覆盖三条路径: 正常结束、上游出错、客户端中断。用 `defer` 而非在分支里手工调用。

## Step 5 测试

`streamproxy_test.go`（`package streamproxy_test`）用 `httptest.NewServer` 起假上游:

| 用例 | 假上游行为 | 断言 |
|---|---|---|
| `TestProxy_RangePassthrough` | 读请求的 `Range`, 返回 206 + `Content-Range` | 客户端收到 206; 上游收到的 Range 与客户端一致; 响应头含 `Content-Range` |
| `TestProxy_AcceptRangesBackfill` | 206 但不带 `Accept-Ranges` | 客户端响应含 `Accept-Ranges: bytes` |
| `TestProxy_RedirectFollow` | 第一次 302 → 第二次 200 | 客户端收到 200; 假上游观测到两次请求且第二次带 Range |
| `TestProxy_RedirectDepthExceeded` | 无限 302 | 返回 `written=false` 的非空错误 |
| `TestProxy_RetryOnInvalidLink` | 第一次 403 → 第二次 200 | 客户端收到 200; 假上游观测到两次请求 |
| `TestProxy_LargeResponseNoBuffering` | 返回一个超过缓冲上限的响应体 | 客户端收到完整字节数; 用 `runtime.ReadMemStats` 对比前后 `HeapAlloc` 增量远小于响应体大小 |
| `TestProxy_ClientDisconnect` | 慢速持续输出 | 客户端提前取消 context; 假上游在超时内观测到连接关闭 |
| `TestProxy_HeaderWhitelist` | 回显收到的头 | `Authorization`/`Cookie`/`X-Emby-Token`/`Host` 未透传; `Range` 透传 |
| `TestProxy_NoUpstreamWhenDisabled` | 计数请求数 | `enable: false` 时请求数为 0 |
| `TestProxy_ConcurrencyLimit` | 假上游阻塞直到被放行; 上限设为 2, 同时发起 5 个请求 | 假上游观测到的并发峰值不超过 2（AC11） |
| `TestProxy_WaitReleasedOnClientCancel` | 上限设为 1 并占满; 第二个请求在等待中取消 Context | 第二个请求立即返回错误, 且未向上游发起请求 |
| `TestProxy_SlotNotLeaked` | 上限设为 1; 连续发起 5 个串行请求 | 5 个全部完成, 无永久阻塞（AC13） |
| `TestProxy_UnlimitedSkipsWaiting` | 上限设为 0 | 不产生 L13 语义的等待, 并发不设限（AC12） |

并发相关用例的**上限值通过配置注入**, 不要把 16 写死在测试里 —— 测试用 1 或 2 才能快速触发边界。

`link_internal_test.go`（`package streamproxy`）覆盖缓存:

| 用例 | 验证 |
|---|---|
| 写入后命中 | 返回缓存地址 |
| 过期后视为未命中 | 惰性清理, 且条目不残留 |
| 未发生跳转时不写缓存 | `linkCache` 中无该 key |
| 失效状态码清缓存 | 重试后 key 被删除或更新 |

`TestProxy_LargeResponseNoBuffering` 的上限值取 `cache.MaxBufferedRespSize`, 但假上游只需返回比它略大的数据即可 —— 若该常量较大（100MB）, 该用例会消耗较多内存与时间, **改为在一个较小的可注入上限下验证**: 在测试内构造 `respCacheWriter` 等价物不可行（跨包）, 因此该用例退化为"验证 `Proxy` 自身不缓冲"（用 `HeapAlloc` 增量断言），不依赖缓存包的上限值。

## Step 6 全量验证

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -l internal/service/streamproxy/ internal/config/
go vet ./internal/...
go build ./...
go test ./internal/service/streamproxy/... -v
go test ./internal/...
```

## Review Gate

- [ ] `streamproxy` 包没有 import gin, 也没有 import `internal/web/...`
- [ ] 所有出站请求走 `internal/util/https`
- [ ] 上游请求绑定了调用方 Context
- [ ] 请求头采用白名单策略, 没有任何路径会把 `Authorization` / `Cookie` / `X-Emby-*` 透传给上游
- [ ] 日志不含 api key / token; 全部带 `[直链代理]` 前缀
- [ ] 归一化对已规范的地址幂等
- [ ] 前缀匹配的边界校验有测试覆盖
- [ ] `enable: false` 时全程零上游请求
- [ ] 槽位用 `defer` 释放, 三条退出路径都被覆盖
- [ ] 等待槽位期间响应客户端 Context 取消, 不做无界等待
- [ ] `max-concurrent-streams: 0` 时信号量 channel 为 `nil`, 不参与控制
- [ ] 本包无任何按响应体大小增长的常驻内存结构（部署机器 <1GB, 父 PRD C9）
- [ ] 阈值使用具名常量且带中文注释
- [ ] 错误消息为中文, 使用 `%w` 包装

## 回滚点

本子任务独立提交。回滚时 `internal/config` 中的 `StrmProxy` 与 `config-example.yml` 的 `proxy` 段一同移除;
由于此时还没有任何 handler 调用 `streamproxy`, 回滚是干净的（不会留下悬空调用）。
