package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/constant"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs"

	"github.com/gin-gonic/gin"
)

// logCollector 把日志收集到内存里, 供用例断言
type logCollector struct {
	mu      sync.Mutex
	content strings.Builder
}

// Log 实现 logs.Logger
func (c *logCollector) Log(content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.content.WriteString(content)
	c.content.WriteString("\n")
}

// contains 判断是否收集到包含指定片段的日志
func (c *logCollector) contains(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Contains(c.content.String(), sub)
}

// TestCustomLogger_SilencesAgentHeartbeat 心跳请求不进请求日志
//
// 心跳 15s 一次 × N 个节点, 记进请求日志会把日志刷爆且无排查价值
// (心跳失败由 agent 侧退避日志体现)。只静音心跳, 其余请求照常记录。
func TestCustomLogger_SilencesAgentHeartbeat(t *testing.T) {
	collector := new(logCollector)
	id, ok := logs.RegisterLogger(collector)
	if !ok {
		t.Fatal("注册日志收集器失败")
	}
	t.Cleanup(func() { logs.RemoveLogger(id) })

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(CustomLogger("8095"))
	engine.Any("/*vars", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	// 心跳请求: 不应出现
	engine.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, constant.Route_AgentHeartbeat, nil))
	if collector.contains(constant.Route_AgentHeartbeat) {
		t.Error("心跳请求不应出现在请求日志里")
	}

	// 普通请求: 照常记录
	engine.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/emby/System/Ping", nil))
	if !collector.contains("/emby/System/Ping") {
		t.Error("普通请求应照常记录")
	}

	// 控制面其余端点(频率低、有排查价值)照常记录
	engine.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/api/agent/download-link?file_id=x", nil))
	if !collector.contains("/api/agent/download-link") {
		t.Error("download-link 请求应照常记录")
	}
}
