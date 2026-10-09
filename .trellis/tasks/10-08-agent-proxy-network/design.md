# design：master/agent 代理网络（网关为 master）

> 协议与决策的源头 = 参考冻结稿
> `/home/debian/project/gd/.trellis/tasks/archive/2026-10/10-08-agent-proxy-network/design.md`
> （下文简称「冻结稿」，其 §2 协议 / §4 调度 / §6 风险 / §7 开关已被用户逐条确认，**沿用不重抄**）。
> 本文件是本项目**重新设计落点**的唯一技术依据：改协议 = 先改本文件（与冻结稿对应节），
> 两侧实现与固定向量测试同步。需求全集见同目录 `prd.md`，执行步骤见 `implement.md`。

## 1. 架构与边界（本项目版）

```
        ┌──────────────────────────────────────────────────────┐
        │                    master = 本网关（Go，单进程）       │
        │  ┌──────────────┐ ┌──────────────┐ ┌───────────────┐ │
        │  │ 注册表(内存)  │ │ agents.json  │ │ 调度器(最少连接)│ │
        │  │ + RWMutex    │ │ (持久化,D-d) │ │ + 签名(HMAC)   │ │
        │  └──────────────┘ └──────────────┘ └───────────────┘ │
        └───┬───────────────┬───────────────────┬──────────────┘
  ①enroll/心跳(agent→master) │ ②播放入口 302 到签名 URL ③拉直链(agent→master)
            │               ▼                    │
     ┌──────┴─────┐   客户端（播放器）            │
     │   agent    │◄────────┘  ④直连拉流(数据面)  │
     │ (Go 二进制) │  ⑤Range/流式，字节不经过网关  │
     └──────┬─────┘                             │
            ▼                                   ▼
      Google Drive ◄──────── 网关调 GD 面板换直链 ──────┘
```

| 角色 | 做 | 不做 |
|---|---|---|
| **master（本网关）** | 注册表/心跳/调度/签名；调 GD 面板换直链并下发给 agent；把客户端 302 到 agent | 不转发任何字节；不直连客户端数据面；不做 Google OAuth（直链一律经面板，见 gdrive-panel.md） |
| **agent** | 注册/心跳（主动出站）、验签、代理流（Range/流式/并发闸门）、缓存直链（向网关拉） | 不存业务库、不跑前端、不管理其他 agent |
| **客户端** | 跟随 302 直连 agent 拉流 | 永远拿不到 Google 凭据 |

- **触发点唯一**：`internal/service/emby/redirect.go:101-123` 的 `MatchMountPath` 分支。
  strm 远程代理分支（:133-161）**不接入**（面向非 GD 上游，与面板直链/agent 网络无关）。
- 前置条件：`agent-network.enable` 且 `gdrive.IsEnabled()`（直链来源是面板，gdrive 关则不调度）。
- 依赖方向：`agentnet → gdrive / config / util`（gdrive 包内核复用，见 §2.3）；不依赖 Emby 包。

## 2. 协议落地（本项目）

### 2.1 端点与鉴权

| 端点 | 方法 | 鉴权 | 响应形状（= 冻结稿 §2 字面形状） |
|---|---|---|---|
| `/api/agent/enroll` | POST | body `enroll_token` 常数时间比较 | `{agent_id, agent_secret, sign_key, heartbeat_interval_seconds:15}` |
| `/api/agent/heartbeat` | POST | `Authorization: Bearer <agent_secret>` + `X-Agent-Id` | `{ok:true, heartbeat_interval_seconds:15, enabled:<bool>}` |
| `/api/agent/download-link?file_id=` | GET | 同上 | `{url, headers, expires_at}` |
| `/install.sh` | GET/HEAD | 无（脚本本身不含密钥） | 脚本字节（见 §2.8） |

- 选字面形状而不是 `{ok,data}` 包装：agent 客户端两种都接受（`internal/api/client.go` 的
  decodeData），选字面 = 零 agent 改动、协议保真最高。
- 错误形状统一 `{"ok":false,"error":{"code":"…","message":"中文"}}` + 合理状态码
  （agent 端 `parseError` 优先取 `error.message`）。
