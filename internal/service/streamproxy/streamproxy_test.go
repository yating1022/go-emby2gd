package streamproxy_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/streamproxy"
)

// callProxy 直接调用 Proxy, 返回是否已写出响应、错误与录制的响应
func callProxy(t *testing.T, target string, header map[string]string) (bool, error, *httptest.ResponseRecorder) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, "http://project.local/stream", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	for key, value := range header {
		req.Header.Set(key, value)
	}

	rec := httptest.NewRecorder()
	written, err := streamproxy.Proxy(rec, req, target)
	return written, err, rec
}

// proxyConfigWith 构造并注入一份可定制的测试配置
func proxyConfigWith(t *testing.T, mutate func(proxy *config.StrmProxy)) *config.StrmProxy {
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
	setupProxyConfig(t, proxy)
	return proxy
}

func TestProxy_RangePassthrough(t *testing.T) {
	var gotRange atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange.Store(r.Header.Get("Range"))
		w.Header().Set("Content-Type", "video/x-matroska")
		w.Header().Set("Content-Range", "bytes 0-1023/655902790")
		w.WriteHeader(http.StatusPartialContent)
		w.Write(bytes.Repeat([]byte{1}, 1024))
	}))
	defer upstream.Close()

	proxyConfigWith(t, func(proxy *config.StrmProxy) { proxy.Domains = []string{upstream.URL} })

	written, err, rec := callProxy(t, upstream.URL+"/redirect?path=/a.mkv", map[string]string{"Range": "bytes=0-1023"})
	if err != nil {
		t.Fatalf("Proxy() 返回错误: %v", err)
	}
	if !written {
		t.Fatal("Proxy() written = false, 期望 true")
	}
	if rec.Code != http.StatusPartialContent {
		t.Errorf("响应码 = %d, want %d", rec.Code, http.StatusPartialContent)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 0-1023/655902790" {
		t.Errorf("Content-Range = %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "video/x-matroska" {
		t.Errorf("Content-Type = %q", got)
	}
	if rec.Body.Len() != 1024 {
		t.Errorf("响应体长度 = %d, want 1024", rec.Body.Len())
	}
	if got := gotRange.Load(); got != "bytes=0-1023" {
		t.Errorf("上游收到的 Range = %v, want bytes=0-1023", got)
	}
}

func TestProxy_AcceptRangesBackfill(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-1/10")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("ab"))
	}))
	defer upstream.Close()

	proxyConfigWith(t, func(proxy *config.StrmProxy) { proxy.Domains = []string{upstream.URL} })

	_, err, rec := callProxy(t, upstream.URL+"/media.mkv", map[string]string{"Range": "bytes=0-1"})
	if err != nil {
		t.Fatalf("Proxy() 返回错误: %v", err)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
}

