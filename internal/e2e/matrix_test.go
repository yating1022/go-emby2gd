package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/constant"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/agentnet"

	"github.com/gin-gonic/gin"
)

// 配置片段与 config-example.yml 的 agent-network 段同构
const (
	// defaultAgentCfgDoc 常规场景: 离线判定 45s(合法默认值)
	defaultAgentCfgDoc = `
enable: true
enroll-token: ` + e2eEnrollToken + `
offline-seconds: 45
url-ttl: 24h
`

	// offlineAgentCfgDoc 离线场景: 判定窗口压到合法下界 16s
	//
	// 下界由协议决定: offline-seconds 必须【严格大于】心跳周期 15s(见
	// config.AgentNetwork.Init), 因此 16 是本项目允许的最小值 —— E9 要真的
	// 等满这个窗口, 而不是伪造内部状态。同时关掉本机回退: E9/E11 断言的
	// 正是"没有节点就明确拒绝"。
	offlineAgentCfgDoc = `
enable: true
enroll-token: ` + e2eEnrollToken + `
offline-seconds: 16
url-ttl: 24h
fallback-to-local: false
`

	// disabledAgentCfgDoc 关闭特性(E14)
	disabledAgentCfgDoc = `
enable: false
enroll-token: ` + e2eEnrollToken + `
`
)

// TestE1_EnrollIdempotent E1: 同一 machine_id 重复注册幂等, 凭据轮换
func TestE1_EnrollIdempotent(t *testing.T) {
	h := newHarness(t, defaultAgentCfgDoc)

	first := h.enrollOverHTTP(t, "e2e-machine-1", 8790)
	second := h.enrollOverHTTP(t, "e2e-machine-1", 8790)

	firstID, secondID := strField(t, first, "agent_id"), strField(t, second, "agent_id")
	if firstID != secondID {
		t.Errorf("同一 machine_id 重复注册应复用同一条记录: first=%s, second=%s", firstID, secondID)
	}
	for _, key := range []string{"agent_secret", "sign_key"} {
		before, after := strField(t, first, key), strField(t, second, key)
		if before == after {
			t.Errorf("%s 在重复注册时必须轮换", key)
		}
	}
	if got := intField(t, second, "heartbeat_interval_seconds"); got != config.AgentHeartbeatIntervalSeconds {
		t.Errorf("心跳周期 = %d, want %d", got, config.AgentHeartbeatIntervalSeconds)
	}
	// 冻结稿的注册响应是裸对象: 重新注册这条路也不能悄悄套上 {ok,data} 信封
	for _, key := range []string{"ok", "data"} {
		if _, exists := second[key]; exists {
			t.Errorf("注册响应不应包含 %q 字段: %v", key, second)
		}
	}

	// 落盘: 仍是一条记录, 且持有的是轮换后的凭据(写盘成功之后才响应)
	doc := h.readAgentsFile()
	if doc.Version != 1 || len(doc.Agents) != 1 {
		t.Fatalf("注册表应只有 1 条记录: version=%d, agents=%d", doc.Version, len(doc.Agents))
	}
	entry := doc.Agents[0]
	if entry.ID != firstID || entry.MachineID != "e2e-machine-1" {
		t.Errorf("注册表记录与响应不一致: %+v", entry)
	}
	if entry.Secret != strField(t, second, "agent_secret") || entry.SignKey != strField(t, second, "sign_key") {
		t.Error("注册表里应落轮换后的凭据")
	}

	// 旧凭据立即失效, 新凭据可用(重新注册会清零运行时状态, 等下一次心跳回归)
	if status, _ := h.heartbeatOverHTTP(t, firstID, strField(t, first, "agent_secret")); status != http.StatusUnauthorized {
		t.Errorf("轮换后旧 agent_secret 应立即失效: HTTP %d", status)
	}
	status, heartbeat := h.heartbeatOverHTTP(t, firstID, strField(t, second, "agent_secret"))
	if status != http.StatusOK || heartbeat["enabled"] != true {
		t.Fatalf("新 agent_secret 应可用且下发 enabled: HTTP %d, body=%v", status, heartbeat)
	}

	t.Logf("E1 证据: 两次注册 agent_id 相同(%s), secret/sign_key 均已轮换, 注册表 1 条记录, 旧凭据 401", firstID)
}

