package emby_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/emby"
)

// preheatAgentEntryJSON 拼一条 public_base_url 指向假 agent 的节点记录
//
// 其余字段与 redirect_agent_test.go 的固定凭据保持一致(id / secret / sign_key),
// 直接复用那里的 heartbeatTestAgent 让节点进入可调度状态。
func preheatAgentEntryJSON(publicBaseURL string) string {
	return fmt.Sprintf(`{"id":%q,"machine_id":%q,"name":"node-preheat","secret":%q,"sign_key":%q,`+
		`"public_base_url":%q,"listen_port":8790,"version":"v1.0.0","last_ip":"","enabled":true,`+
		`"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`,
		agentTestID, agentTestMachineID, agentTestSecret, agentTestSignKey, publicBaseURL)
}

// withPreheatTestConfig 注入预热用例需要的整份配置
//
// 比 withAgentTestConfig 多补了 Cache 与 VideoPreview 两个子配置:
// TransferPlaybackInfo / LoadCacheItems 主流程会直接取它们的字段,
// 空指针会在 handler 里崩掉(生产环境由配置加载器的反射填充保证非空)。
func withPreheatTestConfig(t *testing.T, host, apiBase string, agentCfg *config.AgentNetwork, basePath string) {
	t.Helper()

	oldConfig, oldBasePath := config.C, config.BasePath

	gdriveCfg := &config.GDrive{MountPrefix: "/home/googleDrive"}
	if apiBase != "" {
		gdriveCfg.Enable = true
		gdriveCfg.ApiBase = apiBase
		gdriveCfg.ApiToken = "test-panel-api-token"
	}

	config.C = &config.Config{
		Emby:         &config.Emby{Host: host},
		GDrive:       gdriveCfg,
		AgentNetwork: agentCfg,
		Cache:        &config.Cache{},
		VideoPreview: &config.VideoPreview{},
	}
	config.BasePath = basePath

	t.Cleanup(func() {
		// 先等所有预热 goroutine 退出, 再还原全局配置:
		// 预热会读取全局配置, 还原的写与它们的读必须有确定的先后关系
		waitPreheatIdle(t)
		config.C = oldConfig
		config.BasePath = oldBasePath
	})
}

// waitPreheatIdle 有上限地等待所有预热 goroutine 退出
//
// 预热请求本身自带 8s 超时, 这里加一层独立上限: 万一节点侧出问题, 让用例明确
// 失败而不是把测试进程拖住。
func waitPreheatIdle(t *testing.T) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		emby.WaitingPreheat()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("等待预热 goroutine 退出超时")
	}
}

// preheatEmbyOrigin 预热用例专用的假 Emby 源
//
// 同时响应条目详情(非 PlaybackInfo 路径)与 PlaybackInfo 查询, 并给 PlaybackInfo
// 计数: 预热是异步的, 计数是"预热 goroutine 已跑到取路径口"的同步点。
type preheatEmbyOrigin struct {
	// server 假服务端
	server *httptest.Server
	// mediaPath PlaybackInfo 返回的媒体路径
	mediaPath string
	// itemType 条目详情响应中的 Type
	itemType string
	// playbackInfoHits PlaybackInfo 查询的命中次数
	playbackInfoHits atomic.Int64
	// brokenPlaybackInfo 为 true 时 PlaybackInfo 一律返回失败响应(模拟路径解析失败)
	brokenPlaybackInfo atomic.Bool
}

// newPreheatEmbyOrigin 启动假 Emby 源
func newPreheatEmbyOrigin(t *testing.T, mediaPath, itemType string) *preheatEmbyOrigin {
	t.Helper()

	f := &preheatEmbyOrigin{mediaPath: mediaPath, itemType: itemType}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/PlaybackInfo") {
			f.playbackInfoHits.Add(1)
			if f.brokenPlaybackInfo.Load() {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("boom"))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaSources": []map[string]any{{"Path": f.mediaPath, "Id": "ms-1"}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id":   path.Base(r.URL.Path),
			"Type": f.itemType,
			"Path": f.mediaPath,
		})
	}))
	t.Cleanup(f.server.Close)

	return f
}

