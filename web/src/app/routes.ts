import {
  type RouteConfig,
  index,
  layout,
  route,
} from "@react-router/dev/routes";

export default [
  layout("routes/layout.tsx", [
    index("routes/index.tsx"),
    route(
      "api/openlist_local_tree",
      "routes/api/openlist_local_tree/index.tsx",
    ),
    route("agent_network", "routes/agent_network/index.tsx"),
    route("log", "routes/log/index.tsx"),
  ]),
] satisfies RouteConfig;
