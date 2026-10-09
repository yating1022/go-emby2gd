/**
 * 复制文本到剪贴板
 *
 * 优先使用异步 Clipboard API(仅 https / localhost 等安全上下文可用);
 * 局域网 http 访问时退化为临时 textarea + execCommand 方案。
 * 返回是否复制成功, 失败时由调用方提示用户手动复制。
 */
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 权限被拒绝等情况: 继续尝试兜底方案
    }
  }

  try {
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.setAttribute("readonly", "");
    textarea.style.position = "fixed";
    textarea.style.top = "0";
    textarea.style.opacity = "0";
    document.body.appendChild(textarea);
    textarea.select();
    const copied = document.execCommand("copy");
    document.body.removeChild(textarea);
    return copied;
  } catch {
    return false;
  }
}
