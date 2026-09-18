# 执行计划: strm 代理接入与端到端验证

## 涉及文件

| 文件 | 改动 |
|---|---|
| `internal/service/emby/redirect.go` | strm 分支插入代理判断与缓存旁路; 补 L1/L2/L2' 日志 |
| `internal/service/emby/redirect.go` | strm 分支头部补 L1（检测到远程地址） |

不改动 `internal/service/streamproxy/*` 与 `internal/config/*`（前置子任务已交付）。若实现中发现契约缺陷, 回到 `09-16-strm-proxy-core` 修契约, 不要在本子任务里就地打补丁。

## Step 1 接入决策分支

按子任务 `prd.md` 的"目标代码形态"改写 `redirect.go` 的 strm 分支。

三个检查点, 实现后逐条自查:

1. **是否引入了任何缓存相关调用**? 不应引入。字节流路由已移出缓存白名单（前置子任务）, handler 中不需要也不应该出现缓存旁路代码。
2. **`written == true` 是否立即 `return`**? 若漏掉 `return`, 代码会继续走 302, 产生"已经流式写完 625MB 又追加重定向"的错乱响应。
3. **`written == false` 时是否落到原有 302 流程**? 不得在此处转向 `checkErr`（见父 `design.md` §8 的取舍说明）。

## Step 2 日志补齐

| 位置 | 条目 |
|---|---|
| `urls.IsHttpRemote(embyPath)` 判定为真之后 | L1 `[直链代理] 检测到 strm 远程地址: %s` |
| `MatchDomain` 返回 true | L2 `[直链代理] 命中代理前缀: %s` |
| `MatchDomain` 返回 false | L2' `[直链代理] 未命中任何代理前缀, 走原有 302 流程` |

**只打印地址, 不打印 `itemInfo`**。`logging-guidelines.md` 记录了既有违规（`redirect.go:71` 的 `logs.Info("解析到的 itemInfo: %v", itemInfo)` 会泄漏 ApiKey）; 本子任务不扩大该违规, 新增日志只含地址字段。

**L2' 的调用条件**: 仅在 `StrmProxyEnabled()` 为真时打印。开关关闭时不应产生任何 `[直链代理]` 日志, 否则改动前用户的日志会被新噪音污染（R4）。

## Step 3 编译与静态检查

```bash
export PATH=$PATH:/usr/local/go/bin
mkdir -p web/dist
gofmt -l internal/service/emby/
go vet ./internal/...
go build ./...
go test ./internal/...
```

## Step 4 端到端验证

按父任务 `implement.md` 的 V1-V6 顺序执行。每项都要留下可复查的证据:

| 验证 | 需要留下的证据 |
|---|---|
| V1 链路方向 | `ss -tnp` 输出片段（本项目进程 ↔ 上游的 ESTABLISHED） |
| V2 Range/seek | 日志中同一播放会话内递增的 Range 与对应 206 |
| V3 内存 | 播放前后的 RSS 数值对比 |
| V4 日志 | `grep '\[直链代理\]'` 的完整输出 |
| V5 回归 | 未命中前缀、本地媒体、字幕、图片各一条日志或现象记录 |
| V6 OpenList 相关 | 本子任务阶段还不需要, 留到 `09-16-remove-openlist` |

### 故障注入用例（AC8）

临时把 `emby.strm.proxy.domains` 改成一个不可达地址（如 `http://127.0.0.1:1`）, 播放:

- 期望: 客户端收到 302（行为与改动前一致）, 日志出现 L12 与"代理失败, 回退原有 302 流程"
- 验证完**立即改回**正确配置

## Review Gate

- [ ] handler 中未新增任何缓存旁路调用
- [ ] AC0 已验证: `RequestCacher` 对 `/Videos/{id}/stream` 确实走了跳过分支（临时日志/断点已删除）
- [ ] `written == true` 路径立即返回, 不会继续执行 302
- [ ] 代理失败回退到 302, 没有叠加 `checkErr`
- [ ] 开关关闭时零 `[直链代理]` 日志
- [ ] 新增日志不含 `itemInfo` 整体打印
- [ ] `original` 路由路径已手工走查（AC7）: `ProxyOriginalResource` → `Redirect2OpenlistLink` 不会二次进入代理分支
- [ ] 测试值（`domains` 指向 127.0.0.1:1 的故障注入）已改回

## 回滚点

- **代码回滚**: 本子任务独立提交, `git revert` 后回到"strm 一律 302"的状态。
- **配置回滚（优先）**: `emby.strm.proxy.enable: false` 立即恢复改动前行为, 无需改代码、无需重启以外的操作（配置变更需重启进程）。
- 回滚本子任务**不需要**回滚前两个子任务: `streamproxy` 包与缓存改动本身不会改变任何既有行为。

## 已知风险

| 风险 | 表现 | 应对 |
|---|---|---|
| 前置子任务未生效（白名单仍含 stream 路由） | 播放时进程内存暴涨, 大文件必然 OOM | AC0 先于一切功能验证执行; V3 专门验证内存曲线 |
| 上游 IP 白名单未加本项目服务器 | 上游返回 403, 日志出现 L10 反复重试后失败 | 需用户在网关侧加白; 日志会明确指出上游状态码 |
| 带宽瓶颈 | 播放卡顿、缓冲久 | 属架构固有代价, 需要用户确认服务器带宽 |
| 播放器并发 Range 打满上游 | 上游 429 / 连接被拒 | 评估引入单飞或并发限制, 单独立任务 |
