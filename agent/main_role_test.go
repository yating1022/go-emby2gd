package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ROLE 在 main 层的测试（任务 10-10-hub-agent-mode S3/S8）：
//   - enroll --role 非法 → 用法错误退出 2（在发请求之前）；
//   - serve 读到 ROLE=hub 时走 hub 装配（磁盘缓存 / HUB_PORT 监听），
//     node 分支不受影响（既有 main_test.go 的测试继续覆盖）。

// clearServeEnv 清掉 serve/enroll 会读的环境变量（含 hub 专属键，
// 防止开发机上残留的同名变量把测试带偏）。
func clearServeEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"MASTER_URL", "AGENT_ID", "AGENT_SECRET", "SIGN_KEY", "ROLE",
		"LISTEN_PORT", "HUB_PORT", "HUB_ALLOW_IPS", "DISK_CACHE_DIR", "DISK_BUDGET_GB",
		"WARM_HEAD_BYTES", "WARM_TAIL_BYTES", "WARM_RESUME_WINDOW_BYTES",
	} {
		t.Setenv(key, "")
	}
}

// syncBuffer 是并发安全的输出缓冲：serve 的日志来自多个 goroutine
// （heartbeat / sweeper），-race 下普通 bytes.Buffer 会被判数据竞争。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestEnrollInvalidRoleExitsWithUsage(t *testing.T) {
	code, _, stderr := runArgs(t, "enroll", "--role", "worker")
	if code != 2 {
		t.Fatalf("非法 --role 应退出 2，实际 %d", code)
	}
	if !strings.Contains(stderr, "--role") || !strings.Contains(stderr, "node") || !strings.Contains(stderr, "hub") {
		t.Fatalf("用法错误应点名 --role 与合法值：%q", stderr)
	}
}

// hub 启动时磁盘缓存不可用必须立刻失败退出（而不是裸监听一个没缓存的端口）。
func TestServeHubFailsFastWhenDiskCacheUnusable(t *testing.T) {
	clearServeEnv(t)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("准备占位文件失败：%v", err)
	}
	configPath := filepath.Join(t.TempDir(), "config.env")
	body := "MASTER_URL=http://127.0.0.1:1\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\n" +
		"ROLE=hub\nDISK_CACHE_DIR=" + filepath.Join(blocker, "sub") + "\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("写配置失败：%v", err)
	}
	code, _, stderr := runArgs(t, "serve", "--config", configPath)
	if code != 1 {
		t.Fatalf("磁盘缓存不可用应退出 1，实际 %d（stderr=%q）", code, stderr)
	}
	if !strings.Contains(stderr, "serve 启动失败") || !strings.Contains(stderr, "磁盘缓存目录") {
		t.Fatalf("错误应是可读的中文（点名磁盘缓存目录）：%q", stderr)
	}
}

// hub 分支必须监听 HUB_PORT（而不是 node 的 LISTEN_PORT）：
// 事先占住 HUB_PORT，hub 应在监听失败后快速退出 1。
func TestServeHubListensOnHubPort(t *testing.T) {
	clearServeEnv(t)
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("占用端口失败：%v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	configPath := filepath.Join(t.TempDir(), "config.env")
	body := "MASTER_URL=http://127.0.0.1:1\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\n" +
		"ROLE=hub\nHUB_PORT=" + fmt.Sprint(port) +
		"\nDISK_CACHE_DIR=" + t.TempDir() + "\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("写配置失败：%v", err)
	}

	out, errOut := &syncBuffer{}, &syncBuffer{}
	done := make(chan int, 1)
	go func() { done <- run([]string{"serve", "--config", configPath}, out, errOut) }()
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("HUB_PORT 被占用应退出 1，实际 %d（stderr=%q）", code, errOut.String())
		}
		if !strings.Contains(out.String(), "启动（hub）") {
			t.Fatalf("应走 hub 装配（日志应含“启动（hub）”）：%q", out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve 未按预期快速失败：实现的监听口可能不是 HUB_PORT")
	}
}
