package emby_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/emby"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs"

	"github.com/gin-gonic/gin"
)

// fakeEmbyOrigin 假 Emby 源服务器
//
// PlaybackInfo 返回指定的媒体路径, 其余请求(即回源转发)记录命中次数并返回固定响应体。
type fakeEmbyOrigin struct {
	// server 假服务端
	server *httptest.Server
	// mediaPath PlaybackInfo 返回的媒体路径
	mediaPath string
	// originHits 回源转发的命中次数
	//
	// PlaybackInfo 不计入: 那是每条路径都会发的探测请求, 计入后就分不清
	// "回源处理"与"正常探测"了。
	originHits atomic.Int64
	// lastOriginURI 最近一次回源转发的 RequestURI
	lastOriginURI atomic.Value
	// playbackProbes 每次收到 PlaybackInfo 探测时发一个信号
	//
	// 供 waitForPlaybackProbe 使用: 播放入口是异步发探测的, 不等它落地,
	// 用例结束还原 config.C 后那个 goroutine 会读到空配置而崩掉整个测试进程。
	playbackProbes chan struct{}
}

// newFakeEmbyOrigin 启动假 Emby 源服务器
func newFakeEmbyOrigin(t *testing.T, mediaPath string) *fakeEmbyOrigin {
	t.Helper()

	f := &fakeEmbyOrigin{mediaPath: mediaPath, playbackProbes: make(chan struct{}, 16)}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/PlaybackInfo") {
			select {
			case f.playbackProbes <- struct{}{}:
			default:
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaSources": []map[string]any{{"Path": f.mediaPath, "Id": "ms-1"}},
			})
			return
		}

		f.originHits.Add(1)
		f.lastOriginURI.Store(r.URL.RequestURI())
		w.Header().Set("Content-Type", "video/x-matroska")
		_, _ = w.Write([]byte("origin-bytes"))
	}))
	t.Cleanup(f.server.Close)

	return f
}

// playbackProbeQuiet 判定"探测已全部落地"的静默窗口
const playbackProbeQuiet = 100 * time.Millisecond

// waitForPlaybackProbes 等待所有异步 PlaybackInfo 探测落地
//
// Redirect2OpenlistLink 在直链分支里 go sendOpenStreamPlaybackInfoReqToOrigin(...),
// 该 goroutine 会读取全局 config.C; 用例结束时会还原 config.C, 因此必须等它跑完,
// 否则它会在用例之外读到空配置而崩溃, 把整个测试进程带走。
//
// 一次请求会先后产生两次 PlaybackInfo 命中(解析媒体信息的同步请求 + 异步通知),
// 数量不是契约, 所以这里按"静默窗口内不再有新的探测"判定结束, 而不是数个数。
func (f *fakeEmbyOrigin) waitForPlaybackProbes(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Error("等待异步 PlaybackInfo 探测超时")
			return
		}
		select {
		case <-f.playbackProbes:
		case <-time.After(playbackProbeQuiet):
			return
		}
	}
}

// fakePanel 假 GD 管理面板
type fakePanel struct {
	// server 假服务端
	server *httptest.Server
	// calls 被调用的次数
	calls atomic.Int64
}

// newFakePanel 启动假面板, 总是返回指向 directURL 的成功响应
func newFakePanel(t *testing.T, directURL string) *fakePanel {
	t.Helper()

	p := &fakePanel{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"url":        directURL,
				"headers":    map[string]string{"Authorization": "Bearer panel-issued-token"},
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			},
		})
	}))
	t.Cleanup(p.server.Close)

	return p
}

// newFailingPanel 启动一个总是返回指定错误的假面板
func newFailingPanel(t *testing.T, status int, code, message string) *fakePanel {
	t.Helper()

	p := &fakePanel{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": map[string]string{"code": code, "message": message},
		})
	}))
	t.Cleanup(p.server.Close)

	return p
}

// newDirectLinkServer 启动假 Google 下载端点
func newDirectLinkServer(t *testing.T, body string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/x-matroska")
		w.Header().Set("Accept-Ranges", "bytes")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	return server
}

// withEmbyTestConfig 注入一份 emby 与 gdrive 测试配置
//
// apiBase 为空表示 gdrive 未启用(行为与未部署本功能一致);
// 传入假面板地址表示启用, 并配上测试令牌。
func withEmbyTestConfig(t *testing.T, host string, localRoots []string, apiBase string) {
	t.Helper()

	oldConfig := config.C

	gdriveCfg := &config.GDrive{MountPrefix: "/home/googleDrive"}
	if apiBase != "" {
		gdriveCfg.Enable = true
		gdriveCfg.ApiBase = apiBase
		gdriveCfg.ApiToken = "test-panel-api-token"
	}

	config.C = &config.Config{
		Emby:   &config.Emby{Host: host, LocalMediaRoots: localRoots},
		GDrive: gdriveCfg,
	}

	t.Cleanup(func() { config.C = oldConfig })
}