// TestE2ToE6AndE12_AgentDataPlane E2-E6 + E12: 控制面与数据面的完整链路
//
// 一条真实 agent 子进程跑完: 心跳入库(E2) → 302 到签名地址(E3) →
// 客户端直连 Range(E4) → 篡改签名/时效被拒(E5/E6) →
// 数据面不经过网关, 网关停机后仍可继续拉取(E12)。
func TestE2ToE6AndE12_AgentDataPlane(t *testing.T) {
	h := newHarness(t, defaultAgentCfgDoc)
	agent, view := h.startAgent()

	// ---- E2: 心跳把节点带进调度池, 字段齐全 ----
	if !view.Online || !view.Enabled {
		t.Fatalf("节点应在心跳后处于在线且启用的状态: %+v", view)
	}
	if view.Version != e2eAgentVersion {
		t.Errorf("节点版本 = %q, want %q(由心跳上报)", view.Version, e2eAgentVersion)
	}
	if view.MachineID == "" || view.Name == "" {
		t.Errorf("machine_id / name 不应为空: %+v", view)
	}
	if view.LastIP != "127.0.0.1" {
		t.Errorf("来源 IP = %q, want 127.0.0.1", view.LastIP)
	}
	if view.ListenPort != agent.port {
		t.Errorf("监听端口 = %d, want %d", view.ListenPort, agent.port)
	}
	if want := fmt.Sprintf("http://127.0.0.1:%d", agent.port); view.Address != want {
		t.Errorf("调度地址 = %q, want %q(未配置 public_base_url 时由 IP + 端口推导)", view.Address, want)
	}
	if view.PublicBaseURL != "" {
		t.Errorf("未配置对外地址时不应凭空出现: %q", view.PublicBaseURL)
	}
	seen, err := time.Parse(time.RFC3339, view.LastSeenAt)
	if err != nil {
		t.Fatalf("last_seen_at 不是 RFC3339: %q", view.LastSeenAt)
	}
	if age := time.Since(seen); age > e2eLastSeenWindow || age < 0 {
		t.Errorf("last_seen_at 距现在 %v, 不在 %v 窗口内", age, e2eLastSeenWindow)
	}
	t.Logf("E2 证据: id=%s version=%s address=%s last_seen_at=%s", view.ID, view.Version, view.Address, view.LastSeenAt)

	// ---- E3: 有在线节点时 302 到节点签名地址 ----
	gdPath := h.uniqueGDPath("E3.mkv")
	redirected := h.playback(gdPath)
	if redirected.Code != http.StatusTemporaryRedirect {
		t.Fatalf("应 302 到节点: HTTP %d, body=%s", redirected.Code, redirected.Body.String())
	}
	location := redirected.Header().Get("Location")
	redirect := parseAgentRedirect(t, location)
	if redirect.baseURL != agent.baseURL() {
		t.Errorf("重定向基址 = %q, want %q", redirect.baseURL, agent.baseURL())
	}
	if redirect.gdPath != gdPath {
		t.Errorf("file_id 解码 = %q, want %q", redirect.gdPath, gdPath)
	}

	entry := h.agentRecordInFile(view.ID)
	wantSign := signLikeMaster(t, entry.SignKey, redirect.fileID, strconv.FormatInt(redirect.expiry, 10))
	if redirect.sign != wantSign {
		t.Errorf("签名必须能被独立重算: 实际 %s, 重算 %s", redirect.sign, wantSign)
	}
	if delta := time.Until(time.Unix(redirect.expiry, 0)) - h.agentCfg.ClientURLTTL(); delta > time.Minute || delta < -time.Minute {
		t.Errorf("签名时效偏差 %v, want %v", delta, h.agentCfg.ClientURLTTL())
	}
	// 与既有 302 分支同一惯例: 10 分钟内不重复调度
	expired, err := strconv.ParseInt(redirected.Header().Get("Expired"), 10, 64)
	if err != nil {
		t.Errorf("Expired 头不是时间戳: %q", redirected.Header().Get("Expired"))
	} else if delta := expired - time.Now().Add(10*time.Minute).UnixMilli(); delta > time.Minute.Milliseconds() || delta < -time.Minute.Milliseconds() {
		t.Errorf("Expired 头与 10 分钟后相差 %d ms", delta)
	}
	// 调度阶段只发签名, 不取直链、不搬字节
	if got := h.panel.calls.Load(); got != 0 {
		t.Errorf("调度阶段不应调面板, 调用次数 = %d", got)
	}
	if got := h.google.requests.Load(); got != 0 {
		t.Errorf("调度阶段不应有媒体请求, 次数 = %d", got)
	}
	t.Logf("E3 证据: Location=%s(签名可由 agents.json 的 sign_key 独立重算)", location)

	// ---- E4: 客户端直连节点, Range 命中 206 且字节精确 ----
	rangeHeader := "bytes=0-99"
	resp, body := doRange(t, location, rangeHeader)
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range 请求应得 206: HTTP %d, body=%s", resp.StatusCode, truncate(string(body)))
	}
	if want := fmt.Sprintf("bytes 0-99/%d", e2eMediaSize); resp.Header.Get("Content-Range") != want {
		t.Errorf("Content-Range = %q, want %q", resp.Header.Get("Content-Range"), want)
	}
	if !bytes.Equal(body, h.google.content[:100]) {
		t.Errorf("返回的字节与源文件不一致: %d 字节", len(body))
	}
	if !h.google.sawRange(rangeHeader) {
		t.Errorf("节点应把客户端的 Range 原样透传给 Google, 实际收到的 Range: %q", h.google.lastRange())
	}
	if got := h.google.badAuth.Load(); got != 0 {
		t.Errorf("凭据错误的媒体请求数 = %d, 说明直链请求头没有正确带上", got)
	}
	if got := h.gateway.countRequests("/api/agent/download-link"); got < 1 {
		t.Errorf("节点应通过控制面拉取直链, download-link 调用次数 = %d", got)
	}
	t.Logf("E4 证据: 206 + Content-Range=%q, 前 100 字节与源文件逐字节一致",
		resp.Header.Get("Content-Range"))

	// ---- E12(上): 数据面不经过网关 ----
	bigRange := fmt.Sprintf("bytes=0-%d", 256*1024-1)
	bigResp, bigBody := doRange(t, location, bigRange)
	if bigResp.StatusCode != http.StatusPartialContent || len(bigBody) != 256*1024 {
		t.Fatalf("大段 Range 应得 206 + 256 KiB: HTTP %d, %d 字节", bigResp.StatusCode, len(bigBody))
	}
	if !bytes.Equal(bigBody, h.google.content[:256*1024]) {
		t.Error("大段 Range 返回的字节与源文件不一致")
	}

	records := h.gateway.recordedRequests()
	for _, req := range records {
		controlPlane := strings.HasPrefix(req.Path, "/api/agent/") ||
			strings.HasPrefix(req.Path, constant.Route_SelfBase+"/")
		if !controlPlane {
			t.Errorf("网关收到了控制面之外的请求: %s %s", req.Method, req.Path)
		}
		if strings.HasPrefix(req.Path, "/dl/") {
			t.Errorf("数据面请求不应经过网关: %s", req.Path)
		}
	}
	dataPlaneBytes := int64(256*1024 + 100)
	if total := h.gateway.totalResponseBytes(); total >= dataPlaneBytes/4 {
		t.Errorf("数据面字节疑似流经网关: 网关共写出 %d 字节(本次拉取 %d 字节)", total, dataPlaneBytes)
	}
	t.Logf("E12 证据(上): 网关共记录 %d 个请求(全部为控制面), 写出 %d 字节; 客户端从节点直取 %d 字节",
		len(records), h.gateway.totalResponseBytes(), dataPlaneBytes)

	// ---- E5: 篡改签名 → 403 ----
	linksBefore := h.gateway.countRequests("/api/agent/download-link")
	tampered := redirect.sign
	if strings.HasSuffix(tampered, "0") {
		tampered = tampered[:len(tampered)-1] + "1"
	} else {
		tampered = tampered[:len(tampered)-1] + "0"
	}
	badResp, badBody := doRange(t, withQuery(t, location, func(v url.Values) { v.Set("s", tampered) }), "")
	if badResp.StatusCode != http.StatusForbidden {
		t.Errorf("篡改签名应被拒: HTTP %d, body=%s", badResp.StatusCode, truncate(string(badBody)))
	}
	mustContain(t, string(badBody), "链接无效或已过期")
	if got := h.gateway.countRequests("/api/agent/download-link"); got != linksBefore {
		t.Errorf("验签失败不应触发直链下发: %d -> %d", linksBefore, got)
	}

	// ---- E6: 篡改时效 → 403 ----
	badExpiry := withQuery(t, location, func(v url.Values) {
		v.Set("e", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	})
	expResp, expBody := doRange(t, badExpiry, "")
	if expResp.StatusCode != http.StatusForbidden {
		t.Errorf("篡改时效应被拒: HTTP %d, body=%s", expResp.StatusCode, truncate(string(expBody)))
	}
	mustContain(t, string(expBody), "链接无效或已过期")
	// 验签覆盖 e 的证明: 篡改 e 同样在取直链之前被拦下
	if got := h.gateway.countRequests("/api/agent/download-link"); got != linksBefore {
		t.Errorf("验签失败不应触发直链下发: %d -> %d", linksBefore, got)
	}
	t.Log("E5/E6 证据: 篡改 s / e 均得 403 + 统一文案, 且均不再触发直链下发")

	// ---- E12(下): 网关停机, 节点凭已缓存直链继续服务 ----
	//
	// 停机后先跨过一个完整心跳周期(15s)再续传: 过期确认的是"续命不来自 master"
	// (期间至少有一次心跳失败), 而不只是"下一个请求恰好还没超时"。
	// 验收里人工版的 ≥5min 长拉流仍留在用户环境验收清单中执行。
	h.gateway.stop()
	time.Sleep(config.AgentHeartbeatIntervalSeconds*time.Second + 5*time.Second)
	linksBefore = h.gateway.countRequests("/api/agent/download-link")
	offResp, offBody := doRange(t, location, "bytes=1000-1099")
	if offResp.StatusCode != http.StatusPartialContent {
		t.Fatalf("网关停机后节点仍应可服务: HTTP %d, body=%s", offResp.StatusCode, truncate(string(offBody)))
	}
	if !bytes.Equal(offBody, h.google.content[1000:1100]) {
		t.Error("网关停机后返回的字节与源文件不一致")
	}
	if got := h.gateway.countRequests("/api/agent/download-link"); got != linksBefore {
		t.Errorf("网关停机期间不应有新的直链下发: %d -> %d", linksBefore, got)
	}
	t.Logf("E12 证据(下): 网关停机 %v 后 Range 1000-1099 仍得 206, 且未产生新的控制面请求",
		config.AgentHeartbeatIntervalSeconds*time.Second+5*time.Second)
}

