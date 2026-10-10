package emby

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs"
)

// preheatLogCollector 收集测试期间写入的日志
//
// 外部测试包(emby_test)的 redirectLogCollector 靠重定向 stdout 工作, 内部测试
// 直接用 logs.RegisterLogger 挂载收集器: 与生产代码走的是同一条写日志路径。
type preheatLogCollector struct {
	mu    sync.Mutex
	lines []string
}

// Log 实现 logs.Logger
func (c *preheatLogCollector) Log(content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, content)
}

// String 返回已收集的全部日志文本
func (c *preheatLogCollector) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "")
}

// contains 判断已收集日志中是否出现指定片段
func (c *preheatLogCollector) contains(sub string) bool {
	return strings.Contains(c.String(), sub)
}

// capturePreheatLogs 注册一个日志收集器, 用例结束时自动注销
func capturePreheatLogs(t *testing.T) *preheatLogCollector {
	t.Helper()

	collector := &preheatLogCollector{}
	id, ok := logs.RegisterLogger(collector)
	if !ok {
		t.Fatal("注册日志收集器失败")
	}
	t.Cleanup(func() { logs.RemoveLogger(id) })

	return collector
}

// TestPreheatRequest_NotSentGradedWarn 请求未送达(dial 类失败)判为真失败: WARN
//
// 分级第一半(design §2.4): 请求字节都没写出去, 节点侧什么都没发生, 这才是预热
// 唯一的真失败场景, 必须保留 WARN 与「预热请求发送失败」措辞。同时错误文本不得
// 回显含 s 参数的完整签名地址(agent-network.md §3.7)。
func TestPreheatRequest_NotSentGradedWarn(t *testing.T) {
	signed := "http://127.0.0.1:1/dl/L3lhL2EubWt2?e=1730000000&s=abc123def456"

	collector := capturePreheatLogs(t)

	outcome, err := preheatRequest(signed)
	if outcome != preheatNotSent {
		t.Fatalf("outcome = %v, want preheatNotSent (err = %v)", outcome, err)
	}
	if err == nil || !strings.Contains(err.Error(), "请求节点失败") {
		t.Fatalf("错误应含 请求节点失败, 实际: %v", err)
	}
	if strings.Contains(err.Error(), signed) || strings.Contains(err.Error(), "&s=") {
		t.Fatalf("错误文本泄露了签名地址: %v", err)
	}

	logPreheatOutcome(outcome, err, "/影视库/预热/不可达.mkv")
	if !collector.contains("[WARN]") || !collector.contains("预热请求发送失败") {
		t.Errorf("未送达应记 WARN 级「预热请求发送失败」, 实际: %s", collector.String())
	}
	if collector.contains("已触发") {
		t.Errorf("未送达不得记已触发, 实际: %s", collector.String())
	}
}

// TestPreheatRequest_SentButNoReplyGradedInfo 请求已送达但响应超时判为已触发: INFO
//
// 分级第二半(design §2.4): 假 agent 收下请求后一声不吭, 靠缩短后的 preheatTimeout
// 收场。此时节点已经在跑首触预取, 触发语义完成, 应记 INFO「已触发（响应超时…）」
// 而不是 WARN —— 这正是旧实现的假告警场景。
func TestPreheatRequest_SentButNoReplyGradedInfo(t *testing.T) {
	sawRequest := make(chan struct{})
	var sawOnce sync.Once

	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawOnce.Do(func() { close(sawRequest) })
		<-r.Context().Done() // 永不响应: 等客户端超时收场
	}))
	t.Cleanup(agent.Close)

	// 临时调小超时以便快速覆盖超时分支(生产值 8s 保持不变)
	oldTimeout := preheatTimeout
	preheatTimeout = 300 * time.Millisecond
	t.Cleanup(func() { preheatTimeout = oldTimeout })

	collector := capturePreheatLogs(t)

	start := time.Now()
	outcome, err := preheatRequest(agent.URL + "/dl/anVzdC5ta3Y?e=1730000000&s=abc123def456")
	elapsed := time.Since(start)

	if outcome != preheatSentButNoReply {
		t.Fatalf("outcome = %v, want preheatSentButNoReply (err = %v)", outcome, err)
	}
	if err == nil || !strings.Contains(err.Error(), "等待节点响应失败") {
		t.Fatalf("错误应含 等待节点响应失败, 实际: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("应在缩短后的超时内返回, 实际耗时 %v", elapsed)
	}

	select {
	case <-sawRequest:
	case <-time.After(time.Second):
		t.Fatal("假 agent 没有收到请求, 未送达与已送达的语义区分不成立")
	}

	logPreheatOutcome(outcome, err, "/影视库/预热/超时.mkv")
	if !collector.contains("[INFO]") || !collector.contains("已触发（响应超时") {
		t.Errorf("已送达后的响应超时应记 INFO 级「已触发（响应超时…）」, 实际: %s", collector.String())
	}
	if collector.contains("[WARN]") {
		t.Errorf("送达后的超时不是真失败, 不得记 WARN, 实际: %s", collector.String())
	}
}

