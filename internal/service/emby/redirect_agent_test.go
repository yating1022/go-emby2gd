package emby_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/agentnet"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/emby"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// 测试用节点凭据(固定值: 签名要能被独立重算, 不能每次随机)
const (
	// agentTestID 节点 id
	agentTestID = "agent-test-1"
	// agentTestMachineID 节点机器标识
	agentTestMachineID = "machine-test-1"
	// agentTestSignKey 节点签名密钥(64 位 hex, 协议要求)
	agentTestSignKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// agentTestPublicBaseURL 节点上报的对外地址
	agentTestPublicBaseURL = "http://node.example.com:8790"
)

// agentTestSecret 节点心跳凭据(64 位 hex)
var agentTestSecret = strings.Repeat("5", 64)

// gdPathSeq 生成用例内唯一 Drive 路径的自增序号
var gdPathSeq atomic.Uint64

// uniqueGDPath 把路径的文件名加上进程内唯一序号
//
// gdrive 的直链缓存是进程级全局的、按路径为键且条目不会主动过期: 同一条路径在
// 多次执行之间(如 go test -count=2)会直接命中上一轮缓存的直链 —— 而那条直链
// 指向的假服务器已经关闭, 用例会在看不出来的地方失败。每次执行换一条路径,
// 各用例就只可能命中自己本轮启动的假面板。
func uniqueGDPath(path string) string {
	ext := filepath.Ext(path)
	return fmt.Sprintf("%s-%d%s", strings.TrimSuffix(path, ext), gdPathSeq.Add(1), ext)
}

// agentEntryJSON 拼一条 agents.json 里的节点记录
func agentEntryJSON() string {
	return fmt.Sprintf(`{"id":%q,"machine_id":%q,"name":"node-1","secret":%q,"sign_key":%q,`+
		`"public_base_url":%q,"listen_port":8790,"version":"v1.0.0","last_ip":"","enabled":true,`+
		`"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`,
		agentTestID, agentTestMachineID, agentTestSecret, agentTestSignKey, agentTestPublicBaseURL)
}

// prepareAgentStateDir 准备一个装着 agents.json 的状态目录
//
// 直接写生产格式的注册表(而不是调用注册接口): 本组用例验证的是播放入口
// 与注册表落盘格式之间的黑盒契约, 不依赖 agentnet 包的内部实现。
func prepareAgentStateDir(t *testing.T, content string) string {
	t.Helper()

	basePath := t.TempDir()
	dir := filepath.Join(basePath, agentnet.DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建状态目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agents.json"), []byte(content), 0o600); err != nil {
		t.Fatalf("写入注册表失败: %v", err)
	}
	return basePath
}

// mustAgentConfig 按真实配置解析路径构造 agent 网络配置
//
// 走 yaml + Init 而不是直接构造结构体: fallback-to-local 的默认值与显式 false
// 必须靠 UnmarshalYAML 区分, 直接构造绕过了这条契约。
func mustAgentConfig(t *testing.T, doc string) *config.AgentNetwork {
	t.Helper()

	t.Setenv(config.AgentEnrollTokenEnvName, "")

	cfg := new(config.AgentNetwork)
	if err := yaml.Unmarshal([]byte(doc), cfg); err != nil {
		t.Fatalf("解析测试配置失败: %v", err)
	}
	if err := cfg.Init(); err != nil {
		t.Fatalf("初始化测试配置失败: %v", err)
	}
	return cfg
}

// withAgentTestConfig 注入 emby + gdrive + agent 网络三份测试配置
func withAgentTestConfig(t *testing.T, host, apiBase string, agentCfg *config.AgentNetwork, basePath string) {
	t.Helper()

	oldConfig, oldBasePath := config.C, config.BasePath

	gdriveCfg := &config.GDrive{MountPrefix: "/home/googleDrive"}
	if apiBase != "" {
		gdriveCfg.Enable = true
		gdriveCfg.ApiBase = apiBase
		gdriveCfg.ApiToken = "test-panel-api-token"
	}

	config.C = &config.Config{
		Emby:         &config.Emby{Host: host},
		GDrive:       gdriveCfg,
		AgentNetwork: agentCfg,
	}
	config.BasePath = basePath

	t.Cleanup(func() {
		config.C = oldConfig
		config.BasePath = oldBasePath
	})
}