// TestE7E8_AdminDisableEnable E7/E8: 网页停用节点回退本机代理, 重新启用恢复 302
func TestE7E8_AdminDisableEnable(t *testing.T) {
	h := newHarness(t, defaultAgentCfgDoc)
	agent, view := h.startAgent()

	// 基线: 在线节点会被调度
	baseline := h.playback(h.uniqueGDPath("E7-基线.mkv"))
	if baseline.Code != http.StatusTemporaryRedirect {
		t.Fatalf("基线应 302 到节点: HTTP %d", baseline.Code)
	}

	// ---- E7: 停用 → 回退本机代理 ----
	h.setAgentEnabled(view.ID, false)
	if entry := h.agentRecordInFile(view.ID); entry.Enabled {
		t.Error("停用状态应落盘(重启后仍然生效)")
	}
	if got := h.listAgents()[0].Enabled; got {
		t.Error("管理接口应立刻反映停用状态")
	}

	panelBefore := h.panel.calls.Load()
	disabled := h.playback(h.uniqueGDPath("E7-停用.mkv"))
	if location := disabled.Header().Get("Location"); location != "" {
		t.Errorf("节点被停用后不应 302: %q", location)
	}
	if disabled.Code != http.StatusOK {
		t.Errorf("停用后应回退本机代理: HTTP %d, body=%s", disabled.Code, truncate(disabled.Body.String()))
	}
	if !bytes.Equal(disabled.Body.Bytes(), h.google.content) {
		t.Errorf("本机代理应把完整媒体字节交给客户端: %d 字节, want %d",
			disabled.Body.Len(), len(h.google.content))
	}
	if got := h.panel.calls.Load(); got != panelBefore+1 {
		t.Errorf("回退本机代理应走面板取直链: 调用次数 %d -> %d", panelBefore, got)
	}
	if got := h.origin.originHits.Load(); got != 0 {
		t.Errorf("面板可用时不应回源: %d 次", got)
	}
	t.Log("E7 证据: 停用后无 Location、200 + 完整 1 MiB 媒体字节, 面板调用 +1")

	// ---- E8: 重新启用 → 恢复 302 ----
	h.setAgentEnabled(view.ID, true)
	if entry := h.agentRecordInFile(view.ID); !entry.Enabled {
		t.Error("重新启用应落盘")
	}
	reenabled := h.playback(h.uniqueGDPath("E8-启用.mkv"))
	if reenabled.Code != http.StatusTemporaryRedirect {
		t.Fatalf("重新启用后应恢复 302: HTTP %d, body=%s", reenabled.Code, truncate(reenabled.Body.String()))
	}
	if got := parseAgentRedirect(t, reenabled.Header().Get("Location")).baseURL; got != agent.baseURL() {
		t.Errorf("重定向基址 = %q, want %q", got, agent.baseURL())
	}
	t.Log("E8 证据: 重新启用后立刻恢复 302 到节点")
}