// preheatFakeAgent 假 agent 节点
//
// 只做预热用例需要的事: 记录收到的请求(完整地址与 Range 头), 并按需挂起不响应。
type preheatFakeAgent struct {
	// server 假服务端
	server *httptest.Server
	// calls 收到的请求数
	calls atomic.Int64
	// lastURL 最近一次请求的完整地址(含 e 与 s 参数)
	lastURL atomic.Value
	// lastRange 最近一次请求的 Range 头
	lastRange atomic.Value
	// hang 为 true 时挂起不响应, 直到 unblock 被调用
	hang bool
	// release 解除挂起的信号
	release chan struct{}
	// releaseOnce 保证 release 只被关闭一次
	releaseOnce sync.Once
}

// newPreheatFakeAgent 启动假 agent
func newPreheatFakeAgent(t *testing.T, hang bool) *preheatFakeAgent {
	t.Helper()

	f := &preheatFakeAgent{hang: hang, release: make(chan struct{})}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.lastURL.Store(r.URL.String())
		f.lastRange.Store(r.Header.Get("Range"))

		if f.hang {
			<-f.release
		}
		w.Header().Set("Content-Type", "video/x-matroska")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("preheat-bytes"))
	}))
	// 注册顺序即执行顺序的反向: unblock 必须先于 Close, 否则 Close 会一直等挂起的请求
	t.Cleanup(f.server.Close)
	t.Cleanup(f.unblock)

	return f
}

// unblock 解除挂起(幂等)
func (f *preheatFakeAgent) unblock() {
	f.releaseOnce.Do(func() { close(f.release) })
}

// preheatQuietWindow 判定"不会再有请求发生"的静默窗口
const preheatQuietWindow = 200 * time.Millisecond

// waitForAgentCalls 等待假 agent 收到至少 want 个请求
func waitForAgentCalls(t *testing.T, agent *preheatFakeAgent, want int64) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if agent.calls.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 agent 收到 %d 个请求超时, 实际: %d", want, agent.calls.Load())
}

// assertNoAgentCalls 断言静默窗口内没有预热请求落到节点上
func assertNoAgentCalls(t *testing.T, agent *preheatFakeAgent) {
	t.Helper()

	time.Sleep(preheatQuietWindow)
	if got := agent.calls.Load(); got != 0 {
		t.Errorf("不应触发预热, 但节点收到了 %d 个请求", got)
	}
}

// waitForLog 等待日志收集器出现指定片段
func waitForLog(t *testing.T, logger *redirectLogCollector, sub string) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if logger.contains(sub) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待日志 %q 超时, 实际: %s", sub, logger.String())
}

// assertPreheatProbe 校验一次预热请求的协议形状, 返回完整地址
//
// 独立解码 file_id(base64url 无填充)得到 Drive 路径 —— 刻意不调用被测代码里的
// 编码函数, 否则"两侧用同一个错误实现"就测不出来了。
func assertPreheatProbe(t *testing.T, agent *preheatFakeAgent, wantGDPath string) string {
	t.Helper()

	raw, _ := agent.lastURL.Load().(string)
	if raw == "" {
		t.Fatal("agent 没有记录到任何请求地址")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("预热请求地址不是合法 URL: %v", err)
	}

	if !strings.HasPrefix(u.Path, "/dl/") {
		t.Fatalf("预热请求路径 = %q, want /dl/<file_id>", u.Path)
	}
	fileID := strings.TrimPrefix(u.Path, "/dl/")
	decoded, err := base64.RawURLEncoding.DecodeString(fileID)
	if err != nil {
		t.Fatalf("file_id 不是合法的 base64url: %v", err)
	}
	if string(decoded) != wantGDPath {
		t.Errorf("file_id 解码结果 = %q, want %q", string(decoded), wantGDPath)
	}

	if got, _ := agent.lastRange.Load().(string); got != "bytes=0-65535" {
		t.Errorf("Range 头 = %q, want bytes=0-65535", got)
	}

	expiry, sign := u.Query().Get("e"), u.Query().Get("s")
	if expiry == "" || sign == "" {
		t.Fatalf("签名参数缺失: e=%q s=%q", expiry, sign)
	}
	expirySeconds, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil {
		t.Fatalf("e 参数不是十进制时间戳: %v", err)
	}
	if time.Unix(expirySeconds, 0).Before(time.Now()) {
		t.Errorf("e 参数 %d 已经过期", expirySeconds)
	}

	return raw
}

