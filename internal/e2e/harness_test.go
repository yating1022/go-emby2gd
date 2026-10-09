// Package e2e 是父任务 10-08-agent-proxy-network 的端到端集成矩阵(E1-E14)
//
// 组件形态(按 research/e2e-harness-plan.md 的决策):
//
//   - 网关: 进程内真实 HTTP 服务(自控监听器 + 真实 gin 路由), 挂 agent 三端点
//     与安装脚本; 播放入口按 redirect_*_test.go 的既有模式直调
//     emby.Redirect2OpenlistLink —— 本组用例验证的是端到端契约, 不是路由表本身;
//   - agent: 测试内 go build 出的真实二进制, 以子进程 enroll + serve;
//   - 假面板 / 假 Google(http.ServeContent, 真实 Range 语义) / 假 Emby 源。
//
// 配置注入约定(同样是为了避免数据竞争): config.C / config.BasePath 只在
// 【没有 agent 在跑、网关无在途请求】的静默点改写 —— 例如启动 agent 之前,
// 或停掉 agent 与服务器之后。这也正是生产语义里"改配置重启"的等价形式。
package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/constant"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/agentnet"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/emby"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// 测试常量
const (
	// e2eEnrollToken 注册 Token
	e2eEnrollToken = "e2e-enroll-token-0123456789"
	// e2eAdminSecret 本地密钥(/ge2o 惯例)
	e2eAdminSecret = "e2e-ge2o-secret"
	// e2ePanelToken 面板直链接口令牌
	e2ePanelToken = "e2e-panel-api-token"
	// e2eGoogleCredential 面板下发的账号级 Google 凭据
	e2eGoogleCredential = "Bearer e2e-google-access-token"
	// e2eMountPrefix 假 Emby 上的 Google Drive 挂载前缀
	e2eMountPrefix = "/home/googleDrive"
	// e2eAgentVersion 注入 agent 二进制的版本号(E2 断言它被心跳带回来)
	e2eAgentVersion = "e2e-0.0.1"
	// e2eMediaSize 假 Google 上的媒体大小(1 MiB)
	e2eMediaSize = 1 << 20
	// e2eLastSeenWindow 断言心跳时间戳新鲜度用的窗口
	e2eLastSeenWindow = 2 * time.Minute
)

// e2eMediaContent 生成假 Google 上的媒体内容
//
// 逐字节可复算: E4 断言 Range 返回的确实是"这一段"字节, 而不只是长度对得上。
func e2eMediaContent() []byte {
	content := make([]byte, e2eMediaSize)
	for i := range content {
		content[i] = byte((i*7 + 11) % 251)
	}
	return content
}

// agentBinaryPath 测试内构建出的 agent 二进制
var agentBinaryPath string

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)

	binary, err := buildAgentBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "E2E 前置失败: %v\n", err)
		os.Exit(1)
	}
	agentBinaryPath = binary

	code := m.Run()
	_ = os.RemoveAll(filepath.Dir(binary))
	os.Exit(code)
}

// buildAgentBinary 在测试内构建 agent 二进制
//
// 刻意走真实构建而不是 stub: 本矩阵的意义就是让两侧真凭据、真签名、真 Range 地联调。
func buildAgentBinary() (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}

	dir, err := os.MkdirTemp("", "gd-agent-e2e-")
	if err != nil {
		return "", fmt.Errorf("创建临时目录失败: %v", err)
	}
	binary := filepath.Join(dir, "gd-agent")

	cmd := exec.Command(goTool(), "build", "-trimpath",
		"-ldflags", "-X main.version="+e2eAgentVersion, "-o", binary, ".")
	cmd.Dir = filepath.Join(root, "agent")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("go build agent 失败: %v\n%s", err, out)
	}
	return binary, nil
}

// repoRoot 定位仓库根目录(本文件位于 <root>/internal/e2e/)
func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("无法定位本测试文件路径")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file))), nil
}

// goTool 定位 go 工具链
//
// PATH 里没有时退回固定安装路径: 与文档里"每条 go 命令前 export PATH"的约定一致。
func goTool() string {
	if path, err := exec.LookPath("go"); err == nil {
		return path
	}
	return "/usr/local/go/bin/go"
}

