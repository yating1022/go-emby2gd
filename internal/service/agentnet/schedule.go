package agentnet

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// defaultListenPort 节点数据面的默认监听端口
//
// 与 agent 侧 config.DefaultListenPort 一致; 仅在节点记录里的端口缺失时兜底。
const defaultListenPort = 8790

// schedule 选出一个可调度节点
//
// 候选条件(缺一不可):
//   - Enabled: 管理员没有禁用;
//   - 心跳在 offline 窗口内: LastSeenAt 为零(从未心跳)视为离线;
//   - 地址可推导: public_base_url 合法或来源 IP 已知。
//
// 选取规则: 活跃流最少的节点; 平局随机(避免每次把负载压在同一台上)。
// 没有候选时返回 (nil, nil) —— "没有节点"不是错误, 与"读取注册表失败"必须区分开。
func (r *registry) schedule(now time.Time, offline time.Duration) (*agentRecord, error) {
	if err := r.ensureLoaded(); err != nil {
		return nil, err
	}

	r.mu.RLock()
	candidates := make([]*agentRecord, 0, len(r.records))
	for _, rec := range r.records {
		if !rec.Enabled {
			continue
		}
		if rec.LastSeenAt.IsZero() || now.Sub(rec.LastSeenAt) > offline {
			continue
		}
		if agentBaseURL(rec) == "" {
			continue
		}
		candidates = append(candidates, rec.clone())
	}
	r.mu.RUnlock()

	if len(candidates) == 0 {
		return nil, nil
	}

	minStreams := candidates[0].ActiveStreams
	for _, rec := range candidates[1:] {
		if rec.ActiveStreams < minStreams {
			minStreams = rec.ActiveStreams
		}
	}
	ties := make([]*agentRecord, 0, len(candidates))
	for _, rec := range candidates {
		if rec.ActiveStreams == minStreams {
			ties = append(ties, rec)
		}
	}
	return ties[rand.Intn(len(ties))], nil
}

// agentBaseURL 推导节点对客户端可见的基址
//
// 优先用节点上报的 public_base_url(NAT 后的节点必须显式配置), 否则按
// 心跳来源 IP 与监听端口拼 http://host:port。两者都拿不到时返回空串,
// 该节点视为"地址不可推导"、不参与调度。
func agentBaseURL(rec *agentRecord) string {
	if rec == nil {
		return ""
	}

	if normalized, err := parsePublicBaseURL(rec.PublicBaseURL); err == nil && normalized != "" {
		return normalized
	}

	ip := strings.TrimSpace(rec.LastIP)
	if ip == "" {
		return ""
	}
	port := rec.ListenPort
	if port <= 0 || port > 65535 {
		port = defaultListenPort
	}
	// JoinHostPort: IPv6 地址会自动补方括号, 直接拼字符串会拼出不可用的地址
	return "http://" + net.JoinHostPort(ip, strconv.Itoa(port))
}

// parsePublicBaseURL 归一化并校验节点上报的对外地址
//
// 归一化: 去首尾空白 + 去结尾的 '/'。
// 校验: 必须是 http/https 绝对地址且带主机名; 不接受 userinfo、查询参数与片段 ——
// 客户端拿到的地址就是这个基址拼出来的, 带上后两者只会拼出无意义的地址。
//
// 传入空字符串返回 ("", nil): "未配置"不是错误(此时按来源 IP 推导)。
func parsePublicBaseURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}

	// 最常见的错法: 上报了 "1.2.3.4:8790" 或纯主机名。
	// 这类串连 url.Parse 都过不去(冒号会被当成非法 scheme 的一部分),
	// 所以先按 "://" 给出可操作的提示, 而不是笼统的"不是合法的地址"。
	if !strings.Contains(value, "://") {
		return "", errors.New("缺少 http:// 或 https:// 前缀")
	}

	u, err := url.Parse(value)
	if err != nil {
		return "", errors.New("不是合法的地址")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("只支持 http/https, 当前 scheme: %s", scheme)
	}
	if u.Hostname() == "" {
		return "", errors.New("缺少主机名")
	}
	if u.User != nil {
		return "", errors.New("不能包含用户名或密码")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("不能包含查询参数或片段")
	}

	return strings.TrimRight(value, "/"), nil
}
