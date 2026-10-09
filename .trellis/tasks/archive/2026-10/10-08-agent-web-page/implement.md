# implement：Web 节点页（B1–B3 顺序清单）

> 上下文顺序：本任务 `prd.md` → `design.md` → `research/frontend-conventions.md`。
> 硬性约束：**不改任何 Go 代码**；不引入新依赖；不提交 git；只动 `web/src/**`。
> `web/dist` 是构建产物，`./build_web.sh` 会重建它（embed 需要）。

- [x] B1 路由与导航：`web/src/app/routes.ts` 注册 `agent_network`；`layout.tsx` `navData` 加
      「节点管理」；新建 `routes/agent_network/index.tsx` 骨架（先渲染空壳）。
      验证：`./build_web.sh` 通过
- [x] B2 页面实现：节点表格 + 状态组合逻辑（禁用/在线/离线）+ 启停/删除（删除二次确认）+
      「复制安装命令」（fetch → `data.command` → `navigator.clipboard.writeText` → toast）+
      刷新；secret 走 `LOCAL_STORAGE_KEY_API_SECRET`，未设置时 toast「请先设置接口密钥」；
      失败时把后端 `message` 原样提示。组件优先复用 `web/src/app/components/ui/*` 已有件。
      验证：构建通过；与 A 的 `admin.go` 契约逐字段核对（AgentView 字段名不得拼错）
- [x] B3 冒烟（实测端口 38211，证据见交付报告）：
      1) `export PATH=/usr/local/go/bin:$PATH && go build -o /tmp/ge2o .`
      2) 临时数据根（如 `/tmp/ge2o-webtest/config.yml`）：`emby.host` 填占位、`ge2o.api-secret` 设值、
         `agent-network.enable: true` + `enroll-token: dev-token`、缓存/ssl 关闭；
         端口选非占用（如 38196）
      3) `/tmp/ge2o -dr /tmp/ge2o-webtest` 起服务
      4) `curl -X POST localhost:38196/ge2o/agent-network/agents -d '{"secret":"…"}'` 等四个接口实测
      5) `curl -s localhost:38196/ge2o/web/agent_network` 返回 index.html（SPA 回退）
      6) 清理进程与临时目录（trap 或显式）

## 回滚点

纯前端新增 + 两处小改（routes.ts / layout.tsx），删除页面目录并还原两处改动即可回滚；
不影响任何既有页面路由（`./build_web.sh` 全量重建）。
