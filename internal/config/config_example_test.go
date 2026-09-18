package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
)

// TestConfigExample_Loadable 配置示例文件必须能通过完整的加载与校验
//
// 避免示例文件与配置结构体脱节, 尤其是在新增配置段之后
func TestConfigExample_Loadable(t *testing.T) {
	examplePath := filepath.Join("..", "..", "config-example.yml")
	if _, err := os.Stat(examplePath); err != nil {
		t.Skipf("找不到配置示例文件: %v", err)
	}

	config.C = nil
	if err := config.ReadFromFile(examplePath); err != nil {
		t.Fatalf("加载配置示例文件失败: %v", err)
	}

	if config.C.Emby == nil || config.C.Emby.Strm == nil || config.C.Emby.Strm.Proxy == nil {
		t.Fatal("配置示例文件应包含 emby.strm.proxy 段")
	}
	if config.C.Emby.StrmProxyEnabled() {
		t.Error("配置示例文件的 emby.strm.proxy.enable 应默认为 false")
	}

	proxy := config.C.Emby.StrmProxyConfig()
	if proxy.MaxConcurrentStreams != 16 {
		t.Errorf("配置示例文件显式配置的 max-concurrent-streams = %d, want 16", proxy.MaxConcurrentStreams)
	}
	if want := "http://vault.example.com:7811"; len(proxy.DomainsNormalized()) != 1 || proxy.DomainsNormalized()[0] != want {
		t.Errorf("配置示例文件的 domains 归一化结果 = %v, want [%s]", proxy.DomainsNormalized(), want)
	}

	if config.C.GDrive == nil {
		t.Fatal("配置示例文件应包含 gdrive 段")
	}
	if config.C.GDrive.IsEnabled() {
		t.Error("配置示例文件的 gdrive.enable 应默认为 false")
	}
	// api-base 的结尾斜杠必须被归一化掉, 否则示例文件本身就会拼出 //api/dl
	if got := config.C.GDrive.ApiBase; got != "https://gd.bjyt.de" {
		t.Errorf("配置示例文件的 gdrive.api-base = %q, want 去掉结尾斜杠后的 https://gd.bjyt.de", got)
	}
	if got := config.C.GDrive.MountPrefix; got != "/home/googleDrive" {
		t.Errorf("配置示例文件的 gdrive.mount-prefix = %q, want /home/googleDrive", got)
	}
}
