package agentnet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
)

// 本文件是包内测试(白盒): 需要直接核对 agents.json 的落盘内容与内存态,
// 这些细节不适合为了测试而导出成公开接口.

// setupStateDir 为当前用例准备独享的状态目录
//
// 注册表按 config.BasePath 懒加载, 因此每个用例换一个临时目录即可获得
// 干净的初始状态, 不需要导出"测试专用重置接口"。
func setupStateDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	oldBasePath := config.BasePath
	config.BasePath = dir
	t.Cleanup(func() { config.BasePath = oldBasePath })
	return dir
}

// setupAgentConfig 装载一份启用状态的 agent 网络配置
func setupAgentConfig(t *testing.T, enable bool) *config.AgentNetwork {
	t.Helper()

	agent := &config.AgentNetwork{
		Enable:         enable,
		EnrollToken:    "test-enroll-token-0123456789",
		OfflineSeconds: 45,
		URLTTL:         "24h",
	}
	if err := agent.Init(); err != nil {
		t.Fatalf("初始化测试配置失败: %v", err)
	}

	oldCfg := config.C
	config.C = &config.Config{AgentNetwork: agent}
	t.Cleanup(func() { config.C = oldCfg })
	return agent
}

// simulateRestart 清空内存态, 强制下一次访问从磁盘重新加载
//
// 用于验证"哪些字段落盘、哪些只存在于内存"。
func simulateRestart() {
	defaultRegistry.mu.Lock()
	defer defaultRegistry.mu.Unlock()
	defaultRegistry.records = map[string]*agentRecord{}
	defaultRegistry.byMachine = map[string]string{}
	defaultRegistry.loadedPath = ""
}

// agentsFilePath 返回当前状态目录下的注册表路径
func agentsFilePath(basePath string) string {
	return filepath.Join(basePath, DirName, fileName)
}

// testEnrollParams 生成一份可用的注册入参
func testEnrollParams(machineID string) enrollParams {
	return enrollParams{
		MachineID:  machineID,
		Hostname:   "node-" + machineID,
		Version:    "v1.0.0",
		ListenPort: 8790,
		LastIP:     "192.168.1.10",
		Now:        time.Now(),
	}
}

func TestEnroll_CreatesRecordAndPersists(t *testing.T) {
	basePath := setupStateDir(t)
	setupAgentConfig(t, true)

	result, err := defaultRegistry.enroll(testEnrollParams("machine-1"))
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if result.AgentID == "" || len(result.Secret) != credentialBytes*2 || len(result.SignKey) != credentialBytes*2 {
		t.Fatalf("注册返回的凭据不完整: %+v", result)
	}
	if result.Reused {
		t.Error("首次注册不应标记为复用")
	}

	path := agentsFilePath(basePath)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("注册表未落盘: %v", err)
	}
	if perm := info.Mode().Perm(); perm != agentsFilePerm {
		t.Errorf("注册表权限 = %o, want %o", perm, agentsFilePerm)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	var file agentsFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("注册表不是合法 JSON: %v", err)
	}
	if file.Version != agentsFileVersion {
		t.Errorf("注册表版本 = %d, want %d", file.Version, agentsFileVersion)
	}
	if len(file.Agents) != 1 || file.Agents[0].ID != result.AgentID {
		t.Fatalf("注册表内容与注册结果不一致: %+v", file.Agents)
	}
	if file.Agents[0].Secret != result.Secret || file.Agents[0].SignKey != result.SignKey {
		t.Error("注册表应保存节点凭据(重启后节点无需重新注册)")
	}
	if !file.Agents[0].Enabled {
		t.Error("新注册的节点应默认为启用")
	}
	if file.Agents[0].Priority != 0 {
		t.Errorf("新注册节点的优先级应默认为 0, 实际: %d", file.Agents[0].Priority)
	}
	// 易失字段不落盘
	if strings.Contains(string(data), "last_seen_at") || strings.Contains(string(data), "active_streams") {
		t.Error("last_seen_at / active_streams 属于易失状态, 不应落盘")
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("注册表文件应以换行结尾")
	}
}

