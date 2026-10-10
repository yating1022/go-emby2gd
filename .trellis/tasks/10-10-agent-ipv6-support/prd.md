# agent IPv6 客户端接入配置

> 设计见 `design.md`；执行见 `implement.md`。
> **范围修订（2026-10-10 用户明确）**：只做「客户端→节点 走 v6」；**节点→Google 出站保持现状、零改动**。

## Goal

让有 v6 的客户端通过 **IPv6 地址** 直连 agent 节点：节点配置 `PUBLIC_BASE_URL=http://[v6]:8790` 后，
经心跳上报，master 签发的所有客户端 URL（307 Location / 签名地址）自动使用该 v6 地址。

## Requirements

- **N1 链路固化**：`PUBLIC_BASE_URL` → 心跳每拍携带 → master 非空覆盖 → `agentBaseURL` 归一化（v6 方括号）
  → 签名 URL。补全 v6 用例：`parsePublicBaseURL`（带端口/无端口/非法）、`agentBaseURL`（v6 `LastIP`）、
  签名全链（v6 基址）。
- **N2 安装脚本**：幂等/升级路径支持 `--public-url`（锚定替换 config.env 的 `PUBLIC_BASE_URL` 行 +
  重启 + 打印）；帮助文本补 v6 示例。
- **N3** 零协议变更、零默认行为变化（不设置 = 按来源 IP 推导，与现状一致）。
- **N4 明确不做**：节点→Google 出站地址族控制（保持现状）。
- **N5** 随 v0.3.1 发布（与 v2 前缀先行同一版本）。

## Acceptance Criteria

- [ ] **A1** master：v6 `public_base_url` / v6 `last_ip` / 签名全链用例（**生产代码零改动**——链路已具备，只补测试）。
- [ ] **A2** 安装脚本：`--public-url` 幂等路径写通、失败不改文件（沙盒用例）。
- [ ] **A3** 真机（随发布）：一台有 v6 的节点设 v6 `PUBLIC_BASE_URL` → 节点页地址为 v6 → 客户端 307 落到 v6 地址并成功拉流。

## Out of Scope

- 节点→Google 出站（保持现状；用户明确）。
- 节点→master 地址族（master 单栈 v4；如需走 v6 用 v6 形态的 MASTER_URL）。
