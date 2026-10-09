package agentnet

import (
	"net/http"
	"strings"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/constant"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/model"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/cryptos"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/https"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs/colors"

	"github.com/gin-gonic/gin"
)

// 管理接口一律沿用 /ge2o 的既有惯例: POST JSON + body 里的 secret +
// model.Response 信封 + 恒 200(靠 message 表达失败原因, 前端按 message 提示)。

// adminRequest 管理接口的公共请求体
type adminRequest struct {
	// Secret 本地密钥(ge2o.api-secret)
	Secret string `json:"secret"`
}

// adminUpdateRequest 启停节点
type adminUpdateRequest struct {
	Secret string `json:"secret"`
	// ID 节点 id
	ID string `json:"id"`
	// Enabled 目标状态
	Enabled bool `json:"enabled"`
}

// adminDeleteRequest 删除节点
type adminDeleteRequest struct {
	Secret string `json:"secret"`
	// ID 节点 id
	ID string `json:"id"`
}

// agentView 管理接口返回的节点视图
//
// 与 agentRecord 的差别就是【脱敏】: 绝不包含 secret / sign_key 与
// 任何 Google 凭据 —— 节点列表会渲染在网页上, 那里不是凭据该出现的地方。
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

// AdminListAgents 节点列表
func AdminListAgents(c *gin.Context) {
	cfg := agentNetworkConfig()
	if !cfg.IsEnabled() {
		c.JSON(http.StatusOK, model.Response{Message: "agent 网络未启用, 请先在配置文件中开启 agent-network.enable"})
		return
	}

	var req adminRequest
	if !bindAdminRequest(c, &req) {
		return
	}
	if !checkAdminSecret(c, req.Secret) {
		return
	}

	list, err := defaultRegistry.snapshot()
	if err != nil {
		logs.Error("[agent 网络] 读取节点列表失败: %v", err)
		c.JSON(http.StatusOK, model.Response{Message: "读取节点列表失败: " + err.Error()})
		return
	}

	now := time.Now()
	views := make([]agentView, 0, len(list))
	for _, rec := range list {
		views = append(views, newAgentView(rec, now, time.Duration(cfg.OfflineSeconds)*time.Second))
	}

	// 数据放在 data.agents 下(而不是直接放数组): 空列表时也保证 data 字段存在,
	// 前端不需要区分"没有 data"与"data 是空数组"
	c.JSON(http.StatusOK, model.Response{
		Success: true,
		Message: "获取成功",
		Data:    gin.H{"agents": views},
	})
}

// AdminUpdateAgent 启用 / 禁用节点
func AdminUpdateAgent(c *gin.Context) {
	cfg := agentNetworkConfig()
	if !cfg.IsEnabled() {
		c.JSON(http.StatusOK, model.Response{Message: "agent 网络未启用, 请先在配置文件中开启 agent-network.enable"})
		return
	}

	var req adminUpdateRequest
	if !bindAdminRequest(c, &req) {
		return
	}
	if !checkAdminSecret(c, req.Secret) {
		return
	}

	id := strings.TrimSpace(req.ID)
	if id == "" {
		c.JSON(http.StatusOK, model.Response{Message: "缺少节点 id"})
		return
	}

	rec, err := defaultRegistry.setEnabled(id, req.Enabled, time.Now())
	if err != nil {
		if err == errAgentNotFound {
			c.JSON(http.StatusOK, model.Response{Message: "节点不存在"})
			return
		}
		logs.Error("[agent 网络] 更新节点状态失败: %v", err)
		c.JSON(http.StatusOK, model.Response{Message: "更新节点状态失败: " + err.Error()})
		return
	}

	action := "已启用"
	if !rec.Enabled {
		action = "已禁用"
	}
	logf(colors.Blue, "节点状态更新: %s(%s) %s", rec.Name, rec.ID, action)
	c.JSON(http.StatusOK, model.Response{Success: true, Message: "更新成功"})
}

