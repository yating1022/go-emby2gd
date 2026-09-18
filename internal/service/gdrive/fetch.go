package gdrive

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/https"
)

// maxFailedBodyDrain 失败响应体的丢弃上限
//
// 不使用正文, 但仍要把连接上的剩余字节读掉, 否则这条 TCP 连接不会归还连接池。
const maxFailedBodyDrain = 4 << 10

// fetchDirect 带上面板给的请求头请求下载地址
//
// 返回的 resp.Body 【不读取】, 交给调用方流式转发, 调用方负责关闭。
// 只接受 200/206, 其余一律视为失败并关闭响应体, 由调用方回退到回源处理。
func fetchDirect(ctx context.Context, tgt *target, clientRange string) (*http.Response, error) {
	if tgt == nil || strings.TrimSpace(tgt.directURL) == "" {
		return nil, fmt.Errorf("下载地址为空")
	}

	// 必须复制一份再交给 RequestHolder: Header() 是直接赋值, 而后面的
	// net/http 会往这个 map 里补 User-Agent 等字段 —— 直接把缓存里那份交出去,
	// 多个并发请求会同时写同一个 map, 属于数据竞争。
	header := make(http.Header, len(tgt.headers)+2)
	for key, value := range tgt.headers {
		header.Set(key, value)
	}
	if clientRange = strings.TrimSpace(clientRange); clientRange != "" {
		header.Set("Range", clientRange)
	}
	// 与出站代理规范一致: 显式声明 identity, 避免 Transport 自行追加 gzip
	// 并透明解压 —— 那会抹掉 Content-Length, 播放器拿不到文件大小。
	header.Set("Accept-Encoding", "identity")

	resp, err := https.Get(tgt.directURL).
		Header(header).
		Context(ctx).
		DoSingle()
	if err != nil {
		return nil, fmt.Errorf("请求下载地址失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		// 响应体不读也不进日志: 内容不受本项目控制, 状态码已足够定位问题
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxFailedBodyDrain))
		resp.Body.Close()
		return nil, &fetchError{statusCode: resp.StatusCode, directURL: tgt.directURL}
	}

	return resp, nil
}

// Error 实现 error
func (e *fetchError) Error() string {
	return fmt.Sprintf("下载失败: HTTP %d, %s", e.statusCode, e.directURL)
}

// retryableStatus 判断状态码是否代表"刚才用的凭据或直链已经失效"
func retryableStatus(statusCode int) bool {
	return slices.Contains(panelRetryStatusCodes, statusCode)
}