// TestPreheatRequest_HeaderOnlyReturnsFast 响应头一到立即返回: 不读 body、不等超时
//
// 假 agent 先写 206 响应头并 Flush, 之后扣住 body 不放。旧实现会去读 body, 只能
// 等满 preheatTimeout 才返回(并记假告警); 新实现拿到响应头就 Close 返回, 用例用
// 远小于超时上限的耗时断言"没有在等 body"。
func TestPreheatRequest_HeaderOnlyReturnsFast(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	releaseBody := func() { releaseOnce.Do(func() { close(release) }) }

	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/x-matroska")
		w.WriteHeader(http.StatusPartialContent)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		enteredOnce.Do(func() { close(entered) })
		<-release // 扣住响应体
	}))
	// 注册顺序即执行顺序的反向: 释放体必须先于 Close 执行
	t.Cleanup(agent.Close)
	t.Cleanup(releaseBody)

	collector := capturePreheatLogs(t)

	start := time.Now()
	outcome, err := preheatRequest(agent.URL + "/dl/anVzdC5ta3Y?e=1730000000&s=abc123def456")
	elapsed := time.Since(start)

	if outcome != preheatTriggered || err != nil {
		t.Fatalf("拿到响应头即应判定已触发, outcome = %v, err = %v", outcome, err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("响应体未被读取, 应立即返回(生产超时 8s), 实际耗时 %v", elapsed)
	}

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("假 agent 没有收到请求")
	}

	logPreheatOutcome(outcome, err, "/影视库/预热/扣体.mkv")
	if !collector.contains("[INFO]") || !collector.contains("已触发:") {
		t.Errorf("正常触发应记 INFO 级「已触发」, 实际: %s", collector.String())
	}
	if collector.contains("[WARN]") {
		t.Errorf("读了不存在的响应体失败不应出现 WARN, 实际: %s", collector.String())
	}

	releaseBody()
}

// TestLogPreheatOutcome_Graded 四种结果的分级日志文案
//
// 纯函数校验: 结果 → 级别与措辞的一一映射(design §2.4),
// 避免上面几个用例只覆盖到实际会走到的那两种结果。
func TestLogPreheatOutcome_Graded(t *testing.T) {
	cases := []struct {
		name      string
		outcome   preheatOutcome
		err       error
		wantLevel string
		wantText  string
	}{
		{
			"已触发",
			preheatTriggered,
			nil,
			"[INFO]",
			"[网关预热] 已触发:",
		},
		{
			"已送达后响应超时",
			preheatSentButNoReply,
			errors.New("等待节点响应失败: context deadline exceeded"),
			"[INFO]",
			"[网关预热] 已触发（响应超时",
		},
		{
			"节点错误状态码",
			preheatBadStatus,
			errors.New("节点返回了错误的响应码: 502"),
			"[WARN]",
			"[网关预热] 预热请求失败:",
		},
		{
			"请求未送达",
			preheatNotSent,
			errors.New("请求节点失败: dial tcp 127.0.0.1:1: connect: connection refused"),
			"[WARN]",
			"[网关预热] 预热请求发送失败:",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			collector := capturePreheatLogs(t)

			logPreheatOutcome(tc.outcome, tc.err, "/影视库/预热/分级.mkv")

			if !collector.contains(tc.wantLevel) {
				t.Errorf("应记 %s 级, 实际: %s", tc.wantLevel, collector.String())
			}
			if !collector.contains(tc.wantText) {
				t.Errorf("日志应含 %q, 实际: %s", tc.wantText, collector.String())
			}
			if other := levelOf(tc.wantLevel); collector.contains(other) {
				t.Errorf("不应出现 %s 级, 实际: %s", other, collector.String())
			}
		})
	}
}

// levelOf 返回与给定级别相对的另一个级别标记(测试辅助)
func levelOf(level string) string {
	if level == "[WARN]" {
		return "[INFO]"
	}
	return "[WARN]"
}
