package agentnet

import (
	"strings"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
)

// seedRecord 直接往注册表里塞一条记录(绕过注册流程, 便于构造调度场景)
func seedRecord(t *testing.T, rec *agentRecord) *agentRecord {
	t.Helper()

	if err := defaultRegistry.ensureLoaded(); err != nil {
		t.Fatalf("加载注册表失败: %v", err)
	}
	if rec.SignKey == "" {
		rec.SignKey = masterSignKeyHex
	}
	if rec.Secret == "" {
		rec.Secret = strings.Repeat("a", credentialBytes*2)
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	defaultRegistry.mu.Lock()
	defer defaultRegistry.mu.Unlock()
	if rec.ID == "" {
		rec.ID = "agent-" + rec.MachineID
	}
	defaultRegistry.records[rec.ID] = rec
	defaultRegistry.byMachine[rec.MachineID] = rec.ID
	return rec
}

func TestSchedule_CandidateFiltering(t *testing.T) {
	setupStateDir(t)
	setupAgentConfig(t, true)
	simulateRestart()

	now := time.Now()
	seedRecord(t, &agentRecord{
		MachineID: "online", Name: "online", LastIP: "10.0.0.1", ListenPort: 8790,
		Enabled: true, LastSeenAt: now,
	})
	seedRecord(t, &agentRecord{
		MachineID: "disabled", Name: "disabled", LastIP: "10.0.0.2", ListenPort: 8790,
		Enabled: false, LastSeenAt: now,
	})
	seedRecord(t, &agentRecord{
		MachineID: "stale", Name: "stale", LastIP: "10.0.0.3", ListenPort: 8790,
		Enabled: true, LastSeenAt: now.Add(-46 * time.Second),
	})
	seedRecord(t, &agentRecord{
		MachineID: "never", Name: "never", LastIP: "10.0.0.4", ListenPort: 8790,
		Enabled: true,
	})
	seedRecord(t, &agentRecord{
		MachineID: "no-addr", Name: "no-addr",
		Enabled: true, LastSeenAt: now,
	})

	rec, err := defaultRegistry.schedule(now, 45*time.Second, config.ScheduleStrategyLeastActive)
	if err != nil {
		t.Fatalf("调度失败: %v", err)
	}
	if rec == nil || rec.MachineID != "online" {
		t.Fatalf("应只选中在线且启用的节点, 实际: %+v", rec)
	}
}

func TestSchedule_OfflineBoundary(t *testing.T) {
	setupStateDir(t)
	setupAgentConfig(t, true)
	simulateRestart()

	now := time.Now()
	// 恰好等于窗口边界: 仍算在线(判定条件是 "超过窗口才算离线")
	seedRecord(t, &agentRecord{
		MachineID: "boundary", Name: "boundary", LastIP: "10.0.0.1", ListenPort: 8790,
		Enabled: true, LastSeenAt: now.Add(-45 * time.Second),
	})

	if rec, err := defaultRegistry.schedule(now, 45*time.Second, config.ScheduleStrategyLeastActive); err != nil || rec == nil {
		t.Fatalf("恰好落在窗口边界上的节点应仍可用: rec=%+v err=%v", rec, err)
	}
}

func TestSchedule_LeastActiveStreamsAndTieBreak(t *testing.T) {
	setupStateDir(t)
	setupAgentConfig(t, true)
	simulateRestart()

	now := time.Now()
	seedRecord(t, &agentRecord{
		MachineID: "busy", Name: "busy", LastIP: "10.0.0.1", ListenPort: 8790,
		Enabled: true, LastSeenAt: now, ActiveStreams: 5,
	})
	seedRecord(t, &agentRecord{
		MachineID: "idle", Name: "idle", LastIP: "10.0.0.2", ListenPort: 8790,
		Enabled: true, LastSeenAt: now, ActiveStreams: 0,
	})
	seedRecord(t, &agentRecord{
		MachineID: "mid", Name: "mid", LastIP: "10.0.0.3", ListenPort: 8790,
		Enabled: true, LastSeenAt: now, ActiveStreams: 2,
	})

	for i := 0; i < 5; i++ {
		rec, err := defaultRegistry.schedule(now, 45*time.Second, config.ScheduleStrategyLeastActive)
		if err != nil {
			t.Fatalf("调度失败: %v", err)
		}
		if rec.MachineID != "idle" {
			t.Fatalf("应选中活跃流最少的节点, 实际: %s", rec.MachineID)
		}
	}

	// 平局: 只会在并列集合内随机, 不会选中负载更高的节点
	seedRecord(t, &agentRecord{
		MachineID: "tie-a", Name: "tie-a", LastIP: "10.0.0.4", ListenPort: 8790,
		Enabled: true, LastSeenAt: now, ActiveStreams: 0,
	})
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		rec, err := defaultRegistry.schedule(now, 45*time.Second, config.ScheduleStrategyLeastActive)
		if err != nil {
			t.Fatalf("调度失败: %v", err)
		}
		if rec.ActiveStreams != 0 {
			t.Fatalf("平局随机不应越出并列集合: %+v", rec)
		}
		seen[rec.MachineID] = true
	}
	if !seen["idle"] || !seen["tie-a"] {
		t.Errorf("平局节点都应有机会被选中(200 次抽样), 实际命中: %v", seen)
	}
}

