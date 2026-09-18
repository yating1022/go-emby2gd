# Implement · 接入 GD 管理面板直链接口

> 约定：本仓库**不提交代码**，改动只留工作区。
> Go 不在 PATH，每条命令前先 `export PATH=$PATH:/usr/local/go/bin`。

## 0 准备

- [ ] `export PATH=$PATH:/usr/local/go/bin`
- [ ] 记录改动前基线：`git status --short`（回滚点 R0）
- [ ] 通读 `GD_DIRECT_LINK_API.md` §3 / §5，确认契约
- [ ] ⚠️ 该文档 **§6.2 不适用本链路**（实测零重定向，见 design §5.4）：
  不要照它加自定义 `CheckRedirect`，也**不要在任何代码注释里复述「Google 会 302」** ——
  那会引导后来的人去修一个不存在的问题

## 1 出站 HTTP：确认不需要新增辅助（已定论，不要在此展开）

`GD_DIRECT_LINK_API.md` §6.2 要求的「跨主机跳转保留 `Authorization`」不适用本场景，
**实测 10/10 零重定向**，详见 design §5.4。本步骤不写任何代码，只做确认：

- [ ] `internal/util/https` **保持不动**
- [ ] 面板调用与拉流都用现有的 `https.Get(...).DoSingle()`（不自动重定向）
- [ ] 拉流只接受 200/206，3xx 与其余状态码判为失败并回退

## 2 配置层

- [ ] `internal/config/gdrive.go` 重写：
  - 字段收敛为 `Enable` / `ApiBase` / `ApiToken` / `MountPrefix`
  - `GDriveApiTokenEnvName = "GDRIVE_API_TOKEN"`；`Init()` 中环境变量非空即覆盖 `ApiToken`
  - `api-base` 归一化 + http/https 校验（复用 `url.Parse`，给出中文报错）
  - 启用时 `api-base` / `api-token` 非空校验；错误消息只提字段名
  - 保留 `IsEnabled()`；删除 `PathCacheExpire()`
  - 保留 `validateGDriveMountPrefix` / `isLoopbackHost`（若 `isLoopbackHost` 不再被引用则一并删除）
- [ ] `internal/config/gdrive_test.go` 重写：默认值、归一化、非法地址、启用时缺项、环境变量覆盖
- [ ] `internal/config/gdrive_mount_test.go` 保留并瘦身
- [ ] `internal/config/config_example_test.go` 断言改为 `gdrive.enable=false` / `api-base` / `mount-prefix`
- [ ] 验证：`go test ./internal/config/...`

## 3 gdrive 包重写

- [ ] 删除 `oauth.go`、`token.go`、`api.go`、`resolve.go` 及对应 `_test.go`
- [ ] `sanitize.go`：只保留 `redactSecret(text, secret)`（过短的值不替换）与
  `redactConfigSecrets`，凭据来源为 `cfg.ApiToken`
  - **实施时的偏离**：计划里写的"迁入 `sanitizeUpstreamText` / `whitelistUpstreamText` /
    `isUpstreamTextRune`"没有做。那套 ASCII 字符白名单是给 Google OAuth 错误页准备的，
    面板文案是中文，套上去会把整句打成 `?`，正好毁掉"直接沿用面板 message"这条诊断链；
    而 Google 侧现在只请求媒体地址、其响应体不进日志，白名单也没有第二个消费者。
    删掉比留着更诚实，理由已写进 design + `spec/backend/gdrive-panel.md` §4
