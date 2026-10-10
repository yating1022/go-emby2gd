package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"time"
)

// 数据面缓冲与超时的既定参数（冻结稿 §2.4 / §9-5）。
const (
	// copyBufferSize 是流式转发缓冲；**不整段缓冲**，边读边写。
	copyBufferSize = 32 * 1024
)

// upstreamTransport 是 node/hub 共用的连接池参数（改一处即两处生效）；
// 唯一差异是 DialContext：node 用标准 dialer，hub 换多地址快速失败拨号
// （见 multidial.go 的 NewHubUpstreamClient）。
func upstreamTransport(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// NewUpstreamClient 构造访问 Google 的 HTTP 客户端。
//
// 关键取舍（不要改回去）：
//   - **不设 Client.Timeout**：body 是大文件流，整体超时会把长下载砍断；
//     只对建连（DialContext 10s）与响应头（ResponseHeaderTimeout 30s）设限。
//   - **不改 CheckRedirect**：保持 Go 默认行为。默认策略不会把凭据带过跨主机
//     跳转（目标 URL 自带签名参数）；手动把 Authorization 加回反而会把 Google
//     凭据泄漏给重定向目标（冻结稿 §9-4）。本部署链路实测无重定向
//     （网关侧 spec gdrive-panel.md §3.3），此项仅作惰性防线。
//   - **DialContext 保持标准 dialer**（v0.3.2 冻结行为）：多地址快速失败只给
//     hub 用（NewHubUpstreamClient）。
func NewUpstreamClient() *http.Client {
	return &http.Client{
		Transport: upstreamTransport((&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext),
	}
}

// passthroughHeaderNames 是透传给客户端的响应头白名单（冻结稿 §2.4）。
//
// 刻意不用"照单全收"：上游的 Set-Cookie / Server / Via 等与下载无关，
// 透传只会扩大攻击面与污染缓存语义。
var passthroughHeaderNames = []string{
	"Content-Type",
	"Content-Length",
	"Content-Range",
	"Accept-Ranges",
	"ETag",
	"Last-Modified",
}

func copyPassthroughHeaders(dst, src http.Header) {
	for _, name := range passthroughHeaderNames {
		key := http.CanonicalHeaderKey(name)
		if values, ok := src[key]; ok {
			dst[key] = append([]string(nil), values...)
		}
	}
}

// newUpstreamRequest 组装发往 Google 的请求：
// 注入 master 给的凭据头，并透传客户端的 Range（播放器 seek / 断点续传）。
//
// ctx 来自客户端请求：客户端断开即取消上游（冻结稿 §9-5）。
func newUpstreamRequest(ctx context.Context, method string, link Link, rangeHeader string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, link.URL, nil)
	if err != nil {
		return nil, err
	}
	for name, value := range link.Headers {
		req.Header.Set(name, value)
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	return req, nil
}

// flushWriter 在每次写成功后立刻把数据推给客户端。
//
// 必要的原因：net/http 的响应写有 ~4KiB 缓冲，而上游一小段一小段地给数据时
// （播放器起播、慢镜头），不主动 Flush 会让客户端迟迟拿不到已就绪的字节。
// 不能用 `w.(http.Flusher)`：中间的 statusRecorder 包装会挡住该接口，
// 所以走 http.ResponseController（它认得 Unwrap 链）。
type flushWriter struct {
	w      io.Writer
	flush  func() error
	broken bool
}

func newFlushWriter(w http.ResponseWriter) io.Writer {
	return &flushWriter{w: w, flush: http.NewResponseController(w).Flush}
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if err != nil {
		return n, err
	}
	if !fw.broken {
		if ferr := fw.flush(); ferr != nil {
			// 该连接不支持 Flush（或已坏）：不再尝试，写入错误会从下一次 Write 报出来。
			fw.broken = true
		}
	}
	return n, nil
}
