package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"

	"gopkg.in/yaml.v3"
)

// loadEmbyConfig 解析 yaml 并完成 emby 配置初始化
func loadEmbyConfig(t *testing.T, raw string) (*config.Emby, error) {
	t.Helper()
	cfg := new(config.Config)
	if err := yaml.Unmarshal([]byte(raw), cfg); err != nil {
		t.Fatalf("解析 yaml 失败: %v", err)
	}
	if cfg.Emby == nil {
		cfg.Emby = new(config.Emby)
	}
	return cfg.Emby, cfg.Emby.Init()
}

// embyConfigWith 生成一份 strm.proxy 段内容位于 emby.strm 下的 yaml
//
// proxyExtra 的每一行需要缩进 6 个空格
func embyConfigWith(proxyExtra string) string {
	return "emby:\n  host: http://emby.local:8096\n  strm:\n    proxy:\n" + proxyExtra
}

func TestEmbyInit_StrmProxyDefaults(t *testing.T) {
	emby, err := loadEmbyConfig(t, embyConfigWith("      enable: false\n      domains:\n        - http://vault.example.com:7811\n"))
	if err != nil {
		t.Fatalf("配置初始化返回错误: %v", err)
	}

	proxy := emby.StrmProxyConfig()
	if proxy.Enable {
		t.Error("enable 应默认为 false")
	}
	if proxy.MaxRedirectDepth != 5 {
		t.Errorf("max-redirect-depth 默认值 = %d, want 5", proxy.MaxRedirectDepth)
	}
	if proxy.MaxConcurrentStreams != 16 {
		t.Errorf("max-concurrent-streams 默认值 = %d, want 16", proxy.MaxConcurrentStreams)
	}
	if proxy.LinkCacheExpire() != time.Minute*10 {
		t.Errorf("link-cache-expired 默认值 = %v, want 10m", proxy.LinkCacheExpire())
	}
	for _, code := range []int{403, 404, 410} {
		if _, ok := proxy.RetryCodes()[code]; !ok {
			t.Errorf("retry-status-codes 默认值缺少 %d", code)
		}
	}
	domains := proxy.DomainsNormalized()
	if len(domains) != 1 || domains[0] != "http://vault.example.com:7811" {
		t.Errorf("domains 归一化结果 = %v", domains)
	}
}

func TestEmbyInit_StrmProxyAbsentSection(t *testing.T) {
	emby, err := loadEmbyConfig(t, "emby:\n  host: http://emby.local:8096\n")
	if err != nil {
		t.Fatalf("缺省 proxy 段时应能正常初始化, 实际错误: %v", err)
	}
	if emby.StrmProxyEnabled() {
		t.Error("缺省 proxy 段时 StrmProxyEnabled() 应为 false")
	}
	if got := emby.StrmProxyConfig().MaxConcurrentStreams; got != 16 {
		t.Errorf("缺省 proxy 段时 max-concurrent-streams = %d, want 16", got)
	}
}

func TestEmbyInit_StrmProxyExplicitZeroMeansUnlimited(t *testing.T) {
	emby, err := loadEmbyConfig(t, embyConfigWith("      enable: true\n      domains:\n        - http://vault.example.com:7811\n      max-concurrent-streams: 0\n"))
	if err != nil {
		t.Fatalf("配置初始化返回错误: %v", err)
	}
	if got := emby.StrmProxyConfig().MaxConcurrentStreams; got != 0 {
		t.Errorf("显式配置 max-concurrent-streams: 0 应保持 0(不限制), 实际 %d", got)
	}
}

func TestEmbyInit_StrmProxyErrors(t *testing.T) {
	tests := []struct {
		name     string
		proxy    string
		wantWord string
	}{
		{
			name:     "开启代理但未配置域名",
			proxy:    "      enable: true\n",
			wantWord: "domains",
		},
		{
			name:     "域名不是 http 地址",
			proxy:    "      domains:\n        - ftp://vault.example.com\n",
			wantWord: "domains",
		},
		{
			name:     "域名为空白",
			proxy:    "      domains:\n        - \"   \"\n",
			wantWord: "domains",
		},
		{
			name:     "缓存时长单位非法",
			proxy:    "      domains:\n        - http://a.example.com\n      link-cache-expired: 10x\n",
			wantWord: "link-cache-expired",
		},
		{
			name:     "缓存时长为 0",
			proxy:    "      domains:\n        - http://a.example.com\n      link-cache-expired: 0m\n",
			wantWord: "link-cache-expired",
		},
		{
			name:     "重定向跳数超过上限",
			proxy:    "      domains:\n        - http://a.example.com\n      max-redirect-depth: 11\n",
			wantWord: "max-redirect-depth",
		},
		{
			name:     "重定向跳数为负",
			proxy:    "      domains:\n        - http://a.example.com\n      max-redirect-depth: -1\n",
			wantWord: "max-redirect-depth",
		},
		{
			name:     "失效状态码非法",
			proxy:    "      domains:\n        - http://a.example.com\n      retry-status-codes: [99]\n",
			wantWord: "retry-status-codes",
		},
		{
			name:     "并发上限为负",
			proxy:    "      domains:\n        - http://a.example.com\n      max-concurrent-streams: -1\n",
			wantWord: "max-concurrent-streams",
		},
		{
			name:     "固定请求头名称为空",
			proxy:    "      domains:\n        - http://a.example.com\n      request-header:\n        \"\": value\n",
			wantWord: "request-header",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadEmbyConfig(t, embyConfigWith(tt.proxy))
			if err == nil {
				t.Fatalf("期望配置初始化报错, 实际通过")
			}
			if !strings.Contains(err.Error(), tt.wantWord) {
				t.Errorf("错误信息 = %q, 应包含配置项名 %q", err.Error(), tt.wantWord)
			}
		})
	}
}

func TestEmbyInit_StrmProxyNormalizesDomains(t *testing.T) {
	emby, err := loadEmbyConfig(t, embyConfigWith("      domains:\n        - HTTP://Vault.Example.com:7811/\n        - http://b.example.com/path/\n"))
	if err != nil {
		t.Fatalf("配置初始化返回错误: %v", err)
	}

	proxy := emby.StrmProxyConfig()
	got := proxy.DomainsNormalized()
	want := []string{"http://Vault.Example.com:7811", "http://b.example.com/path"}
	if len(got) != len(want) {
		t.Fatalf("domains 数量 = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("domains[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
