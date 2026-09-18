package gdrive

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFetchDirectLink_Success(t *testing.T) {
	expiresAt := futureRFC3339(time.Hour)

	panel := newFakePanel(t, func(_, _ string) (int, string) {
		return http.StatusOK, successBody("https://www.googleapis.com/drive/v3/files/test-file-id?alt=media", expiresAt)
	})
	withTestConfig(t, panel.url())

	link, err := fetchDirectLink(context.Background(), testPath)
	if err != nil {
		t.Fatalf("fetchDirectLink() 返回错误: %v", err)
	}

	if link.URL != "https://www.googleapis.com/drive/v3/files/test-file-id?alt=media" {
		t.Errorf("url = %q", link.URL)
	}
	if got := link.Headers["Authorization"]; got != testProviderToken {
		t.Errorf("headers.Authorization = %q, want %q", got, testProviderToken)
	}
	if link.File.ID != "test-file-id" {
		t.Errorf("file.id = %q, want test-file-id", link.File.ID)
	}
	if link.File.Size != 8589934592 {
		t.Errorf("file.size = %d, want 8589934592", link.File.Size)
	}

	wantTime, _ := time.Parse(time.RFC3339, expiresAt)
	if got := parseExpiresAt(link.ExpiresAt); !got.Equal(wantTime) {
		t.Errorf("expires_at 解析 = %v, want %v", got, wantTime)
	}
}

// TestFetchDirectLink_EncodesPath 路径必须交给 QueryEscape
//
// 自行拼接字符串时, 路径里的 '&' 会被面板当成参数分隔符, 中文与空格也极易编错;
// 而面板要求路径与 Drive 内逐字符一致。
func TestFetchDirectLink_EncodesPath(t *testing.T) {
	paths := []string{
		testPath,
		"/影视库/Quinn's Paper (2026)/Quinn's Paper (2026).mkv",
		"/影视库/a&b=c/d#e.mkv",
		"/影视库/带 空格/x.mkv",
	}

	for _, gdPath := range paths {
		t.Run(gdPath, func(t *testing.T) {
			panel := newFakePanel(t, func(_, _ string) (int, string) {
				return http.StatusOK, successBody("https://drive.example.com/x", futureRFC3339(time.Hour))
			})
			withTestConfig(t, panel.url())

			if _, err := fetchDirectLink(context.Background(), gdPath); err != nil {
				t.Fatalf("fetchDirectLink() 返回错误: %v", err)
			}

			received := panel.receivedPaths()
			if len(received) != 1 {
				t.Fatalf("面板被调用 %d 次, want 1", len(received))
			}
			if received[0] != gdPath {
				t.Errorf("面板收到的 path = %q, want %q (逐字符一致)", received[0], gdPath)
			}
		})
	}
}

// TestFetchDirectLink_SendsTokenAndFixedHeaders 核对发往面板的请求头
func TestFetchDirectLink_SendsTokenAndFixedHeaders(t *testing.T) {
	panel := newFakePanel(t, func(_, _ string) (int, string) {
		return http.StatusOK, successBody("https://drive.example.com/x", futureRFC3339(time.Hour))
	})
	withTestConfig(t, panel.url())

	if _, err := fetchDirectLink(context.Background(), testPath); err != nil {
		t.Fatalf("fetchDirectLink() 返回错误: %v", err)
	}

	headers := panel.receivedHeaders()
	if len(headers) != 1 {
		t.Fatalf("面板被调用 %d 次, want 1", len(headers))
	}

	// Bearer 后面必须有空格, 漏掉会直接得到 401
	if got := headers[0].Get("Authorization"); got != bearerPrefix+testApiToken {
		t.Errorf("Authorization = %q, want %q", got, bearerPrefix+testApiToken)
	}
	// 与出站代理规范一致: 显式写死 identity, 避免 Transport 追加 gzip 并透明解压
	if got := headers[0].Get("Accept-Encoding"); got != "identity" {
		t.Errorf("Accept-Encoding = %q, want identity", got)
	}
}

// TestFetchDirectLink_PanelErrors 面板的各类错误码都要走同一条路径
//
// 不做按 code 的特殊分支, 但必须原样保留面板的中文文案与错误码 ——
// 例如 PATH_NOT_IN_CACHE 与 PATH_NOT_FOUND 的区别是排查时的关键信息。
func TestFetchDirectLink_PanelErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		code       string
		message    string
		wantCode   string
		wantPhrase string
	}{
		{
			name:       "401 令牌无效",
			status:     http.StatusUnauthorized,
			code:       "INVALID_TOKEN",
			message:    "令牌缺失、错误或已轮换",
			wantCode:   "INVALID_TOKEN",
			wantPhrase: "令牌缺失、错误或已轮换",
		},
		{
			name:       "404 路径尚未缓存",
			status:     http.StatusNotFound,
			code:       "PATH_NOT_IN_CACHE",
			message:    "路径尚未缓存, 请先缓存它",
			wantCode:   "PATH_NOT_IN_CACHE",
			wantPhrase: "路径尚未缓存, 请先缓存它",
		},
		{
			name:       "404 路径不存在",
			status:     http.StatusNotFound,
			code:       "PATH_NOT_FOUND",
			message:    "路径在 Drive 里确实不存在",
			wantCode:   "PATH_NOT_FOUND",
			wantPhrase: "路径在 Drive 里确实不存在",
		},
		{
			name:       "400 路径是目录",
			status:     http.StatusBadRequest,
			code:       "PATH_IS_DIRECTORY",
			message:    "路径指向的是目录",
			wantCode:   "PATH_IS_DIRECTORY",
			wantPhrase: "路径指向的是目录",
		},
		{
			name:       "400 无法下载的原生文档",
			status:     http.StatusBadRequest,
			code:       "PATH_NOT_DOWNLOADABLE",
			message:    "Google 原生文档没有字节流",
			wantCode:   "PATH_NOT_DOWNLOADABLE",
			wantPhrase: "Google 原生文档没有字节流",
		},
		{
			name:       "422 参数不合法",
			status:     http.StatusUnprocessableEntity,
			code:       "VALIDATION_ERROR",
			message:    "参数不合法",
			wantCode:   "VALIDATION_ERROR",
			wantPhrase: "参数不合法",
		},
		{
			name:       "502 上游报错",
			status:     http.StatusBadGateway,
			code:       "GD_QUOTA_EXCEEDED",
			message:    "上游 Google 配额超限",
			wantCode:   "GD_QUOTA_EXCEEDED",
			wantPhrase: "上游 Google 配额超限",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			panel := newFakePanel(t, func(_, _ string) (int, string) {
				return tt.status, errorBody(tt.code, tt.message)
			})
			withTestConfig(t, panel.url())

			_, err := fetchDirectLink(context.Background(), testPath)
			if err == nil {
				t.Fatal("期望返回错误, 实际成功")
			}
			if !strings.Contains(err.Error(), tt.wantCode) {
				t.Errorf("错误信息应包含错误码 %q, 实际: %v", tt.wantCode, err)
			}
			if !strings.Contains(err.Error(), tt.wantPhrase) {
				t.Errorf("错误信息应原样包含面板文案 %q, 实际: %v", tt.wantPhrase, err)
			}
		})
	}
}

