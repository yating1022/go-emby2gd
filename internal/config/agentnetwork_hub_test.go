package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
)

// hub 缓存中心接入的配置项用例
//
// 三条契约:
//   - 缺省(以及整段缺失/配置对象为空)时行为与未部署 hub 完全一致;
//   - 端口与超时按真实配置路径解析与校验, 且与 enable / hub-enable 无关地提前报错;
//   - 键名必须进 UnmarshalYAML 的显式字段清单 —— 漏了会被静默丢弃(既有的坑)。

func TestAgentNetworkInit_HubDefaults(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	agent, err := loadAgentNetworkConfig(t, agentNetworkConfigEnabled(""))
	if err != nil {
		t.Fatalf("配置初始化返回错误: %v", err)
	}

	if agent.HubEnabled() {
		t.Error("hub-enable 缺省应关闭")
	}
	if got := agent.HubPort(); got != 8791 {
		t.Errorf("hub-port 缺省 = %d, want 8791", got)
	}
	if got := agent.HubWarmTimeout(); got != 3*time.Second {
		t.Errorf("hub-warm-timeout 缺省 = %v, want 3s", got)
	}
}

func TestAgentNetworkInit_HubExplicitValues(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	agent, err := loadAgentNetworkConfig(t, agentNetworkConfigEnabled(
		"  hub-enable: true\n  hub-port: 9001\n  hub-warm-timeout: 7s\n"))
	if err != nil {
		t.Fatalf("配置初始化返回错误: %v", err)
	}

	if !agent.HubEnabled() {
		t.Error("hub-enable: true 应处于开启状态")
	}
	if got := agent.HubPort(); got != 9001 {
		t.Errorf("hub-port = %d, want 9001", got)
	}
	if got := agent.HubWarmTimeout(); got != 7*time.Second {
		t.Errorf("hub-warm-timeout = %v, want 7s", got)
	}
}

// TestAgentNetworkInit_HubExplicitFalse hub-enable 显式 false 与缺省等价
func TestAgentNetworkInit_HubExplicitFalse(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	agent, err := loadAgentNetworkConfig(t, agentNetworkConfigEnabled("  hub-enable: false\n"))
	if err != nil {
		t.Fatalf("配置初始化返回错误: %v", err)
	}
	if agent.HubEnabled() {
		t.Error("hub-enable 显式 false 必须保持关闭")
	}
}

func TestAgentNetworkInit_HubErrors(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	cases := []struct {
		name string
		raw  string
		key  string
	}{
		{"端口低于下界", agentNetworkConfigEnabled("  hub-port: -1\n"), "hub-port"},
		{"端口高于上界", agentNetworkConfigEnabled("  hub-port: 70000\n"), "hub-port"},
		{"超时不是合法时长", agentNetworkConfigEnabled("  hub-warm-timeout: abc\n"), "hub-warm-timeout"},
		{"超时缺少单位", agentNetworkConfigEnabled("  hub-warm-timeout: 3\n"), "hub-warm-timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadAgentNetworkConfig(t, tc.raw); err == nil {
				t.Fatal("非法配置必须导致初始化失败, 而不是静默取默认值")
			} else if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("错误消息应指明配置键 %q, 实际: %v", tc.key, err)
			}
		})
	}
}

// TestAgentNetworkInit_HubValidationIndependentOfEnable 校验与开关无关
//
// 端口/超时是部署期就能发现的问题, 应当在启动时立刻报错,
// 而不是等打开 hub-enable 之后才暴露。
func TestAgentNetworkInit_HubValidationIndependentOfEnable(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	// enable=false + 非法 hub-port: 仍然报错
	disabledBad := "agent-network:\n  enable: false\n  hub-port: 0\n  hub-warm-timeout: 3x\n"
	if _, err := loadAgentNetworkConfig(t, disabledBad); err == nil {
		t.Error("功能关闭时也应校验 hub 配置项")
	}

	// enable=false + 合法 hub 配置: 不报错(关闭状态容忍缺失的注册 Token)
	disabledOK := "agent-network:\n  enable: false\n  hub-enable: true\n  hub-port: 9002\n  hub-warm-timeout: 2s\n"
	agent, err := loadAgentNetworkConfig(t, disabledOK)
	if err != nil {
		t.Fatalf("功能关闭时合法 hub 配置不应报错: %v", err)
	}
	if agent.IsEnabled() {
		t.Error("enable: false 时不应处于启用状态")
	}
	if agent.HubPort() != 9002 || agent.HubWarmTimeout() != 2*time.Second {
		t.Errorf("关闭状态下 hub 配置项仍应解析: port=%d, timeout=%v", agent.HubPort(), agent.HubWarmTimeout())
	}
}

// TestAgentNetworkHubNilSafety 配置对象为空时的取值兜底
//
// 与既有 getter 同一约定: 空指针不得 panic, 一律按默认值(即"未部署 hub")处理。
func TestAgentNetworkHubNilSafety(t *testing.T) {
	var agent *config.AgentNetwork

	if agent.HubEnabled() {
		t.Error("空配置对象的 hub-enable 应为关闭")
	}
	if got := agent.HubPort(); got != 8791 {
		t.Errorf("空配置对象的 hub-port = %d, want 8791", got)
	}
	if got := agent.HubWarmTimeout(); got != 3*time.Second {
		t.Errorf("空配置对象的 hub-warm-timeout = %v, want 3s", got)
	}
}

// TestAgentNetworkInit_HubPortZeroUsesDefault 显式写 0 等价缺省
//
// 0 不是合法端口: 按"未设置"处理, 避免手工写 0 时把内网地址拼成 :0。
func TestAgentNetworkInit_HubPortZeroUsesDefault(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	agent, err := loadAgentNetworkConfig(t, agentNetworkConfigEnabled("  hub-port: 0\n"))
	if err != nil {
		t.Fatalf("配置初始化返回错误: %v", err)
	}
	if got := agent.HubPort(); got != 8791 {
		t.Errorf("hub-port: 0 = %d, want 8791", got)
	}
}
