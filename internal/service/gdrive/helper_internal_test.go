package gdrive

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs"
)

const (
	// testApiToken 测试用的面板令牌
	//
	// 长度超过 minRedactSecretLen, 因此参与脱敏断言时是有意义的。
	testApiToken = "test-panel-api-token"
	// testProviderToken 假面板在 headers 里返回的 Google 令牌
	testProviderToken = "Bearer test-google-access-token"
	// testPath 测试用的团队盘内路径
	testPath = "/影视库/最新电影/72小时 (2026)/72小时 (2026).mkv"
)

// testApiBase 假配置里用的面板地址
//
// 用例不需要真的请求它时可以随便填, 只要格式合法。
const testApiBase = "https://panel.example.com"

// withTestConfig 注入一份启用的面板配置并在用例结束时恢复
//
// 同时清空进程内缓存: 令牌槽是全局的, 不清理会让用例之间互相污染。
func withTestConfig(t *testing.T, apiBase string) *config.GDrive {
	t.Helper()

	// 清空令牌环境变量, 保证用例不受运行环境里已导出的 GDRIVE_API_TOKEN 影响
	t.Setenv(config.GDriveApiTokenEnvName, "")

	if apiBase == "" {
		apiBase = testApiBase
	}

	cfg := &config.GDrive{
		Enable:      true,
		ApiBase:     apiBase,
		ApiToken:    testApiToken,
		MountPrefix: "/home/googleDrive",
	}
	if err := cfg.Init(); err != nil {
		t.Fatalf("初始化面板测试配置失败: %v", err)
	}

	oldConfig := config.C
	config.C = &config.Config{GDrive: cfg}
	resetCache()

	t.Cleanup(func() {
		config.C = oldConfig
		resetCache()
	})

	return cfg
}

// withDisabledConfig 注入一份未启用的配置
func withDisabledConfig(t *testing.T) {
	t.Helper()

	oldConfig := config.C
	config.C = &config.Config{GDrive: &config.GDrive{}}
	resetCache()

	t.Cleanup(func() {
		config.C = oldConfig
		resetCache()
	})
}

// fakePanel 假面板, 记录被调用的次数、收到的 path 参数与请求头
type fakePanel struct {
	mu      sync.Mutex
	calls   int
	paths   []string
	headers []http.Header
	server  *httptest.Server
}

// newFakePanel 启动一个假面板
//
// handler 收到的参数依次是本次请求的 Authorization 头与 path 查询参数;
// 返回状态码与响应体。
func newFakePanel(t *testing.T, handler func(receivedAuth, path string) (int, string)) *fakePanel {
	t.Helper()

	panel := &fakePanel{}
	panel.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != directLinkAPIPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		panel.mu.Lock()
		panel.calls++
		panel.paths = append(panel.paths, r.URL.Query().Get(pathQueryField))
		panel.headers = append(panel.headers, r.Header.Clone())
		panel.mu.Unlock()

		status, body := handler(r.Header.Get("Authorization"), r.URL.Query().Get(pathQueryField))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(panel.server.Close)

	return panel
}

// receivedHeaders 返回收到的全部请求头快照
func (p *fakePanel) receivedHeaders() []http.Header {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]http.Header(nil), p.headers...)
}

// url 返回假面板的地址
func (p *fakePanel) url() string { return p.server.URL }

// callCount 返回被调用的次数
func (p *fakePanel) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// receivedPaths 返回收到过的 path 参数(原始形态, 已解码)
func (p *fakePanel) receivedPaths() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.paths...)
}

// successBody 构造一份面板成功响应
func successBody(directURL, expiresAt string) string {
	payload := map[string]any{
		"ok": true,
		"data": map[string]any{
			"url":        directURL,
			"headers":    map[string]string{"Authorization": testProviderToken},
			"expires_at": expiresAt,
			"file": map[string]any{
				"id":        "test-file-id",
				"name":      "72小时 (2026).mkv",
				"path":      testPath,
				"size":      8589934592,
				"mime_type": "video/x-matroska",
			},
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// errorBody 构造一份面板错误响应
func errorBody(code, message string) string {
	payload := map[string]any{
		"ok":    false,
		"error": map[string]string{"code": code, "message": message},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// futureRFC3339 返回 offset 之后的 RFC3339 时间串
func futureRFC3339(offset time.Duration) string {
	return time.Now().Add(offset).UTC().Format(time.RFC3339)
}

// fakeDrive 假 Google 下载端点
//
// 记录收到的请求头, 便于断言 Range 与 Authorization 是否原样转发。
type fakeDrive struct {
	mu      sync.Mutex
	headers []http.Header
	ranges  []string
	server  *httptest.Server
	handler func(r *http.Request) (int, string)
}

// newFakeDrive 启动一个假下载端点
func newFakeDrive(t *testing.T, handler func(r *http.Request) (int, string)) *fakeDrive {
	t.Helper()

	drive := &fakeDrive{handler: handler}
	drive.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drive.mu.Lock()
		drive.headers = append(drive.headers, r.Header.Clone())
		drive.ranges = append(drive.ranges, r.Header.Get("Range"))
		drive.mu.Unlock()

		status, body := http.StatusOK, "media-bytes"
		if drive.handler != nil {
			status, body = drive.handler(r)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(drive.server.Close)

	return drive
}

// url 返回假下载端点的地址
func (d *fakeDrive) url() string { return d.server.URL }

// requestCount 返回收到的请求数
func (d *fakeDrive) requestCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.headers)
}

// receivedHeaders 返回收到的全部请求头快照
func (d *fakeDrive) receivedHeaders() []http.Header {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]http.Header(nil), d.headers...)
}

// receivedRanges 返回收到的全部 Range 头
func (d *fakeDrive) receivedRanges() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.ranges...)
}

// forceTokenExpired 让当前令牌槽立刻过期
//
// 用于模拟播放跨过令牌有效期边界: 直接写入一条 expires_at 在过去的条目,
// deadline 会落到当下, 读侧立即判为不可用。
func forceTokenExpired() {
	putToken(map[string]string{"Authorization": "Bearer stale-token"}, "", time.Now().Add(-time.Hour), time.Now())
}

// buildEndpoint 拼出本项目会请求的面板地址, 供用例做 URL 编码断言
func buildEndpoint(apiBase, gdPath string) string {
	return apiBase + directLinkAPIPath + "?" + pathQueryField + "=" + url.QueryEscape(gdPath)
}

// logCollector 把日志收集到内存里, 供用例断言日志内容
type logCollector struct {
	mu      sync.Mutex
	content strings.Builder
}

// Log 实现 logs.Logger
func (c *logCollector) Log(content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.content.WriteString(content)
}

// String 返回已收集到的日志
func (c *logCollector) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.content.String()
}

// captureLogs 注册一个内存日志收集器, 用例结束时自动注销
func captureLogs(t *testing.T) *logCollector {
	t.Helper()

	collector := &logCollector{}
	id, ok := logs.RegisterLogger(collector)
	if !ok {
		t.Fatal("注册日志收集器失败")
	}
	t.Cleanup(func() { logs.RemoveLogger(id) })

	return collector
}

// drainAndClose 读完并关闭响应体
func drainAndClose(t *testing.T, resp *http.Response) string {
	t.Helper()
	if resp == nil || resp.Body == nil {
		return ""
	}
	defer resp.Body.Close()

	var sb strings.Builder
	if _, err := io.Copy(&sb, resp.Body); err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}
	return sb.String()
}
