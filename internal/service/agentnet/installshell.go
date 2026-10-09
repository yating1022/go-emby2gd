package agentnet

import (
	_ "embed"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// installScript 节点安装脚本实体
//
// 实体必须落在本包目录树内(go:embed 不能向上引用); 脚本本身不含任何密钥,
// enroll-token 是安装时的命令行参数, 因此本端点无需鉴权。
//
//go:embed installshell/agent-install.sh
var installScript []byte

// installScriptContentType 脚本的响应类型
//
// text/x-shellscript: 浏览器与 curl 都按"可执行文本"处理,
// 且不会被当作 HTML 渲染。
const installScriptContentType = "text/x-shellscript"

// InstallScript 处理 /install.sh
//
// GET 与 HEAD 返回同一组响应头(Content-Type / Content-Length / Cache-Control);
// HEAD 只回头不写 body(gin 里显式处理)。
//
// 本端点在 globalDftHandler 里有一个显式豁免: 它默认把所有 HEAD 请求短路成
// 空 200, 那会破坏"HEAD 与 GET 同头"的契约(见 internal/web/handler.go)。
func InstallScript(c *gin.Context) {
	if !agentNetworkConfig().IsEnabled() {
		// 未启用时 403: `curl -fsSL` 会立刻失败退出, 不会把错误 JSON 当脚本执行
		writeFeatureDisabled(c)
		return
	}

	switch c.Request.Method {
	case http.MethodGet, http.MethodHead:
	default:
		c.String(http.StatusMethodNotAllowed, "只支持 GET/HEAD 请求")
		return
	}

	// 脚本内容随程序发布更新: 不允许中间层长时间缓存
	c.Header("Cache-Control", "no-cache")
	c.Header("Content-Type", installScriptContentType)
	// 显式给出长度: 让客户端与反代在流式响应里也能拿到确定的大小
	c.Header("Content-Length", strconv.Itoa(len(installScript)))

	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Data(http.StatusOK, installScriptContentType, installScript)
}
