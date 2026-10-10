package emby_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/agentnet"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/emby"
)

// hub 缓存中心接入的网关预热用例(浏览链路)
//
// 三条要求驱动这组用例:
//   - hub 接通时, 预热目标从"戳边缘节点"改为向 hub 下发 /warm(携带直链与凭据);
//   - hub 路径任何失败(超时 / 非 200 / 不可达)都必须回退到原有"戳边缘";
//   - hub-enable 关闭时, 行为与未部署 hub 完全一致(边缘探测照旧, 一个 /warm 都不发)。

const (
	// agentTestHubSignKey hub 记录的签名密钥(协议要求 64 位 hex; hub 本身不签名)
	agentTestHubSignKey = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	// testGoogleAuthHeader 假面板下发的 Google 凭据(与面板用例同一个值)
	testGoogleAuthHeader = "Bearer test-google-access-token"
)

// agentTestHubSecret hub 的心跳凭据(64 位 hex)
var agentTestHubSecret = strings.Repeat("7", 64)

// preheatHubEntryJSON 拼一条 role=hub 的注册表记录
//
// last_ip 固定 127.0.0.1: hub 的内网基址由来源 IP + agent-network.hub-port 推导,
// 用例里的假 hub 正好监听 127.0.0.1:<hub-port>。
func preheatHubEntryJSON(hubID string) string {
	return fmt.Sprintf(`{"id":%q,"machine_id":%q,"name":"hub-1","secret":%q,"sign_key":%q,"role":"hub",`+
		`"last_ip":"127.0.0.1","listen_port":8791,"version":"v0.4.0","enabled":true,`+
		`"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`,
		hubID, "machine-"+hubID, agentTestHubSecret, agentTestHubSignKey)
}

// heartbeatTestHub 让 hub 心跳一次, 进入健康候选集
//
// 来源 IP 通过 X-Forwarded-For 固定为 127.0.0.1(gin 默认信任代理头),
// 否则注册表里的来源 IP 会变成测试请求的假地址, 内网基址就拼不出来了。
func heartbeatTestHub(t *testing.T, hubID string) {
	t.Helper()

	recorder := callAgentEndpoint(t, agentnet.Heartbeat, "/api/agent/heartbeat", `{}`,
		map[string]string{
			"X-Agent-Id":      hubID,
			"Authorization":   "Bearer " + agentTestHubSecret,
			"X-Forwarded-For": "127.0.0.1",
		})
	if recorder.Code != http.StatusOK {
		t.Fatalf("hub 心跳失败: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
	}
}

// preheatHubAgentConfig 构造启用 hub 接入的 agent 网络配置
//
// 走 yaml + Init 的真实解析路径(hub 端口/超时是只有配置文件能写的字段)。
func preheatHubAgentConfig(t *testing.T, hubPort int, hubEnabled bool) *config.AgentNetwork {
	t.Helper()

	doc := fmt.Sprintf("enable: true\nenroll-token: test-enroll-token-0123456789\noffline-seconds: 45\n"+
		"url-ttl: 1h\nhub-enable: %t\nhub-port: %d\nhub-warm-timeout: 1s\n", hubEnabled, hubPort)
	return mustAgentConfig(t, doc)
}

// preheatFakePanel 假 GD 面板: 只实现 /api/dl
func newPreheatFakePanel(t *testing.T, directURL string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		payload, err := json.Marshal(map[string]any{
			"ok": true,
			"data": map[string]any{
				"url":        directURL,
				"headers":    map[string]string{"Authorization": testGoogleAuthHeader},
				"expires_at": "",
			},
		})
		if err != nil {
			t.Errorf("构造面板响应失败: %v", err)
			return
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)

	return server
}

// preheatFakeHub 假 hub(只实现控制面 /warm)
type preheatFakeHub struct {
	// server 假服务端
	server *httptest.Server
	// port 监听端口: 即用例配置里的 agent-network.hub-port
	port int
	// status 响应状态码
	status atomic.Int32
	// hang 为 true 时挂起不响应, 直到 unblock
	hang atomic.Bool
	// release 解除挂起
	release chan struct{}
	// releaseOnce 保证 release 只被关闭一次
	releaseOnce sync.Once
	// calls 收到的请求数
	calls atomic.Int64
	// mu 保护 body / method / path
	mu sync.Mutex
	// body 最近一次请求体
	body []byte
	// path 最近一次请求路径
	path string
	// method 最近一次请求方法
	method string
}

// newPreheatFakeHub 启动假 hub
func newPreheatFakeHub(t *testing.T, status int) *preheatFakeHub {
	t.Helper()

	f := &preheatFakeHub{release: make(chan struct{})}
	f.status.Store(int32(status))
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10))

		f.mu.Lock()
		f.body, f.path, f.method = body, r.URL.Path, r.Method
		f.mu.Unlock()

		if f.hang.Load() {
			<-f.release
		}
		w.WriteHeader(int(f.status.Load()))
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	// 注册顺序即执行顺序的反向: unblock 必须先于 Close, 否则 Close 会一直等挂起的请求
	t.Cleanup(f.server.Close)
	t.Cleanup(f.unblock)

	parsed, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatalf("解析假 hub 地址失败: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("解析假 hub 端口失败: %v", err)
	}
	f.port = port

	return f
}

