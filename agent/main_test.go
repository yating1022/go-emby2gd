package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func runArgs(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestVersionSubcommand(t *testing.T) {
	code, stdout, _ := runArgs(t, "version")
	if code != 0 {
		t.Fatalf("version 应退出 0，实际 %d", code)
	}
	if !strings.HasPrefix(stdout, "gd-agent ") {
		t.Fatalf("version 输出不对：%q", stdout)
	}
}

func TestHelpAndUsage(t *testing.T) {
	code, stdout, _ := runArgs(t, "help")
	if code != 0 || !strings.Contains(stdout, "enroll") || !strings.Contains(stdout, "serve") {
		t.Fatalf("help 输出不对：code=%d stdout=%q", code, stdout)
	}
	code, _, stderr := runArgs(t)
	if code != 2 || !strings.Contains(stderr, "用法") {
		t.Fatalf("无参数应打用法并退出 2：code=%d stderr=%q", code, stderr)
	}
}

func TestUnknownSubcommand(t *testing.T) {
	code, _, stderr := runArgs(t, "frobnicate")
	if code != 2 || !strings.Contains(stderr, "未知子命令") {
		t.Fatalf("未知子命令应退出 2：code=%d stderr=%q", code, stderr)
	}
}

func TestUnknownFlag(t *testing.T) {
	code, _, _ := runArgs(t, "enroll", "--不存在的参数")
	if code != 2 {
		t.Fatalf("未知参数应退出 2，实际 %d", code)
	}
}

func TestEnrollWithoutMasterFails(t *testing.T) {
	code, _, stderr := runArgs(t, "enroll", "--token", "t", "--config", filepath.Join(t.TempDir(), "config.env"))
	if code != 1 {
		t.Fatalf("缺 --master 应失败退出 1，实际 %d", code)
	}
	if !strings.Contains(stderr, "enroll 失败") || !strings.Contains(stderr, "master") {
		t.Fatalf("错误信息应是中文可读：%q", stderr)
	}
}

func TestEnrollWithoutTokenFails(t *testing.T) {
	code, _, stderr := runArgs(t, "enroll", "--master", "http://127.0.0.1:1")
	if code != 1 {
		t.Fatalf("缺 --token 应失败退出 1，实际 %d", code)
	}
	if !strings.Contains(stderr, "Token") {
		t.Fatalf("错误信息应提示注册 Token：%q", stderr)
	}
}

func TestServeWithoutConfigFails(t *testing.T) {
	// 清掉可能存在的环境变量，保证走"缺配置"分支。
	for _, key := range []string{"MASTER_URL", "AGENT_ID", "AGENT_SECRET", "SIGN_KEY"} {
		t.Setenv(key, "")
	}
	code, _, stderr := runArgs(t, "serve", "--config", filepath.Join(t.TempDir(), "nope.env"))
	if code != 1 {
		t.Fatalf("缺配置应退出 1，实际 %d", code)
	}
	if !strings.Contains(stderr, "serve 启动失败") {
		t.Fatalf("错误信息应是中文可读：%q", stderr)
	}
}