// TestPreheat_PlaybackInfoFiresRangeProbe PlaybackInfo 时刻触发一次小 Range 预热
func TestPreheat_PlaybackInfoFiresRangeProbe(t *testing.T) {
	gdPath := uniqueGDPath("/影视库/预热/电影.mkv")
	origin := newPreheatEmbyOrigin(t, "/home/googleDrive"+gdPath, "Movie")
	agent := newPreheatFakeAgent(t, false)

	basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+preheatAgentEntryJSON(agent.server.URL)+`]}`)
	agentCfg := mustAgentConfig(t, "enable: true\nenroll-token: test-enroll-token-0123456789\n")
	withPreheatTestConfig(t, origin.server.URL, "http://panel.invalid", agentCfg, basePath)
	heartbeatTestAgent(t, 2)

	logger := captureRedirectLogs(t)
	c, recorder := newRedirectContext(t, "/emby/Items/123/PlaybackInfo?api_key=test-key")
	emby.TransferPlaybackInfo(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("响应码 = %d, want 200, body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "MediaSources") {
		t.Errorf("主流程响应不受影响, 实际: %s", recorder.Body.String())
	}

	waitForAgentCalls(t, agent, 1)
	waitForLog(t, logger, "[网关预热] 已触发")

	signedURL := assertPreheatProbe(t, agent, gdPath)
	if got := agent.calls.Load(); got != 1 {
		t.Errorf("预热请求数 = %d, want 1", got)
	}
	if !logger.contains(gdPath) {
		t.Errorf("预热日志应包含文件路径(便于排查), 实际: %s", logger.String())
	}

	// 日志卫生: 完整签名地址与签名参数都不得进日志
	if parsed, err := url.Parse(signedURL); err == nil {
		if sign := parsed.Query().Get("s"); sign != "" && logger.contains(sign) {
			t.Errorf("日志泄露了签名参数: %s", logger.String())
		}
	}
	if logger.contains(signedURL) {
		t.Errorf("日志泄露了完整签名地址: %s", logger.String())
	}
}

// TestPreheat_ItemsDetailFiresRangeProbe 浏览 single item 详情页时触发预热
//
// VideoPreview 在用例配置里是关闭的: 预热与视频预览功能必须互不依赖。
func TestPreheat_ItemsDetailFiresRangeProbe(t *testing.T) {
	gdPath := uniqueGDPath("/影视库/预热/剧集.mkv")
	origin := newPreheatEmbyOrigin(t, "/home/googleDrive"+gdPath, "Episode")
	agent := newPreheatFakeAgent(t, false)

	basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+preheatAgentEntryJSON(agent.server.URL)+`]}`)
	agentCfg := mustAgentConfig(t, "enable: true\nenroll-token: test-enroll-token-0123456789\n")
	withPreheatTestConfig(t, origin.server.URL, "http://panel.invalid", agentCfg, basePath)
	heartbeatTestAgent(t, 0)

	logger := captureRedirectLogs(t)
	c, recorder := newRedirectContext(t, "/emby/Users/u1/Items/456?api_key=test-key")
	emby.LoadCacheItems(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("响应码 = %d, want 200, body=%s", recorder.Code, recorder.Body.String())
	}

	waitForAgentCalls(t, agent, 1)
	waitForLog(t, logger, "[网关预热] 已触发")
	assertPreheatProbe(t, agent, gdPath)
}

// TestPreheat_ItemsDetailNonPlayableTypeSkipped 非 movie/episode 的条目详情不预热
//
// Movie / Episode 之外的详情页(剧集、文件夹)没有可播放的媒体文件,
// 必须被类型判断挡住: 否则会白白多打一次 Emby PlaybackInfo 查询。
func TestPreheat_ItemsDetailNonPlayableTypeSkipped(t *testing.T) {
	gdPath := uniqueGDPath("/影视库/预热/剧集详情.mkv")
	origin := newPreheatEmbyOrigin(t, "/home/googleDrive"+gdPath, "Series")
	agent := newPreheatFakeAgent(t, false)

	basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+preheatAgentEntryJSON(agent.server.URL)+`]}`)
	agentCfg := mustAgentConfig(t, "enable: true\nenroll-token: test-enroll-token-0123456789\n")
	withPreheatTestConfig(t, origin.server.URL, "http://panel.invalid", agentCfg, basePath)
	heartbeatTestAgent(t, 0)

	logger := captureRedirectLogs(t)
	c, recorder := newRedirectContext(t, "/emby/Users/u1/Items/789?api_key=test-key")
	emby.LoadCacheItems(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("响应码 = %d, want 200", recorder.Code)
	}
	assertNoAgentCalls(t, agent)
	if hits := origin.playbackInfoHits.Load(); hits != 0 {
		t.Errorf("非可播放类型不应发起 PlaybackInfo 查询, 实际 %d 次", hits)
	}
	if logger.contains("[网关预热]") {
		t.Errorf("非可播放类型不应留下预热日志, 实际: %s", logger.String())
	}
}

