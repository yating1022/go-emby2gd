package gdrive

import (
	"strings"
	"testing"
)

func TestRedactSecret(t *testing.T) {
	const secret = "test-panel-api-token"

	tests := []struct {
		name   string
		text   string
		secret string
		want   string
	}{
		{
			name:   "文本中出现凭据",
			text:   "令牌 " + secret + " 已失效",
			secret: secret,
			want:   "令牌 *** 已失效",
		},
		{
			name:   "文本中没有凭据",
			text:   "路径尚未缓存, 请先缓存它",
			secret: secret,
			want:   "路径尚未缓存, 请先缓存它",
		},
		{
			name:   "凭据过短时不替换, 避免误伤正常文本",
			text:   "a short text",
			secret: "a",
			want:   "a short text",
		},
		{
			name:   "空文本",
			text:   "",
			secret: secret,
			want:   "",
		},
		{
			name:   "空凭据",
			text:   "some text",
			secret: "",
			want:   "some text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactSecret(tt.text, tt.secret); got != tt.want {
				t.Errorf("redactSecret(%q, %q) = %q, want %q", tt.text, tt.secret, got, tt.want)
			}
		})
	}
}

// TestRedactConfigSecrets_PreservesChineseText
//
// 旧实现里那套按 ASCII 白名单过滤的做法是给 Google 的 OAuth 错误页准备的;
// 套在面板的中文文案上会把整句打成 '?', 正好毁掉"直接沿用面板 message"这条诊断链。
// 因此这里只做凭据替换, 不做字符白名单。
func TestRedactConfigSecrets_PreservesChineseText(t *testing.T) {
	withTestConfig(t, "")

	const message = "路径尚未缓存, 请先缓存它 (PATH_NOT_IN_CACHE)"
	got := redactConfigSecrets(message)

	if got != message {
		t.Errorf("不含凭据的中文文案必须逐字符保留, 实际: %q", got)
	}
}

// TestRedactConfigSecrets_RedactsConfiguredToken
func TestRedactConfigSecrets_RedactsConfiguredToken(t *testing.T) {
	withTestConfig(t, "")

	got := redactConfigSecrets("上游回显了 " + testApiToken + " 这个值")

	if strings.Contains(got, testApiToken) {
		t.Errorf("配置里的凭据必须被替换, 实际: %q", got)
	}
	if !strings.Contains(got, "***") {
		t.Errorf("凭据应被替换为 ***, 实际: %q", got)
	}
	if !strings.Contains(got, "上游回显了") || !strings.Contains(got, "这个值") {
		t.Errorf("替换凭据不应破坏周围文本, 实际: %q", got)
	}
}

// TestRedactConfigSecrets_NilConfig 没有配置时原样返回, 不应 panic
func TestRedactConfigSecrets_NilConfig(t *testing.T) {
	withDisabledConfig(t)

	const message = "任意文本"
	if got := redactConfigSecrets(message); got != message {
		t.Errorf("redactConfigSecrets() = %q, want %q", got, message)
	}
}