// callAgentEndpoint 直接调用一个 agent 端点处理器
func callAgentEndpoint(t *testing.T, handler gin.HandlerFunc, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	c.Request = httptest.NewRequest(http.MethodPost, target, reader)
	if body != "" {
		c.Request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		c.Request.Header.Set(key, value)
	}

	handler(c)
	return recorder
}

// heartbeatTestAgent 让节点心跳一次, 进入可调度状态
func heartbeatTestAgent(t *testing.T, activeStreams int) {
	t.Helper()

	recorder := callAgentEndpoint(t, agentnet.Heartbeat, "/api/agent/heartbeat",
		fmt.Sprintf(`{"active_streams":%d}`, activeStreams),
		map[string]string{
			"X-Agent-Id":    agentTestID,
			"Authorization": "Bearer " + agentTestSecret,
		})
	if recorder.Code != http.StatusOK {
		t.Fatalf("节点心跳失败: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
	}
}

// agentSignedRedirect 独立校验一次 agent 重定向的地址与签名
//
// 签名按协议(design §3)在小范围内重算: HMAC-SHA256(key = sign_key 的 32 字节,
// msg = "v1\n<file_id>\n<e>") 的小写 hex。刻意不调用被测代码里的签名函数,
// 否则"两侧用同一个错误实现"就测不出来了。
func agentSignedRedirect(t *testing.T, location, wantGDPath string, ttl time.Duration) (fileID, sign string) {
	t.Helper()

	u, err := url.Parse(location)
	if err != nil {
		t.Fatalf("重定向地址不是合法 URL: %v", err)
	}
	if u.Scheme != "http" || u.Host != "node.example.com:8790" {
		t.Errorf("重定向基址 = %s://%s, want http://node.example.com:8790", u.Scheme, u.Host)
	}
	if !strings.HasPrefix(u.Path, "/dl/") {
		t.Fatalf("重定向路径 = %q, want /dl/<file_id>", u.Path)
	}

	fileID = strings.TrimPrefix(u.Path, "/dl/")
	decoded, err := base64.RawURLEncoding.DecodeString(fileID)
	if err != nil {
		t.Fatalf("file_id 不是合法的 base64url: %v", err)
	}
	if string(decoded) != wantGDPath {
		t.Errorf("file_id 解码结果 = %q, want %q", string(decoded), wantGDPath)
	}

	expiry := u.Query().Get("e")
	sign = u.Query().Get("s")
	if expiry == "" || sign == "" {
		t.Fatalf("签名参数缺失: e=%q s=%q", expiry, sign)
	}

	key, err := hex.DecodeString(agentTestSignKey)
	if err != nil {
		t.Fatalf("测试 sign_key 不是合法 hex: %v", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("v1\n" + fileID + "\n" + expiry))
	if want := hex.EncodeToString(mac.Sum(nil)); sign != want {
		t.Errorf("签名不匹配: 实际 %s, 重算得 %s", sign, want)
	}

	expirySeconds, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil {
		t.Fatalf("e 参数不是十进制时间戳: %v", err)
	}
	if delta := time.Until(time.Unix(expirySeconds, 0)) - ttl; delta > time.Minute || delta < -time.Minute {
		t.Errorf("签名时效偏差 %v, 期望约 %v", delta, ttl)
	}

	return fileID, sign
}

// TestRedirect2OpenlistLink_AgentNodeSignedRedirect 有在线节点时 302 到节点
//
// 媒体字节完全绕过本网关与 Emby: 面板与假 Emby 源都不应被命中。
func TestRedirect2OpenlistLink_AgentNodeSignedRedirect(t *testing.T) {
	gdPath := uniqueGDPath("/影视库/agent/成功.mkv")
	origin := newFakeEmbyOrigin(t, "/home/googleDrive"+gdPath)
	drive := newDirectLinkServer(t, "panel-proxied-bytes")
	panel := newFakePanel(t, drive.URL)

	basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+agentEntryJSON()+`]}`)
	agentCfg := mustAgentConfig(t, "enable: true\nenroll-token: test-enroll-token-0123456789\nurl-ttl: 24h\n")
	withAgentTestConfig(t, origin.server.URL, panel.server.URL, agentCfg, basePath)
	heartbeatTestAgent(t, 3)

	logger := captureRedirectLogs(t)
	c, recorder := newRedirectContext(t, "/emby/Videos/123/stream")
	emby.Redirect2OpenlistLink(c)

	if recorder.Code != http.StatusTemporaryRedirect {
		t.Fatalf("响应码 = %d, want %d, body=%s", recorder.Code, http.StatusTemporaryRedirect, recorder.Body.String())
	}
	location := recorder.Header().Get("Location")
	fileID, sign := agentSignedRedirect(t, location, gdPath, agentCfg.ClientURLTTL())

	// 与既有 302 分支同一惯例: 10 分钟内不重复调度
	expired := recorder.Header().Get("Expired")
	if expired == "" {
		t.Error("缺少 Expired 缓存头")
	} else if deadline, err := strconv.ParseInt(expired, 10, 64); err != nil {
		t.Errorf("Expired 头不是时间戳: %q", expired)
	} else {
		want := time.Now().Add(10 * time.Minute).UnixMilli()
		if delta := deadline - want; delta > time.Minute.Milliseconds() || delta < -time.Minute.Milliseconds() {
			t.Errorf("Expired 头 = %d, 与 10 分钟后相差 %d ms", deadline, delta)
		}
	}

	if panel.calls.Load() != 0 {
		t.Errorf("调度到节点时不应调面板, 调用次数 = %d", panel.calls.Load())
	}
	if origin.originHits.Load() != 0 {
		t.Errorf("调度到节点时不应回源, 回源次数 = %d", origin.originHits.Load())
	}

	if !logger.contains("调度到节点") {
		t.Errorf("应留下调度日志, 实际: %s", logger.String())
	}
	// 完整签名地址与签名参数、sign_key 都不得进日志
	for name, secret := range map[string]string{
		"完整签名地址":    location,
		"签名参数":      sign,
		"签名参数(带前缀)": "s=" + sign,
		"sign_key":  agentTestSignKey,
	} {
		if logger.contains(secret) {
			t.Errorf("日志泄露了 %s: %s", name, logger.String())
		}
	}
	if !logger.contains(gdPath) {
		t.Errorf("调度日志应包含文件路径(便于排查): %s", logger.String())
	}
	_ = fileID
	origin.waitForPlaybackProbes(t)
}

// TestRedirect2OpenlistLink_AgentNoNodeFallsBackToLocal 没有可调度节点时回退本机代理
func TestRedirect2OpenlistLink_AgentNoNodeFallsBackToLocal(t *testing.T) {
	gdPath := uniqueGDPath("/影视库/agent/无节点.mkv")
	origin := newFakeEmbyOrigin(t, "/home/googleDrive"+gdPath)
	drive := newDirectLinkServer(t, "panel-proxied-bytes")
	panel := newFakePanel(t, drive.URL)

	// 注册表里有节点, 但从未心跳 → 不在调度窗口内
	basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+agentEntryJSON()+`]}`)
	agentCfg := mustAgentConfig(t, "enable: true\nenroll-token: test-enroll-token-0123456789\n")
	withAgentTestConfig(t, origin.server.URL, panel.server.URL, agentCfg, basePath)

	logger := captureRedirectLogs(t)
	c, recorder := newRedirectContext(t, "/emby/Videos/123/stream")
	emby.Redirect2OpenlistLink(c)

	if recorder.Header().Get("Location") != "" {
		t.Errorf("回退本机代理时不应重定向, Location: %q", recorder.Header().Get("Location"))
	}
	if got := recorder.Body.String(); got != "panel-proxied-bytes" {
		t.Errorf("客户端收到的响应体 = %q, want panel-proxied-bytes", got)
	}
	if panel.calls.Load() != 1 {
		t.Errorf("回退时面板调用次数 = %d, want 1", panel.calls.Load())
	}
	if !logger.contains("当前无可用节点, 回退本机代理") {
		t.Errorf("应留下回退日志, 实际: %s", logger.String())
	}
	origin.waitForPlaybackProbes(t)
}

