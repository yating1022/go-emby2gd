package agentnet

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
)

// adminPost 发起一次管理接口请求
func adminPost(t *testing.T, engine http.Handler, target, body string) map[string]any {
	t.Helper()

	recorder := doRequest(t, engine, http.MethodPost, target, body, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s HTTP = %d, want 200, body=%s", target, recorder.Code, recorder.Body.String())
	}
	return decodeMap(t, recorder.Body.Bytes())
}

// agentsOf 取出列表响应里的 data.agents
//
// 同时断言 data 字段本身存在 —— 空列表时也不能被 omitempty 吃掉。
func agentsOf(t *testing.T, resp map[string]any) []any {
	t.Helper()

	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 data 对象: %v", resp)
	}
	agents, ok := data["agents"].([]any)
	if !ok {
		t.Fatalf("响应缺少 data.agents 数组: %v", data)
	}
	return agents
}

func TestAdminListAgents_Desensitized(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, true, "")
	simulateRestart()

	engine := newTestEngine()
	first := enrollAgent(t, engine, "m-1")
	second := enrollAgent(t, engine, "m-2")

	// 让其中一个节点心跳一次, 观察 online 与运行时字段
	if recorder := doRequest(t, engine, http.MethodPost, "/api/agent/heartbeat",
		`{"active_streams":2,"version":"v1.2.0","listen_port":8890}`,
		agentAuthHeaders(second["agent_id"].(string), second["agent_secret"].(string))); recorder.Code != http.StatusOK {
		t.Fatalf("心跳失败: HTTP %d", recorder.Code)
	}

	recorder := doRequest(t, engine, http.MethodPost, "/ge2o/agent-network/agents",
		`{"secret":"`+testGe2oSecret+`"}`, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP = %d, want 200", recorder.Code)
	}
	resp := decodeMap(t, recorder.Body.Bytes())
	if resp["success"] != true {
		t.Fatalf("列表失败: %v", resp)
	}
	agents := agentsOf(t, resp)
	if len(agents) != 2 {
		t.Fatalf("节点数 = %d, want 2", len(agents))
	}

	// 硬约束: 序列化结果里绝不能出现任何凭据原文
	raw := recorder.Body.String()
	for name, value := range map[string]string{
		"agent_secret": first["agent_secret"].(string),
		"sign_key":     first["sign_key"].(string),
		"注册 Token":     testEnrollToken,
	} {
		if strings.Contains(raw, value) {
			t.Errorf("节点列表泄露了 %s: %s", name, raw)
		}
	}

	// 逐个字段核对形状与取值
	byID := map[string]map[string]any{}
	for _, item := range agents {
		view, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("节点视图不是对象: %v", item)
		}
		for _, key := range []string{
			"id", "name", "machine_id", "enabled", "online", "version", "last_seen_at",
			"last_ip", "active_streams", "public_base_url", "address", "listen_port",
			"created_at", "updated_at",
		} {
			if _, ok := view[key]; !ok {
				t.Errorf("节点视图缺少字段 %s: %v", key, view)
			}
		}
		if _, hasSecret := view["secret"]; hasSecret {
			t.Errorf("节点视图不应包含 secret 字段: %v", view)
		}
		if _, hasSignKey := view["sign_key"]; hasSignKey {
			t.Errorf("节点视图不应包含 sign_key 字段: %v", view)
		}
		byID[view["id"].(string)] = view
	}

	offline := byID[first["agent_id"].(string)]
	if offline["online"] != false {
		t.Errorf("从未心跳的节点应为离线: %v", offline)
	}
	if offline["last_seen_at"] != "" {
		t.Errorf("从未心跳的节点 last_seen_at 应为空串: %v", offline["last_seen_at"])
	}
	if offline["enabled"] != true {
		t.Errorf("新注册节点应默认启用: %v", offline["enabled"])
	}
	if addr, _ := offline["address"].(string); !strings.HasPrefix(addr, "http://") {
		t.Errorf("address 应由来源 IP 推导, 实际 %q", addr)
	}

	online := byID[second["agent_id"].(string)]
	if online["online"] != true {
		t.Errorf("刚心跳过的节点应在线: %v", online)
	}
	if online["active_streams"] != float64(2) || online["version"] != "v1.2.0" || online["listen_port"] != float64(8890) {
		t.Errorf("运行时字段不正确: %v", online)
	}
	if online["last_seen_at"] == "" || online["last_ip"] != "192.0.2.1" {
		t.Errorf("心跳字段不正确: last_seen_at=%v last_ip=%v", online["last_seen_at"], online["last_ip"])
	}
}

