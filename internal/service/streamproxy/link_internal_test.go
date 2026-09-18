package streamproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
)

// internalProxyConfig 构造并注入一份测试配置
func internalProxyConfig(t *testing.T, mutate func(*config.StrmProxy)) *config.StrmProxy {
	t.Helper()

	proxy := &config.StrmProxy{
		Enable:               true,
		Domains:              []string{"http://placeholder.local"},
		LinkCacheExpired:     "10m",
		MaxRedirectDepth:     5,
		MaxConcurrentStreams: 16,
	}
	if mutate != nil {
		mutate(proxy)
	}
	if err := proxy.Init(); err != nil {
		t.Fatalf("初始化 strm 直链代理配置失败: %v", err)
	}
	config.C = &config.Config{Emby: &config.Emby{Strm: &config.Strm{Proxy: proxy}}}
	ResetSlotsForTest(proxy.MaxConcurrentStreams)
	return proxy
}

func TestBuildUpstreamHeader_AcceptEncodingIdentity(t *testing.T) {
	tests := []struct {
		name         string
		clientHeader http.Header
		requestHead  map[string]string
	}{
		{
			name:         "客户端带 gzip",
			clientHeader: http.Header{"Accept-Encoding": {"gzip, deflate"}},
		},
		{
			name:         "客户端不带 Accept-Encoding",
			clientHeader: http.Header{},
		},
		{
			name:        "配置试图覆盖为 gzip",
			requestHead: map[string]string{"Accept-Encoding": "gzip"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.StrmProxy{RequestHeader: tt.requestHead}
			if err := cfg.Init(); err != nil {
				t.Fatalf("初始化配置失败: %v", err)
			}

			got := buildUpstreamHeader(tt.clientHeader, cfg).Get("Accept-Encoding")
			if got != "identity" {
				t.Errorf("上游 Accept-Encoding = %q, want identity", got)
			}
		})
	}
}

func TestLinkCache_WriteAndHit(t *testing.T) {
	key := "http://cache.example.com/orig"
	finalURL := "http://cdn.example.com/final"

	putCachedLink(key, finalURL, time.Minute)
	got, ok := getCachedLink(key)
	if !ok {
		t.Fatal("写入后应命中缓存")
	}
	if got != finalURL {
		t.Errorf("缓存地址 = %q, want %q", got, finalURL)
	}

	removeCachedLink(key)
	if _, ok := getCachedLink(key); ok {
		t.Error("删除后不应再命中缓存")
	}
}

func TestLinkCache_ExpiredEntryLazilyCleaned(t *testing.T) {
	key := "http://cache.example.com/expired"
	putCachedLink(key, "http://cdn.example.com/final", -time.Second)

	if _, ok := getCachedLink(key); ok {
		t.Error("过期条目不应命中")
	}
	if _, exists := linkCache.Load(key); exists {
		t.Error("过期条目应在读取时被惰性清理")
	}
}

func TestResolveLink_DirectResponseNotCached(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("direct"))
	}))
	defer upstream.Close()

	internalProxyConfig(t, func(proxy *config.StrmProxy) { proxy.Domains = []string{upstream.URL} })

	normalized, err := NormalizeURL(upstream.URL + "/media.mp4")
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}

	resp, err := resolveLink(context.Background(), normalized, http.Header{})
	if err != nil {
		t.Fatalf("resolveLink() 返回错误: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("响应码 = %d, want 200", resp.StatusCode)
	}
	if _, ok := getCachedLink(normalized); ok {
		t.Error("上游直接返回 2xx 时不应写入直链缓存")
	}
}

func TestResolveLink_CachesFinalURLAfterRedirect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/orig" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		w.Write([]byte("final"))
	}))
	defer upstream.Close()

	internalProxyConfig(t, func(proxy *config.StrmProxy) { proxy.Domains = []string{upstream.URL} })

	normalized := upstream.URL + "/orig"
	resp, err := resolveLink(context.Background(), normalized, http.Header{})
	if err != nil {
		t.Fatalf("resolveLink() 返回错误: %v", err)
	}
	resp.Body.Close()

	cached, ok := getCachedLink(normalized)
	if !ok {
		t.Fatal("发生过跳转后应写入直链缓存")
	}
	if want := upstream.URL + "/final"; cached != want {
		t.Errorf("缓存地址 = %q, want %q", cached, want)
	}

	removeCachedLink(normalized)
}

func TestResolveLink_ClearsInvalidCachedLinkAndRetries(t *testing.T) {
	var requestCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		if r.URL.Path == "/cached" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write([]byte("fresh"))
	}))
	defer upstream.Close()

	internalProxyConfig(t, func(proxy *config.StrmProxy) { proxy.Domains = []string{upstream.URL} })

	normalized := upstream.URL + "/orig"
	putCachedLink(normalized, upstream.URL+"/cached", time.Minute)

	resp, err := resolveLink(context.Background(), normalized, http.Header{})
	if err != nil {
		t.Fatalf("resolveLink() 返回错误: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "fresh" {
		t.Errorf("重试后的响应 = %d %q, want 200 fresh", resp.StatusCode, string(body))
	}
	if got := requestCount.Load(); got != 2 {
		t.Errorf("上游请求数 = %d, want 2 (失效缓存 + 重试)", got)
	}
	if _, ok := getCachedLink(normalized); ok {
		t.Error("失效直链的缓存应被清除")
	}
}

// TestResolveLink_PathQueryIsJustAParameter
//
// ?path= 只是 strm 地址里的一个普通查询参数, 网关代理分支不解析它 ——
// 必须原样把请求打到上游并把字节取回来。
//
// 这是移除"strm 是 http 地址时从 path 参数走 Drive 取流"那条分支之后的回归钉子:
// 那个分支删掉之后, 带 path 参数的地址必须仍然正常工作。
func TestResolveLink_PathQueryIsJustAParameter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("gateway-bytes"))
	}))
	defer upstream.Close()

	internalProxyConfig(t, func(proxy *config.StrmProxy) { proxy.Domains = []string{upstream.URL} })

	for name, pathValue := range map[string]string{
		"正常路径":  "/影视库/最新电影/72小时 (2026)/72小时 (2026).mkv",
		"只有根斜杠": "/",
		"多个斜杠":  "///",
		"只有空白":  "   ",
		"不带该参数": "",
	} {
		t.Run(name, func(t *testing.T) {
			raw := upstream.URL + "/redirect?pickcode=abc&storage=googledrive-1"
			if pathValue != "" {
				raw += "&path=" + url.QueryEscape(pathValue)
			}

			resp, err := resolveLink(context.Background(), raw, http.Header{})
			if err != nil {
				t.Fatalf("应走原地址, 实际报错: %v", err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("读取响应体失败: %v", err)
			}
			if string(body) != "gateway-bytes" {
				t.Errorf("响应体 = %q, want gateway-bytes", string(body))
			}
		})
	}
}

func TestResolveLink_RetryStillFails(t *testing.T) {
	var requestCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusGone)
	}))
	defer upstream.Close()

	internalProxyConfig(t, func(proxy *config.StrmProxy) { proxy.Domains = []string{upstream.URL} })

	if _, err := resolveLink(context.Background(), upstream.URL+"/media.mp4", http.Header{}); err == nil {
		t.Fatal("重试后仍然失效时应返回错误")
	}
	if got := requestCount.Load(); got != 2 {
		t.Errorf("上游请求数 = %d, want 2 (仅重试一次)", got)
	}
}