// TestPreheat_DedupWithinTTL 同一文件在 TTL 内重复浏览只预热一次
func TestPreheat_DedupWithinTTL(t *testing.T) {
	gdPath := uniqueGDPath("/影视库/预热/去重.mkv")
	origin := newPreheatEmbyOrigin(t, "/home/googleDrive"+gdPath, "Movie")
	agent := newPreheatFakeAgent(t, false)

	basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+preheatAgentEntryJSON(agent.server.URL)+`]}`)
	agentCfg := mustAgentConfig(t, "enable: true\nenroll-token: test-enroll-token-0123456789\n")
	withPreheatTestConfig(t, origin.server.URL, "http://panel.invalid", agentCfg, basePath)
	heartbeatTestAgent(t, 0)

	logger := captureRedirectLogs(t)

	// 第一次浏览: 触发
	c1, _ := newRedirectContext(t, "/emby/Items/123/PlaybackInfo?api_key=test-key")
	emby.TransferPlaybackInfo(c1)
	waitForAgentCalls(t, agent, 1)
	waitForLog(t, logger, "[网关预热] 已触发")

	// 第二次浏览: 主流程都会各自打一次 PlaybackInfo 查询(每次调用两条:
	// handler 自己的 + 预热解析路径的), 等第二次的解析落地后确认没有再打枪
	c2, recorder2 := newRedirectContext(t, "/emby/Items/123/PlaybackInfo?api_key=test-key")
	emby.TransferPlaybackInfo(c2)
	if recorder2.Code != http.StatusOK {
		t.Fatalf("第二次请求响应码 = %d, want 200", recorder2.Code)
	}

	deadline := time.Now().Add(2 * time.Second)
	for origin.playbackInfoHits.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(preheatQuietWindow)

	if got := agent.calls.Load(); got != 1 {
		t.Errorf("TTL 内重复浏览触发了 %d 次预热, want 1", got)
	}
	if got := origin.playbackInfoHits.Load(); got < 4 {
		t.Errorf("第二次浏览应各自完成一次 PlaybackInfo 解析, 实际命中 %d 次", got)
	}
}

// TestPreheat_DisabledSwitchNoProbe 三处守卫任一关闭时零行为变化
//
// 覆盖 preheatReady() 的三个条件: 预热开关、agent 网络、gdrive。
// 其中"preheat-enable 为 false"同时钉住 UnmarshalYAML 的显式字段清单 ——
// 漏登记该键时 false 会被静默丢弃, 预热照常打出探测请求, 本用例即失败。
func TestPreheat_DisabledSwitchNoProbe(t *testing.T) {
	cases := []struct {
		name     string
		agentDoc string
		// heartbeat 是否需要让节点心跳(agent 网络未启用时心跳会被拒绝)
		heartbeat bool
		// gdriveDisabled 是否关闭 gdrive(节点仍在线, 只关这一个守卫)
		gdriveDisabled bool
	}{
		{"preheat-enable 为 false", "enable: true\nenroll-token: test-enroll-token-0123456789\npreheat-enable: false\n", true, false},
		{"agent 网络未启用", "enable: false\npreheat-enable: true\n", false, false},
		{"gdrive 未启用", "enable: true\nenroll-token: test-enroll-token-0123456789\n", true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gdPath := uniqueGDPath("/影视库/预热/关闭.mkv")
			origin := newPreheatEmbyOrigin(t, "/home/googleDrive"+gdPath, "Movie")
			agent := newPreheatFakeAgent(t, false)

			// apiBase 为空表示 gdrive 未启用
			apiBase := "http://panel.invalid"
			if tc.gdriveDisabled {
				apiBase = ""
			}

			basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+preheatAgentEntryJSON(agent.server.URL)+`]}`)
			agentCfg := mustAgentConfig(t, tc.agentDoc)
			withPreheatTestConfig(t, origin.server.URL, apiBase, agentCfg, basePath)
			if tc.heartbeat {
				heartbeatTestAgent(t, 0)
			}

			logger := captureRedirectLogs(t)
			c, recorder := newRedirectContext(t, "/emby/Items/123/PlaybackInfo?api_key=test-key")
			emby.TransferPlaybackInfo(c)

			if recorder.Code != http.StatusOK {
				t.Fatalf("响应码 = %d, want 200", recorder.Code)
			}
			assertNoAgentCalls(t, agent)
			if logger.contains("[网关预热]") {
				t.Errorf("关闭时不应留下预热日志, 实际: %s", logger.String())
			}
		})
	}
}