// TestRedirect2OpenlistLink_AgentNoNodeAndFallbackDisabled 禁用回退时明确拒绝
func TestRedirect2OpenlistLink_AgentNoNodeAndFallbackDisabled(t *testing.T) {
	gdPath := uniqueGDPath("/影视库/agent/拒绝.mkv")
	origin := newFakeEmbyOrigin(t, "/home/googleDrive"+gdPath)
	drive := newDirectLinkServer(t, "panel-proxied-bytes")
	panel := newFakePanel(t, drive.URL)

	basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+agentEntryJSON()+`]}`)
	agentCfg := mustAgentConfig(t,
		"enable: true\nenroll-token: test-enroll-token-0123456789\nfallback-to-local: false\n")
	if agentCfg.FallbackEnabled() {
		t.Fatal("显式 fallback-to-local: false 未被识别")
	}
	withAgentTestConfig(t, origin.server.URL, panel.server.URL, agentCfg, basePath)

	logger := captureRedirectLogs(t)
	c, recorder := newRedirectContext(t, "/emby/Videos/123/stream")
	emby.Redirect2OpenlistLink(c)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("响应码 = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(recorder.Body.String(), "无可用 agent 节点") {
		t.Errorf("响应体应给出中文原因, 实际: %s", recorder.Body.String())
	}
	if panel.calls.Load() != 0 {
		t.Errorf("禁用回退时不应调面板, 调用次数 = %d", panel.calls.Load())
	}
	if origin.originHits.Load() != 0 {
		t.Errorf("禁用回退时不应回源, 回源次数 = %d", origin.originHits.Load())
	}
	if !logger.contains("无可用节点且已禁用本机回退") {
		t.Errorf("应留下拒绝原因, 实际: %s", logger.String())
	}
	origin.waitForPlaybackProbes(t)
}