func TestEnroll_IdempotentByMachineID(t *testing.T) {
	basePath := setupStateDir(t)
	setupAgentConfig(t, true)

	first, err := defaultRegistry.enroll(testEnrollParams("machine-1"))
	if err != nil {
		t.Fatalf("首次注册失败: %v", err)
	}

	// 先心跳一次, 制造"易失状态被清零"的可观察点
	if _, err := defaultRegistry.touch(first.AgentID, heartbeatParams{
		LastIP:        "192.168.1.10",
		ActiveStreams: 3,
		Now:           time.Now(),
	}); err != nil {
		t.Fatalf("心跳失败: %v", err)
	}

	// 管理员先自定义名称与优先级: 重注册不得覆盖这两项(属于管理员状态)
	if _, err := defaultRegistry.setProfile(first.AgentID, "自定义名称", 3, time.Now()); err != nil {
		t.Fatalf("更新节点资料失败: %v", err)
	}

	// 同一台机器重跑安装脚本: 换主机名与端口, 复用记录但轮换凭据
	params := testEnrollParams("machine-1")
	params.Hostname = "node-renamed"
	params.ListenPort = 9999
	second, err := defaultRegistry.enroll(params)
	if err != nil {
		t.Fatalf("重复注册失败: %v", err)
	}

	if second.AgentID != first.AgentID {
		t.Errorf("同一 machine_id 应复用节点 id: %s != %s", second.AgentID, first.AgentID)
	}
	if !second.Reused {
		t.Error("重复注册应标记为复用")
	}
	if second.Secret == first.Secret || second.SignKey == first.SignKey {
		t.Error("重复注册必须轮换 secret 与 sign_key(旧凭据立即失效)")
	}

	list, err := defaultRegistry.snapshot()
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("重复注册不应产生新记录, 实际 %d 条", len(list))
	}
	if !list[0].LastSeenAt.IsZero() || list[0].ActiveStreams != 0 {
		t.Error("重复注册应清零易失状态(等下一次心跳重新探测)")
	}
	if list[0].ListenPort != 9999 {
		t.Errorf("重复注册应更新节点上报的信息: %+v", list[0])
	}
	// 名称与优先级是管理员状态: 节点每次升级都要重跑脚本, 不能被上报的主机名擦掉
	if list[0].Name != "自定义名称" {
		t.Errorf("重复注册不得覆盖管理员设置的名称: %q", list[0].Name)
	}
	if list[0].Priority != 3 {
		t.Errorf("重复注册不得重置管理员设置的优先级: %d", list[0].Priority)
	}
	if second.Name != "自定义名称" {
		t.Errorf("复用分支返回的展示名应是记录里的名称: %q", second.Name)
	}

	data, err := os.ReadFile(agentsFilePath(basePath))
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	if strings.Count(string(data), params.MachineID) != 1 {
		t.Error("落盘内容里 machine_id 应只出现一次")
	}
}

func TestEnroll_PreservesDisabledState(t *testing.T) {
	setupStateDir(t)
	setupAgentConfig(t, true)

	first, err := defaultRegistry.enroll(testEnrollParams("machine-1"))
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if _, err := defaultRegistry.setEnabled(first.AgentID, false, time.Now()); err != nil {
		t.Fatalf("禁用节点失败: %v", err)
	}

	// 被禁用的节点重跑安装脚本后仍然是禁用状态: 管理员的决定不能被节点抹掉
	if _, err := defaultRegistry.enroll(testEnrollParams("machine-1")); err != nil {
		t.Fatalf("重复注册失败: %v", err)
	}

	list, err := defaultRegistry.snapshot()
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	if len(list) != 1 || list[0].Enabled {
		t.Fatalf("重复注册不应把管理员禁用的节点重新启用: %+v", list)
	}
}

