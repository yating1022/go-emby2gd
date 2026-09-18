package config_test

import (
	"strings"
	"testing"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"

	"gopkg.in/yaml.v3"
)

// gdriveApiToken 测试用面板令牌
//
// 单独抽成常量, 便于逐处断言它不会出现在错误消息或日志里。
const gdriveApiToken = "test-api-token"

// clearGDriveTokenEnv 清空令牌环境变量
//
// 保证"期望配置报错""期望用配置值"这类用例不受运行环境里已导出的
// GDRIVE_API_TOKEN 影响 —— 否则同一份测试在开发机与 CI 上结论可能不同。
func clearGDriveTokenEnv(t *testing.T) {
	t.Helper()
	t.Setenv(config.GDriveApiTokenEnvName, "")
}

// loadGDriveConfig 解析 yaml 并完成 gdrive 配置初始化
func loadGDriveConfig(t *testing.T, raw string) (*config.GDrive, error) {
	t.Helper()

	cfg := new(config.Config)
	if err := yaml.Unmarshal([]byte(raw), cfg); err != nil {
		t.Fatalf("解析 yaml 失败: %v", err)
	}
	if cfg.GDrive == nil {
		cfg.GDrive = new(config.GDrive)
	}
	return cfg.GDrive, cfg.GDrive.Init()
}

// gdriveConfigEnabled 生成一份启用且合法的 gdrive 配置
//
// extra 的每一行需要缩进 2 个空格。
func gdriveConfigEnabled(extra string) string {
	return "gdrive:\n" +
		"  enable: true\n" +
		"  api-base: https://gd.example.com\n" +
		"  api-token: " + gdriveApiToken + "\n" + extra
}

func TestGDriveInit_Enabled(t *testing.T) {
	clearGDriveTokenEnv(t)

	gdrive, err := loadGDriveConfig(t, gdriveConfigEnabled(""))
	if err != nil {
		t.Fatalf("配置初始化返回错误: %v", err)
	}

	if !gdrive.IsEnabled() {
		t.Error("enable 为 true 时应处于启用状态")
	}
	if gdrive.ApiBase != "https://gd.example.com" {
		t.Errorf("api-base = %q, want https://gd.example.com", gdrive.ApiBase)
	}
	if gdrive.ApiToken != gdriveApiToken {
		t.Errorf("api-token = %q, want %q", gdrive.ApiToken, gdriveApiToken)
	}
}

func TestGDriveInit_AbsentSection(t *testing.T) {
	clearGDriveTokenEnv(t)

	gdrive, err := loadGDriveConfig(t, "emby:\n  host: http://emby.local:8096\n")
	if err != nil {
		t.Fatalf("缺省 gdrive 段时应能正常初始化, 实际错误: %v", err)
	}
	if gdrive.IsEnabled() {
		t.Error("缺省 gdrive 段时不应启用")
	}
	if gdrive.ApiBase != "" || gdrive.ApiToken != "" || gdrive.MountPrefix != "" {
		t.Errorf("缺省 gdrive 段时各字段应为空, 实际: %+v", gdrive)
	}
}

func TestGDriveInit_TrimsFields(t *testing.T) {
	clearGDriveTokenEnv(t)

	gdrive, err := loadGDriveConfig(t, "gdrive:\n"+
		"  enable: true\n"+
		"  api-base: \"  https://gd.example.com  \"\n"+
		"  api-token: \"  "+gdriveApiToken+"  \"\n")
	if err != nil {
		t.Fatalf("配置初始化返回错误: %v", err)
	}

	if gdrive.ApiBase != "https://gd.example.com" {
		t.Errorf("api-base = %q, want 去除首尾空白后的值", gdrive.ApiBase)
	}
	if gdrive.ApiToken != gdriveApiToken {
		t.Errorf("api-token = %q, want 去除首尾空白后的值", gdrive.ApiToken)
	}
}

// TestGDriveInit_ApiBaseTrailingSlash 结尾斜杠必须被去掉
//
// 否则拼接出的是 `https://host//api/dl`, 某些反向代理会把它当成另一个路径而 404。
func TestGDriveInit_ApiBaseTrailingSlash(t *testing.T) {
	clearGDriveTokenEnv(t)

	for _, raw := range []string{
		"https://gd.example.com/",
		"https://gd.example.com///",
	} {
		gdrive, err := loadGDriveConfig(t, "gdrive:\n"+
			"  enable: true\n"+
			"  api-base: \""+raw+"\"\n"+
			"  api-token: "+gdriveApiToken+"\n")
		if err != nil {
			t.Fatalf("api-base %q 应能初始化, 实际: %v", raw, err)
		}
		if gdrive.ApiBase != "https://gd.example.com" {
			t.Errorf("api-base = %q, want 去掉结尾斜杠后的 https://gd.example.com", gdrive.ApiBase)
		}
	}
}

// TestGDriveInit_ApiBaseWithSubPath 面板挂在反向代理子路径下时应当允许
func TestGDriveInit_ApiBaseWithSubPath(t *testing.T) {
	clearGDriveTokenEnv(t)

	gdrive, err := loadGDriveConfig(t, "gdrive:\n"+
		"  enable: true\n"+
		"  api-base: https://example.com/gd/\n"+
		"  api-token: "+gdriveApiToken+"\n")
	if err != nil {
		t.Fatalf("带子路径的 api-base 应能初始化, 实际: %v", err)
	}
	if gdrive.ApiBase != "https://example.com/gd" {
		t.Errorf("api-base = %q, want https://example.com/gd", gdrive.ApiBase)
	}
}