// TestE9E10_OfflineFallbackDisabledAndAgentRestart E9/E10: 真实等满离线窗口与节点重启回归
//
// E9 真的要等满 offline-seconds(16s, 该项目允许的最小合法值)才做断言:
// 伪造内部状态验不出"心跳过期判定"这条契约本身。
func TestE9E10_OfflineFallbackDisabledAndAgentRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过: 本用例包含真实超过 offline-seconds(16s) 的离线等待")
	}

	h := newHarness(t, offlineAgentCfgDoc)
	agent, view := h.startAgent()

	// ---- E9: 停掉 agent, 等真实超过 offline-seconds(回退关闭) → 503 ----
	timer := startTimer()
	agent.stop()
	offline := h.waitAgentOffline(view.ID, 60*time.Second)
	t.Logf("E9 证据: offline-seconds=16, 节点在 %v 后判为离线(last_seen_at=%s)",
		timer().Round(time.Second), offline.LastSeenAt)

	panelBefore := h.panel.calls.Load()
	refused := h.playback(h.uniqueGDPath("E9-离线.mkv"))
	if refused.Code != http.StatusServiceUnavailable {
		t.Fatalf("无节点 + 回退关闭应 503: HTTP %d, body=%s", refused.Code, truncate(refused.Body.String()))
	}
	mustContain(t, refused.Body.String(), "无可用 agent 节点")
	if location := refused.Header().Get("Location"); location != "" {
		t.Errorf("拒绝时不应重定向: %q", location)
	}
	if got := h.panel.calls.Load(); got != panelBefore {
		t.Errorf("禁用回退时不得改走本机代理(不应调面板): %d -> %d", panelBefore, got)
	}
	if got := h.origin.originHits.Load(); got != 0 {
		t.Errorf("禁用回退时不得回源: %d 次", got)
	}

	// ---- E10: 重启 agent(复用同一份配置) → 下一次心跳自动回归调度池 ----
	regressTimer := startTimer()
	agent.startServe()
	back := h.waitAgentOnline(60 * time.Second)
	t.Logf("E10 证据: agent 重启后 %v 回归可调度(启动即发首次心跳)", regressTimer().Round(time.Millisecond))
	if back.ID != view.ID {
		t.Errorf("重启后节点 id = %q, want %q(复用原记录, 不新增注册)", back.ID, view.ID)
	}
	if doc := h.readAgentsFile(); len(doc.Agents) != 1 {
		t.Errorf("重启不应新增注册记录: %d 条", len(doc.Agents))
	}

	gdPath := h.uniqueGDPath("E10-回归.mkv")
	recovered := h.playback(gdPath)
	if recovered.Code != http.StatusTemporaryRedirect {
		t.Fatalf("节点回归后应恢复 302: HTTP %d, body=%s", recovered.Code, truncate(recovered.Body.String()))
	}
	location := recovered.Header().Get("Location")
	if got := parseAgentRedirect(t, location).baseURL; got != agent.baseURL() {
		t.Errorf("重定向基址 = %q, want %q", got, agent.baseURL())
	}
	resp, body := doRange(t, location, "bytes=0-9")
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, h.google.content[:10]) {
		t.Errorf("回归后的节点应能直接服务媒体: HTTP %d, %d 字节", resp.StatusCode, len(body))
	}
	t.Log("E10 证据: agent 重启后无需重新注册, 心跳即回归调度池并可直接服务媒体")
}