// unblock 解除挂起(幂等)
func (f *preheatFakeHub) unblock() {
	f.releaseOnce.Do(func() { close(f.release) })
}

// lastPayload 取回最近一次 /warm 的路径与载荷
//
// 解码成 map 而不是生产结构体: 字段名是冻结的协议契约, 两侧共用结构体会一起错。
func (f *preheatFakeHub) lastPayload(t *testing.T) (string, string, map[string]any) {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.body) == 0 {
		t.Fatal("假 hub 没有收到任何请求体")
	}

	var payload map[string]any
	if err := json.Unmarshal(f.body, &payload); err != nil {
		t.Fatalf("warm 请求体不是合法 JSON: %v, body=%s", err, f.body)
	}
	return f.method, f.path, payload
}

// waitForHubCalls 等待假 hub 收到至少 want 个请求
func waitForHubCalls(t *testing.T, hub *preheatFakeHub, want int64) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if hub.calls.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 hub 收到 %d 个请求超时, 实际: %d", want, hub.calls.Load())
}

// firePreheatViaPlaybackInfo 打一次 PlaybackInfo, 触发一次异步预热
func firePreheatViaPlaybackInfo(t *testing.T) {
	t.Helper()

	c, recorder := newRedirectContext(t, "/emby/Items/123/PlaybackInfo?api_key=test-key")
	emby.TransferPlaybackInfo(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("主流程响应码 = %d, want 200, body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "MediaSources") {
		t.Errorf("主流程响应不受预热影响, 实际: %s", recorder.Body.String())
	}
}

// TestPreheat_HubAcceptedSkipsEdge hub 接受预热时不再打扰边缘节点
func TestPreheat_HubAcceptedSkipsEdge(t *testing.T) {
	seq := gdPathSeq.Add(1)
	gdPath := fmt.Sprintf("/影视库/预热/中枢-%d.mkv", seq)
	fileID := fmt.Sprintf("1PreheatHub-%d", seq)
	hubID := fmt.Sprintf("hub-preheat-%d", seq)
	directURL := "https://www.googleapis.com/drive/v3/files/" + fileID + "?alt=media"

	origin := newPreheatEmbyOrigin(t, "/home/googleDrive"+gdPath, "Movie")
	edge := newPreheatFakeAgent(t, false)
	hub := newPreheatFakeHub(t, http.StatusOK)
	panel := newPreheatFakePanel(t, directURL)

	basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+
		preheatAgentEntryJSON(edge.server.URL)+`,`+preheatHubEntryJSON(hubID)+`]}`)
	agentCfg := preheatHubAgentConfig(t, hub.port, true)
	withPreheatTestConfig(t, origin.server.URL, panel.URL, agentCfg, basePath)
	heartbeatTestAgent(t, 0)
	heartbeatTestHub(t, hubID)

	logger := captureRedirectLogs(t)
	firePreheatViaPlaybackInfo(t)

	waitForHubCalls(t, hub, 1)
	waitForLog(t, logger, "预热: hub 接受")

	// 边缘节点一次都不该被打扰(它明明在线且可调度)
	assertNoAgentCalls(t, edge)

	method, path, payload := hub.lastPayload(t)
	if method != http.MethodPost || path != "/warm" {
		t.Errorf("warm 请求 = %s %s, want POST /warm", method, path)
	}
	if payload["file_id"] != fileID {
		t.Errorf("file_id = %v, want %s", payload["file_id"], fileID)
	}
	if payload["direct_link"] != directURL {
		t.Errorf("direct_link = %v, want %s", payload["direct_link"], directURL)
	}
	if payload["file_token"] == nil || payload["file_token"] == "" {
		t.Errorf("file_token 缺失: %v", payload)
	}
	auth, ok := payload["auth"].(map[string]any)
	if !ok || auth["Authorization"] != testGoogleAuthHeader {
		t.Errorf("auth 应携带面板下发的凭据, 实际: %v", payload["auth"])
	}
	if _, ok := payload["regions"].(map[string]any); !ok {
		t.Errorf("regions 缺失: %v", payload)
	}

	// 日志卫生: 直链与凭据不得进日志
	logged := logger.String()
	for name, secretValue := range map[string]string{
		"Google 凭据": testGoogleAuthHeader,
		"完整直链":      directURL,
		"hub 心跳凭据":  agentTestHubSecret,
	} {
		if strings.Contains(logged, secretValue) {
			t.Errorf("日志里出现了 %s: %q", name, logged)
		}
	}
}

