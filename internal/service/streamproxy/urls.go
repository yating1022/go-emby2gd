package streamproxy

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/strs"
)

// schemePrefixes 需要做大小写归一化的 scheme 前缀
var schemePrefixes = [2]string{"http://", "https://"}

// NormalizeURL 归一化 strm 地址
//
// 修复 strm 原文中的裸空格、未编码中文与全角标点:
// 直接把它们交给 http.NewRequest 会生成请求目标被第一个空格截断的畸形请求行,
// 且 Go 不会报错, 属于静默失败。
//
// 处理步骤:
//  1. 解析原始地址;
//  2. 查询串按 url.Values 解码后再编码, 保证 "解码-再编码" 无损;
//  3. Encode 会把空格编码成 '+', 统一替换为 %20, 避免上游实现差异带来的歧义
//     (字面 '+' 已被编码成 %2B, 因此该替换是无损的)。
//
// 路径部分由 url.URL.String() 经 EscapedPath 自动编码, 不需要手工处理。
func NormalizeURL(rawURL string) (string, error) {
	if strs.AnyEmpty(rawURL) {
		return "", errors.New("strm 地址为空")
	}

	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("解析 strm 地址失败: %w", err)
	}

	if u.RawQuery != "" {
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			// 不做静默降级: 被截断的请求会以更难排查的方式失败
			return "", fmt.Errorf("解析 strm 地址查询参数失败: %w", err)
		}
		u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
	}

	return u.String(), nil
}

// MatchDomain 判断地址是否命中配置的代理前缀
//
// 返回命中的前缀; 未开启代理或未命中任何前缀时 ok 为 false。
//
// 匹配规则:
//   - 两侧统一去除首尾空白, 并把 scheme 部分转成小写;
//   - 配置项尾部多余的 '/' 在初始化时已去掉;
//   - 按配置顺序线性匹配, 第一个命中即生效;
//   - 命中后要求边界成立: 前缀之后的第一个字符必须是 '/'、'?'、'#' 或字符串结束,
//     否则 http://host:7811 会错误命中 http://host:78111.example.com。
func MatchDomain(rawURL string) (prefix string, ok bool) {
	cfg := proxyConfig()
	if cfg == nil || !cfg.Enable {
		return "", false
	}

	target := normalizeScheme(strings.TrimSpace(rawURL))
	if target == "" {
		return "", false
	}

	for _, domain := range cfg.DomainsNormalized() {
		if domain == "" || !strings.HasPrefix(target, domain) {
			continue
		}
		rest := target[len(domain):]
		if rest == "" || strings.HasPrefix(rest, "/") || strings.HasPrefix(rest, "?") || strings.HasPrefix(rest, "#") {
			return domain, true
		}
	}

	return "", false
}

// normalizeScheme 只将地址开头的 scheme 部分转换为小写
//
// 路径与查询串保持原样, 避免破坏大小写敏感的上游路径。
func normalizeScheme(rawURL string) string {
	lower := strings.ToLower(rawURL)
	for _, prefix := range schemePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return prefix + rawURL[len(prefix):]
		}
	}
	return rawURL
}
