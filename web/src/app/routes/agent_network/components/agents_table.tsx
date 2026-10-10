import { useState } from "react";
import { toast } from "sonner";
import { Button } from "~/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "~/components/ui/dialog";
import { Field, FieldGroup, FieldLabel } from "~/components/ui/field";
import { Input } from "~/components/ui/input";
import { Spinner } from "~/components/ui/spinner";
import type { AgentView } from "../types";

type Props = {
  agents: AgentView[];
  /** 正在执行行操作的节点 id, 空串表示当前没有行操作在进行 */
  actingID: string;
  onToggle: (agent: AgentView) => void;
  onDelete: (agent: AgentView) => void;
  /** 保存节点资料(名称 + 优先级); 返回 true 表示保存成功(弹窗据此关闭) */
  onEdit: (agent: AgentView, name: string, priority: number) => Promise<boolean>;
};

/** 节点列表表格 */
export default function AgentsTable({
  agents,
  actingID,
  onToggle,
  onDelete,
  onEdit,
}: Props) {
  // 编辑弹窗: 打开时用节点当前值预填
  const [editTarget, setEditTarget] = useState<AgentView | null>(null);
  const [editName, setEditName] = useState("");
  const [editPriority, setEditPriority] = useState("0");
  const [saving, setSaving] = useState(false);

  // 缓存中心(hub)排最前: 它与边缘节点职责完全不同, 列表里一眼可辨
  const ordered = [...agents].sort(
    (a, b) => (a.role === "hub" ? 0 : 1) - (b.role === "hub" ? 0 : 1),
  );

  const openEditDialog = (agent: AgentView) => {
    setEditTarget(agent);
    setEditName(agent.name);
    setEditPriority(String(agent.priority));
  };

  const handleSaveEdit = async () => {
    const target = editTarget;
    if (!target) {
      return;
    }

    // 数字框可能被清空或输入非整数: 本地先拦一次, 避免把 null 发给后端
    const priority = Number(editPriority);
    if (
      !editPriority.trim() ||
      !Number.isInteger(priority) ||
      priority < 0 ||
      priority > 9999
    ) {
      toast.error("优先级必须是 0-9999 的整数");
      return;
    }

    setSaving(true);
    try {
      const ok = await onEdit(target, editName.trim(), priority);
      if (ok) {
        setEditTarget(null);
      }
    } finally {
      setSaving(false);
    }
  };

  return (
    <>
      <div className="w-full overflow-hidden rounded-xl bg-card text-card-foreground shadow-xs ring-1 ring-foreground/10">
        <div className="w-full overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b">
                {[
                  "名称",
                  "角色",
                  "状态",
                  "优先级",
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
              {ordered.map((agent) => {
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
                      <AgentRole role={agent.role} />
                    </td>
                    <td className="px-4 py-2.5 align-middle whitespace-nowrap">
                      <AgentStatus agent={agent} />
                    </td>
                    <td className="px-4 py-2.5 align-middle whitespace-nowrap">
                      {agent.priority}
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
                          onClick={() => openEditDialog(agent)}
                        >
                          编辑
                        </Button>
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

      {/* 编辑资料弹窗(名称 + 优先级) */}
      <Dialog
        open={editTarget !== null}
        onOpenChange={(open) => {
          if (!open && !saving) {
            setEditTarget(null);
          }
        }}
      >
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>编辑节点</DialogTitle>
            <DialogDescription>
              名称与调度优先级一起保存。优先级数字越小越优先（0
              为默认值），仅在「优先级」调度策略下生效。
            </DialogDescription>
          </DialogHeader>

          <FieldGroup>
            <Field>
              <FieldLabel htmlFor="agent-edit-name">名称</FieldLabel>
              <Input
                id="agent-edit-name"
                value={editName}
                maxLength={64}
                placeholder="节点名称(最多 64 字符)"
                onChange={(e) => setEditName(e.target.value)}
              />
            </Field>
            <Field>
              <FieldLabel htmlFor="agent-edit-priority">优先级</FieldLabel>
              <Input
                id="agent-edit-priority"
                type="number"
                min={0}
                max={9999}
                value={editPriority}
                onChange={(e) => setEditPriority(e.target.value)}
              />
            </Field>
          </FieldGroup>

          <DialogFooter>
            <Button
              variant="outline"
              disabled={saving}
              onClick={() => setEditTarget(null)}
            >
              取 消
            </Button>
            <Button disabled={saving} onClick={handleSaveEdit}>
              {saving && <Spinner data-icon="inline-start" />}
              {saving ? "保存中..." : "保 存"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}

/** 角色列: hub=缓存中心(唯一从 Google 拉流并缓存, 不参与客户端调度); 其余=边缘节点(面向客户端透传) */
function AgentRole({ role }: { role: string }) {
  if (role === "hub") {
    return (
      <span
        className="inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium bg-violet-500/15 text-violet-600 dark:text-violet-400"
        title="缓存中心：唯一从 Google 拉流并缓存的机器，为边缘节点供流；不参与客户端调度"
      >
        缓存中心
      </span>
    );
  }
  return (
    <span
      className="inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium bg-sky-500/15 text-sky-600 dark:text-sky-400"
      title="边缘节点：面向客户端透传，上游走缓存中心"
    >
      边缘节点
    </span>
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
