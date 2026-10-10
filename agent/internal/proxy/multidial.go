package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// 多地址快速失败拨号（design 10-10-hub-agent-mode §5）。
//
// 病灶：Twon 出口解析 googleapis 会拿到多个地址，其中坏 IP 的 TCP 握手实测要
// 7.5–22.5s 才失败；默认 dialer 逐个地址串行、单地址要等满 10s（Dialer.Timeout）
// 才换下一个，于是一次请求可能白等十几秒。
//
// 方案：自己解析出全部地址，逐地址拨号；**单个地址限时（默认 1.5s）内没建立
// 就换下一个**，全部失败才报错。最后一个地址给数倍宽限（它是"唯一可用地址"
// 的最可能候选，1.5s 硬切会把它误杀）。
const (
	// multiDialPerAddrTimeout 是单地址拨号的限时（design §5 的 ~1.5s）。
	multiDialPerAddrTimeout = 1500 * time.Millisecond
	// multiDialLastAddrGrace 是最后一个候选地址的宽限倍数。
	multiDialLastAddrGrace = 4
)

// multiDialer 实现"解析全部地址、逐地址快速失败"的 DialContext。
//
// resolver/dial 可注入（测试用）。只有"带端口的域名"才走多地址逻辑：
// 字面 IP（含 IPv6）与解析失败的场景都回落到标准 dialer，错误口径与现状一致。
type multiDialer struct {
	resolver func(ctx context.Context, host string) ([]net.IPAddr, error)
	dial     func(ctx context.Context, network, address string) (net.Conn, error)
	perAddr  time.Duration
}

// newMultiDialer 构造生产形态的 multiDialer：标准解析器 + 标准 dialer。
func newMultiDialer(dialer *net.Dialer) *multiDialer {
	return &multiDialer{
		resolver: net.DefaultResolver.LookupIPAddr,
		dial:     dialer.DialContext,
		perAddr:  multiDialPerAddrTimeout,
	}
}

// DialContext 实现 http.Transport 需要的拨号函数。
func (d *multiDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return d.dial(ctx, network, addr)
	}
	if host == "" || net.ParseIP(host) != nil {
		// 字面 IP（或空主机）：没有可解析的候选，直接拨。
		return d.dial(ctx, network, addr)
	}
	addrs, lerr := d.resolver(ctx, host)
	if lerr != nil || len(addrs) == 0 {
		// 解析失败：交给标准 dialer 复现正常错误（而不是自造一个）。
		return d.dial(ctx, network, addr)
	}
	candidates := filterAddrsByNetwork(addrs, network)
	if len(candidates) == 0 {
		candidates = addrs
	}

	var lastErr error
	for i, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		timeout := d.perAddr
		if i == len(candidates)-1 {
			timeout = d.perAddr * multiDialLastAddrGrace
		}
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		conn, derr := d.dial(attemptCtx, network, net.JoinHostPort(candidate.IP.String(), port))
		cancel()
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	return nil, fmt.Errorf("全部地址均连接失败（共 %d 个地址）：%w", len(candidates), lastErr)
}

// filterAddrsByNetwork 按网络族过滤候选（tcp4 只留 IPv4，tcp6 只留 IPv6）。
func filterAddrsByNetwork(addrs []net.IPAddr, network string) []net.IPAddr {
	switch network {
	case "tcp4":
		out := make([]net.IPAddr, 0, len(addrs))
		for _, a := range addrs {
			if a.IP.To4() != nil {
				out = append(out, a)
			}
		}
		return out
	case "tcp6":
		out := make([]net.IPAddr, 0, len(addrs))
		for _, a := range addrs {
			if a.IP.To4() == nil {
				out = append(out, a)
			}
		}
		return out
	}
	return addrs
}

// NewHubUpstreamClient 构造 hub 的出站客户端：连接池参数与 node 完全一致，
// 唯一差别是 DialContext 换成多地址快速失败拨号（design §5）。
//
// 只给 hub 用：node 的出口按 v0.3.2 冻结行为保持不变（同样的收益留给
// "需要时再启用"的后续任务，避免在零回归验收里动 node 的网络栈）。
func NewHubUpstreamClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{Transport: upstreamTransport(newMultiDialer(dialer).DialContext)}
}