func TestSchedule_NoCandidates(t *testing.T) {
	setupStateDir(t)
	setupAgentConfig(t, true)
	simulateRestart()

	rec, err := defaultRegistry.schedule(time.Now(), 45*time.Second, config.ScheduleStrategyLeastActive)
	if err != nil {
		t.Fatalf("没有候选不应是错误: %v", err)
	}
	if rec != nil {
		t.Fatalf("没有候选时应返回 nil, 实际: %+v", rec)
	}
}

// TestSchedule_PriorityStrategy priority 策略: 优先级数字最小者胜出, 忽略活跃流
//
// 覆盖 N2/N3: 最高优先级的在线节点承接全部新播放, 哪怕它已很忙、
// 其它节点完全空闲; 未设置优先级(0)的节点视为最优先。
func TestSchedule_PriorityStrategy(t *testing.T) {
	setupStateDir(t)
	setupAgentConfig(t, true)
	simulateRestart()

	now := time.Now()
	// 忙但优先级最高(未设置 = 0): 必须仍然由它承接
	seedRecord(t, &agentRecord{
		MachineID: "primary", Name: "primary", LastIP: "10.0.0.1", ListenPort: 8790,
		Enabled: true, LastSeenAt: now, ActiveStreams: 9,
	})
	seedRecord(t, &agentRecord{
		MachineID: "backup", Name: "backup", LastIP: "10.0.0.2", ListenPort: 8790,
		Enabled: true, LastSeenAt: now, ActiveStreams: 0, Priority: 5,
	})
	seedRecord(t, &agentRecord{
		MachineID: "spare", Name: "spare", LastIP: "10.0.0.3", ListenPort: 8790,
		Enabled: true, LastSeenAt: now, ActiveStreams: 0, Priority: 9,
	})

	for i := 0; i < 200; i++ {
		rec, err := defaultRegistry.schedule(now, 45*time.Second, config.ScheduleStrategyPriority)
		if err != nil {
			t.Fatalf("调度失败: %v", err)
		}
		if rec.MachineID != "primary" {
			t.Fatalf("priority 策略应始终选中优先级最小的节点(忽略活跃流), 实际: %s(优先级 %d)",
				rec.MachineID, rec.Priority)
		}
	}
}

// TestSchedule_PriorityTieBreak 同优先级平局: 在并列集合内随机
func TestSchedule_PriorityTieBreak(t *testing.T) {
	setupStateDir(t)
	setupAgentConfig(t, true)
	simulateRestart()

	now := time.Now()
	seedRecord(t, &agentRecord{
		MachineID: "tie-a", Name: "tie-a", LastIP: "10.0.0.1", ListenPort: 8790,
		Enabled: true, LastSeenAt: now, ActiveStreams: 0, Priority: 3,
	})
	seedRecord(t, &agentRecord{
		MachineID: "tie-b", Name: "tie-b", LastIP: "10.0.0.2", ListenPort: 8790,
		Enabled: true, LastSeenAt: now, ActiveStreams: 7, Priority: 3,
	})
	seedRecord(t, &agentRecord{
		MachineID: "worse", Name: "worse", LastIP: "10.0.0.3", ListenPort: 8790,
		Enabled: true, LastSeenAt: now, ActiveStreams: 0, Priority: 4,
	})

	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		rec, err := defaultRegistry.schedule(now, 45*time.Second, config.ScheduleStrategyPriority)
		if err != nil {
			t.Fatalf("调度失败: %v", err)
		}
		if rec.Priority != 3 {
			t.Fatalf("平局随机不应越出并列集合(优先级 3): %+v", rec)
		}
		seen[rec.MachineID] = true
	}
	if !seen["tie-a"] || !seen["tie-b"] {
		t.Errorf("同优先级节点都应有机会被选中(200 次抽样), 实际命中: %v", seen)
	}
}