func TestAuthenticate(t *testing.T) {
	setupStateDir(t)
	setupAgentConfig(t, true)

	result, err := defaultRegistry.enroll(testEnrollParams("machine-1"))
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	rec, err := defaultRegistry.authenticate(result.AgentID, result.Secret)
	if err != nil {
		t.Fatalf("鉴权失败: %v", err)
	}
	if rec == nil || rec.ID != result.AgentID {
		t.Fatal("正确凭据应通过鉴权")
	}

	cases := []struct {
		name   string
		id     string
		secret string
	}{
		{"凭据错误", result.AgentID, strings.Repeat("f", credentialBytes*2)},
		{"节点不存在", "no-such-agent", result.Secret},
		{"凭据为空", result.AgentID, ""},
		{"id 与凭据都为空", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := defaultRegistry.authenticate(tc.id, tc.secret)
			if err != nil {
				t.Fatalf("鉴权失败不应返回内部错误: %v", err)
			}
			if rec != nil {
				t.Fatalf("非法凭据不应通过鉴权: %+v", rec)
			}
		})
	}
}

func TestHeartbeat_MemoryOnly(t *testing.T) {
	basePath := setupStateDir(t)
	setupAgentConfig(t, true)

	result, err := defaultRegistry.enroll(testEnrollParams("machine-1"))
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	before, err := os.ReadFile(agentsFilePath(basePath))
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}

	now := time.Now()
	rec, err := defaultRegistry.touch(result.AgentID, heartbeatParams{
		LastIP:        "10.0.0.7",
		ActiveStreams: 2,
		Version:       "v1.2.3",
		ListenPort:    8890,
		PublicBaseURL: "http://node.example.com:8890",
		Now:           now,
	})
	if err != nil {
		t.Fatalf("心跳失败: %v", err)
	}
	if rec == nil {
		t.Fatal("已注册节点的心跳应返回记录")
	}
	if !rec.LastSeenAt.Equal(now) || rec.ActiveStreams != 2 {
		t.Errorf("心跳未更新运行时状态: %+v", rec)
	}
	if rec.LastIP != "10.0.0.7" || rec.Version != "v1.2.3" || rec.ListenPort != 8890 {
		t.Errorf("心跳未更新节点信息: %+v", rec)
	}

	after, err := os.ReadFile(agentsFilePath(basePath))
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	if string(before) != string(after) {
		t.Error("心跳不应触发落盘(每 15 秒一次的写盘既无必要也磨损磁盘)")
	}

	// 进程重启后运行时状态归零, 节点需要重新心跳
	simulateRestart()
	list, err := defaultRegistry.snapshot()
	if err != nil {
		t.Fatalf("重启后加载注册表失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("重启后应保留注册记录, 实际 %d 条", len(list))
	}
	if !list[0].LastSeenAt.IsZero() || list[0].ActiveStreams != 0 {
		t.Errorf("重启后易失状态应归零: %+v", list[0])
	}
	if list[0].Secret != result.Secret {
		t.Error("重启后凭据应保持不变(节点无需重新注册)")
	}
}

func TestHeartbeat_EmptyFieldsDoNotOverride(t *testing.T) {
	setupStateDir(t)
	setupAgentConfig(t, true)

	result, err := defaultRegistry.enroll(testEnrollParams("machine-1"))
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	params := testEnrollParams("machine-1")
	params.PublicBaseURL = "http://node.example.com:8790"
	result2, err := defaultRegistry.enroll(params)
	if err != nil {
		t.Fatalf("重复注册失败: %v", err)
	}
	_ = result2

	// agent 侧字段可空: 一次"没带"不能把已知信息清空
	if _, err := defaultRegistry.touch(result.AgentID, heartbeatParams{Now: time.Now()}); err != nil {
		t.Fatalf("心跳失败: %v", err)
	}
	list, err := defaultRegistry.snapshot()
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	rec := list[0]
	if rec.PublicBaseURL != "http://node.example.com:8790" {
		t.Errorf("空 public_base_url 不应覆盖已有值: %q", rec.PublicBaseURL)
	}
	if rec.ListenPort != 8790 || rec.LastIP != "192.168.1.10" || rec.Version != "v1.0.0" {
		t.Errorf("空字段不应覆盖已有值: %+v", rec)
	}
	if rec.LastSeenAt.IsZero() {
		t.Error("心跳本身必须刷新 LastSeenAt")
	}
}

