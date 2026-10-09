# research：前端惯例速查（写页面必须遵守）

来源：2026-10-08~09 对仓库的实际勘察（主会话 + 探查子代理），文件锚点可直接核对。

## 栈与构建

- React Router 7 SPA（`ssr:false`）、Vite、Tailwind v4、shadcn/radix；`basename`/`base` 均为
  `/ge2o/web/`（`web/src/react-router.config.ts:5,7`、`web/src/vite.config.ts:8`）。
- 构建：根目录 `./build_web.sh`（`npm ci && npm run build` → `web/dist`）；`web/dist` gitignore，
  但 `web/embed.go` 用 `//go:embed all:dist`——**Go 构建前必须先构建前端**。
- 服务：`internal/web/handler.go` 的 `handleWebStatic`（`Route_Web="/ge2o/web"`），SPA 未知路径回退
  index.html；受 `config.C.Ge2o.Web.IsEnabled()` 门控。

## 路由与导航

- 路由表：`web/src/app/routes.ts` —— `layout("routes/layout.tsx", [...])` 下逐个
  `route("路径", "routes/<目录>/index.tsx")`（现有：`api/openlist_local_tree`、`log`）。
- 导航：`web/src/app/routes/layout.tsx:50-66` 的 `navData`（label/to/children；
  顶级且无 children 的项渲染为可直接点击的 NavigationMenuLink）。
- 新页面目录惯例：`web/src/app/routes/<name>/index.tsx` + 页面私有组件放其下 `components/`。

## 密钥与请求

- localStorage 键常量：`LOCAL_STORAGE_KEY_API_SECRET = "api_secret"`，导出在
  `web/src/app/components/settings_modal/settings_modal.tsx:24`；顶部设置弹窗负责写入/校验
  （校验走 `POST /ge2o/secret/validate`）。
- 请求范例（照抄对象）：`web/src/app/routes/api/openlist_local_tree/components/update_request_collapse.tsx:80-110`
  —— `fetch("/ge2o/...", {method:"POST", headers:{"Content-Type":"application/json"},
  body: JSON.stringify({secret, ...})})` → 校验 `fetchState.ok && status==200` →
  `res = {success, message}` → `!res.success` 时把 `message` 交给提示。
- 提示：`sonner` 的 `toast`（`toast.info/error/success`）。

## UI 组件

- shadcn 组件在 `web/src/app/components/ui/*`（button/dialog/dropdown-menu/field/input-group/
  navigation-menu/spinner/table?——按需先用 `ls web/src/app/components/ui` 确认已有组件，
  缺 table 时可用 `<table>` 原生或先看 shadcn 组件是否已存在，**不引入新依赖**）。

## 现状

- 既有页面：首页、OpenList 本地目录树、日志（WS `ws(s)://…/ge2o/ws/log/sync?secret=`）。
- 无任何前端测试设施；验收靠构建通过 + 本地起服务手测。
