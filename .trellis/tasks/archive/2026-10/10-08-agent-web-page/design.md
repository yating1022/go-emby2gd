# design：Web 节点页（落点）

> 技术契约 = 父任务 `design.md` §2.9 + 子任务 A 已落地的 `internal/service/agentnet/admin.go`
> （`AgentView` 视图、四个接口、恒 200 信封）。本文件只列前端落点与页面结构。

## 落点

| 文件 | 改动 |
|---|---|
| `web/src/app/routes.ts` | 加一行 `route("agent_network", "routes/agent_network/index.tsx")` |
| `web/src/app/routes/layout.tsx` | `navData`（:50-66）加导航项 |
| `web/src/app/routes/agent_network/index.tsx` | 页面主体（新建） |
| `web/src/app/routes/agent_network/components/*` | 页面私有组件（按需：行操作、安装命令弹窗） |

后端零改动。

## 页面结构（建议）

```
┌ 节点管理 ─────────────────────────────────────────────┐
│  [复制安装命令]  [刷新]                                │
│ ┌──────┬────────┬──────┬─────┬──────┬────────┬──────┐ │
│ │ 名称  │ 状态    │ 版本  │ 活跃流│ 最近心跳│ 地址    │ 操作 │
│ │ ...  │ 在线/…│ 0.2.0│ 2    │ 12:03  │ http://…│ [禁用][删除] │
│ └──────┴────────┴──────┴─────┴──────┴────────┴──────┘ │
└───────────────────────────────────────────────────────┘
```

- 状态列由 `online`/`enabled` 组合：`!enabled` →「禁用」；`enabled && online` →「在线」；
  `enabled && !online` →「离线」。
- 删除需二次确认（Dialog 或 `confirm`；行内操作失败时把后端 `message` 原样 toast）。
- 刷新：进页自动拉一次 + 手动刷新按钮（无实时推送，MVP 不轮询）。
- 未启用功能 / 密钥错误：直接把后端 `message` toast 出来，页面保留空态说明，不白屏。

## 不做

- 不做实时推送/自动轮询、不做分页/搜索（节点量级小）、不展示 `machine_id` 等次要字段（表格可省，
  需要时后续加）、不动既有页面、不引依赖（shadcn 已有组件优先，缺组件用原生元素）。
