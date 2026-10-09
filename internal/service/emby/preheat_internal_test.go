package emby

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// size 读取去重表当前大小(测试内部使用)
func (p *preheatRecords) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.records)
}

// TestPreheatRecords_MarkFired 去重表的 TTL 语义
func TestPreheatRecords_MarkFired(t *testing.T) {
	records := newPreheatRecords()
	now := time.Now()

	if !records.markFired("/影视库/a.mkv", now) {
		t.Fatal("首次触发应被允许")
	}
	if records.markFired("/影视库/a.mkv", now.Add(time.Minute)) {
		t.Error("TTL 内重复触发应被拒绝")
	}
	if !records.markFired("/影视库/b.mkv", now.Add(time.Minute)) {
		t.Error("不同路径应各自独立计数")
	}
	if !records.markFired("/影视库/a.mkv", now.Add(preheatDedupTTL)) {
		t.Error("到达 TTL 边界后应重新允许触发")
	}
}

// TestPreheatRecords_Bounded 去重表容量有界
func TestPreheatRecords_Bounded(t *testing.T) {
	records := newPreheatRecords()
	now := time.Now()

	// 全部记录都在 TTL 内(同一时刻写入), 只能靠容量兜底淘汰
	for i := range preheatDedupLimit * 2 {
		records.markFired(fmt.Sprintf("/影视库/%d.mkv", i), now)
	}

	if got := records.size(); got > preheatDedupLimit {
		t.Errorf("去重表大小 = %d, 不得超过上限 %d", got, preheatDedupLimit)
	}
}

// TestRedactSignedURL 错误文本里的完整签名地址必须被抹掉, 失败原因保留
func TestRedactSignedURL(t *testing.T) {
	signed := "http://node.example.com:8790/dl/L3YvYS5ta3Y?e=1730000000&s=abc123def456"

	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			"传输错误带出完整地址",
			fmt.Errorf(`Get %q: dial tcp 127.0.0.1:1: connect: connection refused`, signed),
			`Get "<签名地址>": dial tcp 127.0.0.1:1: connect: connection refused`,
		},
		{
			"地址在错误文本中出现多次",
			fmt.Errorf("%q -> %q: 上游拒绝", signed, signed),
			`"<签名地址>" -> "<签名地址>": 上游拒绝`,
		},
		{
			"不含地址的错误原样保留",
			errors.New("节点地址不可推导"),
			"节点地址不可推导",
		},
		{
			"空错误返回空串",
			nil,
			"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactSignedURL(tc.err, signed); got != tc.want {
				t.Errorf("redactSignedURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPreheatRecords_PrunesExpired 触发时顺带裁剪过期记录
func TestPreheatRecords_PrunesExpired(t *testing.T) {
	records := newPreheatRecords()
	now := time.Now()

	for i := range 10 {
		records.markFired(fmt.Sprintf("/影视库/旧-%d.mkv", i), now)
	}
	records.markFired("/影视库/新.mkv", now.Add(preheatDedupTTL+time.Second))

	if got := records.size(); got != 1 {
		t.Errorf("裁剪后应只剩 1 条记录, 实际 %d 条", got)
	}
}
