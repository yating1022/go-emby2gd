package agentnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs"

	"gopkg.in/yaml.v3"
)

// hubPathSeq / hubFileIDSeq 生成用例内唯一 Drive 路径与文件 id 的自增序号
//
// gdrive 的直链缓存是进程级全局的、按路径为键且条目不会主动过期: 同一条路径在
// 多次执行之间(如 go test -count=3)会命中上一轮缓存的直链; 而 warm 接受标记
// 以 Drive 文件 id 为键, 同一次进程里的重复执行也会互相污染。两者都换新值,
// 每个用例、每次执行就只可能命中自己本轮制造的状态。
var (
	hubPathSeq   atomic.Uint64
	hubFileIDSeq atomic.Uint64
)

// uniqueHubGDPath 生成唯一的 Drive 逻辑路径
func uniqueHubGDPath(name string) string {
	return fmt.Sprintf("/影视库/hub/%s-%d.mkv", name, hubPathSeq.Add(1))
}

// uniqueDriveFileID 生成唯一的 Drive 文件 id
func uniqueDriveFileID() string {
	return fmt.Sprintf("1HubFile-%d", hubFileIDSeq.Add(1))
}

// driveDirectLink 拼一条面板风格的 Google 直链
func driveDirectLink(fileID string) string {
	return "https://www.googleapis.com/drive/v3/files/" + fileID + "?alt=media&supportsAllDrives=true"
}

// resetHubWarmStore 清空 hub 预热的小状态(接受标记 + 失败冷却)
//
// 该状态是进程级全局的: 用例之间必须互相隔离, 否则 -count>1 或跨用例执行时
// "warm 已被接受"的残留标记会让断言看到 0 次请求。
func resetHubWarmStore() {
	hubWarmStore.mu.Lock()
	defer hubWarmStore.mu.Unlock()
	hubWarmStore.accepted = map[string]hubWarmAcceptEntry{}
	hubWarmStore.failedAt = map[string]time.Time{}
}

// setupHubFullConfig 注入一份启用 hub 接入的完整配置
//
// 走 yaml + Init 而不是直接构造结构体: hubPort / hubWarmTimeout 是未导出字段
// (只有配置文件才允许写), 与真实配置解析路径保持一致。
// hubPort 为 0 表示用默认值; warmTimeout 为空表示用默认值。
func setupHubFullConfig(t *testing.T, hubPort int, hubEnabled bool, apiBase, warmTimeout string) *config.AgentNetwork {
	t.Helper()

	t.Setenv(config.AgentEnrollTokenEnvName, "")
	t.Setenv(config.GDriveApiTokenEnvName, "")

	doc := fmt.Sprintf("enable: true\nenroll-token: %s\noffline-seconds: 45\nurl-ttl: 1h\nhub-enable: %t\n",
		testEnrollToken, hubEnabled)
	if hubPort > 0 {
		doc += fmt.Sprintf("hub-port: %d\n", hubPort)
	}
	if warmTimeout != "" {
		doc += "hub-warm-timeout: " + warmTimeout + "\n"
	}

	agent := new(config.AgentNetwork)
	if err := yaml.Unmarshal([]byte(doc), agent); err != nil {
		t.Fatalf("解析 agent 网络测试配置失败: %v", err)
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

	resetHubWarmStore()
	t.Cleanup(resetHubWarmStore)

	return agent
}

// seedHub 往注册表里塞一条 hub 记录
//
// lastSeenAgo 为心跳距当前时刻的间隔(0 = 刚刚心跳过); lastIP 为空表示地址不可推导。
func seedHub(t *testing.T, id, lastIP string, priority int, lastSeenAgo time.Duration) *agentRecord {
	t.Helper()

	rec := &agentRecord{
		ID:         id,
		MachineID:  "machine-" + id,
		Name:       id,
		Role:       RoleHub,
		LastIP:     lastIP,
		ListenPort: 8791,
		Enabled:    true,
		Priority:   priority,
	}
	if lastSeenAgo >= 0 {
		rec.LastSeenAt = time.Now().Add(-lastSeenAgo)
	}
	return seedRecord(t, rec)
}

// fakeHubCall 一次 /warm 请求的原始记录
type fakeHubCall struct {
	path        string
	contentType string
	body        []byte
}

// fakeHub 假 hub(只实现控制面 /warm)
type fakeHub struct {
	// server 假服务端
	server *httptest.Server
	// port 监听端口: 注册表记录的 last_ip 配上 master 配置的 hub-port 即 hub 内网基址
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
	// mu 保护 requests
	mu sync.Mutex
	// requests 收到的请求
	requests []fakeHubCall
}

// newFakeHub 启动假 hub
func newFakeHub(t *testing.T, status int) *fakeHub {
	t.Helper()

	f := &fakeHub{release: make(chan struct{})}
	f.status.Store(int32(status))
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10))

		f.mu.Lock()
		f.requests = append(f.requests, fakeHubCall{path: r.URL.Path, contentType: r.Header.Get("Content-Type"), body: body})
		f.mu.Unlock()

		if f.hang.Load() {
			<-f.release
		}
		w.Header().Set("Content-Type", "application/json")
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
func (f *fakeHub) unblock() {
	f.releaseOnce.Do(func() { close(f.release) })
}

// lastPayload 取回最近一次 /warm 请求的原始报文信息
//
// 刻意解码成 map 而不是生产结构体: 字段名(JSON tag)是冻结的协议契约,
// 用同一份结构体解码两边会一起错。
func (f *fakeHub) lastPayload(t *testing.T) (fakeHubCall, map[string]any) {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("假 hub 没有收到任何请求")
	}
	last := f.requests[len(f.requests)-1]

	var payload map[string]any
	if err := json.Unmarshal(last.body, &payload); err != nil {
		t.Fatalf("warm 请求体不是合法 JSON: %v, body=%s", err, last.body)
	}
	return last, payload
}

