package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"

	"gopkg.in/yaml.v3"
)

// agentEnrollToken 测试用注册 Token
//
// 单独抽成常量, 便于逐处断言它不会出现在错误消息里。
const agentEnrollToken = "test-enroll-token-0123456789"

// clearAgentEnrollTokenEnv 清空注册 Token 环境变量
//
// 保证"期望配置报错""期望用配置值"这类用例不受运行环境里已导出的
// AGENT_ENROLL_TOKEN 影响 —— 否则同一份测试在开发机与 CI 上结论可能不同。
func clearAgentEnrollTokenEnv(t *testing.T) {
	t.Helper()
	t.Setenv(config.AgentEnrollTokenEnvName, "")
}

// loadAgentNetworkConfig 解析 yaml 并完成 agent-network 配置初始化
func loadAgentNetworkConfig(t *testing.T, raw string) (*config.AgentNetwork, error) {
	t.Helper()

	cfg := new(config.Config)
	if err := yaml.Unmarshal([]byte(raw), cfg); err != nil {
		t.Fatalf("解析 yaml 失败: %v", err)
	}
	if cfg.AgentNetwork == nil {
		cfg.AgentNetwork = new(config.AgentNetwork)
	}
	return cfg.AgentNetwork, cfg.AgentNetwork.Init()
}

// agentNetworkConfigEnabled 生成一份启用且合法的 agent-network 配置
//
// extra 的每一行需要缩进 2 个空格。
func agentNetworkConfigEnabled(extra string) string {
	return "agent-network:\n" +
		"  enable: true\n" +
		"  enroll-token: " + agentEnrollToken + "\n" + extra
}

func TestAgentNetworkInit_Enabled(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	agent, err := loadAgentNetworkConfig(t, agentNetworkConfigEnabled(""))
	if err != nil {
		t.Fatalf("配置初始化返回错误: %v", err)
	}

	if !agent.IsEnabled() {
		t.Error("enable 为 true 时应处于启用状态")
	}
	if agent.EnrollToken != agentEnrollToken {
		t.Errorf("enroll-token = %q, want %q", agent.EnrollToken, agentEnrollToken)
	}
}

func TestAgentNetworkInit_AbsentSection(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	agent, err := loadAgentNetworkConfig(t, "emby:\n  host: http://emby.local:8096\n")
	if err != nil {
		t.Fatalf("缺省 agent-network 段时应能正常初始化, 实际错误: %v", err)
	}
	if agent.IsEnabled() {
		t.Error("缺省 agent-network 段时不应启用")
	}
	if agent.EnrollToken != "" {
		t.Errorf("缺省 agent-network 段时 enroll-token 应为空, 实际: %q", agent.EnrollToken)
	}
	// 默认值的填充与 enable 无关: 之后打开开关不需要再补一次配置
	if agent.OfflineSeconds != 45 {
		t.Errorf("缺省 agent-network 段时 offline-seconds 应取默认值 45, 实际: %d", agent.OfflineSeconds)
	}
	if got := agent.ClientURLTTL(); got != 24*time.Hour {
		t.Errorf("缺省 agent-network 段时 url-ttl 应取默认值 24h, 实际: %v", got)
	}
	if !agent.FallbackEnabled() {
		t.Error("缺省 agent-network 段时 fallback-to-local 应默认为 true")
	}
	if got := agent.ScheduleStrategy(); got != config.ScheduleStrategyLeastActive {
		t.Errorf("缺省 agent-network 段时 schedule-strategy 应取默认值 %q, 实际: %q",
			config.ScheduleStrategyLeastActive, got)
	}
}

func TestAgentNetworkInit_TrimsFields(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	agent, err := loadAgentNetworkConfig(t,
		"agent-network:\n"+
			"  enable: true\n"+
			"  enroll-token: \"  "+agentEnrollToken+"  \"\n"+
			"  url-ttl: \"  12h  \"\n")
	if err != nil {
		t.Fatalf("配置初始化返回错误: %v", err)
	}
	if agent.EnrollToken != agentEnrollToken {
		t.Errorf("enroll-token 未去除首尾空白: %q", agent.EnrollToken)
	}
	if got := agent.ClientURLTTL(); got != 12*time.Hour {
		t.Errorf("url-ttl = %v, want 12h", got)
	}
}

