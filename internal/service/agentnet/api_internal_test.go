package agentnet

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"

	"github.com/gin-gonic/gin"
)

// testEnrollToken 测试用注册 Token
const testEnrollToken = "test-enroll-token-0123456789"

// testGe2oSecret 测试用管理密钥
const testGe2oSecret = "test-ge2o-secret"

// testPanelToken 测试用面板令牌
const testPanelToken = "test-panel-api-token"

// testGoogleHeader 假面板下发的 Google 凭据
const testGoogleHeader = "Bearer test-google-access-token"

// setupFullConfig 注入一份完整配置(agent 网络 + GD 面板 + ge2o 密钥)
//
// apiBase 为空表示未启用 GD 面板直链。
func setupFullConfig(t *testing.T, enable bool, apiBase string) {
	t.Helper()

	// 清空相关环境变量, 保证用例不受运行环境里已导出的变量影响
	t.Setenv(config.AgentEnrollTokenEnvName, "")
	t.Setenv(config.GDriveApiTokenEnvName, "")

	agent := &config.AgentNetwork{
		Enable:         enable,
		EnrollToken:    testEnrollToken,
		OfflineSeconds: 45,
		URLTTL:         "1h",
	}
	if err := agent.Init(); err != nil {
		t.Fatalf("初始化 agent 网络测试配置失败: %v", err)
	}

	gdriveCfg := &config.GDrive{
		Enable:      apiBase != "",
		ApiBase:     apiBase,
		ApiToken:    testPanelToken,
		MountPrefix: "/home/googleDrive",
	}
	if err := gdriveCfg.Init(); err != nil {
		t.Fatalf("初始化面板测试配置失败: %v", err)
	}

	oldConfig := config.C
	config.C = &config.Config{AgentNetwork: agent, GDrive: gdriveCfg, Ge2o: &config.Ge2o{ApiSecret: testGe2oSecret}}
	t.Cleanup(func() { config.C = oldConfig })
}

// newTestEngine 组装一个只挂载 agentnet 路由的引擎
func newTestEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Any("/api/agent/enroll", Enroll)
	engine.Any("/api/agent/heartbeat", Heartbeat)
	engine.Any("/api/agent/download-link", DownloadLink)
	engine.Any("/install.sh", InstallScript)
	engine.Any("/ge2o/agent-network/agents", AdminListAgents)
	engine.Any("/ge2o/agent-network/agents/update", AdminUpdateAgent)
	engine.Any("/ge2o/agent-network/agents/delete", AdminDeleteAgent)
	engine.Any("/ge2o/agent-network/agents/edit", AdminEditAgent)
	engine.Any("/ge2o/agent-network/install-command", AdminInstallCommand)
	return engine
}

// doRequest 发起一次请求并返回响应记录器
func doRequest(t *testing.T, handler http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

// enrollAgent 走一次真实的注册接口, 返回响应
func enrollAgent(t *testing.T, engine http.Handler, machineID string) map[string]any {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"enroll_token": testEnrollToken,
		"machine_id":   machineID,
		"hostname":     "node-" + machineID,
		"version":      "v1.0.0",
		"listen_port":  8790,
	})
	if err != nil {
		t.Fatalf("序列化注册请求失败: %v", err)
	}

	recorder := doRequest(t, engine, http.MethodPost, "/api/agent/enroll", string(body), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("注册失败: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("注册响应不是合法 JSON: %v", err)
	}
	return resp
}

// agentAuthHeaders 组装节点鉴权头
func agentAuthHeaders(agentID, secret string) map[string]string {
	return map[string]string{
		"X-Agent-Id":    agentID,
		"Authorization": "Bearer " + secret,
	}
}

// decodeMap 解析响应体
func decodeMap(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v, body=%s", err, body)
	}
	return resp
}

// errorCodeOf 取出错误响应里的 error.code
func errorCodeOf(t *testing.T, resp map[string]any) string {
	t.Helper()

	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 error 对象: %v", resp)
	}
	code, _ := errObj["code"].(string)
	return code
}