- **枚举防护**：agent_id 不存在与 secret 错误 → 同状态（401）同文案（"凭证无效"）。
- 路由注册：`internal/constant/constant.go` 新增常量（`Reg_Agent*` 4 条 + `Route_AgentNetwork*`
  管理接口），`internal/web/route.go` 规则表插入 `Reg_All` **之前**。
  中间件不受影响：`ApiKeyChecker`（auth.go:67-77）与 `DownloadStrategyChecker` 按各自正则过滤，
  不匹配 `/api/agent/*`、`/install.sh`、`/ge2o/agent-network/*`。

### 2.2 file token（D-c）

```
file_id := base64.RawURLEncoding.EncodeToString([]byte(gdPath))   // 无填充
客户端 URL := <agent_base>/dl/<token>?e=<unix秒>&s=<hex>
签名 = HMAC-SHA256(key=sign_key, msg="v1\n<token>\n<e>") 的 hex   // 格式与冻结稿一致
```

- 字符集 `[A-Za-z0-9_-]`：无 `/`、`%`、`=` → 不触发 agent handler 的「路径含 `/` → 403」检查
  （handler.go:112-120）；token 对 agent 完全**不透明**（agent 代码零改动）。
- 网关对 token **零记忆**：download-link 收到即解码回 gdPath；重启不影响在途 URL（D-d 的配套）。
- 解码失败 → 400（`AGENT_TOKEN_INVALID`，中文 message）。
- 长度示意：路径 100 个汉字 ≈ token 400 字符 → URL < 600B，播放器/反代无压力。
- **备选与拒因**：真实 Google `file.id` + 服务端映射表 —— 重启后映射丢失，在途 URL 需等下一次
  播放请求才能重建映射（与 D-d「重启不掉节点」目标不一致）；且网关对 file.id 无任何本地用途
  （gdrive 包全程按路径键）。如将来需要隐藏路径/缩短 URL 再演进，签名格式 v1 前缀已预留版本位。

### 2.3 直链换取（网关 → 面板）

- 复用 `internal/service/gdrive/` 既有内核，**不重写**：全局 tokenSlot（账号级凭据仅一份）、
  按路径 urlCache、singleflight 合并并发刷新、30s 安全余量、代次失效重试（cache.go 全部保留）。
- gdrive 包新增第 4 个公共函数（当前公共面 3 个，`gdrive-panel.md` §2 收尾时更新为 4 个）：

  ```go
  // ResolveTarget 换取直链（不取字节）: download-link 端点用
  func ResolveTarget(ctx context.Context, gdPath string) (url string, headers map[string]string, expiresAt string, err error)
  ```

  内部 = `ensureTarget` + 字段导出。**注意**：headers 内含账号级 Authorization，
  只允许交给持 agent_secret 的 download-link 端点，绝不进日志/响应给客户端。

- **余量链不变式（R5 修正，必须遵守）**：

  ```
  agent 直链缓存余量 25s  <  网关 gdrive 余量 30s  <  面板 refresh-ahead 60s
  ```

  参考 agent linkcache 默认 5min 必须改为 25s：5min 时 agent 在 `expires_at−5min` 就要刷新，
  而网关缓存要到 `−30s` 才失效 → 网关原样返回**同一份** token → agent 重算余量仍不足 →
  该窗口内每次请求都重拉一次（病灶与 gdrive-panel.md §3.1 描述的 5 分钟余量完全相同）。
  25s 时：agent 在 `−25s` 刷新 → 网关缓存已失效（`−30s`）→ 真调面板（面板已在 60s 发放窗口内）
  → 拿到新令牌。**两侧各加断言测试**（镜像 `cache_internal_test.go` 的不等式断言写法）。

- `expires_at` 原样透传面板的 RFC3339 串（Go `time.Parse` 原生接受小数秒，参考稿「微秒」坑
  在此天然不触发，但保留解析单测）。
- **不校验「token 是否曾签发给该 agent」**：与冻结稿一致（agent 是受信基础设施，R9）。

### 2.4 播放入口接入（redirect.go）

在 `MatchMountPath` 分支内、`ProxyGDrive` 调用之前插入决策（语义伪代码）：

```go
if gdPath, ok := gdrive.MatchMountPath(embyPath); ok {
    go sendOpenStreamPlaybackInfoReqToOrigin(itemInfo)   // 既有顺序：先触发，再进入传输

    if cfg := config.C.AgentNetwork; cfg.IsEnabled() && gdrive.IsEnabled() {
        if u, ok := agentnet.PickAndSign(gdPath); ok {
            c.Header(cache.HeaderKeyExpired, cache.Duration(time.Minute*10)) // 与既有 302 一致
            c.Redirect(http.StatusTemporaryRedirect, u)
            return
        } else if 无可用节点 && !cfg.FallbackToLocal {
            c.String(503, "无可用 agent 节点且已禁用本机回退")   // 中文原因
            return
        }
    }

    // 现有流程原样保留：streamproxy.ProxyGDrive → written 契约 → 回源
}
```