func TestSetEnabledAndRemove(t *testing.T) {
	basePath := setupStateDir(t)
	setupAgentConfig(t, true)

	result, err := defaultRegistry.enroll(testEnrollParams("machine-1"))
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	if _, err := defaultRegistry.setEnabled(result.AgentID, false, time.Now()); err != nil {
		t.Fatalf("禁用失败: %v", err)
	}
	data, _ := os.ReadFile(agentsFilePath(basePath))
	if !strings.Contains(string(data), `"enabled": false`) {
		t.Error("禁用状态应落盘")
	}

	if _, err := defaultRegistry.setEnabled("no-such-agent", true, time.Now()); err != errAgentNotFound {
		t.Errorf("对不存在的节点操作应返回节点不存在: %v", err)
	}

	if _, err := defaultRegistry.remove(result.AgentID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	list, err := defaultRegistry.snapshot()
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("删除后注册表应为空: %+v", list)
	}
	data, _ = os.ReadFile(agentsFilePath(basePath))
	if strings.Contains(string(data), result.AgentID) {
		t.Error("删除应落盘")
	}

	if _, err := defaultRegistry.remove(result.AgentID); err != errAgentNotFound {
		t.Errorf("重复删除应返回节点不存在: %v", err)
	}
}

// TestSetProfile_PersistsAndReloads 节点资料(名称 + 优先级)的落盘 / 重载
func TestSetProfile_PersistsAndReloads(t *testing.T) {
	basePath := setupStateDir(t)
	setupAgentConfig(t, true)

	result, err := defaultRegistry.enroll(testEnrollParams("machine-1"))
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	rec, err := defaultRegistry.setProfile(result.AgentID, "家人云", 7, time.Now())
	if err != nil {
		t.Fatalf("更新节点资料失败: %v", err)
	}
	if rec.Name != "家人云" || rec.Priority != 7 {
		t.Fatalf("更新结果不正确: %+v", rec)
	}

	// 落盘: 名称与优先级都要写进 agents.json
	data, err := os.ReadFile(agentsFilePath(basePath))
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	if !strings.Contains(string(data), `"name": "家人云"`) || !strings.Contains(string(data), `"priority": 7`) {
		t.Errorf("节点资料应落盘: %s", data)
	}

	// 重启后从磁盘重新加载: 名称与优先级均保留
	simulateRestart()
	list, err := defaultRegistry.snapshot()
	if err != nil {
		t.Fatalf("重启后加载注册表失败: %v", err)
	}
	if len(list) != 1 || list[0].Name != "家人云" || list[0].Priority != 7 {
		t.Fatalf("重启后节点资料应保留: %+v", list)
	}

	if _, err := defaultRegistry.setProfile("no-such-agent", "x", 1, time.Now()); err != errAgentNotFound {
		t.Errorf("对不存在的节点更新资料应返回节点不存在: %v", err)
	}
}

// TestLoadAgentsFile_PriorityBackwardCompatible 落盘结构的优先级向后兼容
//
// 旧版 agents.json 没有 priority 字段: 缺省即为 0(默认值 = 最优先),
// 不需要指针或升级 schema 版本; 有值的文件必须原样加载。
func TestLoadAgentsFile_PriorityBackwardCompatible(t *testing.T) {
	old := `{"version":1,"agents":[{"id":"a1","machine_id":"m1","name":"旧节点","secret":"s1","sign_key":"k1","listen_port":8790,"enabled":true,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`
	newer := `{"version":1,"agents":[{"id":"a1","machine_id":"m1","name":"新节点","secret":"s1","sign_key":"k1","listen_port":8790,"priority":42,"enabled":true,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`

	cases := []struct {
		name         string
		content      string
		wantName     string
		wantPriority int
	}{
		{"旧文件无 priority 字段", old, "旧节点", 0},
		{"新文件带 priority", newer, "新节点", 42},
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
			if rec.Priority != tc.wantPriority {
				t.Errorf("Priority = %d, want %d", rec.Priority, tc.wantPriority)
			}
			if rec.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", rec.Name, tc.wantName)
			}
		})
	}
}