// newRedirectContext 构造一个请求 /emby/Videos/<id>/stream 的 gin 上下文
//
// 用相对路径构造请求: 这样 RequestURI 保持 origin-form(如 /emby/Videos/123/stream),
// 与真实客户端请求一致 —— 传绝对 URL 会让 httptest 把 RequestURI 设成绝对地址,
// 回源转发时就会拼出非法目标地址。
func newRedirectContext(t *testing.T, uri string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, uri, nil)

	return c, recorder
}

// redirectLogCollector 把日志收集到内存里, 供用例断言日志内容
type redirectLogCollector struct {
	mu      sync.Mutex
	content strings.Builder
}

// Log 实现 logs.Logger
func (c *redirectLogCollector) Log(content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.content.WriteString(content)
	c.content.WriteString("\n")
}

// contains 判断是否收集到包含指定片段的日志
func (c *redirectLogCollector) contains(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Contains(c.content.String(), sub)
}

// String 返回已收集到的日志
func (c *redirectLogCollector) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.content.String()
}

// captureRedirectLogs 注册一个内存日志收集器, 用例结束时自动注销
func captureRedirectLogs(t *testing.T) *redirectLogCollector {
	t.Helper()

	collector := new(redirectLogCollector)
	id, ok := logs.RegisterLogger(collector)
	if !ok {
		t.Fatal("注册日志收集器失败")
	}
	t.Cleanup(func() { logs.RemoveLogger(id) })

	return collector
}

// TestRedirect2OpenlistLink_GDriveMountPathProxied 挂载路径应由本项目代理字节流
//
// 这是本功能存在的意义: 客户端拿到的必须是媒体字节, 而不是一个 302;
// 同时 Emby 源服务器不应收到任何回源请求。
func TestRedirect2OpenlistLink_GDriveMountPathProxied(t *testing.T) {
	gdPath := uniqueGDPath("/影视库/最新电影/A/x.mkv")
	origin := newFakeEmbyOrigin(t, "/home/googleDrive"+gdPath)
	drive := newDirectLinkServer(t, "panel-proxied-bytes")
	panel := newFakePanel(t, drive.URL)
	withEmbyTestConfig(t, origin.server.URL, nil, panel.server.URL)

	logger := captureRedirectLogs(t)
	c, recorder := newRedirectContext(t, "/emby/Videos/123/stream")
	emby.Redirect2OpenlistLink(c)

	if got := recorder.Body.String(); got != "panel-proxied-bytes" {
		t.Errorf("客户端收到的响应体 = %q, want panel-proxied-bytes", got)
	}
	if got := recorder.Code; got != http.StatusOK {
		t.Errorf("响应码 = %d, want 200 (代理而非重定向)", got)
	}
	if location := recorder.Header().Get("Location"); location != "" {
		t.Errorf("不应回写 Location 头, 实际: %q", location)
	}

	if !logger.contains("检测到 Google Drive 挂载路径") {
		t.Errorf("挂载路径应走直链分支, 实际日志: %s", logger.String())
	}
	if !logger.contains("开始传输") {
		t.Errorf("应能看出传输已开始, 实际日志: %s", logger.String())
	}

	if panel.calls.Load() != 1 {
		t.Errorf("面板调用次数 = %d, want 1", panel.calls.Load())
	}
	if got := origin.originHits.Load(); got != 0 {
		t.Errorf("代理成功时不应回源, 但假 Emby 源收到了 %d 次回源请求", got)
	}
	origin.waitForPlaybackProbes(t)
}