- 语义分级：① 功能未启用 → 现行为，**零变化**；② 有可用节点 → 302 到签名 URL；
  ③ **无可用节点**是唯一受 `fallback-to-local` 控制的路径（true → 现行为 / false → 503）；
  ④ 任何内部故障（签名/编码异常，理论不可达）→ 记 WARN 后走现行为，**绝不因新功能让播放挂掉**。
- 日志：`[agent 网络] 调度到节点 <name>(<id>)，闸门 …`；**不打印 `s`、不打印完整签名 URL**。
- 302 响应头沿用既有 `cache.HeaderKeyExpired` 10min 惯例（与 strm 302 分支一致）。

### 2.5 注册表、调度与持久化（D-d）

数据结构（与冻结稿 §5 的 agent 表等价）：

```go
type agentRecord struct {
    ID, MachineID, Name        string
    Secret, SignKey            string    // 一律 32 字节 → 64 位 hex
    PublicBaseURL              string    // 可空（NAT 机器必填）
    ListenPort                 int
    Version                    string
    LastSeenAt                 time.Time // 零值 = 从未心跳
    LastIP                     string
    ActiveStreams              int
    Enabled                    bool
    CreatedAt, UpdatedAt       time.Time
}
```

- 内存：`map[id]*agentRecord` + `map[machineID]id`（unique 索引），`sync.RWMutex` 保护；
  心跳路径（每节点 15s）只更新内存字段，不触发磁盘写。
- 持久化：`<BasePath>/agent-network/agents.json`，schema `{"version":1,"agents":[…]}`；
  **0600**、同目录 temp+rename 原子写（做法参考 agent 的 `config.Write`）；
  启动加载（文件不存在 = 空表，正常首启；解析失败 = 启动失败，中文提示 + 处置建议）；
  **变更即存**：enroll（新增/轮换——写盘成功后才回响应，失败 500）、enable/disable、删除。
- `LastSeenAt`/`ActiveStreams` **不落盘**：重启后节点「离线」至多 15s，首个心跳补全；
  避免 15s 一次的磁盘写。
- enroll 幂等：键 = `machine_id`（agent 侧读不到 `/etc/machine-id` 时退化为 hostname）；
  重注册 = 复用记录 + 轮换 secret/sign_key + **置空 LastSeenAt**（防调度到仍持旧密钥的旧进程，
  最长 45s 窗口——旧进程心跳 401 退避重试，直至 systemd 拉起新进程）。
- 调度（D9）：候选 = `Enabled && now−LastSeenAt ≤ offline-seconds && 地址可推导`；
  取 `ActiveStreams` 最小；平局在并列集合内随机（`math/rand`，非安全用途）。节点量级小 → O(N) 线性扫。
- 地址推导：`PublicBaseURL`（去尾斜杠、仅 http/https）优先；否则
  `"http://" + net.JoinHostPort(LastIP, strconv.Itoa(ListenPort))`（IPv6 自动带括号）。
- 心跳更新：`LastSeenAt/LastIP/ActiveStreams/Version` 每次覆盖；`PublicBaseURL/ListenPort`
  **非空才覆盖**（与 env 覆盖惯例一致："没传 = 没设置"）。
- `enabled:false` 下发：心跳响应携带；agent 侧已有语义（缓存直链继续服务、停取新直链）。

### 2.6 配置段 `agent-network`

```yaml
agent-network:
  enable: false                 # 总开关; false = 与未部署本功能完全一致
  enroll-token: ""              # 注册 Token; 建议 >=16 字符; 可被环境变量 AGENT_ENROLL_TOKEN 覆盖
  offline-seconds: 45           # 离线判定; 必须大于心跳周期 15s (校验下限)
  url-ttl: 24h                  # 客户端 URL 签名时效
  fallback-to-local: true       # 无可用节点时回退本机代理; false → 503
```

- Init 惯例照 `internal/config/gdrive.go`：trim 一律；`enable=false` 时只做格式预校验
  （配错但未启用也启动期暴露）；`enable=true` 才要求 `enroll-token` 非空；
  错误信息只提字段名、**绝不回显凭据值**。
