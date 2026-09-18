package streamproxy_test

import (
	"strings"
	"testing"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/streamproxy"
)

// proxyConfigForTest 构造一份可直接使用的 strm 直链代理配置
func proxyConfigForTest(t *testing.T, domains []string) *config.StrmProxy {
	t.Helper()
	proxy := &config.StrmProxy{
		Enable:               true,
		Domains:              domains,
		LinkCacheExpired:     "10m",
		MaxRedirectDepth:     5,
		MaxConcurrentStreams: 16,
	}
	if err := proxy.Init(); err != nil {
		t.Fatalf("初始化 strm 直链代理配置失败: %v", err)
	}
	return proxy
}

// setupProxyConfig 将配置注入全局配置对象
//
// 同时把并发信号量重置为该配置的上限: 信号量在进程中只初始化一次,
// 不重置的话会沿用前一个用例留下的上限, 用例之间互相影响
func setupProxyConfig(t *testing.T, proxy *config.StrmProxy) {
	t.Helper()
	config.C = &config.Config{Emby: &config.Emby{Strm: &config.Strm{Proxy: proxy}}}
	streamproxy.ResetSlotsForTest(proxy.MaxConcurrentStreams)
}

// containsBareUnsafe 判断字符串中是否存在裸空格或裸非 ASCII 字节
func containsBareUnsafe(s string) bool {
	if strings.ContainsAny(s, " ") {
		return true
	}
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7F {
			return true
		}
	}
	return false
}

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{
			name: "含裸空格与中文的查询串",
			raw:  "http://vault.bjyt.de:7811/redirect?path=/影视库/最新电影/世界第一初恋：求婚篇 (2020)/世界第一初恋：求婚篇 (2020).mkv&pickcode=18Vozg&storage=googledrive-1",
		},
		{
			name: "路径含裸空格",
			raw:  "http://vault.bjyt.de:7811/影视库/最新电影/a b.mkv",
		},
		{
			name: "已经是规范编码的地址",
			raw:  "http://vault.bjyt.de:7811/redirect?pickcode=18Vozg&storage=googledrive-1",
		},
		{
			name: "查询串含字面加号",
			raw:  "http://vault.bjyt.de:7811/redirect?pickcode=a+b&storage=g",
		},
		{
			name: "无查询串的普通地址",
			raw:  "http://vault.bjyt.de:7811/media/movie.mp4",
		},
		{name: "空地址", raw: "   ", wantErr: true},
		{name: "无法解析的地址", raw: "http://[::1/x", wantErr: true},
		{name: "非法转义", raw: "http://vault.bjyt.de:7811/%zz", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := streamproxy.NormalizeURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeURL() 期望返回错误, 实际得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeURL() 返回错误: %v", err)
			}
			if containsBareUnsafe(got) {
				t.Errorf("NormalizeURL() = %q, 仍然包含裸空格或裸非 ASCII 字节", got)
			}

			// 幂等性: 归一化两次的结果必须一致
			again, err := streamproxy.NormalizeURL(got)
			if err != nil {
				t.Fatalf("二次归一化失败: %v", err)
			}
			if again != got {
				t.Errorf("归一化不是幂等的: 第一次 %q, 第二次 %q", got, again)
			}
		})
	}
}

func TestNormalizeURL_KeepsQuerySemantics(t *testing.T) {
	raw := "http://vault.bjyt.de:7811/redirect?path=/a b/c d.mkv&pickcode=p1&storage=g1"
	got, err := streamproxy.NormalizeURL(raw)
	if err != nil {
		t.Fatalf("NormalizeURL() 返回错误: %v", err)
	}

	for _, want := range []string{"pickcode=p1", "storage=g1", "%20", "%2F"} {
		if !strings.Contains(got, want) {
			t.Errorf("NormalizeURL() = %q, 缺少片段 %q", got, want)
		}
	}
	if strings.Contains(got, "+") {
		t.Errorf("NormalizeURL() = %q, 不应残留 '+' 号", got)
	}
}

