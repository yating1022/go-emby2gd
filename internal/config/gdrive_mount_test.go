package config_test

import (
	"strings"
	"testing"
)

func TestGDriveInit_MountPrefix(t *testing.T) {
	// 合法前缀: 以 '/' 开头且不以 '/' 结尾
	gdrive, err := loadGDriveConfig(t, gdriveConfigEnabled("  mount-prefix: /home/googleDrive\n"))
	if err != nil {
		t.Fatalf("合法挂载前缀不应报错, 实际: %v", err)
	}
	if gdrive.MountPrefix != "/home/googleDrive" {
		t.Errorf("mount-prefix = %q, want /home/googleDrive", gdrive.MountPrefix)
	}

	// 首尾空白会被去除
	gdrive, err = loadGDriveConfig(t, gdriveConfigEnabled("  mount-prefix: \"  /home/googleDrive  \"\n"))
	if err != nil {
		t.Fatalf("挂载前缀应允许首尾空白, 实际: %v", err)
	}
	if gdrive.MountPrefix != "/home/googleDrive" {
		t.Errorf("mount-prefix = %q, want 去除首尾空白后的值", gdrive.MountPrefix)
	}
}

func TestGDriveInit_MountPrefixOptional(t *testing.T) {
	// 未配置时: 无论是否启用都允许留空, 行为与未部署本功能一致
	raws := map[string]string{
		"未启用且留空": "gdrive:\n  enable: false\n",
		"已启用且留空": gdriveConfigEnabled(""),
	}
	for name, raw := range raws {
		t.Run(name, func(t *testing.T) {
			gdrive, err := loadGDriveConfig(t, raw)
			if err != nil {
				t.Fatalf("未配置 mount-prefix 时不应报错, 实际: %v", err)
			}
			if gdrive.MountPrefix != "" {
				t.Errorf("mount-prefix = %q, want 空", gdrive.MountPrefix)
			}
		})
	}
}

func TestGDriveInit_MountPrefixErrors(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
	}{
		{"不以斜杠开头", "home/googleDrive"},
		{"以斜杠结尾", "/home/googleDrive/"},
		{"仅为斜杠", "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadGDriveConfig(t, gdriveConfigEnabled("  mount-prefix: \""+tt.prefix+"\"\n"))
			if err == nil {
				t.Fatal("非法挂载前缀应导致配置初始化报错")
			}
			if !strings.Contains(err.Error(), "mount-prefix") {
				t.Errorf("错误信息 = %q, 应包含配置项名 mount-prefix", err.Error())
			}
		})
	}
}