- `offline-seconds`：默认 45，校验必须 `> 心跳周期(15s)`（参考稿实测教训：设小了节点周期性判离线）。
- `url-ttl`：用项目既有 Duration 解析惯例（`emby.go:395-413` / `cache.go` 同款，可提炼共用）。
- `config-example.yml` 同步新增段 + `config_example_test.go`（:14-53 模式）增加断言。

### 2.7 签名与 crypto 工具（全新增）

- 新包 `internal/util/cryptos`（util 子包惯例，复数命名）：`RandomHex(nBytes int) string`
  （crypto/rand）；`Equal(a, b string) bool`（长度不等直接 false，等长用
  `subtle.ConstantTimeCompare`）。既有 `randoms.RandomHex` 是 math/rand，**不得**用于密钥。
- sign_key / agent_secret：一律 32 字节 → 64 位 hex（agent `main.go` 的 `signKeyBytes` 硬校验）。
- 签名：`crypto/hmac` + `sha256`，消息 `v1\n<token>\n<e>`（e = 十进制 unix 秒字符串，
  master 侧 `strconv.FormatInt(t.Unix(),10)` —— 与 agent 读到的原始 query 串同构）；
  `s` 为**小写 hex**；agent 用 `hmac.Equal` 常数时间比较。

### 2.8 安装与发布契约

- `/install.sh`：正则精确匹配 `^/install\.sh$`（`Reg_All` 之前）；脚本实体
  `internal/service/agentnet/installshell/agent-install.sh` + `go:embed`
  （embed 不能向上引用，故实体必须落在包目录树内；发布快照包含整树，仓库侧同样能找到）；
  GET/HEAD 同头：`Content-Type: text/x-shellscript`、显式 `Content-Length`、
  `Cache-Control: no-cache`；HEAD 不写 body（gin 里显式处理）。
- 安装脚本：自参考稿 `deploy/agent-install.sh` 搬迁，改动近零（`GITHUB_REPO` 默认已是
  `yating1022/go-emby2gd`；服务/用户/端口保持 `gd-agent` / 8790；`--download-base` 沙箱钩子保留）。
- `.github/workflows/release-agent.yml`：自参考稿照搬（working-directory `agent`；
  tag `agent-v*`；vet + `go test -race` 作为发布门；amd64/arm64 静态构建；checksums；Release）。
  资产名 `gd-agent-linux-{amd64,arm64}` + `checksums.txt`，与安装脚本两侧**互相 grep 校验**。
- **发布执行（D-e，用户已授权，执行前再确认）**：独立检出 `yating1022/go-emby2gd` →
  把本工作区快照同步进去（排除 `.git`，保留其自身 `.git`）→ commit → push main →
  tag `agent-v0.2.0`（版本号执行时可调）→ push tag → 等 Actions → 验证 Release
  （匿名下载 + sha256 + `version` 输出）。本工作区（go-emby2openlist）**不产生任何提交**。

### 2.9 Web 端（D-b）

- 后端（子任务 A）——`/ge2o/agent-network/*`，沿用现有 `/ge2o` 惯例（POST JSON + body `secret` +
  `model.Response`{success,message} 信封、恒 200）：

  | 接口 | 功能 |
  |---|---|
  | `POST /ge2o/agent-network/agents` | 节点列表（**脱敏：绝不回显 secret/sign_key**） |
  | `POST /ge2o/agent-network/agents/update` | `{id, enabled}` 启停 |
  | `POST /ge2o/agent-network/agents/delete` | `{id}` 删除 |
  | `POST /ge2o/agent-network/install-command` | 一键安装命令（含 enroll-token，管理员接口） |

  列表需要携带数据：`model.Response` 增加可选 `Data any \`json:"data,omitempty"\``
  （不影响现有接口的序列化）；若评审倾向不动共享模型，退化为 agentnet 本地响应结构。
  安装命令模板：`curl -fsSL <网关地址>/install.sh | sudo bash -s -- --master <网关地址> --token <enroll-token>`，
  地址由后端按请求 Host/协议推导（前端只展示与复制）。
