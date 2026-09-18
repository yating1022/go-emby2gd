package gdrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/https"
)

const (
	// directLinkAPIPath 面板的直链接口路径
	directLinkAPIPath = "/api/dl"
	// pathQueryField 直链接口承载 Drive 路径的查询参数名
	pathQueryField = "path"
	// bearerPrefix Authorization 头的前缀, 注意后面有空格
	bearerPrefix = "Bearer "
	// panelRequestTimeout 单次面板调用的超时时间
	//
	// 面板是毫秒级响应, 而共享 client 的 ResponseHeaderTimeout 是 5 分钟,
	// 对这里太宽, 因此单独收紧。
	panelRequestTimeout = 15 * time.Second
	// maxPanelResponseSize 面板响应的读取上限
	//
	// 面板响应是小 JSON, 这里只做防御性限制, 不参与媒体数据流的传输。
	maxPanelResponseSize = 1 << 20
)

// fetchDirectLink 调用面板直链接口, 换取直链与请求头
//
// 任何失败都返回错误, 由调用方按"回退到回源处理"应对。
func fetchDirectLink(ctx context.Context, gdPath string) (*panelDirectLink, error) {
	cfg := gdriveConfig()
	if !cfg.IsEnabled() {
		return nil, errors.New("Google Drive 直链未启用")
	}

	gdPath = strings.TrimSpace(gdPath)
	if gdPath == "" {
		return nil, errors.New("Drive 路径为空")
	}

	// 路径必须交给 QueryEscape: 中文、空格、括号、'&' 自行拼接极易编错,
	// 而面板要求路径与 Drive 内逐字符一致。
	endpoint := cfg.ApiBase + directLinkAPIPath + "?" + pathQueryField + "=" + url.QueryEscape(gdPath)

	// 固定用 DoSingle()(不自动重定向): /api/dl 是直接返回 JSON 的同步接口,
	// 跟随跳转只会把配置错误掩盖成"看起来正常"。
	resp, err := https.Get(endpoint).
		AddHeader("Authorization", bearerPrefix+cfg.ApiToken).
		AddHeader("Accept", "application/json").
		// 与出站代理规范一致: 显式声明 identity, 避免 Transport 自行追加 gzip 并透明解压
		AddHeader("Accept-Encoding", "identity").
		Context(ctx).
		DoSingle()
	if err != nil {
		return nil, fmt.Errorf("请求面板直链接口失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPanelResponseSize))
	if err != nil {
		return nil, fmt.Errorf("读取面板直链接口响应失败: %w", err)
	}

	var envelope panelEnvelope
	parsed := json.Unmarshal(body, &envelope) == nil

	if resp.StatusCode != http.StatusOK {
		return nil, panelAPIError(resp.StatusCode, envelope, parsed)
	}
	if !parsed {
		return nil, fmt.Errorf("面板直链接口返回的不是合法 JSON, status: %d", resp.StatusCode)
	}
	// 状态码 200 但 ok 为 false: 以响应体里的错误为准, 不看状态码
	if !envelope.OK {
		return nil, panelAPIError(resp.StatusCode, envelope, true)
	}
	if strings.TrimSpace(envelope.Data.URL) == "" {
		return nil, fmt.Errorf("面板直链接口未返回下载地址, status: %d", resp.StatusCode)
	}

	return &envelope.Data, nil
}

// panelAPIError 构造面板接口的错误信息
//
// 直接沿用面板给出的中文 message, 不另编一套文案 —— 面板的 message 是专门写过的,
// 照搬能给排查者最准确的信息(例如 PATH_NOT_IN_CACHE 与 PATH_NOT_FOUND 的区别)。
//
// 唯一加工是把本项目的面板 Token 替换掉: message 会原样进日志,
// 即使面板把 Token 回声回来也不该落进日志文件。
func panelAPIError(statusCode int, envelope panelEnvelope, parsed bool) error {
	if !parsed {
		return fmt.Errorf("面板直链接口返回 %d", statusCode)
	}

	message := redactConfigSecrets(envelope.Error.Message)
	if message == "" {
		return fmt.Errorf("面板直链接口返回 %d", statusCode)
	}
	if envelope.Error.Code == "" {
		return fmt.Errorf("取直链失败: %s", message)
	}
	return fmt.Errorf("取直链失败 [%s] %s", envelope.Error.Code, message)
}

// parseExpiresAt 解析面板返回的过期时刻
//
// 解析失败返回零值, 由调用方按"没有过期信息"处理(立即视为过期, 下次重新调面板);
// 而不是让整个响应解析失败 —— 一个格式不对的时间戳不该把一次可用的取直链打掉。
func parseExpiresAt(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}

	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