// TestE11_FallbackDisabledWithoutNode E11: 回退关闭且从未注册过任何节点 → 503
func TestE11_FallbackDisabledWithoutNode(t *testing.T) {
	h := newHarness(t, offlineAgentCfgDoc)

	if _, err := os.Stat(h.agentsFilePath()); !os.IsNotExist(err) {
		t.Errorf("本场景不应存在注册表文件: err=%v", err)
	}

	refused := h.playback(h.uniqueGDPath("E11-无节点.mkv"))
	if refused.Code != http.StatusServiceUnavailable {
		t.Fatalf("回退关闭 + 没有节点应 503: HTTP %d, body=%s", refused.Code, truncate(refused.Body.String()))
	}
	mustContain(t, refused.Body.String(), "无可用 agent 节点")
	if location := refused.Header().Get("Location"); location != "" {
		t.Errorf("拒绝时不应重定向: %q", location)
	}
	if got := h.panel.calls.Load(); got != 0 {
		t.Errorf("禁用回退时不得调面板: %d 次", got)
	}
	if got := h.origin.originHits.Load(); got != 0 {
		t.Errorf("禁用回退时不得回源: %d 次", got)
	}
	t.Log("E11 证据: 从未注册节点的实例上, 回退关闭时播放直接 503, 不回源也不取直链")
}