- 前端（子任务 B）：`web/src/app/routes/agent_network/` 新页 + `routes.ts` 注册 +
  `layout.tsx:50-66` 导航项；表格（名称/ID/在线/版本/last_seen/active_streams/地址）+ 启停/删除 +
  「复制安装命令」；沿用现有页面模式（`settings_modal` 的 localStorage `api_secret`、fetch 调用）。
  无实时推送，进页拉取 + 手动刷新即可（MVP）。

### 2.10 开关、兼容与回滚

| 开关（配置） | 默认 | 作用 |
|---|---|---|
| `agent-network.enable` | `false` | **整特性回滚开关**；false = 字节级现行为 |
| `agent-network.fallback-to-local` | `true` | 无可用节点回退本机代理；false → 503 |
| `agent-network.offline-seconds` | `45` | 离线判定（须 > 心跳周期） |
| `agent-network.url-ttl` | `24h` | 客户端 URL 签名时效 |

- 回滚：改配置重启即回滚；`agents.json` 留存无害；agent 侧卸载 = `systemctl disable --now gd-agent`
  + 删除文件/网页删除节点行。
- 部署清单：`docker-compose.yml:14-20` 卷列表追加 `agent-network` 状态目录；根 `.gitignore`
  追加该目录（容器外裸跑时状态落在 BasePath）。
- 既有领域零回归：strm 分支、openlist、转码、响应缓存行为全部不动（A5 对照测试钉住）。

## 3. 数据流（时序，差异版）

（沿用冻结稿 §3.1–3.4，仅记差异）

1. **接入**：agent 安装脚本注册后立即心跳 → 网页可见；无需人工审批（D6）。
2. **一次播放**：客户端 → 网关 `/videos/.../stream` →
   `MatchMountPath` 命中 → 选点 + 签名 → **302** → 客户端直连 agent `/dl/<token>?e&s` →
   agent 验签 → 直链缓存/向网关 download-link 拉取 → 上游 Google（**网关不出现在字节链路**）。
   （对照冻结稿：原 `/api/dl` 的角色 = 本项目"播放入口决策"，语义不变、落点换到 redirect.go。）
3. **时效链**：Google token ≈1h（面板余量 60s > 网关 30s > agent 25s，三级自动续换，
   客户端 URL 24h 内全程无感）；24h 后客户端重新请求 → 网关重新选点（可能换节点）。
4. **掉线/恢复**：心跳超 45s 不再被调度（在途 URL 客户端会失败 → 重取播放 → 重新调度）；
   进程恢复后下个心跳即回归；**无自动摘除**，仅状态变化。

## 4. 安全模型与已接受风险

| # | 风险 | 说明 / 处置 |
|---|---|---|
| R1 | 注册 Token ≈ 共享盘访问权 | 复刻冻结稿；README/网页写明；轮换 = 改配置重启（全体 agent 需重新注册） |
| R2 | secret/sign_key 明文落 `agents.json` | 0600；与既有「Google refresh_token 明文」同级取舍（需取回原值才能签发/校验） |
| R3 | 客户端 ↔ agent http 明文 | D2 决策；需保密自行反代 agent 并配 `--public-url https://…` |
| R4 | 心跳 15s 的调度滞后 | D9 接受（下载场景） |
| R5 | 已发 URL 时效内不受禁停/删除影响 | 时效封顶 24h；删 agent = 密钥随行删除、其 URL 全失效 |
| R6 | 回退路径 = 本机代理 | 既有能力，不引入任何新暴露面（比冻结稿的直链回退更保守） |
| R7 | 网关是控制面单点 | agent 已缓存直链继续可服务；新拉取失败 → agent 502 → 播放侧重试/重取（可接受） |
| R8 | 地址推导用 `c.ClientIP()`（本项目无信任代理链，gin 默认信任所有代理） | enroll/heartbeat 均需凭据，XFF 伪造只能误导「自己节点」的对外地址；**NAT 机器必须 `--public-url`**；网关在反代后须正确传 XFF；推导地址被实际调度使用时可打 WARN 便于排查 |
| R9 | download-link 不校验 token 与 agent 的签发关系 | 与冻结稿一致：agent 为受信基础设施；签名绑定生效在客户端入口 |
| R10 | 新 admin API 沿用 ge2o 明文 `api-secret` 比较（非常数时间） | 与现状完全一致，不新发明机制（该 secret 为内网管理面单一凭据） |

## 5. 错误语义表（master 侧）

