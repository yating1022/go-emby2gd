package proxy

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 多地址快速失败拨号的单测（design §5/§7）：
// 坏地址 ~1.5s 跳过、全失败聚合报错、字面 IP 走原路、解析失败回落。

// fakeDial 造一个可注入的拨号函数：按地址决定成功或失败。
func fakeDial(t *testing.T, succeed string, seen *[]string, deadlines map[string]time.Duration) func(context.Context, string, string) (net.Conn, error) {
	t.Helper()
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if seen != nil {
			*seen = append(*seen, addr)
		}
		if deadlines != nil {
			if dl, ok := ctx.Deadline(); ok {
				deadlines[addr] = time.Until(dl)
			}
		}
		host, _, _ := net.SplitHostPort(addr)
		if host == succeed {
			server, client := net.Pipe()
			t.Cleanup(func() { _ = server.Close() })
			return client, nil
		}
		<-ctx.Done() // 模拟坏 IP：拖到单地址超时被掐
		return nil, ctx.Err()
	}
}

func TestMultiDialerSkipsBadAddr(t *testing.T) {
	d := &multiDialer{
		resolver: func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}, {IP: net.ParseIP("10.0.0.2")}}, nil
		},
		dial:    fakeDial(t, "10.0.0.2", nil, nil),
		perAddr: 30 * time.Millisecond,
	}
	start := time.Now()
	conn, err := d.DialContext(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer conn.Close()
	// 第一个地址被 ~perAddr 掐掉后立刻换第二个：总耗时远小于"逐个等满 10s"。
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("跳过坏地址耗时 %v，应约为单地址限时", elapsed)
	}
}

func TestMultiDialerAllFailAggregate(t *testing.T) {
	d := &multiDialer{
		resolver: func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("10.1.0.1")}, {IP: net.ParseIP("10.1.0.2")}}, nil
		},
		dial:    fakeDial(t, "", nil, nil),
		perAddr: 10 * time.Millisecond,
	}
	_, err := d.DialContext(context.Background(), "tcp", "example.com:443")
	if err == nil {
		t.Fatal("全部地址失败时应报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "全部地址均连接失败") || !strings.Contains(msg, "2") {
		t.Fatalf("聚合错误文案不符：%q", msg)
	}
}

func TestMultiDialerLiteralIPAndDNSFallback(t *testing.T) {
	var resolverCalls atomic.Int64
	var seen []string
	// 回落路径用的是原始 ctx（可能没有 deadline），拨号必须立即返回而不是等 ctx。
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		seen = append(seen, addr)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		server, client := net.Pipe()
		t.Cleanup(func() { _ = server.Close() })
		return client, nil
	}
	d := &multiDialer{
		resolver: func(context.Context, string) ([]net.IPAddr, error) {
			resolverCalls.Add(1)
			return nil, errors.New("dns down")
		},
		dial:    dial,
		perAddr: 10 * time.Millisecond,
	}
	// 字面 IP：完全不走 resolver，原样交给底层 dialer。
	conn, err := d.DialContext(context.Background(), "tcp", "1.2.3.4:443")
	if err != nil {
		t.Fatalf("字面 IP 拨号失败：%v", err)
	}
	_ = conn.Close()
	if resolverCalls.Load() != 0 {
		t.Fatalf("字面 IP 不应触发解析（%d 次）", resolverCalls.Load())
	}

	// 解析失败：回落到底层 dialer 复现标准错误口径（地址原样传下去）。
	conn, err = d.DialContext(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("解析失败时应回落标准拨号：%v", err)
	}
	_ = conn.Close()
	if resolverCalls.Load() != 1 {
		t.Fatalf("解析失败场景 resolver 调用 %d 次", resolverCalls.Load())
	}
	if len(seen) == 0 || seen[len(seen)-1] != "example.com:443" {
		t.Fatalf("回落拨号的目标地址 = %v，应为原地址", seen)
	}
}

func TestMultiDialerLastAddrGrace(t *testing.T) {
	deadlines := make(map[string]time.Duration)
	d := &multiDialer{
		resolver: func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("10.2.0.1")}, {IP: net.ParseIP("10.2.0.2")}}, nil
		},
		dial:    fakeDial(t, "10.2.0.2", nil, deadlines),
		perAddr: 40 * time.Millisecond,
	}
	conn, err := d.DialContext(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	_ = conn.Close()
	first := deadlines["10.2.0.1:443"]
	last := deadlines["10.2.0.2:443"]
	if first <= 0 || last <= first {
		t.Fatalf("单地址限时 = %v/%v，最后一个地址应有宽限", first, last)
	}
	if last < multiDialLastAddrGrace*first/2 {
		t.Fatalf("最后地址宽限不足：first=%v last=%v", first, last)
	}
}

func TestFilterAddrsByNetwork(t *testing.T) {
	addrs := []net.IPAddr{
		{IP: net.ParseIP("10.0.0.1")},
		{IP: net.ParseIP("2001:db8::1")},
		{IP: net.ParseIP("10.0.0.2")},
	}
	if got := filterAddrsByNetwork(addrs, "tcp4"); len(got) != 2 {
		t.Fatalf("tcp4 过滤 = %d 个", len(got))
	}
	if got := filterAddrsByNetwork(addrs, "tcp6"); len(got) != 1 {
		t.Fatalf("tcp6 过滤 = %d 个", len(got))
	}
	if got := filterAddrsByNetwork(addrs, "tcp"); len(got) != 3 {
		t.Fatalf("tcp 不应过滤（%d 个）", len(got))
	}
}

// TestNewHubUpstreamClient 确认 hub 出站客户端可以构造并沿用同一套连接池参数。
func TestNewHubUpstreamClient(t *testing.T) {
	c := NewHubUpstreamClient()
	if c == nil || c.Transport == nil {
		t.Fatal("NewHubUpstreamClient 返回不完整")
	}
	if c.Timeout != 0 {
		t.Fatalf("Client.Timeout = %v，必须为 0（大文件流不设整体超时）", c.Timeout)
	}
}