// lockedBuffer 并发安全的日志缓冲
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ---------------------------------------------------------------------------
// 网关
// ---------------------------------------------------------------------------

// gwRequest 网关侧记录到的一次请求
type gwRequest struct {
	Method string
	Path   string
	Status int
	// Bytes 网关写给该请求的响应字节数
	Bytes int64
}

// gateway 进程内的真实网关服务
//
// 监听器由用例自己持有: E13 要在【同一地址】上重启(模拟网关进程重启),
// httptest.NewServer 关掉后无法在同地址重开。
type gateway struct {
	addr string
	ln   net.Listener
	srv  *http.Server

	mu       sync.Mutex
	requests []gwRequest
	stopped  bool
	// active 正在处理中的请求数
	//
	// config.C / config.BasePath 是无锁全局变量(生产里只在启动时写一次),
	// 用例中途改写它们之前必须确认没有处理器还在读 —— 见 waitIdle。
	active int
}

// newGateway 启动网关并注册用例结束时的清理
func newGateway(t *testing.T) *gateway {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听网关端口失败: %v", err)
	}

	g := &gateway{addr: ln.Addr().String(), ln: ln}
	g.start()
	t.Cleanup(g.stop)
	return g
}

// masterURL 网关对外地址(即 agent 的 MASTER_URL)
func (g *gateway) masterURL() string { return "http://" + g.addr }

// start 在现有监听器上开始服务
func (g *gateway) start() {
	srv := &http.Server{Handler: g.handler()}

	g.mu.Lock()
	g.stopped = false
	g.srv = srv
	ln := g.ln
	g.mu.Unlock()

	// 用局部变量起服务: goroutine 不再读 g.srv, 重启时换 srv 就不会与它并发
	go func() {
		// 停机(Shutdown/Close)会让 Serve 返回, 属预期;
		// 其余错误不在此处 t.Errorf: 用例结束后从 goroutine 里记日志会 panic。
		_ = srv.Serve(ln)
	}()
}

// stop 停止服务(幂等): 关监听器 + 等在途请求退出 + 释放端口
//
// "等在途退出"不是洁癖: 处理器会读 config.C / config.BasePath, 用例又要在这之后
// 改写它们。优雅停机(Shutdown)会先关掉监听器与空闲连接, 再等在途连接回到空闲,
// 因此返回时不会再有请求被分发进来。
func (g *gateway) stop() {
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		return
	}
	g.stopped = true
	srv := g.srv
	g.mu.Unlock()

	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := srv.Shutdown(ctx); err != nil {
			// 有连接卡住时兜底强关, 不让用例挂在这里
			_ = srv.Close()
		}
		cancel()
	}
	g.waitIdle()
	waitForPortReleased(g.addr, 5*time.Second)
}

