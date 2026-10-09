package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/constant"

	"github.com/gin-gonic/gin"
)

// ruleIndex 返回某个路由规则在规则表中的下标, 不存在时返回 -1
func ruleIndex(pattern string) int {
	for i, rule := range rules {
		if reg, ok := rule[0].(*regexp.Regexp); ok && reg.String() == pattern {
			return i
		}
	}
	return -1
}

// TestRules_AgentRoutesOrderedBeforeCatchAll 规则表顺序约束
//
// 规则表是从前到后的未锚定子串匹配, 先命中者先处理, 因此:
//   - agent 端点必须排在 Reg_All(回源兜底)之前, 否则永远走不到;
//   - /agents/update 与 /agents/delete 必须排在 /agents 之前(前缀会截胡);
//   - 安装脚本同样必须排在 Reg_All 之前。
func TestRules_AgentRoutesOrderedBeforeCatchAll(t *testing.T) {
	initRulePatterns()

	catchAll := ruleIndex(constant.Reg_All)
	if catchAll < 0 {
		t.Fatal("规则表中找不到 Reg_All 兜底规则")
	}

	agentRules := map[string]string{
		"节点注册": constant.Reg_AgentEnroll,
		"节点心跳": constant.Reg_AgentHeartbeat,
		"直链下发": constant.Reg_AgentDownloadLink,
		"安装脚本": constant.Reg_InstallScript,
		"节点列表": constant.Route_AgentNetworkAgents,
		"启停节点": constant.Route_AgentNetworkAgentsUpdate,
		"删除节点": constant.Route_AgentNetworkAgentsDelete,
		"编辑资料": constant.Route_AgentNetworkAgentsEdit,
		"安装命令": constant.Route_AgentNetworkInstallCommand,
	}
	for name, pattern := range agentRules {
		index := ruleIndex(pattern)
		if index < 0 {
			t.Errorf("%s 规则未注册: %s", name, pattern)
			continue
		}
		if index > catchAll {
			t.Errorf("%s 规则排在了 Reg_All 之后(下标 %d > %d), 永远不会被命中", name, index, catchAll)
		}
	}

	// 具体路径必须排在它的前缀规则之前
	agents := ruleIndex(constant.Route_AgentNetworkAgents)
	for name, pattern := range map[string]string{
		"启停节点": constant.Route_AgentNetworkAgentsUpdate,
		"删除节点": constant.Route_AgentNetworkAgentsDelete,
		"编辑资料": constant.Route_AgentNetworkAgentsEdit,
	} {
		index := ruleIndex(pattern)
		if index >= agents {
			t.Errorf("%s 规则(下标 %d)必须排在节点列表规则(下标 %d)之前, 否则会被前缀截胡", name, index, agents)
		}
	}
}

// TestGlobalDftHandler_InstallScriptHeadReachesRoute HEAD 请求的安装脚本豁免
//
// globalDftHandler 默认把所有 HEAD 请求短路成空 200; 安装脚本需要 HEAD 与 GET
// 同头, 所以在那里有一条显式豁免。本用例同时验证两点: 豁免生效, 且其余 HEAD
// 请求仍保持原有的短路行为。
func TestGlobalDftHandler_InstallScriptHeadReachesRoute(t *testing.T) {
	oldConfig := config.C
	agentCfg := &config.AgentNetwork{Enable: true, EnrollToken: "test-enroll-token-0123456789"}
	if err := agentCfg.Init(); err != nil {
		t.Fatalf("初始化测试配置失败: %v", err)
	}
	config.C = &config.Config{
		AgentNetwork: agentCfg,
		Ge2o:         &config.Ge2o{Web: &config.Web{Disable: true}},
	}
	t.Cleanup(func() { config.C = oldConfig })

	initRulePatterns()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Any("/*vars", globalDftHandler)

	serveHead := func(target string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodHead, target, nil))
		return recorder
	}

	install := serveHead(constant.Route_InstallScript)
	if install.Code != http.StatusOK {
		t.Fatalf("HEAD %s HTTP = %d, want 200", constant.Route_InstallScript, install.Code)
	}
	if got := install.Header().Get("Content-Type"); got != "text/x-shellscript" {
		t.Errorf("HEAD 安装脚本被默认处理器短路了: Content-Type = %q, want text/x-shellscript", got)
	}
	if install.Header().Get("Content-Length") == "" {
		t.Error("HEAD 安装脚本应带上 Content-Length(与 GET 同头)")
	}
	if install.Body.Len() != 0 {
		t.Errorf("HEAD 不应返回 body, 实际 %d 字节", install.Body.Len())
	}

	// 其余 HEAD 请求仍走默认短路: 空 200、不参与路由匹配
	other := serveHead("/emby/Videos/123/stream")
	if other.Code != http.StatusOK || other.Body.Len() != 0 {
		t.Errorf("非安装脚本的 HEAD 请求仍应短路成空 200: code=%d body=%q", other.Code, other.Body.String())
	}
}

// TestHandleWebStatic_WebRootRedirectsToTrailingSlash 裸 /ge2o/web 的尾斜杠归一化
//
// 前端 basename 是 "/ge2o/web/"(带尾斜杠), 裸 /ge2o/web 会落进 SPA 回落拿到
// index.html, 但 React Router 因 basename 不匹配什么都不渲染(白屏)。
// 因此服务端必须在 SPA 回落之前 301 到带尾斜杠形式(与 /ge2o 的既有重定向同款)。
func TestHandleWebStatic_WebRootRedirectsToTrailingSlash(t *testing.T) {
	oldConfig := config.C
	config.C = &config.Config{Ge2o: &config.Ge2o{Web: &config.Web{Disable: false}}}
	t.Cleanup(func() { config.C = oldConfig })

	initRulePatterns()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Any("/*vars", globalDftHandler)

	// 带查询串请求: 重定向必须保留查询串
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, constant.Route_Web+"?a=1", nil))
	if recorder.Code != http.StatusMovedPermanently {
		t.Fatalf("GET %s HTTP = %d, want 301(不能落进 SPA 回落)", constant.Route_Web, recorder.Code)
	}
	if got, want := recorder.Header().Get("Location"), constant.Route_Web+"/?a=1"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}
