package agentnet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs"
)

// newFakePanel 启动一个假 GD 面板
//
// 只实现 /api/dl: 成功时返回一条直链与请求头(含 Google 凭据),
// 失败时返回统一错误信封。
func newFakePanel(t *testing.T, status int, directURL, expiresAt, code, message string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/dl" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if message != "" {
			payload, err := json.Marshal(map[string]any{
				"ok":    false,
				"error": map[string]string{"code": code, "message": message},
			})
			if err != nil {
				t.Errorf("构造面板错误响应失败: %v", err)
				return
			}
			_, _ = w.Write(payload)
			return
		}
		payload, err := json.Marshal(map[string]any{
			"ok": true,
			"data": map[string]any{
				"url":        directURL,
				"headers":    map[string]string{"Authorization": testGoogleHeader},
				"expires_at": expiresAt,
			},
		})
		if err != nil {
			t.Errorf("构造面板成功响应失败: %v", err)
			return
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)
	return server
}

// fileIDFor 把 Drive 路径编码为 file_id
func fileIDFor(gdPath string) string {
	return fileToken(gdPath)
}

// downloadLinkURL 拼出下载直链接口的地址
func downloadLinkURL(gdPath string) string {
	return "/api/agent/download-link?file_id=" + url.QueryEscape(fileIDFor(gdPath))
}

// downloadLinkPathSeq TestDownloadLink_Success 每次执行用的路径序号
//
// gdrive 的直链缓存是进程级全局的、按路径为键且条目不会主动过期: 同一条路径在
// 多次执行之间(如 go test -count=2)会直接命中上一轮缓存的直链与令牌串,
// "expires_at 原样透传"的断言便会读到上一轮的过期时间(见 gdrive.cachedTarget)。
// 每次执行换一条路径, 保证用例只可能命中本轮自己启动的假面板。
var downloadLinkPathSeq atomic.Uint64

func TestDownloadLink_Success(t *testing.T) {
	setupStateDir(t)
	gdPath := fmt.Sprintf("/影视库/下载直链/成功-%d.mkv", downloadLinkPathSeq.Add(1))
	expiresAt := time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05") + ".5Z"
	panel := newFakePanel(t, http.StatusOK, "https://drive.example.com/direct", expiresAt, "", "")
	setupFullConfig(t, true, panel.URL)
	simulateRestart()

	engine := newTestEngine()
	enrolled := enrollAgent(t, engine, "m-1")
	agentID := enrolled["agent_id"].(string)
	secret := enrolled["agent_secret"].(string)

	recorder := doRequest(t, engine, http.MethodGet, downloadLinkURL(gdPath), "", agentAuthHeaders(agentID, secret))
	if recorder.Code != http.StatusOK {
		t.Fatalf("下发直链应成功: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
	}

	resp := decodeMap(t, recorder.Body.Bytes())
	if resp["url"] != "https://drive.example.com/direct" {
		t.Errorf("url = %v, want https://drive.example.com/direct", resp["url"])
	}
	if resp["expires_at"] != expiresAt {
		t.Errorf("expires_at 必须原样透传: %v, want %s", resp["expires_at"], expiresAt)
	}
	headers, ok := resp["headers"].(map[string]any)
	if !ok || headers["Authorization"] != testGoogleHeader {
		t.Errorf("headers 应带上面板下发的凭据, 实际: %v", resp["headers"])
	}
}

func TestDownloadLink_Errors(t *testing.T) {
	t.Run("未鉴权", func(t *testing.T) {
		setupStateDir(t)
		panel := newFakePanel(t, http.StatusOK, "https://drive.example.com/direct", "", "", "")
		setupFullConfig(t, true, panel.URL)
		simulateRestart()

		recorder := doRequest(t, newTestEngine(), http.MethodGet, downloadLinkURL("/影视库/x.mkv"), "", nil)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("HTTP = %d, want 401", recorder.Code)
		}
		if code := errorCodeOf(t, decodeMap(t, recorder.Body.Bytes())); code != codeUnauthorized {
			t.Errorf("错误码 = %q, want %q", code, codeUnauthorized)
		}
	})

	t.Run("凭据错误", func(t *testing.T) {
		setupStateDir(t)
		panel := newFakePanel(t, http.StatusOK, "https://drive.example.com/direct", "", "", "")
		setupFullConfig(t, true, panel.URL)
		simulateRestart()

		engine := newTestEngine()
		enrolled := enrollAgent(t, engine, "m-1")

		recorder := doRequest(t, engine, http.MethodGet, downloadLinkURL("/影视库/x.mkv"), "",
			agentAuthHeaders(enrolled["agent_id"].(string), strings.Repeat("f", credentialBytes*2)))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("HTTP = %d, want 401", recorder.Code)
		}
	})

	t.Run("缺少 file_id", func(t *testing.T) {
		setupStateDir(t)
		panel := newFakePanel(t, http.StatusOK, "https://drive.example.com/direct", "", "", "")
		setupFullConfig(t, true, panel.URL)
		simulateRestart()

		engine := newTestEngine()
		enrolled := enrollAgent(t, engine, "m-1")
		auth := agentAuthHeaders(enrolled["agent_id"].(string), enrolled["agent_secret"].(string))

		recorder := doRequest(t, engine, http.MethodGet, "/api/agent/download-link", "", auth)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("HTTP = %d, want 400", recorder.Code)
		}
		if code := errorCodeOf(t, decodeMap(t, recorder.Body.Bytes())); code != codeValidationError {
			t.Errorf("错误码 = %q, want %q", code, codeValidationError)
		}
	})

	t.Run("file_id 解码失败", func(t *testing.T) {
		setupStateDir(t)
		panel := newFakePanel(t, http.StatusOK, "https://drive.example.com/direct", "", "", "")
		setupFullConfig(t, true, panel.URL)
		simulateRestart()

		engine := newTestEngine()
		enrolled := enrollAgent(t, engine, "m-1")
		auth := agentAuthHeaders(enrolled["agent_id"].(string), enrolled["agent_secret"].(string))

		recorder := doRequest(t, engine, http.MethodGet, "/api/agent/download-link?file_id=abc+123", "", auth)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("HTTP = %d, want 400, body=%s", recorder.Code, recorder.Body.String())
		}
		if code := errorCodeOf(t, decodeMap(t, recorder.Body.Bytes())); code != codeTokenInvalid {
			t.Errorf("错误码 = %q, want %q", code, codeTokenInvalid)
		}
	})

	t.Run("面板失败", func(t *testing.T) {
		setupStateDir(t)
		panel := newFakePanel(t, http.StatusForbidden, "", "", "PATH_NOT_IN_CACHE", "路径不在缓存中, 请稍后重试")
		setupFullConfig(t, true, panel.URL)
		simulateRestart()

		engine := newTestEngine()
		enrolled := enrollAgent(t, engine, "m-1")
		auth := agentAuthHeaders(enrolled["agent_id"].(string), enrolled["agent_secret"].(string))

		recorder := doRequest(t, engine, http.MethodGet, downloadLinkURL("/影视库/下载直链/面板失败.mkv"), "", auth)
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("HTTP = %d, want 502, body=%s", recorder.Code, recorder.Body.String())
		}
		resp := decodeMap(t, recorder.Body.Bytes())
		if code := errorCodeOf(t, resp); code != codeLinkUnavailable {
			t.Errorf("错误码 = %q, want %q", code, codeLinkUnavailable)
		}
		message, _ := resp["error"].(map[string]any)["message"].(string)
		if !strings.Contains(message, "路径不在缓存中") {
			t.Errorf("错误消息应透传面板的中文原因: %q", message)
		}
		if strings.Contains(recorder.Body.String(), testPanelToken) {
			t.Errorf("错误响应回显了面板令牌: %s", recorder.Body.String())
		}
	})
}