// TestE13_GatewayRestartKeepsNodes E13: 网关重启不丢节点, 心跳后自动回归
func TestE13_GatewayRestartKeepsNodes(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过: 本用例包含等待节点心跳回归(最长 90s)")
	}

	h := newHarness(t, defaultAgentCfgDoc)
	_, view := h.startAgent()
	stateBefore := h.readAgentsFileRaw()

	// ---- 重启: 停机(含等所有在途请求退出) → 冷加载生产格式的注册表 →
	//      同地址重新开始服务 ----
	h.gateway.stop()

	restartBase := t.TempDir()
	h.basePath = restartBase
	config.BasePath = restartBase
	stateDir := filepath.Join(restartBase, agentnet.DirName)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("创建状态目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "agents.json"), stateBefore, 0o600); err != nil {
		t.Fatalf("搬迁注册表失败: %v", err)
	}

	// 先配置后注册表, 与生产启动顺序一致
	if err := config.C.AgentNetwork.Init(); err != nil {
		t.Fatalf("重新初始化 agent 网络配置失败: %v", err)
	}
	if err := agentnet.Init(); err != nil {
		t.Fatalf("冷加载注册表失败: %v", err)
	}

	// 冷加载的第一印象: 记录还在, 但运行时字段归零 → 等下一次心跳才回归
	cold := h.listAgentsDirect(t)
	if len(cold) != 1 || cold[0].ID != view.ID {
		t.Fatalf("重启后注册表应原样恢复: %+v", cold)
	}
	if cold[0].Online {
		t.Error("重启后节点不应假装在线(运行时状态不持久化)")
	}
	coldPlay := h.playback(h.uniqueGDPath("E13-冷启动.mkv"))
	if location := coldPlay.Header().Get("Location"); location != "" {
		t.Errorf("未心跳的节点不应被调度: %q", location)
	}
	if coldPlay.Body.Len() != e2eMediaSize {
		t.Errorf("未心跳时应回退本机代理(完整媒体): %d 字节", coldPlay.Body.Len())
	}
	t.Logf("E13 证据: 冷加载恢复 %d 条记录(id=%s), 未心跳前不调度", len(cold), cold[0].ID)

	h.gateway.restart(t)
	regressTimer := startTimer()
	back := h.waitAgentOnline(90 * time.Second)
	t.Logf("E13 证据: 网关重新监听后 %v 收到下一次心跳并回归(心跳周期 15s)",
		regressTimer().Round(time.Millisecond))
	if back.ID != view.ID {
		t.Errorf("重启后回归的节点 id = %q, want %q", back.ID, view.ID)
	}
	if now := h.readAgentsFileRaw(); !bytes.Equal(stateBefore, now) {
		t.Error("心跳不应改写注册表(运行时状态只存在内存里)")
	}

	gdPath := h.uniqueGDPath("E13-回归.mkv")
	recovered := h.playback(gdPath)
	if recovered.Code != http.StatusTemporaryRedirect {
		t.Fatalf("节点回归后应恢复 302: HTTP %d, body=%s", recovered.Code, truncate(recovered.Body.String()))
	}
	location := recovered.Header().Get("Location")
	resp, body := doRange(t, location, "bytes=0-9")
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, h.google.content[:10]) {
		t.Errorf("回归后的节点应能直接服务媒体: HTTP %d, %d 字节", resp.StatusCode, len(body))
	}
	t.Log("E13 证据: 网关重启后注册表原样恢复, 下一次心跳即回归, 注册表文件字节未变")
}