// TestSchedule_PriorityPushDownAndRecovery 心跳消失下推, 恢复回归
//
// 覆盖 N4/N5: 仅当高优先级节点的心跳超出 offline 窗口才顺位下推;
// 心跳恢复后, 新播放重新回到最高优先级节点。
func TestSchedule_PriorityPushDownAndRecovery(t *testing.T) {
	setupStateDir(t)
	setupAgentConfig(t, true)
	simulateRestart()

	now := time.Now()
	primary := seedRecord(t, &agentRecord{
		MachineID: "primary", Name: "primary", LastIP: "10.0.0.1", ListenPort: 8790,
		Enabled: true, LastSeenAt: now, Priority: 0,
	})
	seedRecord(t, &agentRecord{
		MachineID: "backup", Name: "backup", LastIP: "10.0.0.2", ListenPort: 8790,
		Enabled: true, LastSeenAt: now, Priority: 1,
	})

	pick := func() string {
		t.Helper()
		rec, err := defaultRegistry.schedule(now, 45*time.Second, config.ScheduleStrategyPriority)
		if err != nil {
			t.Fatalf("调度失败: %v", err)
		}
		if rec == nil {
			t.Fatal("应有可调度节点")
		}
		return rec.MachineID
	}

	if got := pick(); got != "primary" {
		t.Fatalf("首选应为最高优先级节点, 实际: %s", got)
	}

	// 顶层节点心跳消失(超过 offline-seconds): 顺位下推到第二优先级
	primary.LastSeenAt = now.Add(-46 * time.Second)
	if got := pick(); got != "backup" {
		t.Fatalf("高优先级节点心跳消失后应顺位下推, 实际: %s", got)
	}

	// 心跳恢复: 新播放重新回到最高优先级节点(已播会话不迁移)
	primary.LastSeenAt = now
	if got := pick(); got != "primary" {
		t.Fatalf("高优先级节点恢复心跳后应重新被选中, 实际: %s", got)
	}
}

// TestSchedule_PrioritySkipsDisabled 禁用节点在 priority 策略下不参与调度
//
// 覆盖 N7: 禁用与"心跳消失"是两回事 —— 禁用是管理员的决定, 永不出池。
func TestSchedule_PrioritySkipsDisabled(t *testing.T) {
	setupStateDir(t)
	setupAgentConfig(t, true)
	simulateRestart()

	now := time.Now()
	seedRecord(t, &agentRecord{
		MachineID: "disabled-top", Name: "disabled-top", LastIP: "10.0.0.1", ListenPort: 8790,
		Enabled: false, LastSeenAt: now, Priority: 0,
	})
	seedRecord(t, &agentRecord{
		MachineID: "backup", Name: "backup", LastIP: "10.0.0.2", ListenPort: 8790,
		Enabled: true, LastSeenAt: now, Priority: 7,
	})

	for i := 0; i < 20; i++ {
		rec, err := defaultRegistry.schedule(now, 45*time.Second, config.ScheduleStrategyPriority)
		if err != nil {
			t.Fatalf("调度失败: %v", err)
		}
		if rec == nil || rec.MachineID != "backup" {
			t.Fatalf("被禁用的节点不应参与调度, 实际: %+v", rec)
		}
	}
}

// TestSchedule_LeastActiveIgnoresPriority least-active 策略不读取优先级(回归)
//
// A3: 缺省策略下行为与历史一致 —— 优先级只是给 priority 策略用的字段,
// 不能悄悄改变 least-active 的选点结果。
func TestSchedule_LeastActiveIgnoresPriority(t *testing.T) {
	setupStateDir(t)
	setupAgentConfig(t, true)
	simulateRestart()

	now := time.Now()
	seedRecord(t, &agentRecord{
		MachineID: "busy-top", Name: "busy-top", LastIP: "10.0.0.1", ListenPort: 8790,
		Enabled: true, LastSeenAt: now, ActiveStreams: 5, Priority: 0,
	})
	seedRecord(t, &agentRecord{
		MachineID: "idle-low", Name: "idle-low", LastIP: "10.0.0.2", ListenPort: 8790,
		Enabled: true, LastSeenAt: now, ActiveStreams: 1, Priority: 99,
	})

	for i := 0; i < 20; i++ {
		rec, err := defaultRegistry.schedule(now, 45*time.Second, config.ScheduleStrategyLeastActive)
		if err != nil {
			t.Fatalf("调度失败: %v", err)
		}
		if rec == nil || rec.MachineID != "idle-low" {
			t.Fatalf("least-active 应按活跃流选点, 忽略优先级, 实际: %+v", rec)
		}
	}
}

