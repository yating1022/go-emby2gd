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
import { Spinner } from "~/components/ui/spinner";
import { copyText } from "./clipboard";

type Props = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** 后端返回的安装命令(含注册 Token, 仅管理员页面可见) */
  command: string;
};

/** 安装命令回显弹窗: 命令含注册 Token, 提供手动复制按钮 */
export default function InstallCommandDialog({
  open,
  onOpenChange,
  command,
}: Props) {
  const [copying, setCopying] = useState(false);

  const handleCopy = async () => {
    setCopying(true);
    try {
      const copied = await copyText(command);
      if (copied) {
        toast.success("安装命令已复制到剪贴板");
      } else {
        toast.error("复制失败, 请手动选中命令内容复制");
      }
    } finally {
      setCopying(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>节点安装命令</DialogTitle>
          <DialogDescription>
            在需要接入的机器上以 root 权限执行该命令。命令包含注册 Token，请勿外传。
          </DialogDescription>
        </DialogHeader>

        <div className="max-h-40 overflow-y-auto rounded-md bg-secondary p-3 font-mono text-xs break-all text-secondary-foreground">
          {command}
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            关 闭
          </Button>
          <Button disabled={copying || !command} onClick={handleCopy}>
            {copying && <Spinner data-icon="inline-start" />}
            复制
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
