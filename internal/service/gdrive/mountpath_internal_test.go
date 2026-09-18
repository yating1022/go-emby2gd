package gdrive

import "testing"

func TestMatchMountPath(t *testing.T) {
	withTestConfig(t, "")

	tests := []struct {
		name          string
		strmContent   string
		wantOK        bool
		wantDrivePath string
	}{
		{
			name:          "命中前缀",
			strmContent:   "/home/googleDrive/影视库/最新电影/72小时 (2026)/72小时 (2026).mkv",
			wantOK:        true,
			wantDrivePath: "/影视库/最新电影/72小时 (2026)/72小时 (2026).mkv",
		},
		{
			name:        "前缀之后没有内容, 说明这本身就是挂载点",
			strmContent: "/home/googleDrive",
			wantOK:      false,
		},
		{
			name:          "前缀之后紧跟斜杠但没有文件名",
			strmContent:   "/home/googleDrive/",
			wantOK:        true,
			wantDrivePath: "/",
		},
		{
			name:        "同前缀但不同目录, 不能被误判",
			strmContent: "/home/googleDriveBackup/x.mkv",
			wantOK:      false,
		},
		{
			name:        "不以前缀开头",
			strmContent: "/mnt/other/x.mkv",
			wantOK:      false,
		},
		{
			name:        "http 地址不是挂载路径",
			strmContent: "http://vault.example.com:7811/api/dl?path=/x.mkv",
			wantOK:      false,
		},
		{
			name:          "首尾空白会被忽略",
			strmContent:   "  /home/googleDrive/影视库/x.mkv  ",
			wantOK:        true,
			wantDrivePath: "/影视库/x.mkv",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPath, gotOK := MatchMountPath(tt.strmContent)
			if gotOK != tt.wantOK {
				t.Fatalf("MatchMountPath(%q) ok = %v, want %v", tt.strmContent, gotOK, tt.wantOK)
			}
			if gotOK && gotPath != tt.wantDrivePath {
				t.Errorf("MatchMountPath(%q) = %q, want %q", tt.strmContent, gotPath, tt.wantDrivePath)
			}
		})
	}
}

func TestMatchMountPath_DisabledNeverMatches(t *testing.T) {
	withDisabledConfig(t)

	if _, ok := MatchMountPath("/home/googleDrive/影视库/x.mkv"); ok {
		t.Error("未启用时不应命中任何路径")
	}
}

func TestMatchMountPath_EmptyPrefixNeverMatches(t *testing.T) {
	cfg := withTestConfig(t, "")
	cfg.MountPrefix = ""

	if _, ok := MatchMountPath("/home/googleDrive/影视库/x.mkv"); ok {
		t.Error("未配置挂载前缀时不应命中任何路径")
	}
}

// TestNormalizeMountPrefix 归一化必须容错末尾多余的斜杠
//
// 配置校验会拦截这种写法, 但测试与将来的配置来源都可能绕过 Init,
// 而 '//' 会让前缀匹配失败, 表现为"功能静默不生效"。
func TestNormalizeMountPrefix(t *testing.T) {
	tests := []struct {
		prefix string
		want   string
	}{
		{"/home/googleDrive", "/home/googleDrive"},
		{"/home/googleDrive/", "/home/googleDrive"},
		{"/home/googleDrive///", "/home/googleDrive"},
		{"  /home/googleDrive  ", "/home/googleDrive"},
		{"home/googleDrive", ""},
		{"", ""},
		{"/", ""},
	}

	for _, tt := range tests {
		if got := normalizeMountPrefix(tt.prefix); got != tt.want {
			t.Errorf("normalizeMountPrefix(%q) = %q, want %q", tt.prefix, got, tt.want)
		}
	}
}