func TestEnroll_Success(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, true, "")
	simulateRestart()

	engine := newTestEngine()
	recorder := doRequest(t, engine, http.MethodPost, "/api/agent/enroll",
		`{"enroll_token":"`+testEnrollToken+`","machine_id":"m-1","hostname":"node-1","version":"v1.0.0","listen_port":8790}`,
		nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("注册应成功: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
	}

	// 响应形状与协议逐字一致: 裸对象, 不套 {ok,data} 信封
	resp := decodeMap(t, recorder.Body.Bytes())
	if _, hasOK := resp["ok"]; hasOK {
		t.Error("注册响应是裸对象, 不应出现 ok 字段")
	}
	for _, key := range []string{"agent_id", "agent_secret", "sign_key", "heartbeat_interval_seconds"} {
		if _, ok := resp[key]; !ok {
			t.Errorf("注册响应缺少字段 %s: %v", key, resp)
		}
	}
	if got := resp["heartbeat_interval_seconds"]; got != float64(config.AgentHeartbeatIntervalSeconds) {
		t.Errorf("heartbeat_interval_seconds = %v, want %d", got, config.AgentHeartbeatIntervalSeconds)
	}
	if got, want := len(resp["agent_secret"].(string)), credentialBytes*2; got != want {
		t.Errorf("agent_secret 长度 = %d, want %d", got, want)
	}
	if got, want := len(resp["sign_key"].(string)), credentialBytes*2; got != want {
		t.Errorf("sign_key 长度 = %d, want %d", got, want)
	}

	list, err := defaultRegistry.snapshot()
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("注册后应有 1 条记录, 实际 %d", len(list))
	}
	if list[0].Secret != resp["agent_secret"] || list[0].SignKey != resp["sign_key"] {
		t.Error("下发的凭据必须与落库的一致")
	}
	if list[0].LastIP == "" {
		t.Error("注册时应记录来源 IP")
	}
}

func TestEnroll_IdempotentAndRotatesCredentials(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, true, "")
	simulateRestart()

	engine := newTestEngine()
	first := enrollAgent(t, engine, "m-1")
	second := enrollAgent(t, engine, "m-1")

	if first["agent_id"] != second["agent_id"] {
		t.Errorf("同一 machine_id 应复用节点 id: %v != %v", first["agent_id"], second["agent_id"])
	}
	if first["agent_secret"] == second["agent_secret"] || first["sign_key"] == second["sign_key"] {
		t.Error("重新注册必须轮换凭据")
	}

	list, err := defaultRegistry.snapshot()
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("重新注册不应产生新记录, 实际 %d 条", len(list))
	}
}

func TestEnroll_OldCredentialsRejectedAfterRotation(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, true, "")
	simulateRestart()

	engine := newTestEngine()
	first := enrollAgent(t, engine, "m-1")
	agentID := first["agent_id"].(string)
	oldSecret := first["agent_secret"].(string)

	// 轮换前旧凭据可用
	if recorder := doRequest(t, engine, http.MethodPost, "/api/agent/heartbeat", `{}`, agentAuthHeaders(agentID, oldSecret)); recorder.Code != http.StatusOK {
		t.Fatalf("轮换前旧凭据应可用: HTTP %d", recorder.Code)
	}

	second := enrollAgent(t, engine, "m-1")
	newSecret := second["agent_secret"].(string)

	recorder := doRequest(t, engine, http.MethodPost, "/api/agent/heartbeat", `{}`, agentAuthHeaders(agentID, oldSecret))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("轮换后旧凭据必须失效: HTTP %d", recorder.Code)
	}
	if code := errorCodeOf(t, decodeMap(t, recorder.Body.Bytes())); code != codeUnauthorized {
		t.Errorf("错误码 = %q, want %q", code, codeUnauthorized)
	}

	if recorder := doRequest(t, engine, http.MethodPost, "/api/agent/heartbeat", `{}`, agentAuthHeaders(agentID, newSecret)); recorder.Code != http.StatusOK {
		t.Fatalf("轮换后新凭据应可用: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestEnroll_TokenRejected(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, true, "")
	simulateRestart()

	engine := newTestEngine()
	cases := []struct {
		name  string
		token string
	}{
		{"错误 Token", "wrong-token-0123456789"},
		{"空 Token", ""},
		{"前缀相同的 Token", testEnrollToken + "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"enroll_token": tc.token, "machine_id": "m-x", "listen_port": 8790,
			})
			if err != nil {
				t.Fatalf("序列化失败: %v", err)
			}

			recorder := doRequest(t, engine, http.MethodPost, "/api/agent/enroll", string(body), nil)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("HTTP = %d, want 401", recorder.Code)
			}
			resp := decodeMap(t, recorder.Body.Bytes())
			if code := errorCodeOf(t, resp); code != codeEnrollTokenInvalid {
				t.Errorf("错误码 = %q, want %q", code, codeEnrollTokenInvalid)
			}
			// 错误消息不得回显任何一方的 Token 值
			if strings.Contains(recorder.Body.String(), testEnrollToken) || (tc.token != "" && strings.Contains(recorder.Body.String(), tc.token)) {
				t.Errorf("错误响应回显了 Token: %s", recorder.Body.String())
			}
		})
	}

	list, err := defaultRegistry.snapshot()
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("注册被拒绝时不得产生任何记录: %+v", list)
	}
}

