package gdrive

import "strings"

// MatchMountPath 判断 strm 内容是否为 Google Drive 挂载点下的文件路径
//
// 命中时返回去掉挂载前缀后的路径(以 '/' 开头), 即团队盘内可直接交给面板的逻辑路径。
//
// 未启用、未配置挂载前缀、不以前缀开头、或前缀之后没有更多路径段时返回 ok=false ——
// 这【不是错误】, 只说明该 strm 内容该走原有流程。
//
// strm 内容是 rclone 挂载点下的本地路径而非 http 地址,
// 因此判断必须独立于"是否为远程地址", 由调用方在 strm 分支之外先行调用。
func MatchMountPath(strmContent string) (gdPath string, ok bool) {
	cfg := gdriveConfig()
	if !cfg.IsEnabled() {
		return "", false
	}

	prefix := normalizeMountPrefix(cfg.MountPrefix)
	if prefix == "" {
		return "", false
	}

	content := strings.TrimSpace(strmContent)
	if !strings.HasPrefix(content, prefix) {
		return "", false
	}

	// 边界校验: 前缀之后必须紧跟 '/' 或字符串结束,
	// 否则 /home/googleDriveBackup/x.mkv 会被误当成挂载点下的文件。
	rest := content[len(prefix):]
	if rest != "" && !strings.HasPrefix(rest, "/") {
		return "", false
	}
	// 前缀之后为空说明这本身就是挂载点(目录), 不是文件
	if rest == "" {
		return "", false
	}

	return rest, true
}

// normalizeMountPrefix 归一化挂载前缀
//
// 容错处理末尾多余的 '/': 配置校验会拦截这种写法, 但这里不能依赖校验 ——
// 测试与将来的配置来源都可能绕过 Init, 而 '//' 会让前缀匹配失败。
// 前缀必须以 '/' 开头, 否则视为未配置(相对路径不能被当作挂载点前缀)。
func normalizeMountPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if !strings.HasPrefix(prefix, "/") {
		return ""
	}
	return strings.TrimRight(prefix, "/")
}
