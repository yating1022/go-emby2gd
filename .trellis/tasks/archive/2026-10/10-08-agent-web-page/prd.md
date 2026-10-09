# Web 节点页（列表/启停/复制安装命令）

> 来源：父任务 `10-08-agent-proxy-network`（已批准执行）。
> 依赖（写死）：**后端接口由子任务 A 提供（已落地）**，本任务只做前端页面与对接，不改任何 Go 代码。

## Goal

在 `/ge2o/web` 管理界面新增「节点管理」页：查看 agent 节点列表（脱敏视图）、启停 / 删除节点、
一键复制安装命令（含注册 Token）。

## 接口契约（子任务 A 已落地；均为 POST JSON、恒 200，靠 `success`/`message` 判定）

| 接口 | body | 成功返回 |
|---|---|---|
| `/ge2o/agent-network/agents` | `{secret}` | `{success, message, data:{agents: AgentView[]}}` |
| `/ge2o/agent-network/agents/update` | `{secret, id, enabled}` | `{success, message}` |
| `/ge2o/agent-network/agents/delete` | `{secret, id}` | `{success, message}` |
| `/ge2o/agent-network/install-command` | `{secret}` | `{success, message, data:{command, master_url}}` |

`AgentView` 字段：`id, name, machine_id, enabled, online, version, last_seen_at`（RFC3339 UTC 或空串）、
`last_ip, active_streams, public_base_url, address, listen_port, created_at, updated_at`。
未启用功能时 `message` =「agent 网络未启用, 请先在配置文件中开启 agent-network.enable」；
密钥错误 =「密钥错误」；空列表仍带 `data.agents: []`。

## Requirements

- B-R1 `web/src/app/routes.ts` 注册 `route("agent_network", "routes/agent_network/index.tsx")`。
- B-R2 `layout.tsx` `navData`（:50-66）增加导航项（建议 label「节点管理」，to `/agent_network`）。
- B-R3 页面内容：节点表格（名称 / 状态[在线·离线·禁用] / 版本 / 活跃流 / 最近心跳 / 地址 / ID）+
  行操作（启用↔禁用、删除需确认）+ 顶部操作（「复制安装命令」「刷新」）；
  空态 / 失败 / 未配置密钥 均有明确提示（`toast` + 页面空态）。
- B-R4 沿用既有模式：secret 从 localStorage `LOCAL_STORAGE_KEY_API_SECRET` 读取（未设置时 toast 提示
  「请先设置接口密钥」）；fetch POST JSON 模式照抄
  `routes/api/openlist_local_tree/components/update_request_collapse.tsx:80-110`。
- B-R5 复制安装命令：调用接口拿 `data.command` 复制到剪贴板（是否在弹窗回显 command 由实现定；
  回显时注意它**含注册 Token**，该页属管理员界面）。不引入任何新依赖。
- B-R6 不改任何 Go 代码；不影响既有页面行为。

## Acceptance Criteria

- [ ] `./build_web.sh` 构建通过（web/dist 产出）
- [ ] 本地起网关（临时配置：`ge2o.api-secret` + `agent-network.enable: true` + enroll-token，
  非占用端口）→ `GET /ge2o/web/agent_network` 返回 SPA 页面（index.html）
- [ ] 四个接口 curl 实测可用；页面交互使用同契约（以代码审查 + 接口实测共同证明）
- [ ] 未配置密钥 / 功能未启用时，页面提示与后端 `message` 一致（不吞错、不白屏）

## Notes

- 视觉与交互参照 `routes/log` 与 `routes/api/openlist_local_tree` 两个既有页面。
- 详细步骤见 `implement.md`；前端惯例速查见 `research/frontend-conventions.md`。
