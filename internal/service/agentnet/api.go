package agentnet

import (
	"net/http"
	"strings"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/gdrive"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/cryptos"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs/colors"

	"github.com/gin-gonic/gin"
)

// 错误码
//
// 与 agent 侧约定的形状: {"ok":false,"error":{"code":"…","message":"中文"}}。
// agent 端的 parseError 优先取 error.message, 因此 message 必须是可读的中文。
const (
	// codeValidationError 请求参数不合法
	codeValidationError = "VALIDATION_ERROR"
	// codeEnrollTokenInvalid 注册 Token 无效
	codeEnrollTokenInvalid = "ENROLL_TOKEN_INVALID"
	// codeUnauthorized 节点凭据无效(不区分"节点不存在"与"凭据错误")
	codeUnauthorized = "UNAUTHORIZED"
	// codeTokenInvalid file_id 无法解码
	codeTokenInvalid = "AGENT_TOKEN_INVALID"
	// codeLinkUnavailable 直链不可用(面板失败等原因)
	codeLinkUnavailable = "AGENT_LINK_UNAVAILABLE"
	// codeDisabled 功能未启用
	codeDisabled = "AGENT_NETWORK_DISABLED"
	// codeStateError 注册表读写失败
	codeStateError = "AGENT_STATE_ERROR"
)