func TestProxy_RedirectFollow(t *testing.T) {
	var requestCount atomic.Int32
	var secondRange atomic.Value

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		if r.URL.Path == "/first" {
			http.Redirect(w, r, "/second", http.StatusFound)
			return
		}
		secondRange.Store(r.Header.Get("Range"))
		w.Header().Set("Content-Type", "video/mp4")
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	proxyConfigWith(t, func(proxy *config.StrmProxy) { proxy.Domains = []string{upstream.URL} })

	written, err, rec := callProxy(t, upstream.URL+"/first", map[string]string{"Range": "bytes=0-9"})
	if err != nil {
		t.Fatalf("Proxy() 返回错误: %v", err)
	}
	if !written {
		t.Fatal("Proxy() written = false, 期望 true")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("响应码 = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("响应体 = %q, want ok", rec.Body.String())
	}
	if got := requestCount.Load(); got != 2 {
		t.Errorf("上游请求数 = %d, want 2", got)
	}
	if got := secondRange.Load(); got != "bytes=0-9" {
		t.Errorf("跳转后上游收到的 Range = %v, want bytes=0-9", got)
	}
}

func TestProxy_RedirectDepthExceeded(t *testing.T) {
	var requestCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer upstream.Close()

	proxyConfigWith(t, func(proxy *config.StrmProxy) {
		proxy.Domains = []string{upstream.URL}
		proxy.MaxRedirectDepth = 2
	})

	written, err, _ := callProxy(t, upstream.URL+"/loop", nil)
	if err == nil {
		t.Fatal("跳数超过上限时应返回错误")
	}
	if written {
		t.Fatal("跳数超过上限时 written = true, 期望 false(可回退)")
	}
	if got := requestCount.Load(); got != 3 {
		t.Errorf("上游请求数 = %d, want 3 (首次 + 2 跳)", got)
	}
}

func TestProxy_RetryOnInvalidLink(t *testing.T) {
	var requestCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestCount.Add(1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	proxyConfigWith(t, func(proxy *config.StrmProxy) { proxy.Domains = []string{upstream.URL} })

	written, err, rec := callProxy(t, upstream.URL+"/media.mkv", nil)
	if err != nil {
		t.Fatalf("Proxy() 返回错误: %v", err)
	}
	if !written {
		t.Fatal("Proxy() written = false, 期望 true")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("响应码 = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("响应体 = %q, want ok", rec.Body.String())
	}
	if got := requestCount.Load(); got != 2 {
		t.Errorf("上游请求数 = %d, want 2 (首次 + 重试)", got)
	}
}

func TestProxy_RetryStillFails(t *testing.T) {
	var requestCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	proxyConfigWith(t, func(proxy *config.StrmProxy) { proxy.Domains = []string{upstream.URL} })

	written, err, _ := callProxy(t, upstream.URL+"/media.mkv", nil)
	if err == nil {
		t.Fatal("重试后仍然失效时应返回错误")
	}
	if written {
		t.Fatal("重试后仍然失效时 written = true, 期望 false(可回退)")
	}
	if got := requestCount.Load(); got != 2 {
		t.Errorf("上游请求数 = %d, want 2 (仅重试一次)", got)
	}
}

func TestProxy_HeaderWhitelist(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sb strings.Builder
		for key, values := range r.Header {
			sb.WriteString(key + ": " + strings.Join(values, ",") + "\n")
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(sb.String()))
	}))
	defer upstream.Close()

	proxyConfigWith(t, func(proxy *config.StrmProxy) {
		proxy.Domains = []string{upstream.URL}
		proxy.RequestHeader = map[string]string{
			"User-Agent":    "ge2o-proxy/1.0",
			"X-Gateway-Key": "gateway-key",
		}
	})

	_, err, rec := callProxy(t, upstream.URL+"/media.mkv", map[string]string{
		"Range":         "bytes=0-9",
		"Authorization": "Bearer emby-secret",
		"Cookie":        "emby-token=secret",
		"X-Emby-Token":  "emby-token",
		"User-Agent":    "client-ua/2.0",
	})
	if err != nil {
		t.Fatalf("Proxy() 返回错误: %v", err)
	}

	echoed := rec.Body.String()
	if !strings.Contains(echoed, "Range: bytes=0-9") {
		t.Errorf("Range 应透传给上游, 实际回显: %q", echoed)
	}
	for _, secret := range []string{"Authorization", "Cookie", "X-Emby-", "emby-secret", "emby-token"} {
		if strings.Contains(echoed, secret) {
			t.Errorf("敏感头 %q 不应透传给上游, 实际回显: %q", secret, echoed)
		}
	}
	if !strings.Contains(echoed, "User-Agent: ge2o-proxy/1.0") {
		t.Errorf("配置的固定请求头应覆盖客户端 UA, 实际回显: %q", echoed)
	}
	if !strings.Contains(echoed, "X-Gateway-Key: gateway-key") {
		t.Errorf("配置的固定请求头应发送给上游, 实际回显: %q", echoed)
	}
}