// waitIdle 等待所有在途请求处理完毕
//
// 连续两次采样为空才返回: 把"请求刚被分发、还没进处理器"的窗口也覆盖掉。
func (g *gateway) waitIdle() {
	idleRounds := 0
	for idleRounds < 2 {
		g.mu.Lock()
		active := g.active
		g.mu.Unlock()

		if active == 0 {
			idleRounds++
		} else {
			idleRounds = 0
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// restart 在同一地址上重新开始服务
//
// 时序是"先 stop、再处理状态、最后 restart": 调用方可以在这中间安全地换配置
// 与冷加载注册表, 不存在任何在途请求。
func (g *gateway) restart(t *testing.T) {
	t.Helper()

	g.stop()

	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", g.addr)
		if err == nil {
			g.mu.Lock()
			g.ln = ln
			g.mu.Unlock()
			g.start()
			return
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("在同一地址 %s 上重启网关失败: %v", g.addr, lastErr)
}

// handler 组装网关路由
//
// 路由形状与 internal/web/route.go 注册的 agent 端点逐字一致(同一批处理器),
// 另外套一层记录器: E12 要断言"数据面字节从不经过网关"。
func (g *gateway) handler() http.Handler {
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.POST("/api/agent/enroll", agentnet.Enroll)
	engine.POST("/api/agent/heartbeat", agentnet.Heartbeat)
	engine.GET("/api/agent/download-link", agentnet.DownloadLink)
	engine.GET(constant.Route_InstallScript, agentnet.InstallScript)
	engine.POST(constant.Route_AgentNetworkAgents, agentnet.AdminListAgents)
	engine.POST(constant.Route_AgentNetworkAgentsUpdate, agentnet.AdminUpdateAgent)
	engine.POST(constant.Route_AgentNetworkAgentsDelete, agentnet.AdminDeleteAgent)
	engine.POST(constant.Route_AgentNetworkInstallCommand, agentnet.AdminInstallCommand)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.active++
		g.mu.Unlock()
		defer func() {
			g.mu.Lock()
			g.active--
			g.mu.Unlock()
		}()

		recorder := &countingResponseWriter{ResponseWriter: w}
		engine.ServeHTTP(recorder, r)

		g.mu.Lock()
		g.requests = append(g.requests, gwRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Status: recorder.statusCode(),
			Bytes:  recorder.bytes,
		})
		g.mu.Unlock()
	})
}

// recordedRequests 取已记录的请求(副本)
func (g *gateway) recordedRequests() []gwRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]gwRequest(nil), g.requests...)
}

// countRequests 统计某个路径被处理的次数
func (g *gateway) countRequests(path string) int {
	count := 0
	for _, req := range g.recordedRequests() {
		if req.Path == path {
			count++
		}
	}
	return count
}

// totalResponseBytes 网关写出的响应字节总数
func (g *gateway) totalResponseBytes() int64 {
	var total int64
	for _, req := range g.recordedRequests() {
		total += req.Bytes
	}
	return total
}

// countingResponseWriter 统计状态码与写出字节数
type countingResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *countingResponseWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *countingResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *countingResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// ---------------------------------------------------------------------------
// 假 Google / 假面板 / 假 Emby 源
// ---------------------------------------------------------------------------

// fakeGoogle 假 Google 下载端点
//
// 用 http.ServeContent: Range / 206 / Content-Range / If-Range 都是 net/http
// 的真实实现, 而不是用例自己糊出来的语义。
type fakeGoogle struct {
	server  *httptest.Server
	content []byte

	// requests 收到的请求数
	requests atomic.Int64
	// badAuth 凭据不对的请求数
	badAuth atomic.Int64
	// ranges 收到的 Range 头
	rangesMu sync.Mutex
	ranges   []string
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()

	g := &fakeGoogle{content: e2eMediaContent()}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests.Add(1)
		if r.Header.Get("Authorization") != e2eGoogleCredential {
			g.badAuth.Add(1)
			w.WriteHeader(http.StatusForbidden)
			return
		}

		g.rangesMu.Lock()
		g.ranges = append(g.ranges, r.Header.Get("Range"))
		g.rangesMu.Unlock()

		http.ServeContent(w, r, "movie.mkv", time.Unix(1700000000, 0), bytes.NewReader(g.content))
	}))
	t.Cleanup(g.server.Close)

	return g
}

// lastRange 最近一次收到的 Range 头
func (g *fakeGoogle) lastRange() string {
	g.rangesMu.Lock()
	defer g.rangesMu.Unlock()
	if len(g.ranges) == 0 {
		return ""
	}
	return g.ranges[len(g.ranges)-1]
}

// sawRange 是否收到过指定的 Range 头
func (g *fakeGoogle) sawRange(want string) bool {
	g.rangesMu.Lock()
	defer g.rangesMu.Unlock()
	for _, got := range g.ranges {
		if got == want {
			return true
		}
	}
	return false
}

// fakePanel 假 GD 管理面板(/api/dl)
type fakePanel struct {
	server    *httptest.Server
	directURL string

	// calls 被调用次数
	calls atomic.Int64
	// badToken 令牌不对的请求数
	badToken atomic.Int64
}