func TestAgentBaseURL(t *testing.T) {
	cases := []struct {
		name string
		rec  *agentRecord
		want string
	}{
		{
			"public_base_url 优先(去尾部斜杠)",
			&agentRecord{PublicBaseURL: "https://node.example.com/", LastIP: "10.0.0.1", ListenPort: 9999},
			"https://node.example.com",
		},
		{
			"按来源 IP 拼默认端口",
			&agentRecord{LastIP: "10.0.0.1", ListenPort: 8790},
			"http://10.0.0.1:8790",
		},
		{
			"端口越界时回退默认端口",
			&agentRecord{LastIP: "10.0.0.1", ListenPort: 70000},
			"http://10.0.0.1:8790",
		},
		{
			"IPv6 来源地址补方括号",
			&agentRecord{LastIP: "2001:db8::1", ListenPort: 8790},
			"http://[2001:db8::1]:8790",
		},
		{
			"v6 public_base_url 原样使用(带端口)",
			&agentRecord{PublicBaseURL: "http://[2408:8207:1234::5]:8790", LastIP: "10.0.0.1", ListenPort: 9999},
			"http://[2408:8207:1234::5]:8790",
		},
		{
			"v6 public_base_url 无端口也可用",
			&agentRecord{PublicBaseURL: "https://[2001:db8::1]"},
			"https://[2001:db8::1]",
		},
		{
			"IPv6 来源地址 + 监听端口 0(回落默认端口)",
			&agentRecord{LastIP: "::1"},
			"http://[::1]:8790",
		},
		{
			"public_base_url 非法时退回 IP 推导",
			&agentRecord{PublicBaseURL: "10.0.0.1:8790", LastIP: "10.0.0.2", ListenPort: 8790},
			"http://10.0.0.2:8790",
		},
		{
			"两者都拿不到",
			&agentRecord{},
			"",
		},
		{
			"空记录",
			nil,
			"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentBaseURL(tc.rec); got != tc.want {
				t.Errorf("agentBaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParsePublicBaseURL(t *testing.T) {
	valid := []struct {
		raw  string
		want string
	}{
		{"  http://node.example.com:8790  ", "http://node.example.com:8790"},
		{"https://node.example.com/", "https://node.example.com"},
		{"http://192.168.1.5:8790", "http://192.168.1.5:8790"},
		// IPv6 字面量: 节点用 --public-url 'http://[v6]:8790' 上报, master 原样保留
		// (含方括号), 拿去拼客户端地址时才是合法 URL。
		{"http://[2408:8207:1234::5]:8790", "http://[2408:8207:1234::5]:8790"},
		{"http://[2001:db8::1]:8790/", "http://[2001:db8::1]:8790"},
		{"https://[2001:db8::1]", "https://[2001:db8::1]"},
		{"http://[::1]:8790", "http://[::1]:8790"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range valid {
		got, err := parsePublicBaseURL(tc.raw)
		if err != nil {
			t.Errorf("parsePublicBaseURL(%q) 返回错误: %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parsePublicBaseURL(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}

	invalid := []struct {
		raw     string
		wantSub string
	}{
		{"10.0.0.1:8790", "缺少 http:// 或 https:// 前缀"},
		{"node.example.com", "缺少 http:// 或 https:// 前缀"},
		{"ftp://node.example.com", "只支持 http/https"},
		{"http://", "缺少主机名"},
		{"http://user:pass@node.example.com", "不能包含用户名或密码"},
		{"http://node.example.com?a=1", "不能包含查询参数或片段"},
		{"http://node.example.com#frag", "不能包含查询参数或片段"},
		// IPv6 非法形态: 方括号未闭合连 url.Parse 都过不去(这是 master 能挡住的
		// 一类手误; 未加方括号的裸 v6 字面量 url.Parse 会放行, 属已知边界)。
		{"http://[2001:db8::1", "不是合法的地址"},
	}
	for _, tc := range invalid {
		_, err := parsePublicBaseURL(tc.raw)
		if err == nil {
			t.Errorf("parsePublicBaseURL(%q) 应返回错误", tc.raw)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("parsePublicBaseURL(%q) 错误消息 %q 应包含 %q", tc.raw, err.Error(), tc.wantSub)
		}
	}
}