// TestEnroll_NewRecordDefaults 全新记录的初始资料
//
// 名称取上报主机名, 优先级为 0(未设置 = 最优先)。
func TestEnroll_NewRecordDefaults(t *testing.T) {
	setupStateDir(t)
	setupAgentConfig(t, true)
	simulateRestart()

	result, err := defaultRegistry.enroll(testEnrollParams("machine-1"))
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	list, err := defaultRegistry.snapshot()
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("注册后应有 1 条记录, 实际 %d", len(list))
	}
	if list[0].Name != "node-machine-1" {
		t.Errorf("新记录的名称应取上报主机名, 实际 %q", list[0].Name)
	}
	if list[0].Priority != 0 {
		t.Errorf("新记录的优先级应为 0, 实际 %d", list[0].Priority)
	}
	if list[0].ID != result.AgentID {
		t.Errorf("返回的 id 与记录不一致: %s != %s", result.AgentID, list[0].ID)
	}
}

func TestMutate_PersistFailureRollsBack(t *testing.T) {
	basePath := setupStateDir(t)
	setupAgentConfig(t, true)

	result, err := defaultRegistry.enroll(testEnrollParams("machine-1"))
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	// 把状态目录换成一个同名文件: MkdirAll 必然失败, 模拟磁盘故障
	stateDir := filepath.Join(basePath, DirName)
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatalf("清理状态目录失败: %v", err)
	}
	if err := os.WriteFile(stateDir, []byte("blocked"), 0o600); err != nil {
		t.Fatalf("构造故障场景失败: %v", err)
	}

	if _, err := defaultRegistry.setEnabled(result.AgentID, false, time.Now()); err == nil {
		t.Fatal("落盘失败时应返回错误")
	}
	if _, err := defaultRegistry.setProfile(result.AgentID, "改名", 5, time.Now()); err == nil {
		t.Fatal("落盘失败时更新资料应返回错误")
	}

	// 内存态必须回滚: 不能出现"界面显示禁用成功, 重启后又是启用"
	list, err := defaultRegistry.snapshot()
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	if len(list) != 1 || !list[0].Enabled {
		t.Errorf("落盘失败后内存变更应回滚: %+v", list)
	}
	if list[0].Name == "改名" || list[0].Priority != 0 {
		t.Errorf("落盘失败后节点资料变更应回滚: %+v", list[0])
	}
}

func TestLoadAgentsFile_Corruption(t *testing.T) {
	valid := `{"version":1,"agents":[{"id":"a1","machine_id":"m1","name":"n","secret":"s1","sign_key":"k1","listen_port":8790,"enabled":true,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`

	cases := []struct {
		name    string
		content string
		wantSub string
	}{
		{"不是 JSON", "{not json", "解析 agent 注册表"},
		{"版本号不支持", `{"version":2,"agents":[]}`, "版本号 2 不受支持"},
		{"缺少版本号", `{"agents":[]}`, "版本号 0 不受支持"},
		{"记录缺少凭据", `{"version":1,"agents":[{"id":"a1","machine_id":"m1"}]}`, "第 1 条记录字段不完整"},
		{
			"节点 id 重复",
			`{"version":1,"agents":[
				{"id":"a1","machine_id":"m1","secret":"s","sign_key":"k"},
				{"id":"a1","machine_id":"m2","secret":"s","sign_key":"k"}]}`,
			"重复的节点 id",
		},
		{
			"machine_id 重复",
			`{"version":1,"agents":[
				{"id":"a1","machine_id":"m1","secret":"s","sign_key":"k"},
				{"id":"a2","machine_id":"m1","secret":"s","sign_key":"k"}]}`,
			"重复的 machine_id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), fileName)
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("写入损坏文件失败: %v", err)
			}
			if _, err := loadAgentsFile(path); err == nil {
				t.Fatal("损坏的注册表应加载失败")
			} else if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误消息 %q 应包含 %q", err.Error(), tc.wantSub)
			}
		})
	}

	// 正常内容必须能加载
	path := filepath.Join(t.TempDir(), fileName)
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}
	records, err := loadAgentsFile(path)
	if err != nil {
		t.Fatalf("合法注册表应加载成功: %v", err)
	}
	if len(records) != 1 || records["a1"].MachineID != "m1" {
		t.Fatalf("加载结果不正确: %+v", records)
	}

	// 文件不存在 = 首次启动
	missing, err := loadAgentsFile(filepath.Join(t.TempDir(), "missing", fileName))
	if err != nil {
		t.Fatalf("文件不存在不应视为错误: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("文件不存在应返回空表: %+v", missing)
	}
}