func TestAgentNetworkInit_Defaults(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	agent, err := loadAgentNetworkConfig(t, "agent-network:\n  enable: true\n  enroll-token: "+agentEnrollToken+"\n")
	if err != nil {
		t.Fatalf("配置初始化返回错误: %v", err)
	}
	if agent.OfflineSeconds != 45 {
		t.Errorf("offline-seconds 缺省值 = %d, want 45", agent.OfflineSeconds)
	}
	if got := agent.ClientURLTTL(); got != 24*time.Hour {
		t.Errorf("url-ttl 缺省值 = %v, want 24h", got)
	}
	if !agent.FallbackEnabled() {
		t.Error("fallback-to-local 缺省值应为 true")
	}
	if got := agent.ScheduleStrategy(); got != config.ScheduleStrategyLeastActive {
		t.Errorf("schedule-strategy 缺省值 = %q, want %q", got, config.ScheduleStrategyLeastActive)
	}
}

// TestAgentNetworkInit_PreheatEnable 预热开关的解析与缺省
//
// 本用例同时钉住 UnmarshalYAML 的显式字段清单:
// preheat-enable 若漏加进 plainAgentNetwork, 会被静默丢弃 —— 用户在配置里
// 明确写下的 false 不生效, 预热照常触发且没有任何报错。
func TestAgentNetworkInit_PreheatEnable(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"显式 false", agentNetworkConfigEnabled("  preheat-enable: false\n"), false},
		{"显式 true", agentNetworkConfigEnabled("  preheat-enable: true\n"), true},
		{"缺省取 true", agentNetworkConfigEnabled(""), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent, err := loadAgentNetworkConfig(t, tc.raw)
			if err != nil {
				t.Fatalf("配置初始化返回错误: %v", err)
			}
			if got := agent.PreheatEnabled(); got != tc.want {
				t.Errorf("PreheatEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAgentNetworkInit_ScheduleStrategy 调度策略的解析 / 缺省 / 校验
//
// 本用例同时钉住 UnmarshalYAML 的显式字段清单:
// schedule-strategy 若漏加进 plainAgentNetwork, 会被静默丢弃而退回默认值。
func TestAgentNetworkInit_ScheduleStrategy(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	cases := []struct {
		name    string
		raw     string
		want    string
		wantSub string
	}{
		{
			"显式 priority",
			agentNetworkConfigEnabled("  schedule-strategy: priority\n"),
			config.ScheduleStrategyPriority,
			"",
		},
		{
			"显式 least-active",
			agentNetworkConfigEnabled("  schedule-strategy: least-active\n"),
			config.ScheduleStrategyLeastActive,
			"",
		},
		{
			"首尾空白被去除",
			agentNetworkConfigEnabled("  schedule-strategy: \"  priority  \"\n"),
			config.ScheduleStrategyPriority,
			"",
		},
		{
			"缺省取 least-active",
			agentNetworkConfigEnabled(""),
			config.ScheduleStrategyLeastActive,
			"",
		},
		{
			"未启用时的非法值同样报错",
			"agent-network:\n  enable: false\n  schedule-strategy: random\n",
			"",
			"agent-network.schedule-strategy 配置错误",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent, err := loadAgentNetworkConfig(t, tc.raw)
			if tc.wantSub != "" {
				if err == nil {
					t.Fatal("应返回配置错误, 实际为 nil")
				}
				if !strings.Contains(err.Error(), tc.wantSub) {
					t.Errorf("错误消息 %q 应包含 %q", err.Error(), tc.wantSub)
				}
				// 错误消息必须列出全部合法值, 便于现场直接照抄
				if !strings.Contains(err.Error(), config.ScheduleStrategyPriority) ||
					!strings.Contains(err.Error(), config.ScheduleStrategyLeastActive) {
					t.Errorf("错误消息应列出合法值, 实际: %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("配置初始化返回错误: %v", err)
			}
			if got := agent.ScheduleStrategy(); got != tc.want {
				t.Errorf("ScheduleStrategy() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAgentNetworkInit_FallbackExplicitValues(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"显式 false", "agent-network:\n  fallback-to-local: false\n", false},
		{"显式 true", "agent-network:\n  fallback-to-local: true\n", true},
		{"完全缺省", "agent-network:\n  enable: false\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent, err := loadAgentNetworkConfig(t, tc.raw)
			if err != nil {
				t.Fatalf("配置初始化返回错误: %v", err)
			}
			if got := agent.FallbackEnabled(); got != tc.want {
				t.Errorf("fallback-to-local = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAgentNetworkInit_EnrollTokenFromEnv(t *testing.T) {
	t.Setenv(config.AgentEnrollTokenEnvName, "  env-token-0123456789  ")

	agent, err := loadAgentNetworkConfig(t, "agent-network:\n  enable: true\n")
	if err != nil {
		t.Fatalf("环境变量提供注册 Token 时应能通过校验, 实际错误: %v", err)
	}
	if agent.EnrollToken != "env-token-0123456789" {
		t.Errorf("enroll-token 未取用环境变量: %q", agent.EnrollToken)
	}
}

func TestAgentNetworkInit_EnrollTokenEnvBlankDoesNotOverride(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	agent, err := loadAgentNetworkConfig(t, agentNetworkConfigEnabled(""))
	if err != nil {
		t.Fatalf("配置初始化返回错误: %v", err)
	}
	if agent.EnrollToken != agentEnrollToken {
		t.Errorf("环境变量留空时不应覆盖配置值: %q", agent.EnrollToken)
	}
}

func TestAgentNetworkInit_Errors(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	cases := []struct {
		name    string
		raw     string
		wantSub string
	}{
		{
			"启用但缺少注册 Token",
			"agent-network:\n  enable: true\n",
			"agent-network.enroll-token 配置不能为空",
		},
		{
			"离线判定秒数等于心跳周期",
			agentNetworkConfigEnabled("  offline-seconds: 15\n"),
			"必须大于心跳周期 15 秒",
		},
		{
			"离线判定秒数小于心跳周期",
			agentNetworkConfigEnabled("  offline-seconds: 10\n"),
			"必须大于心跳周期 15 秒",
		},
		{
			"离线判定秒数为负数",
			agentNetworkConfigEnabled("  offline-seconds: -1\n"),
			"必须大于心跳周期 15 秒",
		},
		{
			"url-ttl 单位不支持",
			agentNetworkConfigEnabled("  url-ttl: 30x\n"),
			"agent-network.url-ttl 配置错误",
		},
		{
			"url-ttl 取值为 0",
			agentNetworkConfigEnabled("  url-ttl: 0h\n"),
			"agent-network.url-ttl 配置错误",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadAgentNetworkConfig(t, tc.raw)
			if err == nil {
				t.Fatal("应返回配置错误, 实际为 nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误消息 %q 应包含 %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestAgentNetworkInit_DisabledToleratesMissingCredentials(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	// 未启用时允许留空凭据: 行为与未部署本功能完全一致
	agent, err := loadAgentNetworkConfig(t, "agent-network:\n  enable: false\n  enroll-token: \"\"\n")
	if err != nil {
		t.Fatalf("未启用时不应因缺凭据而报错, 实际: %v", err)
	}
	if agent.IsEnabled() {
		t.Error("enable 为 false 时不应处于启用状态")
	}

	// 但格式错误(与 enable 无关的预校验)必须照样在启动期暴露
	if _, err := loadAgentNetworkConfig(t, "agent-network:\n  enable: false\n  url-ttl: 30x\n"); err == nil {
		t.Error("未启用时 url-ttl 格式错误也应在启动期暴露")
	}
}

func TestAgentNetworkInit_ErrorDoesNotEchoToken(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	// 校验错误消息里只提字段名, 绝不回显凭据值
	_, err := loadAgentNetworkConfig(t, agentNetworkConfigEnabled("  url-ttl: 30x\n"))
	if err == nil {
		t.Fatal("应返回配置错误, 实际为 nil")
	}
	if strings.Contains(err.Error(), agentEnrollToken) {
		t.Errorf("错误消息回显了注册 Token: %q", err.Error())
	}
}

func TestAgentNetworkNilSafety(t *testing.T) {
	var agent *config.AgentNetwork

	if agent.IsEnabled() {
		t.Error("空配置对象不应处于启用状态")
	}
	if got := agent.ClientURLTTL(); got != 24*time.Hour {
		t.Errorf("空配置对象的 url-ttl 应回退到 24h, 实际 %v", got)
	}
	if !agent.FallbackEnabled() {
		t.Error("空配置对象应默认回退本机代理")
	}
	if got := agent.ScheduleStrategy(); got != config.ScheduleStrategyLeastActive {
		t.Errorf("空配置对象的调度策略应回退到 %q, 实际 %q", config.ScheduleStrategyLeastActive, got)
	}
	if !agent.PreheatEnabled() {
		t.Error("空配置对象应默认开启网关预热")
	}
}