- [ ] `mountpath.go`：自 `resolve.go` 迁入 `MatchMountPath` / `normalizeMountPrefix`，逻辑不变
- [ ] `type.go`：`directLink`、`panelEnvelope`、`panelError`、`tokenEntry`、`target`、`fetchError`
- [ ] `panel.go`：`fetchDirectLink(ctx, gdPath)`，含 URL 编码、超时、错误信封解析、中文 message 透传
- [ ] `cache.go`：**两级缓存**
  - 令牌槽（全局唯一）：`getToken` / `putToken`；TTL 由 `expires_at` 推导
  - URL 缓存（按路径）：`getCachedURL` / `putCachedURL`；**不设自身过期时间**，
    条目带写入时的令牌代次（见 §7.5）
  - `ensureTarget(ctx, gdPath, minGeneration)`：一次面板调用同时刷新两者，包在 singleflight 里
    （**按路径**合并），组内用 `context.WithoutCancel(ctx)` + `panelRequestTimeout`
  - 常量 `panelTokenRefreshAhead(60s)` / `linkCacheSafetyMargin(30s)` /
    `maxLinkCacheTTL(1h)`，并**断言 `linkCacheSafetyMargin < panelTokenRefreshAhead`**
- [ ] `fetch.go`：`fetchDirect(ctx, target, clientRange)`，走 `https.Get(...).DoSingle()`，
  只接受 200/206，失败返回带状态码的 `*fetchError`
- [ ] `gdrive.go`：包文档重写 + `IsEnabled` + `FetchStream`
  （编排：`ensureTarget` → `fetchDirect` → 失败且可重试时把 `minGeneration` 抬到本次代次 → **最多重试一次**）
- [ ] 日志：复用 `log.go` 的 `[直链代理]` 前缀；任何位置都不打印 headers / token / 面板地址带 token
- [ ] 测试：
  - `panel_internal_test.go`：成功解析；`path` 编码（中文/空格/括号/`&`）；`Authorization` 头存在；
    各类错误码（401/404/400/422/502）都走同一个「原样透传面板中文 message + 回退」路径，
    不做按 code 的特殊分支；面板 5xx 同样透传
  - `cache_internal_test.go`：
    - 令牌槽与 URL 缓存是**两件独立的东西**：令牌失效时 URL 缓存仍在（反之亦然）
    - TTL 计算、边距不足不写令牌槽、`expires_at` 零值不写、**格式非法不写**
    - **不变式断言**：`linkCacheSafetyMargin < panelTokenRefreshAhead`
      （防止以后有人把余量调大，把 §6.2 那个 240 秒窗口调回来）
    - 令牌全局唯一：请求两个不同路径后，令牌槽只有一份
  - `longplay_internal_test.go`（长播放不中断，对应 PRD A10.3/A16/A17/A18）：
    - 假面板返回 `expires_at = now+1h`，取流成功；**把令牌槽改成已过期**，再取一次，
      断言假面板**被再次调用**且第二次取流成功（模拟播放跨过 1 小时边界）
    - 假 Google 第一跳返回 401，断言清缓存 + 重取一次 + 第二次成功
    - 401 连续两次 → 返回错误（交由上层回退），**不得**无限重试
    - **并发 401**：多个 goroutine 同时取流且令牌已失效，断言假面板的调用次数为 1
      （singleflight 生效，没有 N 次并发重取）
  - `fetch_internal_test.go`：`Authorization` 与其他 `headers` 原样发出（用 `httptest` 断言收到的请求头）；
    `Range` 转发；200/206 放行；3xx 判为失败并关闭响应体；401/403 清缓存重取一次后成功；连续两次 → 返回错误
  - `sanitize_internal_test.go`：自旧 `token_internal_test.go` 迁移现有用例，
    改为断言 `ApiToken` 被替换成 `***`
  - `mountpath_internal_test.go`：保留
  - `gdrive_internal_test.go`：`FetchStream` 端到端（假面板 + 假 Google）串起来；禁用时不发请求
- [ ] 验证：`go test ./internal/service/gdrive/...`

**审查门 G2**：`MatchMountPath` 行为与旧实现逐条一致；面板中文 message 未被脱敏破坏；
headers 不出现在任何日志断言中。

## 4 streamproxy 接线