func TestAdminListAgents_EmptyListKeepsDataField(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, true, "")
	simulateRestart()

	resp := adminPost(t, newTestEngine(), "/ge2o/agent-network/agents", `{"secret":"`+testGe2oSecret+`"}`)
	if resp["success"] != true {
		t.Fatalf("空列表也应成功: %v", resp)
	}
	if agents := agentsOf(t, resp); len(agents) != 0 {
		t.Errorf("没有节点时 agents 应为空数组, 实际 %v", agents)
	}
}

func TestAdmin_SecretAndStateRejections(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, true, "")
	simulateRestart()

	engine := newTestEngine()
	targets := []string{
		"/ge2o/agent-network/agents",
		"/ge2o/agent-network/agents/update",
		"/ge2o/agent-network/agents/delete",
		"/ge2o/agent-network/install-command",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			// 密钥错误
			resp := adminPost(t, engine, target, `{"secret":"wrong-secret"}`)
			if resp["success"] != false || resp["message"] != "密钥错误" {
				t.Errorf("密钥错误时响应 = %v", resp)
			}

			// 请求参数错误(非法 JSON)
			resp = adminPost(t, engine, target, `{not json`)
			if resp["success"] != false || resp["message"] != "请求参数错误" {
				t.Errorf("非法 JSON 时响应 = %v", resp)
			}

			// 非 POST 一律 404(不暴露接口存在性)
			recorder := doRequest(t, engine, http.MethodGet, target, "", nil)
			if recorder.Code != http.StatusNotFound {
				t.Errorf("GET HTTP = %d, want 404", recorder.Code)
			}
		})
	}
}

func TestAdmin_MissingLocalSecret(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, true, "")
	simulateRestart()

	// 本地密钥未配置时给出明确提示(而不是"密钥错误")
	old := config.C.Ge2o
	config.C.Ge2o = &config.Ge2o{}
	t.Cleanup(func() { config.C.Ge2o = old })

	resp := adminPost(t, newTestEngine(), "/ge2o/agent-network/agents", `{"secret":"x"}`)
	if resp["success"] != false || resp["message"] != "请先配置本地密钥" {
		t.Errorf("未配置本地密钥时响应 = %v", resp)
	}
}

func TestAdmin_DisabledMessages(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, false, "")
	simulateRestart()

	engine := newTestEngine()
	for _, target := range []string{
		"/ge2o/agent-network/agents",
		"/ge2o/agent-network/agents/update",
		"/ge2o/agent-network/agents/delete",
		"/ge2o/agent-network/install-command",
	} {
		resp := adminPost(t, engine, target, `{"secret":"`+testGe2oSecret+`"}`)
		if resp["success"] != false {
			t.Errorf("%s 未启用时不应成功: %v", target, resp)
		}
		message, _ := resp["message"].(string)
		if !strings.Contains(message, "未启用") {
			t.Errorf("%s 未启用时的提示 = %q", target, message)
		}
	}
}