// errorBody 统一错误响应
type errorBody struct {
	OK    bool `json:"ok"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// writeError 输出统一错误形状
func writeError(c *gin.Context, status int, code, message string) {
	body := errorBody{OK: false}
	body.Error.Code = code
	body.Error.Message = message
	c.JSON(status, body)
}

// writeFeatureDisabled 功能未启用时的统一响应
//
// 三个 agent 端点与安装脚本都返回 403 JSON: agent 客户端按状态码与
// error.message 处理(不会把 JSON 当脚本执行), 人工排查时也能一眼看出原因。
func writeFeatureDisabled(c *gin.Context) {
	writeError(c, http.StatusForbidden, codeDisabled, "本网关未启用 agent 代理网络(agent-network.enable)")
}

// bearerToken 从 Authorization 头取出 Bearer 令牌
func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// enrollRequest 注册请求体
//
// 字段名与 agent 侧 internal/enroll 的请求结构逐字一致。
type enrollRequest struct {
	EnrollToken   string  `json:"enroll_token"`
	MachineID     string  `json:"machine_id"`
	Hostname      string  `json:"hostname"`
	Version       string  `json:"version"`
	ListenPort    int     `json:"listen_port"`
	PublicBaseURL *string `json:"public_base_url"`
}

// Enroll 处理 POST /api/agent/enroll
//
// 幂等键是 machine_id: 同一台机器重跑安装脚本会复用原节点记录并轮换凭据。
// 写盘成功之后才响应(int 500 AGENT_STATE_ERROR), 保证节点拿到的凭据一定可用。
func Enroll(c *gin.Context) {
	cfg := agentNetworkConfig()
	if !cfg.IsEnabled() {
		writeFeatureDisabled(c)
		return
	}

	var req enrollRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, codeValidationError, "请求体不是合法 JSON")
		return
	}

	// 注册 Token 用常数时间比较; 失败不透露是"没配"还是"配错了"
	if !cryptos.Equal(strings.TrimSpace(req.EnrollToken), cfg.EnrollToken) {
		logf(colors.Yellow, "注册被拒绝(Token 无效), 来源 IP: %s", c.ClientIP())
		writeError(c, http.StatusUnauthorized, codeEnrollTokenInvalid,
			"注册 Token 无效或已轮换, 请在 master 网页重新复制安装命令")
		return
	}

	machineID := strings.TrimSpace(req.MachineID)
	if machineID == "" {
		writeError(c, http.StatusBadRequest, codeValidationError, "machine_id 不能为空")
		return
	}
	if req.ListenPort <= 0 || req.ListenPort > 65535 {
		writeError(c, http.StatusBadRequest, codeValidationError, "listen_port 取值不合法, 有效范围: [1, 65535]")
		return
	}

	publicBaseURL := ""
	if req.PublicBaseURL != nil {
		normalized, err := parsePublicBaseURL(*req.PublicBaseURL)
		if err != nil {
			// 只回显字段名与原因: 该字段本身不含凭据
			writeError(c, http.StatusBadRequest, codeValidationError, "public_base_url 取值不合法: "+err.Error())
			return
		}
		publicBaseURL = normalized
	}

	result, err := defaultRegistry.enroll(enrollParams{
		MachineID:     machineID,
		Hostname:      strings.TrimSpace(req.Hostname),
		Version:       strings.TrimSpace(req.Version),
		ListenPort:    req.ListenPort,
		PublicBaseURL: publicBaseURL,
		LastIP:        c.ClientIP(),
		Now:           time.Now(),
	})
	if err != nil {
		logf(colors.Red, "注册失败: %v", err)
		writeError(c, http.StatusInternalServerError, codeStateError, "节点注册失败: "+err.Error())
		return
	}

	if result.Reused {
		logf(colors.Yellow, "节点重新注册, 凭据已轮换: %s(%s), 来源 IP: %s", result.Name, result.AgentID, c.ClientIP())
	} else {
		logf(colors.Green, "节点注册成功: %s(%s), 来源 IP: %s", result.Name, result.AgentID, c.ClientIP())
	}

	// 响应形状与冻结稿逐字一致(裸对象, 不套 {ok,data} 信封)
	c.JSON(http.StatusOK, gin.H{
		"agent_id":                   result.AgentID,
		"agent_secret":               result.Secret,
		"sign_key":                   result.SignKey,
		"heartbeat_interval_seconds": config.AgentHeartbeatIntervalSeconds,
	})
}

// heartbeatRequest 心跳请求体
//
// active_streams / listen_port / public_base_url 允许缺省:
// 缺省表示"没设置", 不覆盖已有值。
type heartbeatRequest struct {
	ActiveStreams int     `json:"active_streams"`
	Version       string  `json:"version"`
	UptimeSeconds int64   `json:"uptime_seconds"`
	ListenPort    int     `json:"listen_port"`
	PublicBaseURL *string `json:"public_base_url"`
}

// Heartbeat 处理 POST /api/agent/heartbeat
//
// 心跳只更新内存(每节点 15 秒一次不允许触发磁盘写); 响应下发 enabled,
// 供 agent 侧决定是否继续拉取新直链(已缓存的直链继续服务)。
func Heartbeat(c *gin.Context) {
	if !agentNetworkConfig().IsEnabled() {
		writeFeatureDisabled(c)
		return
	}

	agentID, header := c.Request.Header.Get("X-Agent-Id"), c.Request.Header.Get("Authorization")
	rec, err := defaultRegistry.authenticate(strings.TrimSpace(agentID), bearerToken(header))
	if err != nil {
		logf(colors.Red, "心跳失败(注册表不可用): %v", err)
		writeError(c, http.StatusInternalServerError, codeStateError, "节点状态读取失败: "+err.Error())
		return
	}
	if rec == nil {
		// 枚举防护: "节点不存在"与"凭据错误"同状态同文案
		writeError(c, http.StatusUnauthorized, codeUnauthorized, "agent 凭据无效")
		return
	}

	var req heartbeatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, codeValidationError, "请求体不是合法 JSON")
		return
	}

	publicBaseURL := ""
	if req.PublicBaseURL != nil {
		normalized, err := parsePublicBaseURL(*req.PublicBaseURL)
		if err != nil {
			writeError(c, http.StatusBadRequest, codeValidationError, "public_base_url 取值不合法: "+err.Error())
			return
		}
		publicBaseURL = normalized
	}

	updated, err := defaultRegistry.touch(rec.ID, heartbeatParams{
		LastIP:        c.ClientIP(),
		ActiveStreams: req.ActiveStreams,
		Version:       strings.TrimSpace(req.Version),
		ListenPort:    req.ListenPort,
		PublicBaseURL: publicBaseURL,
		Now:           time.Now(),
	})
	if err != nil {
		logf(colors.Red, "心跳失败(注册表不可用): %v", err)
		writeError(c, http.StatusInternalServerError, codeStateError, "节点状态读取失败: "+err.Error())
		return
	}
	if updated == nil {
		// 鉴权与更新之间节点被删除: 仍按凭据无效处理
		writeError(c, http.StatusUnauthorized, codeUnauthorized, "agent 凭据无效")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":                         true,
		"enabled":                    updated.Enabled,
		"heartbeat_interval_seconds": config.AgentHeartbeatIntervalSeconds,
	})
}

// DownloadLink 处理 GET /api/agent/download-link?file_id=
//
// 把一条 Google 直链与随行请求头下发给节点 —— headers 里是账号级凭据,
// 只允许出现在持 agent_secret 的节点响应里, 绝不进日志。
func DownloadLink(c *gin.Context) {
	if !agentNetworkConfig().IsEnabled() {
		writeFeatureDisabled(c)
		return
	}

	agentID, header := c.Request.Header.Get("X-Agent-Id"), c.Request.Header.Get("Authorization")
	rec, err := defaultRegistry.authenticate(strings.TrimSpace(agentID), bearerToken(header))
	if err != nil {
		logf(colors.Red, "下发直链失败(注册表不可用): %v", err)
		writeError(c, http.StatusInternalServerError, codeStateError, "节点状态读取失败: "+err.Error())
		return
	}
	if rec == nil {
		writeError(c, http.StatusUnauthorized, codeUnauthorized, "agent 凭据无效")
		return
	}

	fileID := strings.TrimSpace(c.Query("file_id"))
	if fileID == "" {
		writeError(c, http.StatusBadRequest, codeValidationError, "缺少 file_id")
		return
	}
	gdPath, err := decodeFileToken(fileID)
	if err != nil {
		writeError(c, http.StatusBadRequest, codeTokenInvalid, "file_id 解码失败: "+err.Error())
		return
	}

	url, headers, expiresAt, err := gdrive.ResolveTarget(c.Request.Context(), gdPath)
	if err != nil {
		// 中文原因已由 gdrive 包脱敏(不含令牌与请求头), 可原样透传
		logf(colors.Yellow, "向节点 %s(%s) 下发直链失败: %v", rec.Name, rec.ID, err)
		writeError(c, http.StatusBadGateway, codeLinkUnavailable, err.Error())
		return
	}

	logf(colors.Green, "已向节点 %s(%s) 下发直链, 文件: %s", rec.Name, rec.ID, gdPath)
	c.JSON(http.StatusOK, gin.H{
		"url":        url,
		"headers":    headers,
		"expires_at": expiresAt,
	})
}
