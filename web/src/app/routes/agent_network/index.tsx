import { Copy, RefreshCw } from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import { toast } from "sonner";
import { LOCAL_STORAGE_KEY_API_SECRET } from "~/components/settings_modal/settings_modal";
import { Button } from "~/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "~/components/ui/dialog";
import { Spinner } from "~/components/ui/spinner";
import AgentsTable from "./components/agents_table";
import { copyText } from "./components/clipboard";
import InstallCommandDialog from "./components/install_command_dialog";
import type {
  AdminResponse,
  AgentView,
  AgentsData,
  InstallCommandData,
} from "./types";

/**
 * 调用 /ge2o 管理接口: POST JSON + 恒 200 信封
 *
 * 与既有页面(如 openlist_local_tree)同一模式: HTTP 层失败抛异常,
 * 业务失败(未启用 / 密钥错误等)由调用方按 message 提示。
 */
async function postAdminAPI<T>(
  path: string,
  body: Record<string, unknown>,
): Promise<AdminResponse<T>> {
  const fetchState = await fetch(path, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify(body),
  });

  if (!fetchState.ok || fetchState.status != 200) {
    throw Error(`请求失败: ${fetchState.statusText}`);
  }
  return (await fetchState.json()) as AdminResponse<T>;
}

export default function AgentNetwork() {
  // 节点列表
  const [agents, setAgents] = useState<AgentView[]>([]);
  // 是否已经发起过至少一次列表请求(区分首屏加载与空态)
  const [loaded, setLoaded] = useState(false);
  const [loading, setLoading] = useState(false);
  // 列表不可用时的页面提示(后端 message 原样保留; 未配置密钥时为本地文案)
  const [notice, setNotice] = useState("");

  // 行操作(启用 / 禁用 / 删除)
  const [actingID, setActingID] = useState("");
  const [deleteTarget, setDeleteTarget] = useState<AgentView | null>(null);
  const [deleting, setDeleting] = useState(false);

  // 安装命令
  const [installDialogOpen, setInstallDialogOpen] = useState(false);
  const [installCommand, setInstallCommand] = useState("");
  const [fetchingInstallCommand, setFetchingInstallCommand] = useState(false);

  // 拉取节点列表
  const loadAgents = useCallback(async () => {
    const secret = localStorage.getItem(LOCAL_STORAGE_KEY_API_SECRET);
    if (!secret) {
      setNotice("请先设置接口密钥");
      setAgents([]);
      setLoaded(true);
      toast.info("请先设置接口密钥");
      return;
    }

    setLoading(true);
    try {
      const res = await postAdminAPI<AgentsData>(
        "/ge2o/agent-network/agents",
        { secret },
      );
      if (!res.success) {
        // 功能未启用 / 密钥错误等: 后端 message 原样展示, 不吞错
        setNotice(res.message);
        setAgents([]);
        toast.warning(res.message);
        return;
      }
      setNotice("");
      setAgents(res.data?.agents ?? []);
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setNotice(`获取节点列表异常: ${message}`);
      setAgents([]);
      toast.error(`获取节点列表异常: ${message}`);
    } finally {
      setLoading(false);
      setLoaded(true);
    }
  }, []);

  // 进入页面时自动加载一次(无实时推送, 之后靠手动刷新)
  useEffect(() => {
    loadAgents();
  }, [loadAgents]);

  // 启用 / 禁用节点
  const handleToggle = async (agent: AgentView) => {
    const secret = localStorage.getItem(LOCAL_STORAGE_KEY_API_SECRET);
    if (!secret) {
      toast.info("请先设置接口密钥");
      return;
    }

    setActingID(agent.id);
    try {
      const res = await postAdminAPI("/ge2o/agent-network/agents/update", {
        secret,
        id: agent.id,
        enabled: !agent.enabled,
      });
      if (!res.success) {
        toast.warning(res.message);
        return;
      }
      toast.success(res.message);
      await loadAgents();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      toast.error(`更新节点状态异常: ${message}`);
    } finally {
      setActingID("");
    }
  };

  // 编辑节点资料(名称 + 优先级), 返回是否保存成功(供弹窗决定是否关闭)
  const handleEdit = async (
    agent: AgentView,
    name: string,
    priority: number,
  ): Promise<boolean> => {
    const secret = localStorage.getItem(LOCAL_STORAGE_KEY_API_SECRET);
    if (!secret) {
      toast.info("请先设置接口密钥");
      return false;
    }

    setActingID(agent.id);
    try {
      const res = await postAdminAPI("/ge2o/agent-network/agents/edit", {
        secret,
        id: agent.id,
        name,
        priority,
      });
      if (!res.success) {
        toast.warning(res.message);
        return false;
      }
      toast.success(res.message);
      await loadAgents();
      return true;
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      toast.error(`更新节点资料异常: ${message}`);
      return false;
    } finally {
      setActingID("");
    }
  };

  // 删除节点(需二次确认)
  const handleDelete = async () => {
    const target = deleteTarget;
    if (!target) {
      return;
    }

    const secret = localStorage.getItem(LOCAL_STORAGE_KEY_API_SECRET);
    if (!secret) {
      toast.info("请先设置接口密钥");
      setDeleteTarget(null);
      return;
    }

    setDeleting(true);
    try {
      const res = await postAdminAPI("/ge2o/agent-network/agents/delete", {
        secret,
        id: target.id,
      });
      if (!res.success) {
        toast.warning(res.message);
      } else {
        toast.success(res.message);
      }
      setDeleteTarget(null);
      await loadAgents();
    } catch (err) {
      // 网络层异常: 保留确认弹窗便于重试
      const message = err instanceof Error ? err.message : String(err);
      toast.error(`删除节点异常: ${message}`);
    } finally {
      setDeleting(false);
    }
  };

  // 获取安装命令并复制到剪贴板
  const handleCopyInstallCommand = async () => {
    const secret = localStorage.getItem(LOCAL_STORAGE_KEY_API_SECRET);
    if (!secret) {
      toast.info("请先设置接口密钥");
      return;
    }

    setFetchingInstallCommand(true);
    try {
      const res = await postAdminAPI<InstallCommandData>(
        "/ge2o/agent-network/install-command",
        { secret },
      );
      if (!res.success) {
        toast.warning(res.message);
        return;
      }
      const command = res.data?.command ?? "";
      if (!command) {
        toast.error("获取安装命令失败: 响应缺少 command 字段");
        return;
      }

      setInstallCommand(command);
      setInstallDialogOpen(true);
      const copied = await copyText(command);
      if (copied) {
        toast.success("安装命令已复制到剪贴板");
      } else {
        toast.info("已获取安装命令, 请点击弹窗中的复制按钮手动复制");
      }
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      toast.error(`获取安装命令异常: ${message}`);
    } finally {
      setFetchingInstallCommand(false);
    }
  };

  return (
    <div className="w-full px-12 lg:px-48 space-y-6 pb-12">
      {/* 标题与顶部操作 */}
      <div className="flex flex-wrap items-center justify-between gap-4">
        <div className="space-y-1">
          <h1 className="text-2xl font-bold">节点管理</h1>
          <p className="text-sm text-muted-foreground">
            管理已接入的 agent 节点，或一键复制安装命令注册新节点
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Button
            variant="outline"
            disabled={fetchingInstallCommand}
            onClick={handleCopyInstallCommand}
          >
            {fetchingInstallCommand ? (
              <Spinner data-icon="inline-start" />
            ) : (
              <Copy data-icon="inline-start" />
            )}
            复制安装命令
          </Button>
          <Button variant="outline" disabled={loading} onClick={loadAgents}>
            {loading ? (
              <Spinner data-icon="inline-start" />
            ) : (
              <RefreshCw data-icon="inline-start" />
            )}
            刷新
          </Button>
        </div>
      </div>

      {/* 节点列表 / 空态 */}
      {!loaded ? (
        <div className="flex justify-center py-16">
          <Spinner className="size-6" />
        </div>
      ) : agents.length > 0 ? (
        <AgentsTable
          agents={agents}
          actingID={actingID}
          onToggle={handleToggle}
          onDelete={setDeleteTarget}
          onEdit={handleEdit}
        />
      ) : (
        <div className="space-y-2 rounded-xl border border-dashed px-6 py-16 text-center">
          <p className="text-base font-medium">{notice || "暂无节点"}</p>
          <p className="text-sm text-muted-foreground">
            {notice
              ? "可根据上面提示处理后再点击「刷新」重试"
              : "点击右上角「复制安装命令」，在需要接入的机器上执行即可注册节点"}
          </p>
        </div>
      )}

      {/* 安装命令回显弹窗 */}
      <InstallCommandDialog
        open={installDialogOpen}
        onOpenChange={setInstallDialogOpen}
        command={installCommand}
      />

      {/* 删除确认弹窗 */}
      <Dialog
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (!open && !deleting) {
            setDeleteTarget(null);
          }
        }}
      >
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>删除节点</DialogTitle>
            <DialogDescription>
              {`确定删除节点「${deleteTarget?.name || deleteTarget?.id}」吗？删除后该节点的密钥立即失效，如需重新接入须重新执行安装命令。`}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button
              variant="outline"
              disabled={deleting}
              onClick={() => setDeleteTarget(null)}
            >
              取 消
            </Button>
            <Button variant="destructive" disabled={deleting} onClick={handleDelete}>
              {deleting && <Spinner data-icon="inline-start" />}
              {deleting ? "删除中..." : "删 除"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