// TestE14_FeatureDisabledIsByteIdentical E14: 关闭特性后的行为与未部署一致
func TestE14_FeatureDisabledIsByteIdentical(t *testing.T) {
	h := newHarness(t, defaultAgentCfgDoc)
	agent, view := h.startAgent()

	// 前提: 功能开启且节点在线时会 302 —— 下面的"没变化"才不是假象
	on := h.playback(h.uniqueGDPath("E14-开启.mkv"))
	if on.Code != http.StatusTemporaryRedirect {
		t.Fatalf("功能开启且有在线节点时应 302: HTTP %d", on.Code)
	}

	// 关闭特性 = 改配置重启: 先停 agent, 让配置改写发生在没有并发读者的静默点
	agent.stop()
	if !h.listAgents()[0].Online {
		t.Fatal("节点应仍在新鲜窗口内(45s), 否则本用例失去区分度")
	}
	h.swapAgentConfig(t, mustAgentConfig(t, disabledAgentCfgDoc))

	gdPath := h.uniqueGDPath("E14-关闭.mkv")
	offDisabled := h.playback(gdPath)

	// 未配置该段(nil)的语义等价形态
	h.swapAgentConfig(t, nil)
	offAbsent := h.playback(gdPath)

	for name, recorder := range map[string]*httptest.ResponseRecorder{"enable: false": offDisabled, "未配置该段": offAbsent} {
		if recorder.Code != http.StatusOK {
			t.Errorf("%s: 应走原有本机代理: HTTP %d, body=%s", name, recorder.Code, truncate(recorder.Body.String()))
		}
		if location := recorder.Header().Get("Location"); location != "" {
			t.Errorf("%s: 不应重定向, Location=%q", name, location)
		}
		if !bytes.Equal(recorder.Body.Bytes(), h.google.content) {
			t.Errorf("%s: 应代理完整媒体字节: %d 字节, want %d", name, recorder.Body.Len(), len(h.google.content))
		}
	}

	if diff := compareRecorder(offDisabled, offAbsent); diff != "" {
		t.Errorf("两种关闭形态的响应不一致: %s", diff)
	}
	if got := h.panel.calls.Load(); got < 1 {
		t.Errorf("关闭特性后仍应正常取直链: 面板调用次数 = %d", got)
	}
	if got := h.gateway.countRequests("/api/agent/download-link"); got != 0 {
		t.Errorf("关闭特性后不应再向节点下发直链: %d 次", got)
	}
	t.Logf("E14 证据: 节点在线(id=%s, offline-seconds=45 内)时关闭特性, 两种形态均 200 + 完整 1 MiB 且逐字节一致",
		view.ID)
}