func TestProxy_ExcludedHeadersNeverForwarded(t *testing.T) {
	var echoed atomic.Value

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sb strings.Builder
		for key, values := range r.Header {
			sb.WriteString(key + ": " + strings.Join(values, ",") + "\n")
		}
		echoed.Store(sb.String())
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	// 每个值都带上唯一的 leakcheck 前缀, 避免与其他头部的值发生子串误判
	injected := map[string]string{
		"Authorization":        "leakcheck-authorization",
		"Cookie":               "leakcheck-cookie",
		"Proxy-Authorization":  "leakcheck-proxy-authorization",
		"X-Emby-Token":         "leakcheck-x-emby-token",
		"X-MediaBrowser-Token": "leakcheck-x-mediabrowser-token",
		"TE":                   "leakcheck-te",
		"Host":                 "leakcheck-host",
		"Connection":           "leakcheck-connection",
		"Transfer-Encoding":    "leakcheck-transfer-encoding",
		"Trailer":              "leakcheck-trailer",
		"Upgrade":              "leakcheck-upgrade",
		"Proxy-Connection":     "leakcheck-proxy-connection",
	}

	proxyConfigWith(t, func(proxy *config.StrmProxy) {
		proxy.Domains = []string{upstream.URL}
		proxy.RequestHeader = injected
	})

	written, err, _ := callProxy(t, upstream.URL+"/media.mkv", nil)
	if err != nil {
		t.Fatalf("Proxy() 返回错误: %v", err)
	}
	if !written {
		t.Fatal("Proxy() written = false, 期望 true")
	}

	raw, _ := echoed.Load().(string)
	lower := strings.ToLower(raw)

	// 按名称与值两种方式断言:
	// http.Header 的键会被 CanonicalMIMEHeaderKey 规范化
	// (X-MediaBrowser-Token 实际存成 X-Mediabrowser-Token),
	// 只用名称判断会漏掉规范化之后再泄漏的情况
	for name, value := range injected {
		if strings.Contains(lower, strings.ToLower(name)+":") {
			t.Errorf("请求头 %s 不应透传给上游, 上游实际收到: %q", name, raw)
		}
		if strings.Contains(lower, strings.ToLower(value)) {
			t.Errorf("请求头 %s 的值不应透传给上游, 上游实际收到: %q", name, raw)
		}
	}
}

func TestProxy_NonFollowedRedirectKeepsLocation(t *testing.T) {
	const finalURL = "http://cdn.example.com/final.mp4"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", finalURL)
		w.WriteHeader(http.StatusSeeOther)
	}))
	defer upstream.Close()

	proxyConfigWith(t, func(proxy *config.StrmProxy) { proxy.Domains = []string{upstream.URL} })

	written, err, rec := callProxy(t, upstream.URL+"/media.mkv", nil)
	if err != nil {
		t.Fatalf("Proxy() 返回错误: %v", err)
	}
	if !written {
		t.Fatal("Proxy() written = false, 期望 true")
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("响应码 = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if got := rec.Header().Get("Location"); got != finalURL {
		t.Errorf("Location = %q, want %q (300/303 不在跟随集合内, 必须回传给客户端)", got, finalURL)
	}
}

// TestProxy_NonFollowedRelativeLocationResolved 验证相对的 Location 会被解析成绝对地址
//
// 上游若返回相对 Location (如 /final.mp4), 原样回写会让客户端按**本项目的** origin 去解析,
// 请求打到本项目自身的路由上(通常 404), 而不是上游的同源路径。
func TestProxy_NonFollowedRelativeLocationResolved(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/final.mp4")
		w.WriteHeader(http.StatusSeeOther)
	}))
	defer upstream.Close()

	proxyConfigWith(t, func(proxy *config.StrmProxy) { proxy.Domains = []string{upstream.URL} })

	written, err, rec := callProxy(t, upstream.URL+"/media.mkv", nil)
	if err != nil {
		t.Fatalf("Proxy() 返回错误: %v", err)
	}
	if !written {
		t.Fatal("Proxy() written = false, 期望 true")
	}

	want := upstream.URL + "/final.mp4"
	if got := rec.Header().Get("Location"); got != want {
		t.Errorf("Location = %q, want %q (相对地址必须基于上游地址解析)", got, want)
	}
}

