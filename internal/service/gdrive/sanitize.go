package gdrive

import "strings"

// minRedactSecretLen 参与脱敏的最短凭据长度
//
// 过短的值会在正常文本里误伤(如 "a" 会命中所有含 a 的单词), 也构不成有效凭据。
const minRedactSecretLen = 8

// redactConfigSecrets 用当前配置里的凭据给文本脱敏
func redactConfigSecrets(text string) string {
	if text == "" {
		return text
	}

	cfg := gdriveConfig()
	if cfg == nil {
		return text
	}
	return redactSecret(text, cfg.ApiToken)
}

// redactSecret 把文本中出现的凭据替换成 ***
//
// 面板返回的错误文案会被原样写进日志与回退日志, 因此必须兜一层:
// 即使面板把 Token 回声回来, 也不会落进日志文件。
//
// 这里只做字符串替换, 【不做】字符白名单。旧实现里那套按 ASCII 白名单过滤的做法
// 是给 Google 的 OAuth 错误页准备的, 用在这里会把中文文案整句打成 '?',
// 正好毁掉"直接沿用面板 message"这条最有用的诊断链。
func redactSecret(text, secret string) string {
	if text == "" || len(secret) < minRedactSecretLen {
		return text
	}
	return strings.ReplaceAll(text, secret, "***")
}