| 场景 | 状态 | 形状 |
|---|---|---|
| enroll_token 错误 / 缺失 | 401 | `{ok:false,error:{code:"ENROLL_TOKEN_INVALID",…}}` |
| enroll 参数非法（端口越界 / base url 形状 / machine_id 空） | 400 | 中文 message |
| enroll 写盘失败 | 500 | 中文 message（**不得**先回成功再写盘） |
| heartbeat / download-link 凭证无效（含 agent 不存在） | 401 | 统一文案 |
| download-link token 解码失败 | 400 | `AGENT_TOKEN_INVALID` |
| download-link 面板取链失败 | 502 | `AGENT_LINK_UNAVAILABLE` + 面板中文 message 原样（经 redact 处理） |
| agent 数据面（验签失败 403 / 并发满 503+Retry-After / 上游透传…） | — | 冻结稿 §2.5 原样（agent 代码不改） |

## 6. 关键陷阱（本项目版，实现者必读）

1. **本链路无 302**：`gdrive-panel.md` §3.3 已实测（10/10 返回 200 无 Location）。
   **不得**在注释/文档里写「Google 会 302 / 跨域会丢 Authorization」——参考稿该条对本链路是错的。
   agent 侧保留 Go 默认重定向行为与「两跳头差异」测试，仅作惰性防线。
2. **余量链 25s < 30s < 60s**（§2.3），两侧各加断言测试；改任何一级先读 gdrive-panel.md §3.1。
3. **machine_id 幂等** + 重注册置空 `last_seen`（旧进程最长 45s 窗口 401 退避，等待被拉起）。
4. **`offline-seconds` 必须 > 心跳周期**（默认 45 > 15；配置层下限校验）。
5. **`/install.sh` 注册在 `Reg_All` 之前**；HEAD 与 GET 同头但不写 body。
6. **比较一律常数时间**（enroll_token / agent_secret / 签名）；用新 `cryptos` 包。
7. **token 字符集** `[A-Za-z0-9_-]`；`e` 十进制、`s` 小写 hex —— 与 agent 端读取原始 query 串同构。
8. **expires_at 微秒**：Go 原生可解析，仍保留单测防回归。
9. **`agents.json` 原子写 + 0600 + 损坏 fail-fast**（宁可启动失败，不可静默清空注册表）。
10. **心跳 401 时 agent 不退出**（退避 + ERROR 日志 + 提示人工重注册）——agent 既有语义保持。
11. **安装脚本 enroll 后 chown `gd-agent`**（否则 serve EACCES，看似永久离线）；
    重跑脚本幂等**不轮换密钥**（`--force-enroll` 才重注册）。
12. **数据面不经过网关的证明法**：驱动流量后网关日志 `/dl` 计数 = 0；强化版 = 下载一次入缓存后
    停网关，同一文件仍可持续下载 ≥5min。
13. 调度读数来自心跳（15s 滞后）——接受精度，不加实时通道。
14. 发布快照同步时**排除 `.git`**；安装命令里的地址 = 浏览器同源地址（由后端按请求推导）。

## 7. 子任务拆分与交付边界

| 任务 | 范围 | 独立验收 |
|---|---|---|
| **父任务（本任务）** | 协议落点（本文件）、E2E 集成矩阵、发布执行（D-e）、spec 更新与文档 | `implement.md` 的 E2E 清单全过 |
| **子任务 A：网关 master 侧** | config 段、cryptos util、注册表+持久化、3 个 agent 端点、调度、签名、播放入口接入、/install.sh、admin API、部署清单（compose 卷 / .gitignore / config-example） | 无真实 agent 下 enroll/心跳/调度/签名/download-link/回退全部可测；`go test ./...` 全绿；开关关闭时现行为字节级一致 |
| **子任务 B：Web 节点页** | 前端页面 + 对接 A 的 admin API（契约见 §2.9；**依赖 A**，排在 A 之后） | 页面可打开；列表/启停/删除/复制安装命令可用 |
| **子任务 C：agent 侧** | 搬 `agent/`（余量修正 25s+断言、单测、`go vet`、mockmaster 适配）、安装脚本落位、`release-agent.yml` | `go test -race ./...` 全绿；对 mock master 全链路可跑；workflow 语法自检 |

- 依赖秩序：B 依赖 A 的 admin API 落地；A 与 C 可并行（两侧只靠本文件协议）。
- E2E（父任务）在 A/B/C 完成后执行；发布执行（D-e）在 E2E 通过后、经用户确认再操作。