// countingPanel 假 GD 面板 + 请求计数
//
// 用于断言"没有 hub 时浏览链路零额外网络开销"这类"一次请求都没发生"的契约,
// newFakePanel 不带计数, 这里单独实现一份最小形状。
func newCountingPanel(t *testing.T, directURL string) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	calls := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		payload, err := json.Marshal(map[string]any{
			"ok": true,
			"data": map[string]any{
				"url":        directURL,
				"headers":    map[string]string{"Authorization": testGoogleHeader},
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

	return server, calls
}

// ============================ 选点(hubFor / hubCandidates) ============================

func TestHubFor_NoHealthyHub(t *testing.T) {
	setupStateDir(t)
	setupHubFullConfig(t, 0, true, "", "")
	simulateRestart()

	// 只有普通节点: hub 选点必须返回 (nil, nil), 不报错
	seedRecord(t, &agentRecord{
		ID: "node-1", MachineID: "m-node", Name: "node-1", Role: RoleNode,
		LastIP: "10.0.0.1", ListenPort: 8790, Enabled: true, LastSeenAt: time.Now(),
	})

	hub, err := defaultRegistry.hubFor("file-x", time.Now(), 45*time.Second, 8791)
	if err != nil {
		t.Fatalf("没有 hub 不应报错: %v", err)
	}
	if hub != nil {
		t.Fatalf("只有节点时不应选出 hub, 实际: %+v", hub)
	}
}

func TestHubFor_SingleHubIsIdentity(t *testing.T) {
	setupStateDir(t)
	setupHubFullConfig(t, 0, true, "", "")
	simulateRestart()

	seedHub(t, "hub-1", "10.0.0.9", 0, 0)

	for i := 0; i < 5; i++ {
		hub, err := defaultRegistry.hubFor(fmt.Sprintf("file-%d", i), time.Now(), 45*time.Second, 8791)
		if err != nil {
			t.Fatalf("选点失败: %v", err)
		}
		if hub == nil || hub.ID != "hub-1" {
			t.Fatalf("单 hub 必须恒等选中它, 实际: %+v", hub)
		}
	}
}

func TestHubFor_DeterministicAndDistributed(t *testing.T) {
	setupStateDir(t)
	setupHubFullConfig(t, 0, true, "", "")
	simulateRestart()

	seedHub(t, "hub-a", "10.0.0.1", 0, 0)
	seedHub(t, "hub-b", "10.0.0.2", 0, 0)

	candidates, err := defaultRegistry.hubCandidates(time.Now(), 45*time.Second, 8791)
	if err != nil {
		t.Fatalf("读取候选集失败: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("候选 hub 数 = %d, want 2", len(candidates))
	}

	seen := map[string]int{}
	for i := 0; i < 64; i++ {
		fileID := fmt.Sprintf("1Deterministic-%d", i)

		first, err := defaultRegistry.hubFor(fileID, time.Now(), 45*time.Second, 8791)
		if err != nil {
			t.Fatalf("选点失败: %v", err)
		}
		// 同输入必须同输出(连选 10 次)
		for j := 0; j < 10; j++ {
			again, err := defaultRegistry.hubFor(fileID, time.Now(), 45*time.Second, 8791)
			if err != nil {
				t.Fatalf("选点失败: %v", err)
			}
			if again == nil || first == nil || again.ID != first.ID {
				t.Fatalf("同一 file_id 的选点结果必须确定: %v != %v", first, again)
			}
		}

		// 独立重算: fnv64a(fileID) % 台数(候选集有序, 下标即可复算)
		digest := fnv.New64a()
		_, _ = digest.Write([]byte(fileID))
		want := candidates[int(digest.Sum64()%uint64(len(candidates)))].ID
		if first.ID != want {
			t.Fatalf("选点结果 = %s, 独立重算 = %s", first.ID, want)
		}
		seen[first.ID]++
	}

	// 分布: 两台 hub 都应被分到(防止"永远选第一台"的实现错误)
	if seen["hub-a"] == 0 || seen["hub-b"] == 0 {
		t.Fatalf("64 个 file_id 的分布不合理: %v", seen)
	}
}

func TestHubCandidates_HealthFiltering(t *testing.T) {
	setupStateDir(t)
	setupHubFullConfig(t, 0, true, "", "")
	simulateRestart()

	seedHub(t, "hub-ok", "10.0.0.1", 0, 0)
	seedHub(t, "hub-stale", "10.0.0.2", 0, 46*time.Second)
	seedHub(t, "hub-noip", "", 0, 0)

	disabled := seedHub(t, "hub-disabled", "10.0.0.3", 0, 0)
	disabled.Enabled = false

	// 心跳缺失(从未心跳)的 hub
	seedRecord(t, &agentRecord{
		ID: "hub-never", MachineID: "machine-hub-never", Name: "hub-never",
		Role: RoleHub, LastIP: "10.0.0.4", ListenPort: 8791, Enabled: true,
	})

	// 角色是 node 的健康记录: 与 hub 候选无关
	seedRecord(t, &agentRecord{
		ID: "node-1", MachineID: "machine-node", Name: "node-1", Role: RoleNode,
		LastIP: "10.0.0.5", ListenPort: 8790, Enabled: true, LastSeenAt: time.Now(),
	})

	candidates, err := defaultRegistry.hubCandidates(time.Now(), 45*time.Second, 8791)
	if err != nil {
		t.Fatalf("读取候选集失败: %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != "hub-ok" {
		t.Fatalf("应只剩一台健康 hub, 实际: %+v", candidates)
	}
}

func TestHubCandidates_OrderedByPriorityThenID(t *testing.T) {
	setupStateDir(t)
	setupHubFullConfig(t, 0, true, "", "")
	simulateRestart()

	seedHub(t, "hub-p5", "10.0.0.1", 5, 0)
	seedHub(t, "hub-p1-b", "10.0.0.2", 1, 0)
	seedHub(t, "hub-p1-a", "10.0.0.3", 1, 0)

	candidates, err := defaultRegistry.hubCandidates(time.Now(), 45*time.Second, 8791)
	if err != nil {
		t.Fatalf("读取候选集失败: %v", err)
	}
	order := make([]string, 0, len(candidates))
	for _, rec := range candidates {
		order = append(order, rec.ID)
	}
	want := []string{"hub-p1-a", "hub-p1-b", "hub-p5"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("候选集顺序 = %v, want %v (priority 升序, id 升序)", order, want)
	}
}

func TestHubFor_SelectionSurvivesRestartWithHeartbeats(t *testing.T) {
	setupStateDir(t)
	setupHubFullConfig(t, 0, true, "", "")
	simulateRestart()

	// 走真实注册入口: role 必须落盘, 重启后仍然认出这两台是 hub
	first, err := defaultRegistry.enroll(enrollParams{
		MachineID: "m-hub-1", Hostname: "hub-1", Version: "v0.4.0",
		ListenPort: 8791, Role: RoleHub, LastIP: "10.0.0.1", Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("注册 hub 失败: %v", err)
	}
	second, err := defaultRegistry.enroll(enrollParams{
		MachineID: "m-hub-2", Hostname: "hub-2", Version: "v0.4.0",
		ListenPort: 8791, Role: RoleHub, LastIP: "10.0.0.2", Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("注册 hub 失败: %v", err)
	}

	// 注册本身不进入可调度状态(心跳时间不落盘), 需要各自心跳一次
	fileID := uniqueDriveFileID()
	for _, id := range []string{first.AgentID, second.AgentID} {
		if _, err := defaultRegistry.touch(id, heartbeatParams{LastIP: "10.0.0.1", Now: time.Now()}); err != nil {
			t.Fatalf("心跳失败: %v", err)
		}
	}
	before, err := defaultRegistry.hubFor(fileID, time.Now(), 45*time.Second, 8791)
	if err != nil {
		t.Fatalf("选点失败: %v", err)
	}

	// 模拟重启: 内存清空后从磁盘重新加载(心跳时间不落盘, 需要重新心跳)
	simulateRestart()
	for _, id := range []string{first.AgentID, second.AgentID} {
		if _, err := defaultRegistry.touch(id, heartbeatParams{LastIP: "10.0.0.1", Now: time.Now()}); err != nil {
			t.Fatalf("重新心跳失败: %v", err)
		}
	}
	after, err := defaultRegistry.hubFor(fileID, time.Now(), 45*time.Second, 8791)
	if err != nil {
		t.Fatalf("重启后选点失败: %v", err)
	}
	if before == nil || after == nil || before.ID != after.ID {
		t.Fatalf("重启前后同一 file_id 必须落到同一台 hub: %v -> %v", before, after)
	}
	if normalizeRole(after.Role) != RoleHub {
		t.Fatalf("重启后 hub 角色丢失: %+v", after)
	}
}

func TestPickHub_NilWhenDisabledOrNoCandidate(t *testing.T) {
	setupStateDir(t)
	setupHubFullConfig(t, 0, false, "", "")
	simulateRestart()
	seedHub(t, "hub-1", "10.0.0.1", 0, 0)

	if _, err := PickHub("1X"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("hub 接入关闭时 PickHub 应返回 ErrDisabled, 实际: %v", err)
	}

	// 打开开关但没有健康 hub: (nil, nil), 由调用方静默回退
	setupHubFullConfig(t, 0, true, "", "")
	simulateRestart()
	hub, err := PickHub("1X")
	if err != nil || hub != nil {
		t.Fatalf("没有健康 hub 时应返回 (nil, nil), 实际: hub=%+v, err=%v", hub, err)
	}
}

// ============================ 直链 → Drive 文件 id ============================

func TestDriveFileIDFromDirectLink(t *testing.T) {
	cases := []struct {
		name string
		link string
		want string
	}{
		{
			"googleapis files 形态",
			"https://www.googleapis.com/drive/v3/files/1AbCdEf?alt=media&supportsAllDrives=true",
			"1AbCdEf",
		},
		{
			"files 后仍有路径段",
			"https://www.googleapis.com/drive/v3/files/1AbCdEf/extra?alt=media",
			"1AbCdEf",
		},
		{
			"uc 下载形态",
			"https://drive.google.com/uc?export=download&id=1UcDownload",
			"1UcDownload",
		},
		{"普通外链解析不出", "https://cdn.example.com/video.mp4", ""},
		{"空串", "", ""},
		{"非法地址", "://bad url", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := driveFileIDFromDirectLink(tc.link); got != tc.want {
				t.Errorf("driveFileIDFromDirectLink(%q) = %q, want %q", tc.link, got, tc.want)
			}
		})
	}
}

func TestHubFileURL(t *testing.T) {
	hub := &HubInfo{ID: "hub-1", Name: "hub-1", BaseURL: "http://10.0.0.9:8791"}
	if got, want := hubFileURL(hub, "1AbC"), "http://10.0.0.9:8791/f/1AbC"; got != want {
		t.Errorf("hubFileURL = %q, want %q", got, want)
	}
	if got := hubFileURL(hub, ""); got != "" {
		t.Errorf("空文件 id 不应拼出地址, 实际: %q", got)
	}
	// 文件 id 里出现需要转义的字符时不得破坏地址结构
	if got, want := hubFileURL(hub, "a/b"), "http://10.0.0.9:8791/f/a%2Fb"; got != want {
		t.Errorf("hubFileURL 转义 = %q, want %q", got, want)
	}
}

// ============================ 浏览预热: WarmFile ============================

func TestWarmFile_DisabledDoesNothing(t *testing.T) {
	counted, panelCalls := newCountingPanel(t, driveDirectLink(uniqueDriveFileID()))
	setupStateDir(t)
	setupHubFullConfig(t, 0, false, counted.URL, "")
	simulateRestart()
	seedHub(t, "hub-1", "127.0.0.1", 0, 0)

	accepted, err := WarmFile(context.Background(), uniqueHubGDPath("关闭"))
	if accepted || err != nil {
		t.Fatalf("hub 接入关闭时应静默跳过, 实际: accepted=%v, err=%v", accepted, err)
	}
	if got := panelCalls.Load(); got != 0 {
		t.Errorf("hub 接入关闭时不应请求面板, 实际 %d 次", got)
	}
}

func TestWarmFile_NoHealthyHubDoesNotTouchPanel(t *testing.T) {
	counted, panelCalls := newCountingPanel(t, driveDirectLink(uniqueDriveFileID()))
	setupStateDir(t)
	setupHubFullConfig(t, 8791, true, counted.URL, "")
	simulateRestart()

	accepted, err := WarmFile(context.Background(), uniqueHubGDPath("无hub"))
	if accepted || err != nil {
		t.Fatalf("没有健康 hub 时应静默回退, 实际: accepted=%v, err=%v", accepted, err)
	}
	if got := panelCalls.Load(); got != 0 {
		t.Errorf("没有健康 hub 时不应换取直链, 实际请求面板 %d 次", got)
	}
}

func TestWarmFile_AcceptedCarriesFrozenPayload(t *testing.T) {
	hub := newFakeHub(t, http.StatusOK)
	fileID := uniqueDriveFileID()
	gdPath := uniqueHubGDPath("payload")
	panel := newFakePanel(t, http.StatusOK, driveDirectLink(fileID), "", "", "")
	setupStateDir(t)
	setupHubFullConfig(t, hub.port, true, panel.URL, "")
	simulateRestart()
	seedHub(t, "hub-1", "127.0.0.1", 0, 0)

	accepted, err := WarmFile(context.Background(), gdPath)
	if err != nil {
		t.Fatalf("下发预热指令失败: %v", err)
	}
	if !accepted {
		t.Fatal("hub 返回 200 时必须视为已接受")
	}
	if got := hub.calls.Load(); got != 1 {
		t.Fatalf("假 hub 应收到 1 次请求, 实际 %d", got)
	}

	last, payload := hub.lastPayload(t)
	if last.path != "/warm" {
		t.Errorf("请求路径 = %q, want /warm", last.path)
	}
	if !strings.HasPrefix(last.contentType, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", last.contentType)
	}

	// design §3 冻结的字段逐个校验(用 map 解码: 不许两侧共用同一份结构体一起错)
	if payload["file_id"] != fileID {
		t.Errorf("file_id = %v, want %s", payload["file_id"], fileID)
	}
	if payload["file_token"] != fileToken(gdPath) {
		t.Errorf("file_token = %v, want %s(换新链用的现有通道凭据)", payload["file_token"], fileToken(gdPath))
	}
	if payload["direct_link"] != driveDirectLink(fileID) {
		t.Errorf("direct_link = %v, want %s", payload["direct_link"], driveDirectLink(fileID))
	}
	auth, ok := payload["auth"].(map[string]any)
	if !ok || auth["Authorization"] != testGoogleHeader {
		t.Errorf("auth 应携带面板下发的凭据, 实际: %v", payload["auth"])
	}
	regions, ok := payload["regions"].(map[string]any)
	if !ok {
		t.Fatalf("regions 缺失: %v", payload)
	}
	if regions["head_bytes"] != float64(hubWarmHeadBytes) || regions["tail_bytes"] != float64(hubWarmTailBytes) {
		t.Errorf("regions = %v, want head=%d, tail=%d", regions, hubWarmHeadBytes, hubWarmTailBytes)
	}
	if _, has := regions["resume_offset_bytes"]; has {
		t.Errorf("算不出续播偏移时应省略该字段, 实际: %v", regions)
	}
}

func TestWarmFile_FailureEntersCooldown(t *testing.T) {
	hub := newFakeHub(t, http.StatusInternalServerError)
	fileID := uniqueDriveFileID()
	panel := newFakePanel(t, http.StatusOK, driveDirectLink(fileID), "", "", "")
	setupStateDir(t)
	setupHubFullConfig(t, hub.port, true, panel.URL, "2s")
	simulateRestart()
	seedHub(t, "hub-1", "127.0.0.1", 0, 0)

	accepted, err := WarmFile(context.Background(), uniqueHubGDPath("失败"))
	if accepted {
		t.Fatal("hub 返回 500 时不应视为已接受")
	}
	if err == nil {
		t.Fatal("hub 返回 500 必须回传错误(由调用方记 WARN 后回退)")
	}
	if got := hub.calls.Load(); got != 1 {
		t.Fatalf("第一次应尝试 1 次, 实际 %d", got)
	}

	// 冷却窗口内: 不再打扰 hub, 直接失败(避免"hub 活着但指令不通"时白白等超时)
	accepted, err = WarmFile(context.Background(), uniqueHubGDPath("失败冷却"))
	if accepted || err == nil {
		t.Fatalf("冷却窗口内应直接回退, 实际: accepted=%v, err=%v", accepted, err)
	}
	if got := hub.calls.Load(); got != 1 {
		t.Fatalf("冷却窗口内不应再请求 hub, 实际 %d 次", got)
	}

	// 冷却结束后恢复尝试
	hubWarmStore.mu.Lock()
	hubWarmStore.failedAt["hub-1"] = time.Now().Add(-hubWarmCooldown - time.Second)
	hubWarmStore.mu.Unlock()

	if _, err := WarmFile(context.Background(), uniqueHubGDPath("失败冷却结束")); err == nil {
		t.Fatal("hub 仍然故障时应继续回传错误")
	}
	if got := hub.calls.Load(); got != 2 {
		t.Fatalf("冷却结束后应重新尝试, 实际 %d 次", got)
	}
}

func TestWarmFile_TimeoutFallsBackQuickly(t *testing.T) {
	hub := newFakeHub(t, http.StatusOK)
	hub.hang.Store(true)
	fileID := uniqueDriveFileID()
	panel := newFakePanel(t, http.StatusOK, driveDirectLink(fileID), "", "", "")
	setupStateDir(t)
	setupHubFullConfig(t, hub.port, true, panel.URL, "1s")
	simulateRestart()
	seedHub(t, "hub-1", "127.0.0.1", 0, 0)

	start := time.Now()
	accepted, err := WarmFile(context.Background(), uniqueHubGDPath("超时"))
	elapsed := time.Since(start)

	if accepted || err == nil {
		t.Fatalf("hub 挂起时应按超时失败回退, 实际: accepted=%v, err=%v", accepted, err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("预热超时不受 hub-warm-timeout 约束, 耗时 %v", elapsed)
	}
	if got := hub.calls.Load(); got != 1 {
		t.Fatalf("超时场景应恰好发起 1 次请求, 实际 %d", got)
	}
}

// TestHubWarmStore_Bounded 预热状态必须内存有界(TTL + 容量)
func TestHubWarmStore_Bounded(t *testing.T) {
	resetHubWarmStore()
	t.Cleanup(resetHubWarmStore)

	now := time.Now()
	for i := 0; i < hubWarmAcceptLimit*2; i++ {
		// 登记时刻逐一递增: 淘汰"最老"的判定才有确定的先后关系
		hubWarmStore.markAccepted(fmt.Sprintf("file-%d", i), "hub-1", now.Add(time.Duration(i)*time.Millisecond))
	}
	after := now.Add(time.Duration(hubWarmAcceptLimit*2) * time.Millisecond)
	if got := len(hubWarmStore.accepted); got != hubWarmAcceptLimit {
		t.Fatalf("接受标记表容量 = %d, want %d", got, hubWarmAcceptLimit)
	}
	// 最新的仍在, 最早的已被淘汰
	if !hubWarmStore.isAccepted(fmt.Sprintf("file-%d", hubWarmAcceptLimit*2-1), "hub-1", after) {
		t.Error("最新登记的文件应保留")
	}
	if hubWarmStore.isAccepted("file-0", "hub-1", after) {
		t.Error("容量溢出时应淘汰最老的条目")
	}

	// TTL: 过期标记失效
	hubWarmStore.markAccepted("expired", "hub-1", now)
	if hubWarmStore.isAccepted("expired", "hub-1", now.Add(hubWarmAcceptTTL)) {
		t.Error("超过 TTL 的接受标记必须失效")
	}

	// 多 hub: 标记只对登记的 hub 生效
	hubWarmStore.markAccepted("for-a", "hub-a", now)
	if hubWarmStore.isAccepted("for-a", "hub-b", now) {
		t.Error("不同 hub 的接受标记不得串用")
	}

	for i := 0; i < hubWarmFailuresLimit*2; i++ {
		hubWarmStore.recordFailure(fmt.Sprintf("hub-%d", i), now)
	}
	if got := len(hubWarmStore.failedAt); got > hubWarmFailuresLimit {
		t.Fatalf("失败冷却表容量 = %d, 上限 %d", got, hubWarmFailuresLimit)
	}

	// 冷却语义单独校验(容量压力测试可能清空过整张表)
	hubWarmStore.recordFailure("hub-cool", now)
	if !hubWarmStore.inCooldown("hub-cool", now.Add(hubWarmCooldown-time.Second)) {
		t.Error("冷却窗口内应判定为冷却中")
	}
	if hubWarmStore.inCooldown("hub-cool", now.Add(hubWarmCooldown)) {
		t.Error("冷却窗口结束应恢复")
	}
}

// ============================ 播放链路: 上游改写与回退 ============================

// enrollHub 走真实注册接口注册一台 hub
func enrollHub(t *testing.T, engine http.Handler, machineID string) map[string]any {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"enroll_token": testEnrollToken,
		"machine_id":   machineID,
		"hostname":     machineID,
		"version":      "v0.4.0",
		"listen_port":  8791,
		"role":         RoleHub,
	})
	if err != nil {
		t.Fatalf("构造注册请求失败: %v", err)
	}

	recorder := doRequest(t, engine, http.MethodPost, "/api/agent/enroll", string(body), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("注册 hub 失败: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
	}
	return decodeMap(t, recorder.Body.Bytes())
}

// heartbeatFrom 让节点心跳一次, 并指定来源 IP
//
// 测试引擎信任 X-Forwarded-For(gin 默认信任所有代理), 用它固定注册表里
// 记录的 LastIP —— hub 的内网基址由 LastIP + agent-network.hub-port 推导。
func heartbeatFrom(t *testing.T, engine http.Handler, agentID, secret, ip string) {
	t.Helper()

	headers := agentAuthHeaders(agentID, secret)
	if ip != "" {
		headers["X-Forwarded-For"] = ip
	}
	recorder := doRequest(t, engine, http.MethodPost, "/api/agent/heartbeat", `{}`, headers)
	if recorder.Code != http.StatusOK {
		t.Fatalf("心跳失败: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestDownloadLink_HubRewrite 播放链路整体打通: 节点拿到的上游指向 hub 内网口
func TestDownloadLink_HubRewrite(t *testing.T) {
	hub := newFakeHub(t, http.StatusOK)
	fileID := uniqueDriveFileID()
	expiresAt := time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05") + ".5Z"
	panel := newFakePanel(t, http.StatusOK, driveDirectLink(fileID), expiresAt, "", "")
	setupStateDir(t)
	setupHubFullConfig(t, hub.port, true, panel.URL, "")
	simulateRestart()

	collector := &collectedLogs{}
	logID, ok := logs.RegisterLogger(collector)
	if !ok {
		t.Fatal("注册日志收集器失败")
	}
	defer logs.RemoveLogger(logID)

	engine := newTestEngine()
	node := enrollAgent(t, engine, "m-node")
	hubAgent := enrollHub(t, engine, "m-hub")
	heartbeatFrom(t, engine, hubAgent["agent_id"].(string), hubAgent["agent_secret"].(string), "127.0.0.1")

	gdPath := uniqueHubGDPath("改写")
	auth := agentAuthHeaders(node["agent_id"].(string), node["agent_secret"].(string))

	recorder := doRequest(t, engine, http.MethodGet, downloadLinkURL(gdPath), "", auth)
	if recorder.Code != http.StatusOK {
		t.Fatalf("下发直链应成功: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
	}
	resp := decodeMap(t, recorder.Body.Bytes())

	wantUpstream := fmt.Sprintf("http://127.0.0.1:%d/f/%s", hub.port, fileID)
	if resp["url"] != wantUpstream {
		t.Errorf("节点上游 = %v, want %v", resp["url"], wantUpstream)
	}
	headers, ok := resp["headers"].(map[string]any)
	if !ok {
		t.Fatalf("headers 字段缺失或类型错误: %v", resp["headers"])
	}
	if len(headers) != 0 {
		t.Errorf("hub 上游不带任何凭据, headers 应为空, 实际: %v", headers)
	}
	if resp["expires_at"] != expiresAt {
		t.Errorf("expires_at 必须原样透传: %v, want %s", resp["expires_at"], expiresAt)
	}

	if got := hub.calls.Load(); got != 1 {
		t.Fatalf("首次播放应同步补发 1 次 warm, 实际 %d", got)
	}
	_, payload := hub.lastPayload(t)
	if payload["file_id"] != fileID {
		t.Errorf("warm file_id = %v, want %s", payload["file_id"], fileID)
	}

	// 第二次(同一文件): warm 标记命中, 不再打扰 hub
	recorder = doRequest(t, engine, http.MethodGet, downloadLinkURL(gdPath), "", auth)
	if recorder.Code != http.StatusOK {
		t.Fatalf("第二次下发直链应成功: HTTP %d", recorder.Code)
	}
	if resp := decodeMap(t, recorder.Body.Bytes()); resp["url"] != wantUpstream {
		t.Errorf("第二次上游 = %v, want %v", resp["url"], wantUpstream)
	}
	if got := hub.calls.Load(); got != 1 {
		t.Errorf("warm 已被接受时不应重复下发, 实际 %d 次", got)
	}

	// 日志卫生: 直链、面板令牌与 Google 凭据都不得进日志
	logged := collector.String()
	for name, secretValue := range map[string]string{
		"面板令牌":      testPanelToken,
		"Google 凭据": testGoogleHeader,
		"完整直链":      driveDirectLink(fileID),
		"节点密钥":      node["agent_secret"].(string),
	} {
		if strings.Contains(logged, secretValue) {
			t.Errorf("日志里出现了 %s: %q", name, logged)
		}
	}
}

// TestDownloadLink_HubNeverRewritesHubItself hub 换新链不得指向自己
func TestDownloadLink_HubNeverRewritesHubItself(t *testing.T) {
	hub := newFakeHub(t, http.StatusOK)
	fileID := uniqueDriveFileID()
	expiresAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	panel := newFakePanel(t, http.StatusOK, driveDirectLink(fileID), expiresAt, "", "")
	setupStateDir(t)
	setupHubFullConfig(t, hub.port, true, panel.URL, "")
	simulateRestart()

	engine := newTestEngine()
	hubAgent := enrollHub(t, engine, "m-hub")
	hubID := hubAgent["agent_id"].(string)
	hubSecret := hubAgent["agent_secret"].(string)
	heartbeatFrom(t, engine, hubID, hubSecret, "127.0.0.1")

	recorder := doRequest(t, engine, http.MethodGet, downloadLinkURL(uniqueHubGDPath("hub自取")), "", agentAuthHeaders(hubID, hubSecret))
	if recorder.Code != http.StatusOK {
		t.Fatalf("hub 换取直链应成功: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
	}
	resp := decodeMap(t, recorder.Body.Bytes())
	if resp["url"] != driveDirectLink(fileID) {
		t.Errorf("hub 必须拿到真正的 Google 直链, 实际: %v", resp["url"])
	}
	headers, ok := resp["headers"].(map[string]any)
	if !ok || headers["Authorization"] != testGoogleHeader {
		t.Errorf("hub 换链需要 Google 凭据, 实际: %v", resp["headers"])
	}
	if got := hub.calls.Load(); got != 0 {
		t.Errorf("hub 自己的换链请求不应触发 warm(自指死循环), 实际 %d 次", got)
	}
}

// TestDownloadLink_HubFallback Google 直链回退的硬要求
func TestDownloadLink_HubFallback(t *testing.T) {
	expiresAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	fileID := uniqueDriveFileID()
	panel := newFakePanel(t, http.StatusOK, driveDirectLink(fileID), expiresAt, "", "")

	t.Run("hub 返回异常状态码", func(t *testing.T) {
		hub := newFakeHub(t, http.StatusInternalServerError)
		setupStateDir(t)
		setupHubFullConfig(t, hub.port, true, panel.URL, "2s")
		simulateRestart()

		engine := newTestEngine()
		node := enrollAgent(t, engine, "m-node")
		hubAgent := enrollHub(t, engine, "m-hub")
		heartbeatFrom(t, engine, hubAgent["agent_id"].(string), hubAgent["agent_secret"].(string), "127.0.0.1")

		gdPath := uniqueHubGDPath("回退状态码")
		auth := agentAuthHeaders(node["agent_id"].(string), node["agent_secret"].(string))

		recorder := doRequest(t, engine, http.MethodGet, downloadLinkURL(gdPath), "", auth)
		if recorder.Code != http.StatusOK {
			t.Fatalf("回退路径必须仍然 200: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
		}
		assertGoogleDirectResponse(t, recorder.Body.Bytes(), driveDirectLink(fileID), expiresAt)

		if got := hub.calls.Load(); got != 1 {
			t.Errorf("应尝试同步补发 1 次 warm, 实际 %d", got)
		}

		// 冷却窗口内第二次起播: 不再等 hub, 仍然回退 Google
		recorder = doRequest(t, engine, http.MethodGet, downloadLinkURL(gdPath), "", auth)
		if recorder.Code != http.StatusOK {
			t.Fatalf("第二次也必须 200: HTTP %d", recorder.Code)
		}
		assertGoogleDirectResponse(t, recorder.Body.Bytes(), driveDirectLink(fileID), expiresAt)
		if got := hub.calls.Load(); got != 1 {
			t.Errorf("冷却窗口内不应重复补发, 实际 %d 次", got)
		}
	})

	t.Run("hub 不可达", func(t *testing.T) {
		hub := newFakeHub(t, http.StatusOK)
		hubPort := hub.port
		hub.server.Close() // 端口不再监听: 连接被拒, 回退必须即时发生

		setupStateDir(t)
		setupHubFullConfig(t, hubPort, true, panel.URL, "2s")
		simulateRestart()

		engine := newTestEngine()
		node := enrollAgent(t, engine, "m-node")
		hubAgent := enrollHub(t, engine, "m-hub")
		heartbeatFrom(t, engine, hubAgent["agent_id"].(string), hubAgent["agent_secret"].(string), "127.0.0.1")

		start := time.Now()
		recorder := doRequest(t, engine, http.MethodGet, downloadLinkURL(uniqueHubGDPath("回退不可达")), "",
			agentAuthHeaders(node["agent_id"].(string), node["agent_secret"].(string)))
		elapsed := time.Since(start)

		if recorder.Code != http.StatusOK {
			t.Fatalf("回退路径必须仍然 200: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
		}
		assertGoogleDirectResponse(t, recorder.Body.Bytes(), driveDirectLink(fileID), expiresAt)
		if elapsed > 3*time.Second {
			t.Errorf("hub 不可达时回退不应等待超时, 耗时 %v", elapsed)
		}
	})

	t.Run("没有健康 hub", func(t *testing.T) {
		setupStateDir(t)
		setupHubFullConfig(t, 8791, true, panel.URL, "2s")
		simulateRestart()

		engine := newTestEngine()
		node := enrollAgent(t, engine, "m-node")

		recorder := doRequest(t, engine, http.MethodGet, downloadLinkURL(uniqueHubGDPath("回退无hub")), "",
			agentAuthHeaders(node["agent_id"].(string), node["agent_secret"].(string)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("回退路径必须仍然 200: HTTP %d", recorder.Code)
		}
		assertGoogleDirectResponse(t, recorder.Body.Bytes(), driveDirectLink(fileID), expiresAt)
	})
}

// assertGoogleDirectResponse 断言响应逐字节等于"Google 直链 + 凭据"的现状形状
func assertGoogleDirectResponse(t *testing.T, body []byte, directURL, expiresAt string) {
	t.Helper()

	want, err := json.Marshal(map[string]any{
		"url":        directURL,
		"headers":    map[string]string{"Authorization": testGoogleHeader},
		"expires_at": expiresAt,
	})
	if err != nil {
		t.Fatalf("构造期望响应失败: %v", err)
	}
	if !bytes.Equal(body, want) {
		t.Errorf("回退响应与现状逐字节不一致:\n实际: %s\n期望: %s", body, want)
	}
}

// TestDownloadLink_HubDisabledIsByteIdenticalToBaseline hub-enable=false 的逐字节基线
//
// 同一场景跑两次: 一次 hub 接入关闭(注册表里还有一台健康的 hub, 且假 hub 真在监听),
// 一次场景里根本没有 hub 记录。两次响应体必须逐字节一致, 且整个过程 hub 一次都不能
// 被联系 —— 这就是"关闭时行为与未部署 hub 完全一致"的对照式证明。
//
// 假 hub 必须真实可连通: 若开关检查被绕开, warm 会成功并改写上游, 本用例立刻转红
// (挂起一个不可连通的端口会让"改写失败回退"掩盖开关失效, 对照就失去意义)。
func TestDownloadLink_HubDisabledIsByteIdenticalToBaseline(t *testing.T) {
	expiresAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	fileID := uniqueDriveFileID()
	panel := newFakePanel(t, http.StatusOK, driveDirectLink(fileID), expiresAt, "", "")
	hub := newFakeHub(t, http.StatusOK)

	run := func(t *testing.T, withHubRecord bool) []byte {
		t.Helper()

		setupStateDir(t)
		setupHubFullConfig(t, hub.port, false, panel.URL, "")
		simulateRestart()

		engine := newTestEngine()
		node := enrollAgent(t, engine, "m-node")
		if withHubRecord {
			hubAgent := enrollHub(t, engine, "m-hub")
			heartbeatFrom(t, engine, hubAgent["agent_id"].(string), hubAgent["agent_secret"].(string), "127.0.0.1")
		}

		recorder := doRequest(t, engine, http.MethodGet, downloadLinkURL(uniqueHubGDPath("基线")), "",
			agentAuthHeaders(node["agent_id"].(string), node["agent_secret"].(string)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("下发直链应成功: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
		}
		return recorder.Body.Bytes()
	}

	withRecord := run(t, true)
	baseline := run(t, false)

	if !bytes.Equal(withRecord, baseline) {
		t.Errorf("hub 接入关闭时的响应必须与未部署 hub 逐字节一致:\n有 hub 记录: %s\n基线:      %s", withRecord, baseline)
	}
	assertGoogleDirectResponse(t, withRecord, driveDirectLink(fileID), expiresAt)
	if got := hub.calls.Load(); got != 0 {
		t.Errorf("hub 接入关闭时不得联系 hub, 实际 %d 次", got)
	}
}

// ============================ 调度隔离 ============================

func TestSchedule_HubNeverSelected(t *testing.T) {
	t.Run("最少连接策略", func(t *testing.T) {
		setupStateDir(t)
		setupHubFullConfig(t, 0, true, "", "")
		simulateRestart()

		// hub 0 条活跃流, 节点 5 条: 若 hub 参与调度, 它会赢
		hub := seedHub(t, "hub-1", "10.0.0.1", 0, 0)
		hub.ActiveStreams = 0
		seedRecord(t, &agentRecord{
			ID: "node-1", MachineID: "m-node", Name: "node-1", Role: RoleNode,
			LastIP: "10.0.0.2", ListenPort: 8790, Enabled: true, LastSeenAt: time.Now(), ActiveStreams: 5,
		})

		rec, err := defaultRegistry.schedule(time.Now(), 45*time.Second, config.ScheduleStrategyLeastActive)
		if err != nil {
			t.Fatalf("调度失败: %v", err)
		}
		if rec == nil || rec.ID != "node-1" {
			t.Fatalf("hub 不得参与客户端调度, 实际选中: %+v", rec)
		}
	})

	t.Run("优先级策略", func(t *testing.T) {
		setupStateDir(t)
		setupHubFullConfig(t, 0, true, "", "")
		simulateRestart()

		// hub 优先级 0(最优先), 节点 9: 若 hub 参与调度, 它会赢
		seedHub(t, "hub-1", "10.0.0.1", 0, 0)
		seedRecord(t, &agentRecord{
			ID: "node-1", MachineID: "m-node", Name: "node-1", Role: RoleNode,
			LastIP: "10.0.0.2", ListenPort: 8790, Enabled: true, LastSeenAt: time.Now(), Priority: 9,
		})

		rec, err := defaultRegistry.schedule(time.Now(), 45*time.Second, config.ScheduleStrategyPriority)
		if err != nil {
			t.Fatalf("调度失败: %v", err)
		}
		if rec == nil || rec.ID != "node-1" {
			t.Fatalf("priority 策略下 hub 同样不得参与调度, 实际选中: %+v", rec)
		}
	})

	t.Run("只有 hub 时无点可调", func(t *testing.T) {
		setupStateDir(t)
		setupHubFullConfig(t, 0, true, "", "")
		simulateRestart()
		seedHub(t, "hub-1", "10.0.0.1", 0, 0)

		rec, err := defaultRegistry.schedule(time.Now(), 45*time.Second, config.ScheduleStrategyLeastActive)
		if err != nil {
			t.Fatalf("调度失败: %v", err)
		}
		if rec != nil {
			t.Fatalf("只有 hub 时应无点可调, 实际: %+v", rec)
		}

		if _, err := PickAndSign("/影视库/只有hub.mkv"); err == nil {
			t.Fatal("只有 hub 时播放入口必须走回退(返回错误)")
		}
	})
}

// ============================ 注册与注册表兼容 ============================

func TestEnroll_RoleField(t *testing.T) {
	t.Run("role=hub 落盘并可读回", func(t *testing.T) {
		basePath := setupStateDir(t)
		setupHubFullConfig(t, 0, true, "", "")
		simulateRestart()

		engine := newTestEngine()
		enrollHub(t, engine, "m-hub")

		list, err := defaultRegistry.snapshot()
		if err != nil {
			t.Fatalf("读取注册表失败: %v", err)
		}
		if len(list) != 1 || normalizeRole(list[0].Role) != RoleHub {
			t.Fatalf("hub 记录角色错误: %+v", list)
		}

		data, err := os.ReadFile(agentsFilePath(basePath))
		if err != nil {
			t.Fatalf("读取注册表文件失败: %v", err)
		}
		var file agentsFile
		if err := json.Unmarshal(data, &file); err != nil {
			t.Fatalf("注册表不是合法 JSON: %v", err)
		}
		if len(file.Agents) != 1 || file.Agents[0].Role != RoleHub {
			t.Fatalf("hub 角色必须落盘, 实际: %+v", file.Agents)
		}
	})

	t.Run("缺省 role 等价 node", func(t *testing.T) {
		setupStateDir(t)
		setupHubFullConfig(t, 0, true, "", "")
		simulateRestart()

		engine := newTestEngine()
		enrollAgent(t, engine, "m-node")

		list, err := defaultRegistry.snapshot()
		if err != nil {
			t.Fatalf("读取注册表失败: %v", err)
		}
		if len(list) != 1 || normalizeRole(list[0].Role) != RoleNode {
			t.Fatalf("缺省 role 必须等价 node: %+v", list)
		}
	})

	t.Run("非法 role 返回 400", func(t *testing.T) {
		setupStateDir(t)
		setupHubFullConfig(t, 0, true, "", "")
		simulateRestart()

		engine := newTestEngine()
		recorder := doRequest(t, engine, http.MethodPost, "/api/agent/enroll",
			fmt.Sprintf(`{"enroll_token":%q,"machine_id":"m-x","hostname":"x","version":"v1","listen_port":8790,"role":"master"}`, testEnrollToken),
			nil)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("非法 role 应返回 400, 实际: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
		}
		resp := decodeMap(t, recorder.Body.Bytes())
		if code := errorCodeOf(t, resp); code != codeValidationError {
			t.Errorf("错误码 = %q, want %q", code, codeValidationError)
		}
		message, _ := resp["error"].(map[string]any)["message"].(string)
		if !strings.Contains(message, "role") {
			t.Errorf("错误消息应指明 role 字段: %q", message)
		}
	})
}

func TestEnroll_ReEnrollRefreshesRole(t *testing.T) {
	setupStateDir(t)
	setupHubFullConfig(t, 0, true, "", "")
	simulateRestart()

	// hub 换代为 node(同一 machine_id): 部署上报的角色必须被刷新
	first, err := defaultRegistry.enroll(enrollParams{
		MachineID: "m-1", Hostname: "hub-1", Version: "v0.4.0",
		ListenPort: 8791, Role: RoleHub, LastIP: "10.0.0.1", Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	second, err := defaultRegistry.enroll(enrollParams{
		MachineID: "m-1", Hostname: "node-1", Version: "v0.4.0",
		ListenPort: 8790, Role: RoleNode, LastIP: "10.0.0.1", Now: time.Now(),
	})
	if err != nil {
		t.Fatalf("重新注册失败: %v", err)
	}
	if first.AgentID != second.AgentID {
		t.Fatalf("同一 machine_id 应复用节点 id: %v != %v", first.AgentID, second.AgentID)
	}

	list, err := defaultRegistry.snapshot()
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	if len(list) != 1 || normalizeRole(list[0].Role) != RoleNode {
		t.Fatalf("重新注册后角色应为 node: %+v", list)
	}
}

// TestLoadAgentsFile_RoleBackwardCompatible 注册表落盘格式的角色兼容
//
// 旧文件没有 role 字段 → node; node 记录回写时不得出现 role 键
// (否则 node 注册表的落盘内容就不再与新增 role 之前逐字节一致);
// hub 记录必须带 role 落盘, 重启后身份不丢。
func TestLoadAgentsFile_RoleBackwardCompatible(t *testing.T) {
	old := `{"version":1,"agents":[{"id":"a1","machine_id":"m1","name":"旧节点","secret":"s1","sign_key":"k1","listen_port":8790,"enabled":true,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`
	withHub := `{"version":1,"agents":[{"id":"a1","machine_id":"m1","name":"缓存中心","secret":"s1","sign_key":"k1","role":"hub","listen_port":8791,"enabled":true,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`

	cases := []struct {
		name     string
		content  string
		wantRole string
	}{
		{"旧文件无 role 字段", old, RoleNode},
		{"新文件带 role", withHub, RoleHub},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), fileName)
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("写入注册表失败: %v", err)
			}
			records, err := loadAgentsFile(path)
			if err != nil {
				t.Fatalf("加载注册表失败: %v", err)
			}
			rec := records["a1"]
			if rec == nil {
				t.Fatal("节点记录未加载")
			}
			if normalizeRole(rec.Role) != tc.wantRole {
				t.Errorf("Role = %q, want %q", rec.Role, tc.wantRole)
			}
		})
	}

	// node 记录回写: 不出现 role 键
	if data, err := json.Marshal((&agentRecord{ID: "a1", Role: RoleNode}).toFileEntry()); err != nil {
		t.Fatalf("序列化节点记录失败: %v", err)
	} else if strings.Contains(string(data), `"role"`) {
		t.Errorf("node 记录不得写 role 键(向后兼容), 实际: %s", data)
	}
	// 未知取值同样按 node 落盘(比如手工改坏了文件)
	if data, err := json.Marshal((&agentRecord{ID: "a1", Role: "weird"}).toFileEntry()); err != nil {
		t.Fatalf("序列化节点记录失败: %v", err)
	} else if strings.Contains(string(data), `"role"`) {
		t.Errorf("非法角色必须按 node 归一化, 实际: %s", data)
	}
	// hub 记录: 必须写 role
	if data, err := json.Marshal((&agentRecord{ID: "a1", Role: RoleHub}).toFileEntry()); err != nil {
		t.Fatalf("序列化节点记录失败: %v", err)
	} else if !strings.Contains(string(data), `"role":"hub"`) {
		t.Errorf("hub 记录必须写 role 键, 实际: %s", data)
	}
}

func TestAdminListAgents_RoleField(t *testing.T) {
	setupStateDir(t)
	setupHubFullConfig(t, 0, true, "", "")
	simulateRestart()

	engine := newTestEngine()
	enrollAgent(t, engine, "m-node")
	enrollHub(t, engine, "m-hub")

	recorder := doRequest(t, engine, http.MethodPost, "/ge2o/agent-network/agents",
		fmt.Sprintf(`{"secret":%q}`, testGe2oSecret), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("节点列表应成功: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
	}
	resp := decodeMap(t, recorder.Body.Bytes())

	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 data: %v", resp)
	}
	agents, ok := data["agents"].([]any)
	if !ok || len(agents) != 2 {
		t.Fatalf("agents 应为 2 条, 实际: %v", data["agents"])
	}

	roles := map[string]int{}
	for _, raw := range agents {
		view, _ := raw.(map[string]any)
		name, _ := view["name"].(string)
		role, _ := view["role"].(string)
		if name == "" {
			t.Fatalf("节点视图缺少名称: %v", view)
		}
		roles[role]++
	}
	if roles[RoleNode] != 1 || roles[RoleHub] != 1 || len(roles) != 2 {
		t.Fatalf("节点列表角色字段错误: %v", roles)
	}
	// 视图仍然是脱敏的
	if strings.Contains(recorder.Body.String(), "agent_secret") || strings.Contains(recorder.Body.String(), "sign_key") {
		t.Errorf("节点列表不得包含凭据字段: %s", recorder.Body.String())
	}
}