func TestEnroll_Validation(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, true, "")
	simulateRestart()

	engine := newTestEngine()
	cases := []struct {
		name string
		body string
	}{
		{"不是合法 JSON", `{not json`},
		{"缺少 machine_id", `{"enroll_token":"` + testEnrollToken + `","listen_port":8790}`},
		{"listen_port 为 0", `{"enroll_token":"` + testEnrollToken + `","machine_id":"m","listen_port":0}`},
		{"listen_port 越界", `{"enroll_token":"` + testEnrollToken + `","machine_id":"m","listen_port":70000}`},
		{"public_base_url 非法", `{"enroll_token":"` + testEnrollToken + `","machine_id":"m","listen_port":8790,"public_base_url":"10.0.0.1:8790"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doRequest(t, engine, http.MethodPost, "/api/agent/enroll", tc.body, nil)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("HTTP = %d, want 400, body=%s", recorder.Code, recorder.Body.String())
			}
			if code := errorCodeOf(t, decodeMap(t, recorder.Body.Bytes())); code != codeValidationError {
				t.Errorf("错误码 = %q, want %q", code, codeValidationError)
			}
		})
	}

	list, err := defaultRegistry.snapshot()
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("参数校验失败时不得产生任何记录: %+v", list)
	}
}

func TestAgentEndpoints_Disabled(t *testing.T) {
	basePath := setupStateDir(t)
	setupFullConfig(t, false, "")
	simulateRestart()

	engine := newTestEngine()
	cases := []struct {
		method string
		target string
		body   string
	}{
		{http.MethodPost, "/api/agent/enroll", `{"enroll_token":"` + testEnrollToken + `","machine_id":"m","listen_port":8790}`},
		{http.MethodPost, "/api/agent/heartbeat", `{}`},
		{http.MethodGet, "/api/agent/download-link?file_id=x", ""},
		{http.MethodGet, "/install.sh", ""},
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			recorder := doRequest(t, engine, tc.method, tc.target, tc.body, nil)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("未启用时 HTTP = %d, want 403", recorder.Code)
			}
			if code := errorCodeOf(t, decodeMap(t, recorder.Body.Bytes())); code != codeDisabled {
				t.Errorf("错误码 = %q, want %q", code, codeDisabled)
			}
		})
	}

	// 未启用时不得在磁盘上留下任何痕迹
	if _, err := os.Stat(filepath.Join(basePath, DirName)); !os.IsNotExist(err) {
		t.Error("未启用时不应创建状态目录")
	}
}

func TestHeartbeat_Success(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, true, "")
	simulateRestart()

	engine := newTestEngine()
	enrolled := enrollAgent(t, engine, "m-1")
	agentID := enrolled["agent_id"].(string)
	secret := enrolled["agent_secret"].(string)

	recorder := doRequest(t, engine, http.MethodPost, "/api/agent/heartbeat",
		`{"active_streams":3,"version":"v1.1.0","uptime_seconds":100,"listen_port":8890,"public_base_url":"http://node.example.com:8890"}`,
		agentAuthHeaders(agentID, secret))
	if recorder.Code != http.StatusOK {
		t.Fatalf("心跳应成功: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
	}

	resp := decodeMap(t, recorder.Body.Bytes())
	if resp["ok"] != true || resp["enabled"] != true {
		t.Errorf("心跳响应 = %v, want ok=true enabled=true", resp)
	}
	if got := resp["heartbeat_interval_seconds"]; got != float64(config.AgentHeartbeatIntervalSeconds) {
		t.Errorf("heartbeat_interval_seconds = %v, want %d", got, config.AgentHeartbeatIntervalSeconds)
	}

	list, err := defaultRegistry.snapshot()
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	rec := list[0]
	if rec.ActiveStreams != 3 || rec.Version != "v1.1.0" || rec.ListenPort != 8890 {
		t.Errorf("心跳未更新节点信息: %+v", rec)
	}
	if rec.PublicBaseURL != "http://node.example.com:8890" {
		t.Errorf("心跳未更新 public_base_url: %q", rec.PublicBaseURL)
	}
	if rec.LastSeenAt.IsZero() {
		t.Error("心跳应刷新 LastSeenAt")
	}
}

func TestHeartbeat_AuthFailuresAreIndistinguishable(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, true, "")
	simulateRestart()

	engine := newTestEngine()
	enrolled := enrollAgent(t, engine, "m-1")
	agentID := enrolled["agent_id"].(string)
	secret := enrolled["agent_secret"].(string)

	// 枚举防护: 节点不存在与凭据错误必须同状态同文案
	unknown := doRequest(t, engine, http.MethodPost, "/api/agent/heartbeat", `{}`, agentAuthHeaders("no-such-agent", secret))
	wrongSecret := doRequest(t, engine, http.MethodPost, "/api/agent/heartbeat", `{}`, agentAuthHeaders(agentID, strings.Repeat("f", credentialBytes*2)))
	noHeader := doRequest(t, engine, http.MethodPost, "/api/agent/heartbeat", `{}`, nil)

	for name, recorder := range map[string]*httptest.ResponseRecorder{
		"节点不存在": unknown, "凭据错误": wrongSecret, "缺少鉴权头": noHeader,
	} {
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s: HTTP = %d, want 401", name, recorder.Code)
		}
		if unknown.Body.String() != recorder.Body.String() {
			t.Errorf("%s: 响应体应完全一致\n实际 %s\n基线 %s", name, recorder.Body.String(), unknown.Body.String())
		}
	}
}

func TestHeartbeat_DisabledNodeReportsEnabledFalse(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, true, "")
	simulateRestart()

	engine := newTestEngine()
	enrolled := enrollAgent(t, engine, "m-1")
	agentID := enrolled["agent_id"].(string)
	secret := enrolled["agent_secret"].(string)

	if _, err := defaultRegistry.setEnabled(agentID, false, time.Now()); err != nil {
		t.Fatalf("禁用节点失败: %v", err)
	}

	recorder := doRequest(t, engine, http.MethodPost, "/api/agent/heartbeat", `{}`, agentAuthHeaders(agentID, secret))
	if recorder.Code != http.StatusOK {
		t.Fatalf("被禁用的节点仍应能心跳(解禁后自动恢复): HTTP %d", recorder.Code)
	}
	resp := decodeMap(t, recorder.Body.Bytes())
	if resp["enabled"] != false {
		t.Errorf("被禁用的节点应收到 enabled=false, 实际 %v", resp)
	}
	if resp["ok"] != true {
		t.Errorf("心跳本身仍是成功的: %v", resp)
	}
}

func TestHeartbeat_InvalidPublicBaseURLRejected(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, true, "")
	simulateRestart()

	engine := newTestEngine()
	enrolled := enrollAgent(t, engine, "m-1")
	agentID := enrolled["agent_id"].(string)
	secret := enrolled["agent_secret"].(string)

	recorder := doRequest(t, engine, http.MethodPost, "/api/agent/heartbeat",
		`{"public_base_url":"10.0.0.1:8790"}`, agentAuthHeaders(agentID, secret))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("非法 public_base_url 应返回 400: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
	}

	list, err := defaultRegistry.snapshot()
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	if !list[0].LastSeenAt.IsZero() {
		t.Error("参数不合法时不应刷新心跳时间(不得更新任何状态)")
	}
	if list[0].PublicBaseURL != "" {
		t.Errorf("参数不合法时不应写入 public_base_url: %q", list[0].PublicBaseURL)
	}
}
