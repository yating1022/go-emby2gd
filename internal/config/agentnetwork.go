package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/strs"
	"gopkg.in/yaml.v3"
)

// AgentEnrollTokenEnvName 覆盖 agent-network.enroll-token 的环境变量名
//
// 环境变量非空时优先生效, 便于容器部署不往配置文件里写明文注册 Token。
const AgentEnrollTokenEnvName = "AGENT_ENROLL_TOKEN"

// AgentHeartbeatIntervalSeconds 节点与 master 约定的心跳周期(秒)
//
// 这是协议常量而不是可配置项: agent 侧默认间隔与它一致, 且
// offline-seconds 必须【大于】它, 否则在线节点会被周期性判定为离线。
const AgentHeartbeatIntervalSeconds = 15

// agent 代理网络默认值
const (
	// defaultAgentOfflineSeconds 离线判定秒数默认值
	defaultAgentOfflineSeconds = 45
	// defaultAgentURLTTL 客户端 URL 签名时效默认值
	defaultAgentURLTTL = time.Hour * 24
)

// AgentNetwork agent 代理网络(master 侧)配置
//
// 启用后, 命中 Google Drive 挂载路径的播放请求会先在本网关内选点:
// 有可用节点就把客户端 302 到节点上的签名地址, 媒体字节不经过本网关;
// 没有任何可用节点时按 FallbackToLocal 决定回退本机代理还是返回 503。
//
// 关闭时行为与未部署本功能完全一致。
type AgentNetwork struct {
	// Enable 总开关
	Enable bool `yaml:"enable"`
	// EnrollToken 节点注册 Token, 属敏感凭据, 不得出现在任何日志中
	//
	// 修改本项并重启即完成轮换: 旧 Token 立即失效, 已注册节点不受影响。
	EnrollToken string `yaml:"enroll-token"`
	// OfflineSeconds 离线判定秒数, 默认 45
	//
	// 必须大于心跳周期(15 秒): 大于该时长没有心跳的节点不再被调度。
	// 不做自动删除, 进程恢复后的下一次心跳即回归。
	OfflineSeconds int `yaml:"offline-seconds"`
	// URLTTL 客户端 URL 的签名时效, 默认 24h
	//
	// 格式同 cache.expired: 可配置单位 d / h / m / s。
	URLTTL string `yaml:"url-ttl"`
	// FallbackToLocal 没有任何可用节点时是否回退到本机直链代理, 默认 true
	//
	// 该字段的语义与 strm 代理的 max-concurrent-streams 同款:
	// 显式 false 与"未配置"不能共用零值, 需要靠 UnmarshalYAML 区分。
	FallbackToLocal bool `yaml:"fallback-to-local"`

	// fallbackToLocalSet 记录配置中是否显式出现 fallback-to-local
	fallbackToLocalSet bool
	// urlTTL 初始化后的客户端 URL 签名时效
	urlTTL time.Duration
}

// UnmarshalYAML 自定义解析 agent 网络配置
//
// 唯一的目的是识别 fallback-to-local 是否被显式配置:
// 该字段的默认值是 true, 与 bool 零值 false 冲突, 不能共用零值判断。
func (a *AgentNetwork) UnmarshalYAML(value *yaml.Node) error {
	type plainAgentNetwork struct {
		Enable          bool   `yaml:"enable"`
		EnrollToken     string `yaml:"enroll-token"`
		OfflineSeconds  int    `yaml:"offline-seconds"`
		URLTTL          string `yaml:"url-ttl"`
		FallbackToLocal *bool  `yaml:"fallback-to-local"`
	}

	var v plainAgentNetwork
	if err := value.Decode(&v); err != nil {
		return err
	}

	a.Enable = v.Enable
	a.EnrollToken = v.EnrollToken
	a.OfflineSeconds = v.OfflineSeconds
	a.URLTTL = v.URLTTL
	if v.FallbackToLocal != nil {
		a.FallbackToLocal = *v.FallbackToLocal
		a.fallbackToLocalSet = true
	}
	return nil
}

// Init 配置初始化
func (a *AgentNetwork) Init() error {
	// 0 统一去除首尾空白, 避免从 yaml 复制粘贴时带入不可见字符
	a.EnrollToken = strings.TrimSpace(a.EnrollToken)
	a.URLTTL = strings.TrimSpace(a.URLTTL)

	// 1 注册 Token 的环境变量覆盖
	//
	// 非空才覆盖: 留空表示"没有设置", 而不是"把配置值清空"。
	if envToken := strings.TrimSpace(os.Getenv(AgentEnrollTokenEnvName)); envToken != "" {
		a.EnrollToken = envToken
	}

	// 2 离线判定秒数: 与 enable 无关地提前校验
	//
	// 配错但还没启用的配置也应该在启动阶段暴露, 而不是等启用后发现
	// "节点明明在线却老是被判离线"。
	if a.OfflineSeconds == 0 {
		a.OfflineSeconds = defaultAgentOfflineSeconds
	}
	if a.OfflineSeconds <= AgentHeartbeatIntervalSeconds {
		return fmt.Errorf("agent-network.offline-seconds 配置错误: %d, 必须大于心跳周期 %d 秒",
			a.OfflineSeconds, AgentHeartbeatIntervalSeconds)
	}

	// 3 客户端 URL 签名时效
	if strs.AnyEmpty(a.URLTTL) {
		a.urlTTL = defaultAgentURLTTL
	} else {
		ttl, err := parseDuration(a.URLTTL)
		if err != nil {
			return fmt.Errorf("agent-network.url-ttl 配置错误: %w", err)
		}
		a.urlTTL = ttl
	}

	// 4 无可用节点时的回退策略: 完全缺省取默认值 true
	if !a.fallbackToLocalSet {
		a.FallbackToLocal = true
	}

	// 5 未启用时不再校验凭据, 行为与未部署本功能完全一致
	//
	// 校验错误消息里只提字段名, 绝不回显凭据值。
	if !a.Enable {
		return nil
	}

	if strs.AnyEmpty(a.EnrollToken) {
		return errors.New("agent-network.enroll-token 配置不能为空")
	}
	if len(a.EnrollToken) < 16 {
		// 只提醒不拒绝: Token 强度属于部署方的取舍
		logs.Warn("[agent 网络] 注册 Token 长度不足 16 个字符, 建议换用更长的随机串")
	}

	return nil
}

// IsEnabled agent 代理网络是否启用
func (a *AgentNetwork) IsEnabled() bool {
	if a == nil {
		return false
	}
	return a.Enable
}

// ClientURLTTL 获取客户端 URL 的签名时效
func (a *AgentNetwork) ClientURLTTL() time.Duration {
	if a == nil || a.urlTTL <= 0 {
		return defaultAgentURLTTL
	}
	return a.urlTTL
}

// FallbackEnabled 获取无可用节点时是否回退到本机代理
//
// 配置对象为空时按默认值 true 处理: 回退是保守策略, 不会让播放挂掉。
func (a *AgentNetwork) FallbackEnabled() bool {
	if a == nil {
		return true
	}
	return a.FallbackToLocal
}