// TestPreheat_HangingAgentDoesNotDelayHandler 节点挂起不响应时, 被拦截请求耗时不受影响
//
// 这是"绝不阻塞"的核心断言: 预热请求打给一个永不响应的节点, PlaybackInfo
// 的响应耗时必须与关闭预热时同量级。
func TestPreheat_HangingAgentDoesNotDelayHandler(t *testing.T) {
	gdPath := uniqueGDPath("/影视库/预热/挂起.mkv")
	origin := newPreheatEmbyOrigin(t, "/home/googleDrive"+gdPath, "Movie")
	agent := newPreheatFakeAgent(t, true)

	basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+preheatAgentEntryJSON(agent.server.URL)+`]}`)

	// baseline: 关闭预热, 同一 handler 的耗时
	offCfg := mustAgentConfig(t, "enable: true\nenroll-token: test-enroll-token-0123456789\npreheat-enable: false\n")
	withPreheatTestConfig(t, origin.server.URL, "http://panel.invalid", offCfg, basePath)
	heartbeatTestAgent(t, 0)
	c0, recorder0 := newRedirectContext(t, "/emby/Items/123/PlaybackInfo?api_key=test-key")
	start := time.Now()
	emby.TransferPlaybackInfo(c0)
	baseline := time.Since(start)

	// 开启预热, 节点挂起
	logger := captureRedirectLogs(t)
	onCfg := mustAgentConfig(t, "enable: true\nenroll-token: test-enroll-token-0123456789\n")
	withPreheatTestConfig(t, origin.server.URL, "http://panel.invalid", onCfg, basePath)
	c1, recorder1 := newRedirectContext(t, "/emby/Items/123/PlaybackInfo?api_key=test-key")
	start = time.Now()
	emby.TransferPlaybackInfo(c1)
	elapsed := time.Since(start)

	if recorder0.Code != http.StatusOK || recorder1.Code != http.StatusOK {
		t.Fatalf("响应码 = %d / %d, want 200", recorder0.Code, recorder1.Code)
	}

	// 预热确实打出去了(否则这个用例什么都没验证到)
	waitForAgentCalls(t, agent, 1)

	if elapsed > baseline+200*time.Millisecond {
		t.Errorf("节点挂起时被拦截请求耗时 = %v, baseline = %v: 预热不得拖慢主流程", elapsed, baseline)
	}
	if elapsed > time.Second {
		t.Errorf("被拦截请求耗时 = %v, 预热不得阻塞响应", elapsed)
	}

	// 放行挂起的请求并等它落地, 避免用例结束后预热 goroutine 还在读全局配置
	agent.unblock()
	waitForLog(t, logger, "[网关预热] 已触发")
}