// TestPreheat_HubFailureFallsBackToEdge hub 指令失败时报错回退, 边缘探测照旧
func TestPreheat_HubFailureFallsBackToEdge(t *testing.T) {
	seq := gdPathSeq.Add(1)
	gdPath := fmt.Sprintf("/影视库/预热/中枢失败-%d.mkv", seq)
	hubID := fmt.Sprintf("hub-preheat-fail-%d", seq)

	origin := newPreheatEmbyOrigin(t, "/home/googleDrive"+gdPath, "Movie")
	edge := newPreheatFakeAgent(t, false)
	hub := newPreheatFakeHub(t, http.StatusInternalServerError)
	panel := newPreheatFakePanel(t, "https://www.googleapis.com/drive/v3/files/1PreheatFail?alt=media")

	basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+
		preheatAgentEntryJSON(edge.server.URL)+`,`+preheatHubEntryJSON(hubID)+`]}`)
	agentCfg := preheatHubAgentConfig(t, hub.port, true)
	withPreheatTestConfig(t, origin.server.URL, panel.URL, agentCfg, basePath)
	heartbeatTestAgent(t, 0)
	heartbeatTestHub(t, hubID)

	logger := captureRedirectLogs(t)
	firePreheatViaPlaybackInfo(t)

	// 回退链: hub 失败 → 戳边缘节点(探测形状与未接入 hub 时逐字一致)
	waitForAgentCalls(t, edge, 1)
	waitForLog(t, logger, "hub 预热失败, 回退节点预热")
	assertPreheatProbe(t, edge, gdPath)
	if got := hub.calls.Load(); got != 1 {
		t.Errorf("hub 应恰好被尝试 1 次, 实际 %d", got)
	}
}

// TestPreheat_HubTimeoutFallsBackToEdge hub 超时同样不能拖住回退
func TestPreheat_HubTimeoutFallsBackToEdge(t *testing.T) {
	seq := gdPathSeq.Add(1)
	gdPath := fmt.Sprintf("/影视库/预热/中枢超时-%d.mkv", seq)
	hubID := fmt.Sprintf("hub-preheat-timeout-%d", seq)

	origin := newPreheatEmbyOrigin(t, "/home/googleDrive"+gdPath, "Movie")
	edge := newPreheatFakeAgent(t, false)
	hub := newPreheatFakeHub(t, http.StatusOK)
	hub.hang.Store(true)
	panel := newPreheatFakePanel(t, "https://www.googleapis.com/drive/v3/files/1PreheatTimeout?alt=media")

	basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+
		preheatAgentEntryJSON(edge.server.URL)+`,`+preheatHubEntryJSON(hubID)+`]}`)
	agentCfg := preheatHubAgentConfig(t, hub.port, true)
	withPreheatTestConfig(t, origin.server.URL, panel.URL, agentCfg, basePath)
	heartbeatTestAgent(t, 0)
	heartbeatTestHub(t, hubID)

	logger := captureRedirectLogs(t)
	firePreheatViaPlaybackInfo(t)

	// hub-warm-timeout 是 1s: 超时后必须落到边缘节点
	waitForAgentCalls(t, edge, 1)
	waitForLog(t, logger, "hub 预热失败, 回退节点预热")
	assertPreheatProbe(t, edge, gdPath)
	if got := hub.calls.Load(); got != 1 {
		t.Errorf("hub 应恰好被尝试 1 次, 实际 %d", got)
	}
}

// TestPreheat_HubDisabledKeepsEdgeProbe hub-enable 关闭时行为与未部署 hub 一致
//
// 注册表里放一台健康的 hub: 关闭时它既不能接手预热, 也不能参与客户端调度,
// 预热必须原样落到边缘节点上。
func TestPreheat_HubDisabledKeepsEdgeProbe(t *testing.T) {
	seq := gdPathSeq.Add(1)
	gdPath := fmt.Sprintf("/影视库/预热/中枢关闭-%d.mkv", seq)
	hubID := fmt.Sprintf("hub-preheat-off-%d", seq)

	origin := newPreheatEmbyOrigin(t, "/home/googleDrive"+gdPath, "Movie")
	edge := newPreheatFakeAgent(t, false)
	hub := newPreheatFakeHub(t, http.StatusOK)
	panel := newPreheatFakePanel(t, "https://www.googleapis.com/drive/v3/files/1PreheatOff?alt=media")

	basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+
		preheatAgentEntryJSON(edge.server.URL)+`,`+preheatHubEntryJSON(hubID)+`]}`)
	agentCfg := preheatHubAgentConfig(t, hub.port, false)
	withPreheatTestConfig(t, origin.server.URL, panel.URL, agentCfg, basePath)
	heartbeatTestAgent(t, 0)
	heartbeatTestHub(t, hubID)

	logger := captureRedirectLogs(t)
	firePreheatViaPlaybackInfo(t)

	waitForAgentCalls(t, edge, 1)
	waitForLog(t, logger, "[网关预热] 已触发")
	assertPreheatProbe(t, edge, gdPath)

	if got := hub.calls.Load(); got != 0 {
		t.Errorf("hub 接入关闭时不应下发任何指令, 实际 %d 次", got)
	}
	if strings.Contains(logger.String(), "hub") {
		t.Errorf("关闭时日志不应出现 hub 相关事件: %s", logger.String())
	}
}