- [ ] `internal/service/streamproxy/link.go`：删除第 0 步 gdrive 分支与 `gdrive` import
- [ ] `internal/service/streamproxy/streamproxy.go`：更新 `ProxyGDrive` 文档注释（逻辑不变）
- [ ] `internal/service/streamproxy/streamproxy_gdrive_test.go`：改为假面板端点，断言 URL 编码、headers 透传
- [ ] `internal/service/streamproxy/link_internal_test.go`：删 gdrive 分支用例
- [ ] 验证：`go test ./internal/service/streamproxy/...`

## 5 emby 接线与路由清理

- [ ] `internal/service/emby/redirect.go`：仅更新注释与日志文案，不移动分支位置
- [ ] `internal/service/emby/redirect_gdrive_test.go`：改为假面板
- [ ] `internal/constant/constant.go`：删两个 OAuth 路由常量
- [ ] `internal/web/route.go`：删两条注册与 import
- [ ] `internal/web/route_internal_test.go`：删对应用例
- [ ] 验证：`go test ./internal/service/emby/... ./internal/web/...`

## 6 配置样例

- [ ] `config-example.yml` 重写 `gdrive:` 段：
  `enable` / `api-base` / `api-token`（含 `GDRIVE_API_TOKEN` 覆盖说明）/ `mount-prefix`
  删除 OAuth、drive-id、path-cache-expired、max-path-depth 等项

## 7 全量验证

- [ ] `go build ./...`
- [ ] `go vet ./...`
- [ ] `go test ./...`
- [ ] `grep -rn "oauth\|refreshToken\|ClientSecret\|DriveId\|files.list\|alt=media" internal/ --include=*.go`
      确认无残留
- [ ] `grep -rn "gdrive" internal/ --include=*.go | grep -v _test` 人工过一遍调用面
- [ ] 手工冒烟（可选，需真实面板）：
  - 构造一个指向挂载路径的 strm，`enable: true`，观察日志是否出现
    `[直链代理] 检测到 Google Drive 挂载路径` → `取直链成功` → `开始传输`
  - 面板 Token 故意配错，确认日志出现面板中文文案且**回源**，播放不中断
- [ ] **长播放验证（强烈建议，对应 PRD A16 与 design §6.5 的未验证假设）**：
  播一部超过 1 小时的片子（最好是拖进度条、反复 seek），确认：
  - 1 小时边界处**没有播放失败**；若出现短暂卡顿，日志里应能看到「清除缓存重取」并继续
  - 日志中面板调用次数是 3~4 次量级，而不是每个 Range 请求一次

## 7.5 独立检查后的修复（2026-09-18）

`trellis-check` 独立复查后修掉的问题，均已补测试：

- [x] **失效重试没有重新取直链**（高）：重试判据原本只看令牌代次。若两次尝试之间恰好有别的
  请求刷新过令牌，代次就"看起来更新了"，重试会复用本路径上那份**失效的旧直链**再打一次，
  白白回退。修法：URL 缓存条目也记一个写入时的令牌代次，`cachedTarget` 要求两者都比
  `minGeneration` 新。回归用例 `TestFetchStream_RetryRefreshesStaleDirectLink`
  （已变异验证：去掉判据即变红）；反向用例
  `TestFetchStream_SecondRequestReusesURLAfterUnrelatedTokenRefresh` 防止矫枉过正
- [x] **`api-base` 接受 userinfo 且报错回显原值**（安全）：`https://user:secret@panel` 会被
  接受，口令随后可能经请求错误进日志；查询串里的口令则会在启动报错时被打进 journal。
  修法：拒绝 userinfo，并且这几类报错不再回显配置值（只对非敏感的 scheme 保留回显）
- [x] **文档漂移**：PRD A11/A17、design §13 仍写"5 分钟边距"（正是被判为病态的那个取值）；
  design §6.4 写 singleflight key 为 `"panel-token"`，实现是按路径；`config.go` 的 GDrive
  注释仍写"Google Drive API 直接取流配置"
- [x] **`.gitignore` 的 `gdrive-token.json`**：令牌落盘已随 OAuth 删除，规则指向不存在的产物