func newFakePanel(t *testing.T, directURL string) *fakePanel {
	t.Helper()

	p := &fakePanel{directURL: directURL}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.calls.Add(1)
		if r.URL.Path != "/api/dl" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+e2ePanelToken {
			p.badToken.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		gdPath := r.URL.Query().Get("path")

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"data": map[string]any{
				"url":        p.directURL,
				"headers":    map[string]string{"Authorization": e2eGoogleCredential},
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				"file": map[string]any{
					"id":        "e2e-file-id",
					"name":      "movie.mkv",
					"path":      gdPath,
					"size":      e2eMediaSize,
					"mime_type": "video/x-matroska",
				},
			},
		})
	}))
	t.Cleanup(p.server.Close)

	return p
}

// fakeEmbyOrigin 假 Emby 源
//
// 与 redirect_gdrive_test.go 里的同名设施同构, 差别只有一处: 媒体路径可以在
// 用例中途改写(一个 harness 会走多次播放), 因此用 atomic.Value 而不是裸字段。
type fakeEmbyOrigin struct {
	server *httptest.Server

	// mediaPath PlaybackInfo 返回的媒体路径
	mediaPath atomic.Value
	// originHits 回源转发命中次数(PlaybackInfo 不算)
	originHits atomic.Int64
	// playbackProbes 每次收到 PlaybackInfo 探测发一个信号
	playbackProbes chan struct{}
}

func newFakeEmbyOrigin(t *testing.T) *fakeEmbyOrigin {
	t.Helper()

	f := &fakeEmbyOrigin{playbackProbes: make(chan struct{}, 64)}
	f.mediaPath.Store("")
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/PlaybackInfo") {
			select {
			case f.playbackProbes <- struct{}{}:
			default:
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaSources": []map[string]any{{"Path": f.mediaPath.Load(), "Id": "ms-1"}},
			})
			return
		}

		f.originHits.Add(1)
		w.Header().Set("Content-Type", "video/x-matroska")
		_, _ = w.Write([]byte("origin-bytes"))
	}))
	t.Cleanup(f.server.Close)

	return f
}

// setMediaPath 设置下一次播放要返回的媒体路径
func (f *fakeEmbyOrigin) setMediaPath(path string) { f.mediaPath.Store(path) }

// playbackProbeQuiet 判定"异步探测已全部落地"的静默窗口
const playbackProbeQuiet = 100 * time.Millisecond

