package config_test

import (
	"testing"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
)

// hub 直连 v2 开关(hub-direct-v2)的配置用例
//
// 三条契约:
//   - 缺省(及配置对象为空)为 true: 门槛本身已挡住旧节点与未预热文件, 默认开启
//     才能让新链路生效;
//   - 显式 false 必须被尊重 —— 这是 v0.4.2 的秒级回滚开关(只改配置, 不换镜像);
//   - 键名必须进 UnmarshalYAML 的显式字段清单(漏了会被静默丢弃): 显式 true 与
//     显式 false 两种写法都要能读回, 且与 enable 开关无关。

func TestAgentNetworkInit_HubDirectV2Default(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	agent, err := loadAgentNetworkConfig(t, agentNetworkConfigEnabled(""))
	if err != nil {
		t.Fatalf("配置初始化返回错误: %v", err)
	}
	if !agent.HubDirectV2Enabled() {
		t.Error("hub-direct-v2 缺省应开启")
	}

	// 显式 true 与缺省等价
	agent, err = loadAgentNetworkConfig(t, agentNetworkConfigEnabled("  hub-direct-v2: true\n"))
	if err != nil {
		t.Fatalf("配置初始化返回错误: %v", err)
	}
	if !agent.HubDirectV2Enabled() {
		t.Error("hub-direct-v2: true 应开启")
	}
}

func TestAgentNetworkInit_HubDirectV2ExplicitFalse(t *testing.T) {
	clearAgentEnrollTokenEnv(t)

	agent, err := loadAgentNetworkConfig(t, agentNetworkConfigEnabled("  hub-direct-v2: false\n"))
	if err != nil {
		t.Fatalf("配置初始化返回错误: %v", err)
	}
	if agent.HubDirectV2Enabled() {
		t.Error("hub-direct-v2: false 必须关闭 v2 签发(秒级回滚开关)")
	}

	// 功能未启用时同样要经 yaml 解析落进配置对象: 键不能静默丢弃
	disabled, err := loadAgentNetworkConfig(t, "agent-network:\n  enable: false\n  hub-direct-v2: false\n")
	if err != nil {
		t.Fatalf("配置初始化返回错误: %v", err)
	}
	if disabled.HubDirectV2Enabled() {
		t.Error("关闭状态下 hub-direct-v2: false 同样应被解析")
	}
}

func TestAgentNetworkHubDirectV2NilSafety(t *testing.T) {
	var agent *config.AgentNetwork
	if !agent.HubDirectV2Enabled() {
		t.Error("空配置对象应按默认值 true 处理")
	}
}