// collectLogs 收集模块日志
//
// 用于断言"凭据永不出现在日志里"这条硬约束。
type collectedLogs struct {
	content strings.Builder
}

// Log 实现 logs.Logger 接口
func (c *collectedLogs) Log(content string) {
	c.content.WriteString(content)
}

// String 返回已收集到的日志
func (c *collectedLogs) String() string { return c.content.String() }

func TestAgentEndpoints_SecretsNeverLogged(t *testing.T) {
	setupStateDir(t)
	gdPath := "/影视库/日志/凭据不得出现.mkv"
	expiresAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	panel := newFakePanel(t, http.StatusOK, "https://drive.example.com/direct", expiresAt, "", "")
	setupFullConfig(t, true, panel.URL)
	simulateRestart()

	collector := &collectedLogs{}
	id, ok := logs.RegisterLogger(collector)
	if !ok {
		t.Fatal("注册日志收集器失败")
	}
	defer logs.RemoveLogger(id)

	engine := newTestEngine()
	enrolled := enrollAgent(t, engine, "m-1")
	agentID := enrolled["agent_id"].(string)
	secret := enrolled["agent_secret"].(string)
	signKey := enrolled["sign_key"].(string)
	auth := agentAuthHeaders(agentID, secret)

	doRequest(t, engine, http.MethodPost, "/api/agent/heartbeat", `{"active_streams":1}`, auth)
	doRequest(t, engine, http.MethodGet, downloadLinkURL(gdPath), "", auth)

	logged := collector.String()
	for name, secretValue := range map[string]string{
		"agent_secret":   secret,
		"sign_key":       signKey,
		"注册 Token":       testEnrollToken,
		"面板令牌":           testPanelToken,
		"Google 凭据":      testGoogleHeader,
		"完整直链(含凭据的请求目标)": "https://drive.example.com/direct",
	} {
		if strings.Contains(logged, secretValue) {
			t.Errorf("日志里出现了 %s: %q", name, logged)
		}
	}
	// 但调度/下发这类正常事件应当有记录, 便于运维排查
	if !strings.Contains(logged, gdPath) {
		t.Errorf("正常下发直链的事件应留下日志(不含凭据): %q", logged)
	}
}
