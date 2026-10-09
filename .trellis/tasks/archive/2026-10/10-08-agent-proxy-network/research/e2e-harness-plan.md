# research：E2E 实施笔记（父任务 §4 矩阵的落地勘察）

来源：2026-10-09 主会话对子任务 A 测试设施的勘察（文件锚点已核对）。

## 现成设施（可复用/适配，不要重造）

`internal/service/emby/redirect_gdrive_test.go`（包 `emby_test`）：

- `fakeEmbyOrigin`（:23-68）：PlaybackInfo 返回指定媒体路径；其余请求计 `originHits`（回源证据）。
- `waitForPlaybackProbes`（:81-96）：排空异步 PlaybackInfo 探测——**用例结束前必须调用**，
  否则异步 goroutine 读到空 config.C 会把测试进程带走（A 实现期间踩过）。
- `fakePanel`（:99-126）/ `newFailingPanel`（:129-145）：假面板成功/失败模式。
- `newDirectLinkServer`（:148-159）：假 Google 直链端点（简单 body；E2E 需换成带 Range 的
  `http.ServeContent` 版本，并记录收到的 Authorization 头）。
- `withEmbyTestConfig`（:161-183）/`newRedirectContext`（:190-199）：config.C 注入与 gin 测试上下文。
- `captureRedirectLogs`（:230-241）：内存日志收集器（可断言日志不含 `s`/完整 URL）。

**注意**：以上是 `emby` 包测试文件内的设施，跨包不能 import。E2E 放独立包
（建议 `internal/e2e/`，仅测试文件）时按需**拷贝适配**这 6 个 helper。

## E2E 组件拆法（建议）

| 组件 | 形态 |
|---|---|
| 假面板 | httptest，`/api/dl` 返回 `{url: 假Google, headers:{Authorization}, expires_at:+1h, file:{id}}` |
| 假 Google | httptest + `http.ServeContent`（真 Range/206/416）；记录 Authorization 与请求数 |
| 假 Emby 源 | 照 `fakeEmbyOrigin` 拷贝；mediaPath = `/home/googleDrive/<gdPath>` |
| 网关 | **真实 httptest HTTP server**：注册 agent 三端点 + `/install.sh`（agent 子进程要经网络访问；播放入口用 `newRedirectContext` 直调 handler 驱动，客户端角色 = 测试自己） |
| agent | **真实二进制子进程**：`cd agent && go build -o <tmp>/gd-agent .` → `enroll --master http://127.0.0.1:<gwport> --token ... --config <tmp>/config.env --port <自由端口>` → `serve --config ...`；就绪判定 = 轮询 admin 列表在线；用例结束 kill + 清理临时目录 |
| 网关配置 | `config.C` 注入（Emby/GDrive/`AgentNetwork`）+ **`config.BasePath = t.TempDir()`**（注册表按 BasePath 惰性加载，正是"重启"试验的抓手） |
| 网关"重启"（E13） | 停掉 httptest server → 同 BasePath 起新 server → 节点经下个心跳回归可调度 |

## 两处需要适配/注意

1. **`offline-seconds` 校验下限**：配置校验要求 > 心跳周期（15s）；E9 若等真实离线判定将耗时 45s+。
   先核对 `internal/config/agentnetwork.go` 的实际校验——若硬下限 15s，E9 接受一次 ~46s 等待
   （或 Go 测试内标 `t.Short()` 跳过 + 在报告里注明）；不要伪造内部状态绕过。
2. **E12「数据面不经过网关」的自动化版**：断言"网关 server 收到的请求里只有 `/api/agent/*`，
   零字节数据路由"（等价于参考稿的日志 `/dl` 计数 = 0）；强化版"停网关后 agent 仍可续下"用
   **停 server → 立刻再发一次 Range 请求（206）** 证明（agent 直链缓存内），参考稿的
   "≥5min" 属人工冒烟项，不在自动测试里空等。

## 驱动播放入口的入口

`emby.Redirect2OpenlistLink(c)` 直调（同 A 的 `redirect_agent_test.go` 模式）；302 的 Location
即 agent URL，测试作为"客户端"对其发 GET（含 Range 用例）。