// TestFetchDirectLink_OkFalseWith200 状态码 200 但 ok 为 false 时以响应体为准
func TestFetchDirectLink_OkFalseWith200(t *testing.T) {
	panel := newFakePanel(t, func(_, _ string) (int, string) {
		return http.StatusOK, errorBody("PATH_NOT_IN_CACHE", "路径尚未缓存")
	})
	withTestConfig(t, panel.url())

	_, err := fetchDirectLink(context.Background(), testPath)
	if err == nil {
		t.Fatal("ok 为 false 时应返回错误")
	}
	if !strings.Contains(err.Error(), "PATH_NOT_IN_CACHE") || !strings.Contains(err.Error(), "路径尚未缓存") {
		t.Errorf("错误信息应保留面板的 code 与 message, 实际: %v", err)
	}
}

func TestFetchDirectLink_MalformedResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"不是 JSON", "<html>502 Bad Gateway</html>"},
		{"缺少 url", `{"ok":true,"data":{"headers":{"Authorization":"Bearer x"}}}`},
		{"url 为空白", `{"ok":true,"data":{"url":"   ","headers":{}}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			panel := newFakePanel(t, func(_, _ string) (int, string) {
				return http.StatusOK, tt.body
			})
			withTestConfig(t, panel.url())

			if _, err := fetchDirectLink(context.Background(), testPath); err == nil {
				t.Fatal("期望返回错误, 实际成功")
			}
		})
	}
}

// TestFetchDirectLink_RedactsApiTokenInPanelMessage
//
// 面板文案会原样进日志; 即使面板把 Token 回声回来, 也不该落进日志文件。
func TestFetchDirectLink_RedactsApiTokenInPanelMessage(t *testing.T) {
	panel := newFakePanel(t, func(_, _ string) (int, string) {
		return http.StatusUnauthorized, errorBody("INVALID_TOKEN", "令牌 "+testApiToken+" 已失效")
	})
	withTestConfig(t, panel.url())

	_, err := fetchDirectLink(context.Background(), testPath)
	if err == nil {
		t.Fatal("期望返回错误, 实际成功")
	}
	if strings.Contains(err.Error(), testApiToken) {
		t.Errorf("错误信息不得包含面板 Token, 实际: %v", err)
	}
	if !strings.Contains(err.Error(), "已失效") {
		t.Errorf("脱敏不应破坏中文文案, 实际: %v", err)
	}
}

// TestFetchDirectLink_InvalidExpiresAtStillUsable 过期时刻格式不对不应打掉整次取直链
func TestFetchDirectLink_InvalidExpiresAtStillUsable(t *testing.T) {
	panel := newFakePanel(t, func(_, _ string) (int, string) {
		return http.StatusOK, successBody("https://drive.example.com/x", "not-a-timestamp")
	})
	withTestConfig(t, panel.url())

	link, err := fetchDirectLink(context.Background(), testPath)
	if err != nil {
		t.Fatalf("过期时刻非法时仍应返回可用结果, 实际: %v", err)
	}
	if !parseExpiresAt(link.ExpiresAt).IsZero() {
		t.Error("非法时间戳应解析为零值, 由调用方按立即过期处理")
	}
}

func TestFetchDirectLink_Disabled(t *testing.T) {
	withDisabledConfig(t)

	if _, err := fetchDirectLink(context.Background(), testPath); err == nil {
		t.Error("未启用时应返回错误")
	}
}

func TestFetchDirectLink_EmptyPath(t *testing.T) {
	withTestConfig(t, "")

	if _, err := fetchDirectLink(context.Background(), "   "); err == nil {
		t.Error("路径为空时应返回错误")
	}
}

// TestParseExpiresAt 过期时刻的解析边界
func TestParseExpiresAt(t *testing.T) {
	valid := "2026-09-18T10:35:47Z"
	want, _ := time.Parse(time.RFC3339, valid)

	tests := []struct {
		name string
		raw  string
		want time.Time
	}{
		{"合法时间戳", valid, want},
		{"带首尾空白", "  " + valid + "  ", want},
		{"空字符串", "", time.Time{}},
		{"格式非法", "2026/09/18 10:35:47", time.Time{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseExpiresAt(tt.raw); !got.Equal(tt.want) {
				t.Errorf("parseExpiresAt(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