func TestMatchDomain(t *testing.T) {
	tests := []struct {
		name    string
		domains []string
		raw     string
		want    string
		wantOk  bool
	}{
		{
			name:    "完全命中",
			domains: []string{"http://vault.bjyt.de:7811"},
			raw:     "http://vault.bjyt.de:7811/redirect?path=/a.mkv",
			want:    "http://vault.bjyt.de:7811",
			wantOk:  true,
		},
		{
			name:    "端口号边界不成立",
			domains: []string{"http://vault.bjyt.de:7811"},
			raw:     "http://vault.bjyt.de:78111.evil.com/x",
			wantOk:  false,
		},
		{
			name:    "域名边界不成立",
			domains: []string{"http://vault.bjyt.de:7811"},
			raw:     "http://vault.bjyt.de:7811.evil.com/x",
			wantOk:  false,
		},
		{
			name:    "scheme 大小写归一",
			domains: []string{"HTTP://vault.bjyt.de:7811"},
			raw:     "HTTP://vault.bjyt.de:7811/x",
			want:    "http://vault.bjyt.de:7811",
			wantOk:  true,
		},
		{
			name:    "配置项尾部斜杠",
			domains: []string{"http://vault.bjyt.de:7811/"},
			raw:     "http://vault.bjyt.de:7811/x",
			want:    "http://vault.bjyt.de:7811",
			wantOk:  true,
		},
		{
			name:    "命中路径前缀",
			domains: []string{"http://vault.bjyt.de:7811/prefix"},
			raw:     "http://vault.bjyt.de:7811/prefix/x.mkv",
			want:    "http://vault.bjyt.de:7811/prefix",
			wantOk:  true,
		},
		{
			name:    "路径前缀不匹配",
			domains: []string{"http://vault.bjyt.de:7811/prefix"},
			raw:     "http://vault.bjyt.de:7811/other/x.mkv",
			wantOk:  false,
		},
		{
			name:    "前缀后带查询串即边界成立",
			domains: []string{"http://vault.bjyt.de:7811"},
			raw:     "http://vault.bjyt.de:7811?path=/a.mkv",
			want:    "http://vault.bjyt.de:7811",
			wantOk:  true,
		},
		{
			name:    "地址与前缀完全相同",
			domains: []string{"http://vault.bjyt.de:7811"},
			raw:     "http://vault.bjyt.de:7811",
			want:    "http://vault.bjyt.de:7811",
			wantOk:  true,
		},
		{
			name:    "多个前缀时取第一个命中",
			domains: []string{"http://a.example.com", "http://b.example.com"},
			raw:     "http://b.example.com/x",
			want:    "http://b.example.com",
			wantOk:  true,
		},
		{
			name:    "多前缀时按顺序取第一个(即使后面的也命中)",
			domains: []string{"http://b.example.com", "http://b.example.com/x"},
			raw:     "http://b.example.com/x",
			want:    "http://b.example.com",
			wantOk:  true,
		},
		{
			name:    "完全未命中",
			domains: []string{"http://vault.bjyt.de:7811"},
			raw:     "https://other.example.com/x",
			wantOk:  false,
		},
		{
			name:    "空地址",
			domains: []string{"http://vault.bjyt.de:7811"},
			raw:     "",
			wantOk:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupProxyConfig(t, proxyConfigForTest(t, tt.domains))
			got, ok := streamproxy.MatchDomain(tt.raw)
			if ok != tt.wantOk {
				t.Fatalf("MatchDomain() ok = %v, want %v (prefix=%q)", ok, tt.wantOk, got)
			}
			if ok && got != tt.want {
				t.Errorf("MatchDomain() prefix = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMatchDomain_Disabled(t *testing.T) {
	proxy := proxyConfigForTest(t, []string{"http://vault.bjyt.de:7811"})
	proxy.Enable = false
	if err := proxy.Init(); err != nil {
		t.Fatalf("初始化配置失败: %v", err)
	}
	setupProxyConfig(t, proxy)

	if _, ok := streamproxy.MatchDomain("http://vault.bjyt.de:7811/x"); ok {
		t.Error("MatchDomain() 在未开启代理时不应命中")
	}
	if streamproxy.ProxyEnabled() {
		t.Error("ProxyEnabled() 在未开启代理时应返回 false")
	}
}