// AdminDeleteAgent 删除节点
//
// 删除即吊销: 节点记录里的 secret 与 sign_key 一起消失,
// 节点凭据立即失效(已签发的客户端 URL 也随之失效)。
func AdminDeleteAgent(c *gin.Context) {
	if !agentNetworkConfig().IsEnabled() {
		c.JSON(http.StatusOK, model.Response{Message: "agent 网络未启用, 请先在配置文件中开启 agent-network.enable"})
		return
	}

	var req adminDeleteRequest
	if !bindAdminRequest(c, &req) {
		return
	}
	if !checkAdminSecret(c, req.Secret) {
		return
	}

	id := strings.TrimSpace(req.ID)
	if id == "" {
		c.JSON(http.StatusOK, model.Response{Message: "缺少节点 id"})
		return
	}

	rec, err := defaultRegistry.remove(id)
	if err != nil {
		if err == errAgentNotFound {
			c.JSON(http.StatusOK, model.Response{Message: "节点不存在"})
			return
		}
		logs.Error("[agent 网络] 删除节点失败: %v", err)
		c.JSON(http.StatusOK, model.Response{Message: "删除节点失败: " + err.Error()})
		return
	}

	logf(colors.Blue, "节点已删除(凭据随之失效): %s(%s)", rec.Name, rec.ID)
	c.JSON(http.StatusOK, model.Response{Success: true, Message: "删除成功"})
}

// AdminInstallCommand 生成一键安装命令
//
// 命令里含 enroll-token(等同于"可以往本网关注册一台节点"的凭据),
// 因此只允许管理员接口下发; 地址按请求的 Host 与协议推导。
func AdminInstallCommand(c *gin.Context) {
	cfg := agentNetworkConfig()
	if !cfg.IsEnabled() {
		c.JSON(http.StatusOK, model.Response{Message: "agent 网络未启用, 请先在配置文件中开启 agent-network.enable"})
		return
	}

	var req adminRequest
	if !bindAdminRequest(c, &req) {
		return
	}
	if !checkAdminSecret(c, req.Secret) {
		return
	}

	baseURL := strings.TrimRight(https.ClientRequestHost(c.Request), "/")
	if baseURL == "" {
		c.JSON(http.StatusOK, model.Response{Message: "无法推导本网关地址, 请检查请求的 Host"})
		return
	}

	command := "curl -fsSL " + baseURL + constant.Route_InstallScript +
		" | sudo bash -s -- --master " + baseURL + " --token " + cfg.EnrollToken

	c.JSON(http.StatusOK, model.Response{
		Success: true,
		Message: "获取成功",
		Data:    gin.H{"command": command, "master_url": baseURL},
	})
}

// newAgentView 把节点记录转换为脱敏视图
func newAgentView(rec *agentRecord, now time.Time, offline time.Duration) agentView {
	online := rec.Enabled && !rec.LastSeenAt.IsZero() && now.Sub(rec.LastSeenAt) <= offline
	return agentView{
		ID:            rec.ID,
		Name:          rec.Name,
		MachineID:     rec.MachineID,
		Enabled:       rec.Enabled,
		Online:        online,
		Version:       rec.Version,
		LastSeenAt:    formatViewTime(rec.LastSeenAt),
		LastIP:        rec.LastIP,
		ActiveStreams: rec.ActiveStreams,
		PublicBaseURL: rec.PublicBaseURL,
		Address:       agentBaseURL(rec),
		ListenPort:    rec.ListenPort,
		CreatedAt:     formatViewTime(rec.CreatedAt),
		UpdatedAt:     formatViewTime(rec.UpdatedAt),
	}
}

// formatViewTime 统一时间字段的序列化格式
//
// RFC3339 + UTC: 前端只做展示, 零值时间输出空串(而不是 0001-01-01)。
func formatViewTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// bindAdminRequest 解析管理接口的请求体
func bindAdminRequest(c *gin.Context, dst any) bool {
	if c.Request.Method != http.MethodPost {
		c.String(http.StatusNotFound, "404 not found")
		return false
	}
	if err := c.ShouldBindJSON(dst); err != nil {
		c.JSON(http.StatusOK, model.Response{Message: "请求参数错误"})
		return false
	}
	return true
}

// checkAdminSecret 校验管理密钥
//
// 语义与 /ge2o 既有惯例一致(明文密钥、失败时 message 提示), 但比较改用
// cryptos.Equal 的常数时间比较(本项目对所有密钥比较的统一要求)。
func checkAdminSecret(c *gin.Context, secret string) bool {
	localSecret := ""
	if config.C != nil && config.C.Ge2o != nil {
		localSecret = strings.TrimSpace(config.C.Ge2o.ApiSecret)
	}
	if localSecret == "" {
		c.JSON(http.StatusOK, model.Response{Message: "请先配置本地密钥"})
		return false
	}
	if !cryptos.Equal(secret, localSecret) {
		c.JSON(http.StatusOK, model.Response{Message: "密钥错误"})
		return false
	}
	return true
}