// TestRedirect2OpenlistLink_AgentInternalFaultFallsBack 内部故障不改变播放结果
//
// 注册表损坏属于内部故障(不是"没有节点"): 记 WARN 后走原有流程,
// 绝不因为新功能让播放挂掉。
func TestRedirect2OpenlistLink_AgentInternalFaultFallsBack(t *testing.T) {
	gdPath := uniqueGDPath("/影视库/agent/内部故障.mkv")
	origin := newFakeEmbyOrigin(t, "/home/googleDrive"+gdPath)
	drive := newDirectLinkServer(t, "panel-proxied-bytes")
	panel := newFakePanel(t, drive.URL)

	basePath := prepareAgentStateDir(t, "{损坏的注册表")
	agentCfg := mustAgentConfig(t, "enable: true\nenroll-token: test-enroll-token-0123456789\n")
	withAgentTestConfig(t, origin.server.URL, panel.server.URL, agentCfg, basePath)

	logger := captureRedirectLogs(t)
	c, recorder := newRedirectContext(t, "/emby/Videos/123/stream")
	emby.Redirect2OpenlistLink(c)

	if got := recorder.Body.String(); got != "panel-proxied-bytes" {
		t.Errorf("内部故障时仍应正常播放(回退本机代理), 响应体 = %q", got)
	}
	if panel.calls.Load() != 1 {
		t.Errorf("内部故障时面板调用次数 = %d, want 1", panel.calls.Load())
	}
	if !logger.contains("调度失败, 回退本机代理") {
		t.Errorf("应留下回退原因, 实际: %s", logger.String())
	}
	origin.waitForPlaybackProbes(t)
}

// TestRedirect2OpenlistLink_AgentDisabledIsUnchanged 未启用时行为与改动前一致
func TestRedirect2OpenlistLink_AgentDisabledIsUnchanged(t *testing.T) {
	gdPath := uniqueGDPath("/影视库/agent/未启用.mkv")
	origin := newFakeEmbyOrigin(t, "/home/googleDrive"+gdPath)
	drive := newDirectLinkServer(t, "panel-proxied-bytes")
	panel := newFakePanel(t, drive.URL)

	// 即使注册表里有在线节点, 未启用也不得被调度
	basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+agentEntryJSON()+`]}`)
	agentCfg := mustAgentConfig(t, "enable: false\nenroll-token: test-enroll-token-0123456789\n")
	withAgentTestConfig(t, origin.server.URL, panel.server.URL, agentCfg, basePath)

	logger := captureRedirectLogs(t)
	c, recorder := newRedirectContext(t, "/emby/Videos/123/stream")
	emby.Redirect2OpenlistLink(c)

	if recorder.Header().Get("Location") != "" {
		t.Errorf("未启用时不应重定向, Location: %q", recorder.Header().Get("Location"))
	}
	if got := recorder.Body.String(); got != "panel-proxied-bytes" {
		t.Errorf("未启用时应走原有本机代理, 响应体 = %q", got)
	}
	if panel.calls.Load() != 1 {
		t.Errorf("未启用时面板调用次数 = %d, want 1", panel.calls.Load())
	}
	for _, sub := range []string{"调度到节点", "agent 网络"} {
		if logger.contains(sub) {
			t.Errorf("未启用时不应出现 %q, 实际: %s", sub, logger.String())
		}
	}
	origin.waitForPlaybackProbes(t)
}