// TestPreheat_HangingAgentDoesNotDelayItemsDetail 详情页挂点同样不被挂起的节点拖慢
//
// 与 PlaybackInfo 挂点的差别在于: 这里 TryFire 是直接调用而非 defer, 且同步段
// 多了一次 resolveItemInfo(只读请求元数据)。两条路径都必须零同步等待。
func TestPreheat_HangingAgentDoesNotDelayItemsDetail(t *testing.T) {
	gdPath := uniqueGDPath("/影视库/预热/挂起详情.mkv")
	origin := newPreheatEmbyOrigin(t, "/home/googleDrive"+gdPath, "Episode")
	agent := newPreheatFakeAgent(t, true)

	basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+preheatAgentEntryJSON(agent.server.URL)+`]}`)

	// baseline: 关闭预热, 同一 handler 的耗时
	offCfg := mustAgentConfig(t, "enable: true\nenroll-token: test-enroll-token-0123456789\npreheat-enable: false\n")
	withPreheatTestConfig(t, origin.server.URL, "http://panel.invalid", offCfg, basePath)
	heartbeatTestAgent(t, 0)
	c0, recorder0 := newRedirectContext(t, "/emby/Users/u1/Items/456?api_key=test-key")
	start := time.Now()
	emby.LoadCacheItems(c0)
	baseline := time.Since(start)

	// 开启预热, 节点挂起
	logger := captureRedirectLogs(t)
	onCfg := mustAgentConfig(t, "enable: true\nenroll-token: test-enroll-token-0123456789\n")
	withPreheatTestConfig(t, origin.server.URL, "http://panel.invalid", onCfg, basePath)
	c1, recorder1 := newRedirectContext(t, "/emby/Users/u1/Items/456?api_key=test-key")
	start = time.Now()
	emby.LoadCacheItems(c1)
	elapsed := time.Since(start)

	if recorder0.Code != http.StatusOK || recorder1.Code != http.StatusOK {
		t.Fatalf("响应码 = %d / %d, want 200", recorder0.Code, recorder1.Code)
	}

	// 预热确实打出去了(否则这个用例什么都没验证到)
	waitForAgentCalls(t, agent, 1)

	if elapsed > baseline+200*time.Millisecond {
		t.Errorf("节点挂起时详情页耗时 = %v, baseline = %v: 预热不得拖慢主流程", elapsed, baseline)
	}
	if elapsed > time.Second {
		t.Errorf("详情页响应耗时 = %v, 预热不得阻塞响应", elapsed)
	}

	// 放行挂起的请求并等它落地, 避免用例结束后预热 goroutine 还在读全局配置
	agent.unblock()
	waitForLog(t, logger, "[网关预热] 已触发")
}

// TestPreheat_NoNodeWarnsAndSkips 无可用节点时记 WARN 后忽略, 不影响主流程
func TestPreheat_NoNodeWarnsAndSkips(t *testing.T) {
	gdPath := uniqueGDPath("/影视库/预热/无节点.mkv")
	origin := newPreheatEmbyOrigin(t, "/home/googleDrive"+gdPath, "Movie")
	agent := newPreheatFakeAgent(t, false)

	// 注册表里有节点, 但从未心跳 → 不在调度窗口内
	basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+preheatAgentEntryJSON(agent.server.URL)+`]}`)
	agentCfg := mustAgentConfig(t, "enable: true\nenroll-token: test-enroll-token-0123456789\n")
	withPreheatTestConfig(t, origin.server.URL, "http://panel.invalid", agentCfg, basePath)

	logger := captureRedirectLogs(t)
	c, recorder := newRedirectContext(t, "/emby/Items/123/PlaybackInfo?api_key=test-key")
	emby.TransferPlaybackInfo(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("响应码 = %d, want 200", recorder.Code)
	}
	waitForLog(t, logger, "[网关预热] 无可用节点或签发失败")
	if got := agent.calls.Load(); got != 0 {
		t.Errorf("没有可用节点时不应有请求落到节点上, 实际 %d 次", got)
	}
}

// TestPreheat_ProbeFailureLogsWithoutSignature 节点不可达时静默降级, 且日志不带出签名地址
//
// net/http 的传输错误(*url.Error)会把请求地址原样写进错误文本, 而该地址含 s
// 签名参数 —— "节点不可达 / 超时"正是失败日志最常见的场景, 直接回显错误就等于
// 把签名写进日志(agent-network.md §3.7)。这里让节点的对外地址指向一个必然
// 拒绝连接的端口, 强制走失败分支。
func TestPreheat_ProbeFailureLogsWithoutSignature(t *testing.T) {
	gdPath := uniqueGDPath("/影视库/预热/不可达.mkv")
	origin := newPreheatEmbyOrigin(t, "/home/googleDrive"+gdPath, "Movie")

	// 节点正常心跳(调度可选中它), 但对外 base_url 指向 1 端口: 预热请求必定失败
	basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+preheatAgentEntryJSON("http://127.0.0.1:1")+`]}`)
	agentCfg := mustAgentConfig(t, "enable: true\nenroll-token: test-enroll-token-0123456789\n")
	withPreheatTestConfig(t, origin.server.URL, "http://panel.invalid", agentCfg, basePath)
	heartbeatTestAgent(t, 0)

	logger := captureRedirectLogs(t)
	c, recorder := newRedirectContext(t, "/emby/Items/123/PlaybackInfo?api_key=test-key")
	emby.TransferPlaybackInfo(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("预热失败不得影响主流程, 响应码 = %d", recorder.Code)
	}
	waitForLog(t, logger, "[网关预热] 预热请求失败")

	collected := logger.String()
	if strings.Contains(collected, "/dl/") || strings.Contains(collected, "&s=") {
		t.Errorf("失败日志泄露了签名地址: %s", collected)
	}
	if !strings.Contains(collected, "请求节点失败") {
		t.Errorf("失败原因应保留(只抹掉地址本身), 实际: %s", collected)
	}
}