func TestAdminUpdate_TogglesEnabled(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, true, "")
	simulateRestart()

	engine := newTestEngine()
	enrolled := enrollAgent(t, engine, "m-1")
	agentID := enrolled["agent_id"].(string)
	secret := enrolled["agent_secret"].(string)

	update := func(enabled bool) map[string]any {
		t.Helper()
		body, err := json.Marshal(map[string]any{"secret": testGe2oSecret, "id": agentID, "enabled": enabled})
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		return adminPost(t, engine, "/ge2o/agent-network/agents/update", string(body))
	}

	if resp := update(false); resp["success"] != true {
		t.Fatalf("禁用节点失败: %v", resp)
	}
	if recorder := doRequest(t, engine, http.MethodPost, "/api/agent/heartbeat", `{}`, agentAuthHeaders(agentID, secret)); recorder.Code != http.StatusOK {
		t.Fatalf("被禁用的节点仍应能心跳: HTTP %d", recorder.Code)
	} else if resp := decodeMap(t, recorder.Body.Bytes()); resp["enabled"] != false {
		t.Errorf("禁用状态未生效: %v", resp)
	}

	if resp := update(true); resp["success"] != true {
		t.Fatalf("启用节点失败: %v", resp)
	}

	// 不存在的节点
	body, _ := json.Marshal(map[string]any{"secret": testGe2oSecret, "id": "no-such-agent", "enabled": false})
	if resp := adminPost(t, engine, "/ge2o/agent-network/agents/update", string(body)); resp["message"] != "节点不存在" {
		t.Errorf("不存在的节点应提示节点不存在: %v", resp)
	}

	// 缺少 id
	if resp := adminPost(t, engine, "/ge2o/agent-network/agents/update", `{"secret":"`+testGe2oSecret+`"}`); resp["message"] != "缺少节点 id" {
		t.Errorf("缺少 id 时应明确提示: %v", resp)
	}
}

func TestAdminDelete_RevokesCredentials(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, true, "")
	simulateRestart()

	engine := newTestEngine()
	enrolled := enrollAgent(t, engine, "m-1")
	agentID := enrolled["agent_id"].(string)
	secret := enrolled["agent_secret"].(string)

	body, _ := json.Marshal(map[string]any{"secret": testGe2oSecret, "id": agentID})
	if resp := adminPost(t, engine, "/ge2o/agent-network/agents/delete", string(body)); resp["success"] != true {
		t.Fatalf("删除节点失败: %v", resp)
	}

	// 删除即吊销: 旧凭据立即可知地失效
	if recorder := doRequest(t, engine, http.MethodPost, "/api/agent/heartbeat", `{}`, agentAuthHeaders(agentID, secret)); recorder.Code != http.StatusUnauthorized {
		t.Errorf("删除后旧凭据应失效: HTTP %d", recorder.Code)
	}
	if agents := agentsOf(t, adminPost(t, engine, "/ge2o/agent-network/agents", `{"secret":"`+testGe2oSecret+`"}`)); len(agents) != 0 {
		t.Errorf("删除后列表应为空: %v", agents)
	}

	// 重复删除
	if resp := adminPost(t, engine, "/ge2o/agent-network/agents/delete", string(body)); resp["message"] != "节点不存在" {
		t.Errorf("重复删除应提示节点不存在: %v", resp)
	}
}

func TestAdminInstallCommand(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, true, "")
	simulateRestart()

	resp := adminPost(t, newTestEngine(), "/ge2o/agent-network/install-command", `{"secret":"`+testGe2oSecret+`"}`)
	if resp["success"] != true {
		t.Fatalf("获取安装命令失败: %v", resp)
	}

	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 data: %v", resp)
	}
	if data["master_url"] != "http://example.com" {
		t.Errorf("master_url = %v, want http://example.com", data["master_url"])
	}
	command, _ := data["command"].(string)
	wantCommand := "curl -fsSL http://example.com/install.sh | sudo bash -s -- --master http://example.com --token " + testEnrollToken
	if command != wantCommand {
		t.Errorf("安装命令 = %q, want %q", command, wantCommand)
	}
}