func TestProxy_UpstreamAcceptEncodingIdentity(t *testing.T) {
	var gotAcceptEncoding atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAcceptEncoding.Store(r.Header.Get("Accept-Encoding"))
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	proxyConfigWith(t, func(proxy *config.StrmProxy) {
		proxy.Domains = []string{upstream.URL}
		// 配置试图覆盖为 gzip, 也必须被写死的 identity 压回去
		proxy.RequestHeader = map[string]string{"Accept-Encoding": "gzip"}
	})

	written, err, _ := callProxy(t, upstream.URL+"/media.mp4", map[string]string{"Accept-Encoding": "gzip, deflate"})
	if err != nil {
		t.Fatalf("Proxy() 返回错误: %v", err)
	}
	if !written {
		t.Fatal("Proxy() written = false, 期望 true")
	}
	if got := gotAcceptEncoding.Load(); got != "identity" {
		t.Errorf("上游收到的 Accept-Encoding = %v, want identity (否则 Transport 会透明解压并删掉 Content-Length)", got)
	}
}

func TestProxy_NoUpstreamWhenDisabled(t *testing.T) {
	var requestCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	proxy := proxyConfigForTest(t, []string{upstream.URL})
	proxy.Enable = false
	if err := proxy.Init(); err != nil {
		t.Fatalf("初始化配置失败: %v", err)
	}
	setupProxyConfig(t, proxy)

	written, err, _ := callProxy(t, upstream.URL+"/media.mkv", nil)
	if err == nil {
		t.Fatal("未开启代理时应返回错误")
	}
	if written {
		t.Fatal("未开启代理时 written 应为 false")
	}
	if got := requestCount.Load(); got != 0 {
		t.Errorf("未开启代理时不应请求上游, 实际请求数 = %d", got)
	}
}

func TestProxy_UnmatchedDomainNoUpstream(t *testing.T) {
	var requestCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	proxyConfigWith(t, func(proxy *config.StrmProxy) { proxy.Domains = []string{"http://other.example.com"} })

	written, err, _ := callProxy(t, upstream.URL+"/media.mkv", nil)
	if err == nil {
		t.Fatal("未命中代理前缀时应返回错误")
	}
	if written {
		t.Fatal("未命中代理前缀时 written 应为 false")
	}
	if got := requestCount.Load(); got != 0 {
		t.Errorf("未命中代理前缀时不应请求上游, 实际请求数 = %d", got)
	}
}

func TestProxy_UpstreamServerErrorFallsBack(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	proxyConfigWith(t, func(proxy *config.StrmProxy) { proxy.Domains = []string{upstream.URL} })

	written, err, _ := callProxy(t, upstream.URL+"/media.mkv", nil)
	if err == nil {
		t.Fatal("上游 5xx 时应返回错误以便调用方回退")
	}
	if written {
		t.Fatal("上游 5xx 时 written 应为 false")
	}
}

func TestProxy_UpstreamRequestFailure(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	target := closed.URL
	closed.Close()

	proxyConfigWith(t, func(proxy *config.StrmProxy) { proxy.Domains = []string{target} })

	written, err, _ := callProxy(t, target+"/media.mkv", nil)
	if err == nil {
		t.Fatal("上游建连失败时应返回错误")
	}
	if written {
		t.Fatal("上游建连失败时 written 应为 false")
	}
}

func TestProxy_LargeResponseNoBuffering(t *testing.T) {
	const payloadSize = 8 << 20

	chunk := make([]byte, 32*1024)
	for i := range chunk {
		chunk[i] = byte(i)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", strconv.Itoa(payloadSize))
		w.WriteHeader(http.StatusOK)
		for sent := 0; sent < payloadSize; sent += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	proxyConfigWith(t, func(proxy *config.StrmProxy) { proxy.Domains = []string{upstream.URL} })

	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		written, err := streamproxy.Proxy(w, r, upstream.URL+"/media.mp4")
		if err != nil && !written {
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer proxySrv.Close()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	client := &http.Client{Timeout: time.Minute}
	resp, err := client.Get(proxySrv.URL)
	if err != nil {
		t.Fatalf("请求代理失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("响应码 = %d, want 200", resp.StatusCode)
	}

	sent, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	if sent != payloadSize {
		t.Fatalf("客户端收到 %d 字节, want %d", sent, payloadSize)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	allocated := int64(after.TotalAlloc - before.TotalAlloc)
	t.Logf("代理 %d 字节响应体, 进程累计分配 %d 字节", payloadSize, allocated)
	if allocated > payloadSize/2 {
		t.Errorf("代理 %d 字节响应时累计分配了 %d 字节, 说明响应体被整体缓冲", payloadSize, allocated)
	}
}