// ---------------------------------------------------------------------------
// 基础设施(与用例强相关的小工具放在这里, 便于对照)
// ---------------------------------------------------------------------------

// enrollOverHTTP 走真实 HTTP 调一次注册接口
func (h *harness) enrollOverHTTP(t *testing.T, machineID string, port int) map[string]any {
	t.Helper()

	body := fmt.Sprintf(`{"enroll_token":%q,"machine_id":%q,"hostname":"e2e-host-1","version":%q,"listen_port":%d}`,
		e2eEnrollToken, machineID, e2eAgentVersion, port)
	resp, err := http.Post(h.gateway.masterURL()+"/api/agent/enroll", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("注册请求失败: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取注册响应失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("注册失败: HTTP %d, body=%s", resp.StatusCode, raw)
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("注册响应不是合法 JSON: %v, body=%s", err, raw)
	}
	return parsed
}

// heartbeatOverHTTP 走真实 HTTP 调一次心跳接口
func (h *harness) heartbeatOverHTTP(t *testing.T, id, secret string) (int, map[string]any) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, h.gateway.masterURL()+"/api/agent/heartbeat",
		strings.NewReader(`{"active_streams":0}`))
	if err != nil {
		t.Fatalf("构造心跳请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Id", id)
	req.Header.Set("Authorization", "Bearer "+secret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("心跳请求失败: %v", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	return resp.StatusCode, parsed
}

// listAgentsDirect 不走网络直接调用管理接口处理器
//
// E13 在网关停机期间要观察注册表: 此时没有服务器可请求, 但管理接口本身的
// 生产实现(鉴权 + 脱敏 + 在线判定)仍然应该被复用, 而不是绕开它去读内部状态。
func (h *harness) listAgentsDirect(t *testing.T) []agentView {
	t.Helper()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, constant.Route_AgentNetworkAgents,
		strings.NewReader(`{"secret":"`+e2eAdminSecret+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	agentnet.AdminListAgents(c)

	var envelope adminEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("管理接口响应不是合法 JSON: %v, body=%s", err, recorder.Body.String())
	}
	if !envelope.Success || envelope.Data == nil {
		t.Fatalf("管理接口返回失败: %s", recorder.Body.String())
	}
	return envelope.Data.Agents
}

// strField 取响应里的字符串字段
func strField(t *testing.T, m map[string]any, key string) string {
	t.Helper()

	value, _ := m[key].(string)
	if value == "" {
		t.Fatalf("字段 %s 为空: %v", key, m)
	}
	return value
}

// intField 取响应里的整数字段(JSON 数字解析为 float64)
func intField(t *testing.T, m map[string]any, key string) int {
	t.Helper()

	value, ok := m[key].(float64)
	if !ok {
		t.Fatalf("字段 %s 不是数字: %v", key, m)
	}
	return int(value)
}

// compareRecorder 比对两次响应(状态码 + 关键头 + 字节体)
func compareRecorder(a, b *httptest.ResponseRecorder) string {
	if a.Code != b.Code {
		return fmt.Sprintf("状态码 %d != %d", a.Code, b.Code)
	}
	for _, header := range []string{"Location", "Content-Type", "Content-Length"} {
		if got, want := a.Header().Get(header), b.Header().Get(header); got != want {
			return fmt.Sprintf("响应头 %s: %q != %q", header, got, want)
		}
	}
	if !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) {
		return fmt.Sprintf("响应体不一致: %d 字节 != %d 字节", a.Body.Len(), b.Body.Len())
	}
	return ""
}

// truncate 截断过长的响应体, 避免失败信息刷屏
func truncate(content string) string {
	if len(content) <= 200 {
		return content
	}
	return content[:200] + "...(截断)"
}