// TestRedirect2OpenlistLink_GDriveFailureFallsBackToOrigin 面板失败时回源
func TestRedirect2OpenlistLink_GDriveFailureFallsBackToOrigin(t *testing.T) {
	gdPath := uniqueGDPath("/影视库/最新电影/B/x.mkv")
	origin := newFakeEmbyOrigin(t, "/home/googleDrive"+gdPath)
	panel := newFailingPanel(t, http.StatusNotFound, "PATH_NOT_IN_CACHE", "路径尚未缓存, 请先缓存它")
	withEmbyTestConfig(t, origin.server.URL, nil, panel.server.URL)

	logger := captureRedirectLogs(t)
	c, recorder := newRedirectContext(t, "/emby/Videos/123/stream")
	emby.Redirect2OpenlistLink(c)

	if !logger.contains("GD 面板取流失败, 回源处理") {
		t.Errorf("面板失败后应回源, 实际日志: %s", logger.String())
	}
	// 面板的中文文案必须原样出现, 不另编一套
	if !logger.contains("路径尚未缓存, 请先缓存它") {
		t.Errorf("日志应原样沿用面板文案, 实际: %s", logger.String())
	}
	// 回源路径不应落回原有流程: 那会走到 OpenList 分支并打出误导性的配置错误
	if logger.contains("openlist.host") {
		t.Errorf("回源时不应出现 openlist.host 相关日志, 实际: %s", logger.String())
	}

	if got := origin.originHits.Load(); got == 0 {
		t.Fatal("面板失败后应回源处理, 但假 Emby 源服务器未收到回源请求")
	}
	if got, _ := origin.lastOriginURI.Load().(string); got != "/emby/Videos/123/stream" {
		t.Errorf("回源请求的 URI = %q, want /emby/Videos/123/stream", got)
	}
	if got := recorder.Body.String(); got != "origin-bytes" {
		t.Errorf("客户端收到的响应体 = %q, want origin-bytes", got)
	}
	origin.waitForPlaybackProbes(t)
}

// TestRedirect2OpenlistLink_GDriveTokenNotForwardedToClient
//
// 面板给的 Authorization 是账号级 Google 凭据, 绝不能回写给客户端。
func TestRedirect2OpenlistLink_GDriveTokenNotForwardedToClient(t *testing.T) {
	gdPath := uniqueGDPath("/影视库/最新电影/C/x.mkv")
	origin := newFakeEmbyOrigin(t, "/home/googleDrive"+gdPath)
	drive := newDirectLinkServer(t, "panel-proxied-bytes")
	panel := newFakePanel(t, drive.URL)
	withEmbyTestConfig(t, origin.server.URL, nil, panel.server.URL)

	logger := captureRedirectLogs(t)
	c, recorder := newRedirectContext(t, "/emby/Videos/123/stream")
	emby.Redirect2OpenlistLink(c)

	if got := recorder.Header().Get("Authorization"); got != "" {
		t.Errorf("账号级凭据不得回写给客户端, 实际 Authorization: %q", got)
	}
	if strings.Contains(logger.String(), "panel-issued-token") {
		t.Errorf("账号级凭据不得进日志, 实际: %s", logger.String())
	}
	origin.waitForPlaybackProbes(t)
}

func TestRedirect2OpenlistLink_NonMountPathUnchanged(t *testing.T) {
	// 非挂载路径(115 网盘挂载点)在改动前走本地媒体分支:
	// 用"未启用 gdrive"代表改动前的行为, 与"已启用 gdrive"逐项比对
	run := func(gdEnable bool) (redirectOutcome, *redirectLogCollector) {
		origin := newFakeEmbyOrigin(t, "/home/CloudNAS/115/影视库/x.mkv")

		apiBase := ""
		if gdEnable {
			apiBase = "https://panel.example.com"
		}
		withEmbyTestConfig(t, origin.server.URL, []string{"/home/CloudNAS"}, apiBase)

		logger := captureRedirectLogs(t)
		c, recorder := newRedirectContext(t, "/emby/Videos/123/stream")
		emby.Redirect2OpenlistLink(c)

		return redirectOutcome{
			code:     recorder.Code,
			location: recorder.Header().Get("Location"),
			body:     recorder.Body.String(),
		}, logger
	}

	disabled, disabledLogs := run(false)
	enabled, enabledLogs := run(true)

	if enabled != disabled {
		t.Errorf("非挂载路径下启用 gdrive 改变了行为: enabled=%+v, disabled=%+v", enabled, disabled)
	}
	if enabled.code != http.StatusTemporaryRedirect {
		t.Errorf("响应码 = %d, want %d", enabled.code, http.StatusTemporaryRedirect)
	}
	if enabled.location != "/emby/Videos/123/original" {
		t.Errorf("重定向地址 = %q, want /emby/Videos/123/original", enabled.location)
	}
	for name, captured := range map[string]*redirectLogCollector{"启用": enabledLogs, "未启用": disabledLogs} {
		if captured.contains("Google Drive") {
			t.Errorf("%s gdrive 时非挂载路径不应输出 Google Drive 日志, 实际: %s", name, captured.String())
		}
	}
}

// redirectOutcome 一次重定向的可观测结果
type redirectOutcome struct {
	code     int
	location string
	body     string
}
