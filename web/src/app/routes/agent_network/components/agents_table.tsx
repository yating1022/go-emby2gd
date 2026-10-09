import { Button } from "~/components/ui/button";
import { Spinner } from "~/components/ui/spinner";
import type { AgentView } from "../types";

type Props = {
  agents: AgentView[];
  /** 正在执行行操作的节点 id, 空串表示当前没有行操作在进行 */
  actingID: string;
  onToggle: (agent: AgentView) => void;
  onDelete: (agent: AgentView) => void;
};

/** 节点列表表格 */
export default function AgentsTable({
  agents,
  actingID,
  onToggle,
  onDelete,
}: Props) {
  return (
    <div className="w-full overflow-hidden rounded-xl bg-card text-card-foreground shadow-xs ring-1 ring-foreground/10">
      <div className="w-full overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b">
              {[
                "名称",
                "状态",
                "版本",
                "活跃流",
                "最近心跳",
                "地址",
                "ID",
                "操作",
              ].map((title) => (
                <th
                  key={title}
                  className="h-10 px-4 text-left align-middle font-medium whitespace-nowrap"
                >
                  {title}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {agents.map((agent) => {
              const acting = actingID === agent.id;
              return (
                <tr
                  key={agent.id}
                  className="border-b last:border-0 hover:bg-muted/50"
                >
                  <td className="px-4 py-2.5 align-middle whitespace-nowrap">
                    {agent.name || "-"}
                  </td>
                  <td className="px-4 py-2.5 align-middle whitespace-nowrap">
                    <AgentStatus agent={agent} />
                  </td>
                  <td className="px-4 py-2.5 align-middle whitespace-nowrap">
                    {agent.version || "-"}
                  </td>
                  <td className="px-4 py-2.5 align-middle whitespace-nowrap">
                    {agent.active_streams}
                  </td>
                  <td className="px-4 py-2.5 align-middle whitespace-nowrap">
                    {formatTime(agent.last_seen_at)}
                  </td>
                  <td
                    className="max-w-[16rem] truncate px-4 py-2.5 align-middle"
                    title={agent.address || undefined}
                  >
                    {agent.address || "-"}
                  </td>
                  <td className="px-4 py-2.5 align-middle whitespace-nowrap font-mono text-xs text-muted-foreground">
                    {agent.id}
                  </td>
                  <td className="px-4 py-2.5 align-middle whitespace-nowrap">
                    <div className="flex items-center gap-2">
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={acting}
                        onClick={() => onToggle(agent)}
                      >
                        {acting && <Spinner data-icon="inline-start" />}
                        {agent.enabled ? "禁用" : "启用"}
                      </Button>
                      <Button
                        size="sm"
                        variant="destructive"
                        disabled={acting}
                        onClick={() => onDelete(agent)}
                      >
                        删除
                      </Button>
                    </div>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}

/** 状态列: 由 enabled / online 组合出 禁用 / 在线 / 离线 */
function AgentStatus({ agent }: { agent: AgentView }) {
  let label = "离线";
  let className = "bg-amber-500/15 text-amber-600 dark:text-amber-400";
  if (!agent.enabled) {
    label = "禁用";
    className = "bg-muted text-muted-foreground";
  } else if (agent.online) {
    label = "在线";
    className = "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400";
  }

  return (
    <span
      className={`inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium ${className}`}
    >
      {label}
    </span>
  );
}

/** RFC3339 UTC 时间串转本地时间展示; 空串 = 从未心跳 */
function formatTime(value: string) {
  if (!value) {
    return "从未心跳";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return value;
  }
  return date.toLocaleString();
}