func TestInit_FailsOnCorruptRegistry(t *testing.T) {
	basePath := setupStateDir(t)
	setupAgentConfig(t, true)
	simulateRestart()

	if err := os.MkdirAll(filepath.Join(basePath, DirName), 0o755); err != nil {
		t.Fatalf("创建状态目录失败: %v", err)
	}
	if err := os.WriteFile(agentsFilePath(basePath), []byte("{损坏"), 0o600); err != nil {
		t.Fatalf("写入损坏文件失败: %v", err)
	}

	err := Init()
	if err == nil {
		t.Fatal("注册表损坏时启动必须失败, 不能静默清空")
	}
	if !strings.Contains(err.Error(), "删除后重启") {
		t.Errorf("错误消息应给出处置建议, 实际: %q", err.Error())
	}
}

func TestInit_DisabledDoesNotTouchDisk(t *testing.T) {
	// 状态目录指向一个不可创建的路径: 未启用时 Init 不应触碰磁盘
	oldBasePath := config.BasePath
	config.BasePath = filepath.Join(t.TempDir(), "no-such-dir")
	t.Cleanup(func() { config.BasePath = oldBasePath })

	setupAgentConfig(t, false)
	simulateRestart()

	if err := Init(); err != nil {
		t.Fatalf("未启用时 Init 不应读取磁盘, 实际错误: %v", err)
	}
}

func TestEnsureLoaded_RepathsOnBasePathChange(t *testing.T) {
	first := setupStateDir(t)
	setupAgentConfig(t, true)

	if _, err := defaultRegistry.enroll(testEnrollParams("machine-1")); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	// 换一个状态目录(等价于测试之间互相隔离): 旧记录不应泄漏到新目录
	second := t.TempDir()
	config.BasePath = second

	list, err := defaultRegistry.snapshot()
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("切换状态目录后应重新加载为空表: %+v", list)
	}

	// 切回原目录: 磁盘上的记录仍在
	config.BasePath = first
	list, err = defaultRegistry.snapshot()
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("切回原目录应加载到磁盘上的记录: %+v", list)
	}
}

func TestStateFilePath_RequiresBasePath(t *testing.T) {
	oldBasePath := config.BasePath
	config.BasePath = ""
	t.Cleanup(func() { config.BasePath = oldBasePath })

	if _, err := stateFilePath(); err == nil {
		t.Fatal("BasePath 未初始化时应返回错误, 不能把注册表写到当前目录")
	}
}

func TestRegistry_ConcurrentAccess(t *testing.T) {
	setupStateDir(t)
	setupAgentConfig(t, true)

	result, err := defaultRegistry.enroll(testEnrollParams("machine-1"))
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				switch i % 4 {
				case 0:
					_, _ = defaultRegistry.enroll(testEnrollParams("machine-" + string(rune('a'+i))))
				case 1:
					_, _ = defaultRegistry.touch(result.AgentID, heartbeatParams{LastIP: "10.0.0.1", ActiveStreams: j, Now: time.Now()})
				case 2:
					_, _ = defaultRegistry.schedule(time.Now(), 45*time.Second, config.ScheduleStrategyLeastActive)
				default:
					_, _ = defaultRegistry.snapshot()
				}
			}
		}(i)
	}
	wg.Wait()

	// 所有写入都必须已经落盘
	list, err := defaultRegistry.snapshot()
	if err != nil {
		t.Fatalf("读取注册表失败: %v", err)
	}
	reloaded, err := loadAgentsFile(agentsFilePath(config.BasePath))
	if err != nil {
		t.Fatalf("读取落盘注册表失败: %v", err)
	}
	if len(reloaded) != len(list) {
		t.Errorf("内存态 %d 条与磁盘 %d 条不一致", len(list), len(reloaded))
	}
}