// waitForPlaybackProbes 等待所有异步 PlaybackInfo 探测落地
//
// 与 redirect_gdrive_test.go 里的同名方法同因: 播放入口会 go 一个读全局
// config.C 的探测 goroutine, 用例结束还原 config.C 之前必须等它跑完,
// 否则它会在用例之外读到空配置而崩掉整个测试进程。
func (f *fakeEmbyOrigin) waitForPlaybackProbes(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
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

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// harness 一次端到端场景的全部组件
type harness struct {
	t *testing.T

	gateway *gateway
	panel   *fakePanel
	google  *fakeGoogle
	origin  *fakeEmbyOrigin

	// basePath 当前 config.BasePath(agent 状态目录的父目录)
	basePath string
	// agentCfg 当前注入的 agent 网络配置
	agentCfg *config.AgentNetwork

	oldConfig   *config.Config
	oldBasePath string
}

// gdPathSeq 生成进程内唯一 Drive 路径的序号
//
// 必须是进程级而不是场景级: -count=2 会在同一个进程里把每个用例再跑一遍,
// 场景级计数器会从零开始, 于是第二轮复用第一轮的路径 —— 而 gdrive 的直链缓存
// 按路径为键且条目不会主动过期, 第二轮会拿到指向上一轮假服务器的死直链。
var gdPathSeq atomic.Uint64

// newHarness 组装场景
//
// agentCfgDoc 为 yaml 片段(与 config-example.yml 的 agent-network 段同构),
// 空串表示"没有这段配置"。
func newHarness(t *testing.T, agentCfgDoc string) *harness {
	t.Helper()

	// 环境变量会静默覆盖配置: 先清掉, 否则跑在 CI/开发机上都可能读到外部值
	t.Setenv(config.AgentEnrollTokenEnvName, "")
	t.Setenv(config.GDriveApiTokenEnvName, "")

	h := &harness{t: t, basePath: t.TempDir()}

	h.google = newFakeGoogle(t)
	h.panel = newFakePanel(t, h.google.server.URL)
	h.origin = newFakeEmbyOrigin(t)

	if strings.TrimSpace(agentCfgDoc) != "" {
		h.agentCfg = mustAgentConfig(t, agentCfgDoc)
	}

	gdriveCfg := &config.GDrive{
		Enable:      true,
		ApiBase:     h.panel.server.URL,
		ApiToken:    e2ePanelToken,
		MountPrefix: e2eMountPrefix,
	}
	if err := gdriveCfg.Init(); err != nil {
		t.Fatalf("初始化 gdrive 测试配置失败: %v", err)
	}

	// 先写全局配置, 再启动网关: 处理器的 goroutine 必须"出生"在配置写入之后,
	// 否则它会与这次写入构成并发访问(生产里配置也只在启动时写一次)
	h.oldConfig, h.oldBasePath = config.C, config.BasePath
	config.C = &config.Config{
		Emby:         &config.Emby{Host: h.origin.server.URL},
		GDrive:       gdriveCfg,
		AgentNetwork: h.agentCfg,
		Ge2o:         &config.Ge2o{ApiSecret: e2eAdminSecret},
	}
	config.BasePath = h.basePath

	h.gateway = newGateway(t)

	// 清理顺序: 先停网关(等所有在途处理器退出 —— 这之后没有读者了),
	// 再还原全局配置。agent 子进程的清理注册得更晚, 因此会先执行。
	t.Cleanup(func() {
		h.gateway.stop()
		config.C = h.oldConfig
		config.BasePath = h.oldBasePath
	})

	return h
}

// mustAgentConfig 按真实配置解析路径构造 agent 网络配置
//
// 走 yaml + Init: fallback-to-local 的默认值与显式 false 必须靠 UnmarshalYAML
// 区分, 直接构造结构体会绕过这条契约。
func mustAgentConfig(t *testing.T, doc string) *config.AgentNetwork {
	t.Helper()

	t.Setenv(config.AgentEnrollTokenEnvName, "")

	cfg := new(config.AgentNetwork)
	if err := yaml.Unmarshal([]byte(doc), cfg); err != nil {
		t.Fatalf("解析 agent 网络测试配置失败: %v", err)
	}
	if err := cfg.Init(); err != nil {
		t.Fatalf("初始化 agent 网络测试配置失败: %v", err)
	}
	return cfg
}

// swapAgentConfig 替换 agent 网络配置(等价于生产里的"改配置 + 重启网关")
//
// 必须先把网关停干净再写: config.C 是无锁全局变量, 写入时不能有读者。
// 调用方仍要保证此刻没有 agent 在跑(否则节点会在停机窗口里白白失败一次心跳)。
func (h *harness) swapAgentConfig(t *testing.T, cfg *config.AgentNetwork) {
	t.Helper()

	h.gateway.stop()
	h.agentCfg = cfg
	config.C.AgentNetwork = cfg
	h.gateway.restart(t)
}

// uniqueGDPath 生成进程内唯一的 Drive 路径
//
// gdrive 的直链缓存是进程级全局的、按路径为键且条目不会主动过期:
// go test -count=2 时同一条路径会命中上一轮的直链, 而那条直链指向的假服务器
// 早已关闭。每次调用换一条路径, 用例只可能命中自己本轮启动的假面板。
func (h *harness) uniqueGDPath(name string) string {
	ext := filepath.Ext(name)
	return fmt.Sprintf("/影视库/e2e/%s-%d%s", strings.TrimSuffix(name, ext), gdPathSeq.Add(1), ext)
}

// playback 走一遍播放入口
//
// 直调 emby.Redirect2OpenlistLink(既有用例的同一模式): 本矩阵验证的是端到端
// 契约, 不是 gin 路由表; 请求用相对 URI 构造, 保证 RequestURI 是 origin-form。
func (h *harness) playback(gdPath string) *httptest.ResponseRecorder {
	h.t.Helper()

	h.origin.setMediaPath(e2eMountPrefix + gdPath)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/emby/Videos/123/stream", nil)

	emby.Redirect2OpenlistLink(c)

	// 播放会异步发 PlaybackInfo 探测; 不等它落地, 用例结束还原 config.C 后
	// 那个 goroutine 会读到空配置把整个测试进程带走
	h.origin.waitForPlaybackProbes(h.t)
	return recorder
}

// ---------------------------------------------------------------------------
// agent 子进程
// ---------------------------------------------------------------------------

// agentProcess 一个真实 agent 子进程
type agentProcess struct {
	t          *testing.T
	configPath string
	port       int
	logs       *lockedBuffer

	mu     sync.Mutex
	cmd    *exec.Cmd
	exited chan struct{}
}

// startAgent enroll(一次性) + serve, 等到节点在 master 侧可见为在线
func (h *harness) startAgent() (*agentProcess, agentView) {
	h.t.Helper()

	p := &agentProcess{
		t:          h.t,
		configPath: filepath.Join(h.t.TempDir(), "agent.env"),
		port:       freeTCPPort(h.t),
		logs:       new(lockedBuffer),
	}

	// 走真实二进制的 enroll 子命令: 请求、凭据落盘、配置文件格式都是被测对象
	enroll := exec.Command(agentBinaryPath, "enroll",
		"--master", h.gateway.masterURL(),
		"--token", e2eEnrollToken,
		"--port", strconv.Itoa(p.port),
		"--config", p.configPath,
	)
	if out, err := enroll.CombinedOutput(); err != nil {
		h.t.Fatalf("agent enroll 失败: %v\n%s", err, out)
	}

	p.startServe()
	h.t.Cleanup(func() {
		p.stop()
		if h.t.Failed() {
			h.t.Logf("agent 子进程日志:\n%s", p.logs.String())
		}
	})

	return p, h.waitAgentOnline(45 * time.Second)
}

// startServe 以现有配置启动 serve 并等到端口可连
func (p *agentProcess) startServe() {
	p.t.Helper()

	cmd := exec.Command(agentBinaryPath, "serve", "--config", p.configPath)
	cmd.Stdout, cmd.Stderr = p.logs, p.logs
	if err := cmd.Start(); err != nil {
		p.t.Fatalf("启动 agent serve 失败: %v", err)
	}

	exited := make(chan struct{})
	p.mu.Lock()
	p.cmd, p.exited = cmd, exited
	p.mu.Unlock()

	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	waitForTCP(p.t, "127.0.0.1:"+strconv.Itoa(p.port), 15*time.Second)
}

// stop 结束当前 serve 进程(SIGTERM → 等待 → 兜底 KILL)
//
// 可重复调用: 取走当前进程句柄后置空, 清理阶段再调一次也不会误伤重启后的进程。
func (p *agentProcess) stop() {
	p.mu.Lock()
	cmd, exited := p.cmd, p.exited
	p.cmd, p.exited = nil, nil
	p.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}

	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
	}
}