// TestGDriveInit_ApiTokenFromEnv 环境变量非空时覆盖配置值
func TestGDriveInit_ApiTokenFromEnv(t *testing.T) {
	t.Setenv(config.GDriveApiTokenEnvName, "env-token")

	gdrive, err := loadGDriveConfig(t, gdriveConfigEnabled(""))
	if err != nil {
		t.Fatalf("配置初始化返回错误: %v", err)
	}
	if gdrive.ApiToken != "env-token" {
		t.Errorf("api-token = %q, want 环境变量值 env-token", gdrive.ApiToken)
	}
}

// TestGDriveInit_ApiTokenEnvBlankDoesNotOverride 环境变量为空时不覆盖
//
// 空值表示"没有设置", 而不是"把配置值清空" —— 否则容器里一个未定义的
// 环境变量就能把配置文件里的令牌抹掉, 表现为莫名其妙的 401。
func TestGDriveInit_ApiTokenEnvBlankDoesNotOverride(t *testing.T) {
	for _, value := range []string{"", "   "} {
		t.Setenv(config.GDriveApiTokenEnvName, value)

		gdrive, err := loadGDriveConfig(t, gdriveConfigEnabled(""))
		if err != nil {
			t.Fatalf("配置初始化返回错误: %v", err)
		}
		if gdrive.ApiToken != gdriveApiToken {
			t.Errorf("环境变量为 %q 时不应覆盖配置值, 实际 api-token = %q", value, gdrive.ApiToken)
		}
	}
}

// TestGDriveInit_ApiTokenFromEnvSatisfiesRequired 环境变量可以补足启用所需的令牌
func TestGDriveInit_ApiTokenFromEnvSatisfiesRequired(t *testing.T) {
	t.Setenv(config.GDriveApiTokenEnvName, "env-token")

	_, err := loadGDriveConfig(t, "gdrive:\n"+
		"  enable: true\n"+
		"  api-base: https://gd.example.com\n")
	if err != nil {
		t.Fatalf("环境变量提供了 api-token 时不应报错, 实际: %v", err)
	}
}

func TestGDriveInit_Errors(t *testing.T) {
	clearGDriveTokenEnv(t)

	tests := []struct {
		name     string
		raw      string
		wantWord string
		// wantAbsent 非空时断言错误信息里【不出现】该片段(用于凭据回显检查)
		wantAbsent string
	}{
		{
			name:     "启用时缺少 api-base",
			raw:      "gdrive:\n  enable: true\n  api-token: " + gdriveApiToken + "\n",
			wantWord: "api-base",
		},
		{
			name:     "启用时缺少 api-token",
			raw:      "gdrive:\n  enable: true\n  api-base: https://gd.example.com\n",
			wantWord: "api-token",
		},
		{
			name:     "api-base 为空白字符",
			raw:      "gdrive:\n  enable: true\n  api-base: \"   \"\n  api-token: " + gdriveApiToken + "\n",
			wantWord: "api-base",
		},
		{
			name:     "api-token 为空白字符",
			raw:      "gdrive:\n  enable: true\n  api-base: https://gd.example.com\n  api-token: \"   \"\n",
			wantWord: "api-token",
		},
		{
			name:     "api-base scheme 非法",
			raw:      "gdrive:\n  enable: false\n  api-base: ftp://gd.example.com\n",
			wantWord: "api-base",
		},
		{
			name:     "api-base 缺少主机名",
			raw:      "gdrive:\n  enable: false\n  api-base: https:///api\n",
			wantWord: "api-base",
		},
		{
			name:     "api-base 带查询参数",
			raw:      "gdrive:\n  enable: false\n  api-base: https://gd.example.com?token=x\n",
			wantWord: "api-base",
		},
		{
			// 查询串里的口令最容易顺手写进配置, 因此这条要同时断言"不回显"
			name:       "api-base 带 userinfo",
			raw:        "gdrive:\n  enable: false\n  api-base: https://user:leaked-secret@panel.example.com\n",
			wantWord:   "api-base",
			wantAbsent: "leaked-secret",
		},
		{
			name:       "拒绝的查询参数不回显",
			raw:        "gdrive:\n  enable: false\n  api-base: https://gd.example.com?token=leaked-secret\n",
			wantWord:   "api-base",
			wantAbsent: "leaked-secret",
		},
		{
			name:     "未启用时配置了非法面板地址同样启动即报错",
			raw:      "gdrive:\n  enable: false\n  api-base: not-a-url\n",
			wantWord: "api-base",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadGDriveConfig(t, tt.raw)
			if err == nil {
				t.Fatalf("期望配置初始化报错, 实际通过")
			}
			if !strings.Contains(err.Error(), tt.wantWord) {
				t.Errorf("错误信息 = %q, 应包含配置项名 %q", err.Error(), tt.wantWord)
			}
			// 校验错误消息里只提字段名, 绝不回显凭据值
			if strings.Contains(err.Error(), gdriveApiToken) {
				t.Errorf("错误信息不得回显凭据, 实际: %v", err)
			}
			if tt.wantAbsent != "" && strings.Contains(err.Error(), tt.wantAbsent) {
				t.Errorf("错误信息不得回显 %q, 实际: %v", tt.wantAbsent, err)
			}
		})
	}
}