// TestPreheat_PathResolveFailureSkipsSilently 条目路径解析失败时记 WARN 跳过, 不打断主流程
//
// 预热是旁观者: Emby 源查询失败(此处让所有 PlaybackInfo 返回 500)时它既不能
// panic, 也不能向节点打枪, 更不能改变 handler 自身的响应。
func TestPreheat_PathResolveFailureSkipsSilently(t *testing.T) {
	gdPath := uniqueGDPath("/影视库/预热/解析失败.mkv")
	origin := newPreheatEmbyOrigin(t, "/home/googleDrive"+gdPath, "Movie")
	origin.brokenPlaybackInfo.Store(true)
	agent := newPreheatFakeAgent(t, false)

	basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+preheatAgentEntryJSON(agent.server.URL)+`]}`)
	agentCfg := mustAgentConfig(t, "enable: true\nenroll-token: test-enroll-token-0123456789\n")
	withPreheatTestConfig(t, origin.server.URL, "http://panel.invalid", agentCfg, basePath)
	heartbeatTestAgent(t, 0)

	logger := captureRedirectLogs(t)
	c, recorder := newRedirectContext(t, "/emby/Items/123/PlaybackInfo?api_key=test-key")
	emby.TransferPlaybackInfo(c)

	waitForLog(t, logger, "[网关预热] 获取条目路径失败")
	if got := agent.calls.Load(); got != 0 {
		t.Errorf("路径解析失败时不应向节点发起请求, 实际 %d 次", got)
	}
	// 主流程的响应与预热无关: Emby 源查询失败时照常以错误响应返回
	if recorder.Code == http.StatusOK {
		t.Errorf("Emby 源查询失败时主流程应返回错误响应, 实际 %d", recorder.Code)
	}
}

// TestPreheat_NonMountPathSilentAndNoProbe 非 Google Drive 挂载路径静默跳过
func TestPreheat_NonMountPathSilentAndNoProbe(t *testing.T) {
	origin := newPreheatEmbyOrigin(t, "/data/local/普通电影.mkv", "Movie")
	agent := newPreheatFakeAgent(t, false)

	basePath := prepareAgentStateDir(t, `{"version":1,"agents":[`+preheatAgentEntryJSON(agent.server.URL)+`]}`)
	agentCfg := mustAgentConfig(t, "enable: true\nenroll-token: test-enroll-token-0123456789\n")
	withPreheatTestConfig(t, origin.server.URL, "http://panel.invalid", agentCfg, basePath)
	heartbeatTestAgent(t, 0)

	logger := captureRedirectLogs(t)
	c, recorder := newRedirectContext(t, "/emby/Items/123/PlaybackInfo?api_key=test-key")
	emby.TransferPlaybackInfo(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("响应码 = %d, want 200", recorder.Code)
	}

	// 等预热把路径解析完(handler 1 次 + 预热 1 次), 再确认没有打枪
	deadline := time.Now().Add(2 * time.Second)
	for origin.playbackInfoHits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(preheatQuietWindow)

	if got := origin.playbackInfoHits.Load(); got < 2 {
		t.Errorf("预热应完成一次路径解析, PlaybackInfo 命中 = %d, want >= 2", got)
	}
	if got := agent.calls.Load(); got != 0 {
		t.Errorf("非挂载路径不应触发预热, 实际 %d 次", got)
	}
	if logger.contains("[网关预热]") {
		t.Errorf("非挂载路径属于正常情况, 不应留下预热日志, 实际: %s", logger.String())
	}
}
