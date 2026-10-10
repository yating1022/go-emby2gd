# design：agent IPv6 客户端接入（只做客户端→节点）

## 0. 现状核查（2026-10-10 已读代码确认）

**客户端→节点 v6 链路已具备，本任务 = 固化 + 体验 + 验证，生产代码几乎零改动：**

- `agentBaseURL`（`schedule.go:91`）用 `net.JoinHostPort` 组装（v6 自动补方括号）；
- 心跳每拍上报 `public_base_url`（`heartbeat.go:154` `optionalString`），master 收到后**非空覆盖**（`registry.go:315`）；
- `parsePublicBaseURL`（`schedule.go:119`）基于 `url.Parse`，接受 v6 字面量；
- `PUBLIC_BASE_URL` 已是 config.env 键（enroll 写出，`config.go:200/250`）。

## 1. 需要补的（全部是小项）

1. **master 测试**（生产代码零改动）：`parsePublicBaseURL` v6（带端口/无端口/非法拒绝）；
   `agentBaseURL` 的 `LastIP=2001:db8::1` → `http://[2001:db8::1]:8790`；`sign.go` 以 v6 基址出客户端 URL 全链。
2. **安装脚本**（`installshell/agent-install.sh`）：幂等/升级路径给 `--public-url` 时，
   **锚定 sed 只替换 `PUBLIC_BASE_URL` 行**（失败不改文件）→ 重启 → 打印新值；帮助文本补 v6 示例：
   `--public-url 'http://[2408:xxxx::1]:8790'`。首次 enroll 路径不变（enroll 已写该键）。
3. **文档**：spec 的 agent-network.md 补「v6 客户端接入」小节（配置方法 + 边界）。

## 2. 边界与风险（如实记录）

- **客户端必须有 v6**：无 v6 的客户端连 `[v6]` 地址会失败——按客户端网络选择，或继续用默认（按来源 IP 推导）。
- **master 侧预热可能到不了 v6 节点**：预热请求由 master（单栈 v4）发出；若节点 URL 为纯 v6 且 master 无 v6，
  预热静默降级（WARN），起播退化为"客户端首触触发预取"——功能不受损，仅收益路径少一条。
- 节点 `net.Listen(":8790")` 为 Go 默认双栈（Linux 上 `::` + v6only=0），v4/v6 客户端均可连；节点防火墙需放行 v6（部署注意）。
- **出站不改**（用户明确）：节点→Google 保持 Go 默认拨号。

## 3. 测试

- master：上节 §1.1 全部用例（`-race`）；既有用例不回归。
- 安装脚本：沙盒（`GD_AGENT_INSTALL_ROOT`）内 `--public-url` 幂等路径写通用例：替换收敛在单行、
  非法场景不改文件。
- 真机（A3，随发布）：v6 节点 `PUBLIC_BASE_URL=http://[v6]:8790` → 节点页显示 v6 → 客户端 307 Location 为 v6 → 成功拉流。