未修、已知并接受的项（见检查报告的诚实记录）：

- `urlCache` 无上限（design §6.3 已决策：进程重启即清空；每个播放过的文件留一条字符串）
- `ProxyGDrive` 复用 `emby.strm.proxy.max-concurrent-streams`；该段缺省时无并发上限
  （Zouter 已配 16，不受影响；已在 spec §7 记录这处耦合）
- `MatchMountPath("/home/googleDrive/")` 返回 `ok=true, path="/"`，会多一次必然失败的面板调用
- `putToken` 在一份 `expires_at` 不可用的响应上会覆盖全局令牌槽（方向安全：宁可多打面板，
  也不复用来历不明的凭据）

## 8 回滚点

| 点 | 触发条件 | 回滚方式 |
|---|---|---|
| R0 | 改动前基线 | `git checkout -- <改动文件>` |
| R1 | §3 完成后发现面板契约与文档不符 | `enable: false` 关开关 + `git checkout` gdrive 包与 config |
| R2 | 上线后行为异常 | 优先 `enable: false`（零代码回滚），再按 R1 逐文件还原 |

## 9 完成标准

- 本文件 §7 全部通过
- PRD §6 的 A1–A15 每条都有对应测试或明确的验证记录
- 无自建 Drive API 残留（OAuth / 解析 / 取流 / 相关配置）

## 10 收尾记录（2026-09-18）

### 已交付

| 项 | 结果 |
|---|---|
| 代码 | 工作区改动 38 个路径；交付到私有仓库 `github.com/yating1022/go-emby2gd`（单一初始提交 `f3b4791`，作者 yating1022 <1847997653@qq.com>）。本仓库 HEAD 保持干净基线，零提交 |
| 构建 | `go build ./...` / `go vet ./internal/...` / `gofmt` 干净；四个受影响包 `-race` 全过 |
| 独立检查 | `trellis-check` 跑过一轮，抓到 1 个高优 bug（见 §7.5）并已修复 + 补回归用例 + 变异验证 |
| 凭据扫描 | 面板 Token、Google client_secret/client_id、团队盘 ID、ge2o api-secret 均未进仓库 |

### 生产部署（Zouter 155.117.82.69）

二进制 `6e5b10c0…`，配置 `gdrive` 段换成 enable/api-base/api-token/mount-prefix（权限 600），
服务 active、监听 8099、RSS 21.8 MB。回滚物料 `ge2o.bak.before-panel` / `config.yml.bak.before-panel` /
`gdrive-token.json.bak.before-panel`。

**实测播放通过**：1 次面板调用、5 次传输、140.5 MB、0 次回源、0 次面板错误。日志按
`grep '\[直链代理\]' /var/log/ge2o/ge2o.log` 可还原。

### 遗留（未完成，需人工）

1. **长播放未验**（PRD A16）：实测那次只有几分钟，未跨过 1 小时令牌边界。
   需播一部超过 1 小时的片子，确认日志里出现第二次 `已换取直链` 且连接不断。
2. **Zouter 回源源是断的**（既有问题，非本次引入）：`emby.host: http://158.69.244.4:8099`
   在 OVH 上未 publish（Emby 在容器 mediavault 内，8092-8099 全部 expose 但未 publish，
   也无 DNAT）。后果：面板直链一旦失手，回退到回源同样是失败的 —— design 里写的
   "回退是安全网" 在该主机上不成立。需先确认 Emby 实际暴露地址再改配置。
3. **面板缓存覆盖不足**：`/影视库/媒体库/电视剧/...` 等路径返回 `PATH_NOT_IN_CACHE`。
   面板侧需开启 `DL_LIVE_FALLBACK_ENABLED` 或补扫描任务。
4. `urlCache` 无上限、`ProxyGDrive` 借用 `emby.strm.proxy.max-concurrent-streams`
   （该段缺省时无并发上限）—— 见 §7.5 的"未修、已知并接受"清单。