// ---------------------------------------------------------------------------
// master 侧观测 / 操作
// ---------------------------------------------------------------------------

// agentView 管理接口返回的节点视图(E2 的观测面)
type agentView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	MachineID     string `json:"machine_id"`
	Enabled       bool   `json:"enabled"`
	Online        bool   `json:"online"`
	Version       string `json:"version"`
	LastSeenAt    string `json:"last_seen_at"`
	LastIP        string `json:"last_ip"`
	ActiveStreams int    `json:"active_streams"`
	PublicBaseURL string `json:"public_base_url"`
	Address       string `json:"address"`
	ListenPort    int    `json:"listen_port"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// adminEnvelope 管理接口响应信封(model.Response 的同构解码)
type adminEnvelope struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    *struct {
		Agents []agentView `json:"agents"`
	} `json:"data"`
}

// adminCall 调用一个管理接口
func (h *harness) adminCall(path, body string) (int, adminEnvelope) {
	h.t.Helper()

	target := h.gateway.masterURL() + path
	if body == "" {
		body = "{}"
	}
	resp, err := http.Post(target, "application/json", strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("调用管理接口 %s 失败: %v", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("读取管理接口 %s 响应失败: %v", path, err)
	}

	var envelope adminEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		h.t.Fatalf("管理接口 %s 的响应不是合法 JSON: %v, body=%s", path, err, raw)
	}
	return resp.StatusCode, envelope
}

// listAgents 读取节点列表
func (h *harness) listAgents() []agentView {
	h.t.Helper()

	status, envelope := h.adminCall(constant.Route_AgentNetworkAgents, `{"secret":"`+e2eAdminSecret+`"}`)
	if status != http.StatusOK || !envelope.Success || envelope.Data == nil {
		h.t.Fatalf("读取节点列表失败: HTTP %d, message=%s", status, envelope.Message)
	}
	return envelope.Data.Agents
}

// waitAgentOnline 等到唯一节点被判定为在线
func (h *harness) waitAgentOnline(timeout time.Duration) agentView {
	h.t.Helper()

	deadline := time.Now().Add(timeout)
	var last []agentView
	for time.Now().Before(deadline) {
		last = h.listAgents()
		for _, view := range last {
			if view.Online {
				return view
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.t.Fatalf("等待节点上线超时(%v), 最后一次列表: %+v", timeout, last)
	return agentView{}
}

// waitAgentOffline 等到指定节点被判定为离线(超过 offline-seconds 未心跳)
func (h *harness) waitAgentOffline(id string, timeout time.Duration) agentView {
	h.t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, view := range h.listAgents() {
			if view.ID == id && !view.Online {
				return view
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.t.Fatalf("等待节点 %s 离线超时(%v)", id, timeout)
	return agentView{}
}

// setAgentEnabled 通过管理接口启停节点
func (h *harness) setAgentEnabled(id string, enabled bool) {
	h.t.Helper()

	body := fmt.Sprintf(`{"secret":%q,"id":%q,"enabled":%t}`, e2eAdminSecret, id, enabled)
	status, envelope := h.adminCall(constant.Route_AgentNetworkAgentsUpdate, body)
	if status != http.StatusOK || !envelope.Success {
		h.t.Fatalf("启停节点失败: HTTP %d, message=%s", status, envelope.Message)
	}
}

// ---------------------------------------------------------------------------
// agents.json 观测
// ---------------------------------------------------------------------------

// agentFileEntry agents.json 里的一条节点记录
//
// 生产格式里就有明文 secret / sign_key(文件仅本机可读): E3 的签名独立重算
// 正是从这里取密钥 —— 不调用被测的签名函数, 否则"两侧同一个错误实现"就测不出来。
type agentFileEntry struct {
	ID            string `json:"id"`
	MachineID     string `json:"machine_id"`
	Secret        string `json:"secret"`
	SignKey       string `json:"sign_key"`
	PublicBaseURL string `json:"public_base_url"`
	ListenPort    int    `json:"listen_port"`
	Enabled       bool   `json:"enabled"`
}

// agentsFileDoc agents.json 的同构解码(只覆盖用例关心的字段)
type agentsFileDoc struct {
	Version int              `json:"version"`
	Agents  []agentFileEntry `json:"agents"`
}

// agentsFilePath 当前 BasePath 下的注册表文件路径
func (h *harness) agentsFilePath() string {
	return filepath.Join(h.basePath, agentnet.DirName, "agents.json")
}

// readAgentsFile 读取并解析当前注册表文件
func (h *harness) readAgentsFile() agentsFileDoc {
	h.t.Helper()

	raw, err := os.ReadFile(h.agentsFilePath())
	if err != nil {
		h.t.Fatalf("读取注册表失败: %v", err)
	}
	var doc agentsFileDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		h.t.Fatalf("注册表不是合法 JSON: %v, content=%s", err, raw)
	}
	return doc
}

// readAgentsFileRaw 读取注册表原始字节(E13 比对"重启不改盘")
func (h *harness) readAgentsFileRaw() []byte {
	h.t.Helper()

	raw, err := os.ReadFile(h.agentsFilePath())
	if err != nil {
		h.t.Fatalf("读取注册表失败: %v", err)
	}
	return raw
}

// agentRecordInFile 取注册表里指定节点的记录
func (h *harness) agentRecordInFile(id string) agentFileEntry {
	h.t.Helper()

	for _, entry := range h.readAgentsFile().Agents {
		if entry.ID == id {
			return entry
		}
	}
	h.t.Fatalf("注册表里没有节点 %s", id)
	return agentFileEntry{}
}

// ---------------------------------------------------------------------------
// HTTP 客户端
// ---------------------------------------------------------------------------

// noRedirectClient 不跟随重定向的客户端
//
// 测的是播放入口吐出的 302 本身, 跟随重定向会把断言点带偏。
func noRedirectClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// doRange 直接向 agent 发一个带 Range 的 GET
func doRange(t *testing.T, target, byteRange string) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}

	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", target, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取 %s 响应失败: %v", target, err)
	}
	return resp, body
}

// ---------------------------------------------------------------------------
// 通用小工具
// ---------------------------------------------------------------------------

// freeTCPPort 取一个空闲端口
//
// 一律动态取: 本机 38xxx 段有常驻服务用过, 写死端口迟早撞上。
func freeTCPPort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("探测空闲端口失败: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// waitForTCP 等待地址可连接
func waitForTCP(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待 %s 可连接超时: %v", addr, lastErr)
}

// waitForPortReleased 等待端口不再可连接
func waitForPortReleased(addr string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(30 * time.Millisecond)
	}
}

// mustContain 断言字符串包含片段
func mustContain(t *testing.T, got, want string) {
	t.Helper()

	if !strings.Contains(got, want) {
		t.Errorf("应包含 %q, 实际: %q", want, got)
	}
}

// agentURL agent 服务地址
func (p *agentProcess) baseURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(p.port)
}

// parseAgentRedirect 解析 302 的 Location
type agentRedirect struct {
	baseURL string
	fileID  string
	gdPath  string
	expiry  int64
	sign    string
}

// parseAgentRedirect 解析 302 的 Location
func parseAgentRedirect(t *testing.T, location string) agentRedirect {
	t.Helper()

	u, err := url.Parse(location)
	if err != nil {
		t.Fatalf("重定向地址不是合法 URL: %v", err)
	}
	if !strings.HasPrefix(u.Path, "/dl/") {
		t.Fatalf("重定向路径 = %q, want /dl/<file_id>", u.Path)
	}

	fileID := strings.TrimPrefix(u.Path, "/dl/")
	decoded, err := base64.RawURLEncoding.DecodeString(fileID)
	if err != nil {
		t.Fatalf("file_id 不是合法 base64url: %v", err)
	}

	expiry, err := strconv.ParseInt(u.Query().Get("e"), 10, 64)
	if err != nil {
		t.Fatalf("e 参数不是十进制时间戳: %q", u.Query().Get("e"))
	}

	return agentRedirect{
		baseURL: u.Scheme + "://" + u.Host,
		fileID:  fileID,
		gdPath:  string(decoded),
		expiry:  expiry,
		sign:    u.Query().Get("s"),
	}
}

// withQuery 替换 URL 上的查询参数
func withQuery(t *testing.T, raw string, mutate func(url.Values)) string {
	t.Helper()

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("地址不是合法 URL: %v", err)
	}
	values := u.Query()
	mutate(values)
	u.RawQuery = values.Encode()
	return u.String()
}

// signLikeMaster 按协议独立重算签名
//
// HMAC-SHA256(key = sign_key 的 32 字节, msg = "v1\n<file_id>\n<e>") 的小写 hex
// (design §3)。刻意不调用被测代码里的签名函数。
func signLikeMaster(t *testing.T, signKeyHex, fileID, expiry string) string {
	t.Helper()

	key, err := hex.DecodeString(signKeyHex)
	if err != nil {
		t.Fatalf("sign_key 不是合法 hex: %v", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("v1\n" + fileID + "\n" + expiry))
	return hex.EncodeToString(mac.Sum(nil))
}

// startTimer 记录耗时(证据里带上"真的等满了 offline-seconds")
func startTimer() func() time.Duration {
	start := time.Now()
	return func() time.Duration { return time.Since(start) }
}
