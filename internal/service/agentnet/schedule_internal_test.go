package agentnet

import (
	"strings"
	"testing"
	"time"
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

	rec, err := defaultRegistry.schedule(now, 45*time.Second)
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

	if rec, err := defaultRegistry.schedule(now, 45*time.Second); err != nil || rec == nil {
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
		rec, err := defaultRegistry.schedule(now, 45*time.Second)
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
		rec, err := defaultRegistry.schedule(now, 45*time.Second)
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

	rec, err := defaultRegistry.schedule(time.Now(), 45*time.Second)
	if err != nil {
		t.Fatalf("没有候选不应是错误: %v", err)
	}
	if rec != nil {
		t.Fatalf("没有候选时应返回 nil, 实际: %+v", rec)
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
