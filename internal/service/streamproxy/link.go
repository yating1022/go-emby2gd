package streamproxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/https"
)

// upstreamExcludedHeaders 禁止转发给上游的请求头
//
// 包括逐跳头 (跨代理转发非法) 与 Emby 凭据 (不得泄漏给第三方上游)。
// 请求头采用白名单策略构造, 这里的剔除是纵深防御:
// 配置项 request-header 也可能包含其中某些键。
//
// 注意 Accept-Encoding 不在此列: 它不是"剔除掉就行"的头,
// 必须显式写死为 identity, 原因见 buildUpstreamHeader 的第 4 步。
var upstreamExcludedHeaders = []string{
	"Host", "Connection", "Keep-Alive", "Transfer-Encoding", "Trailer", "Upgrade", "TE",
	"Authorization", "Cookie",
}

// upstreamExcludedHeaderPrefixes 需要按前缀剔除的请求头
//
// 全部写成小写: 比较时会对 header 键先做 ToLower, 见 stripExcludedHeaders。
var upstreamExcludedHeaderPrefixes = []string{"proxy-", "x-emby-", "x-mediabrowser-", "if-"}

// passthroughResponseHeaders 需要回写给客户端的响应头
//
// Location 必须包含在内: 跟随重定向的判定只覆盖 301/302/307/308,
// 上游若返回 300/303 这类同样带 Location 但不在跟随集合内的状态码,
// 不透传 Location 就会给客户端一个"没有 Location 的重定向",
// 而此时响应已经开始写入, 调用方已失去回退机会。
var passthroughResponseHeaders = []string{
	"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag", "Location",
}

// linkCache 直链缓存, key 为归一化之后的 strm 地址, value 为 linkEntry
//
// 只缓存地址字符串, 不缓存响应体, 因此不存在内存风险
var linkCache sync.Map

// getCachedLink 读取直链缓存
//
// 过期条目在读时惰性清理
func getCachedLink(key string) (string, bool) {
	value, ok := linkCache.Load(key)
	if !ok {
		return "", false
	}
	entry, ok := value.(linkEntry)
	if !ok {
		linkCache.Delete(key)
		return "", false
	}
	if time.Now().After(entry.expireAt) {
		linkCache.Delete(key)
		return "", false
	}
	return entry.finalURL, true
}

// putCachedLink 写入直链缓存
func putCachedLink(key, finalURL string, ttl time.Duration) {
	linkCache.Store(key, linkEntry{finalURL: finalURL, expireAt: time.Now().Add(ttl)})
}

// removeCachedLink 删除直链缓存
func removeCachedLink(key string) {
	linkCache.Delete(key)
}

// buildUpstreamHeader 构造发往上游的请求头
//
// 采用白名单策略, 只透传 Range、Accept 与 User-Agent,
// 其余客户端头一律不转发, 避免把 Emby 凭据泄漏给第三方上游。
// User-Agent 取值优先级: 配置的固定 UA > 客户端 UA > 不设置。
//
// Accept-Encoding 不按透传处理, 而是写死为 identity, 防止 Transport 透明解压。
func buildUpstreamHeader(clientHeader http.Header, cfg *config.StrmProxy) http.Header {
	header := make(http.Header, 4)

	// 1 白名单透传
	if clientRange := clientHeader.Get("Range"); clientRange != "" {
		header.Set("Range", clientRange)
	}
	if accept := clientHeader.Get("Accept"); accept != "" {
		header.Set("Accept", accept)
	}
	if ua := clientHeader.Get("User-Agent"); ua != "" {
		header.Set("User-Agent", ua)
	}

	// 2 配置的固定请求头覆盖同名透传头, 值为空表示不设置该头
	for key, values := range cfg.RequestHeaderValues() {
		header.Del(key)
		for _, value := range values {
			if value == "" {
				continue
			}
			header.Add(key, value)
		}
	}

	// 3 纵深防御: 剔除逐跳头与敏感头
	stripExcludedHeaders(header)

	// 4 必须显式设置 Accept-Encoding: identity
	//
	// 仅仅把客户端的 Accept-Encoding 剔除是不够的:
	// net/http/transport.go 中 DisableCompression 的文档说明, Transport 会在
	// "when the Request contains no existing Accept-Encoding value" 时
	// 自行补上 Accept-Encoding: gzip, 并把拿到的 gzip 响应透明解压。
	// 于是会有两个后果:
	//   a. 上游对非 Range 的媒体响应做 gzip 时, 我们要白白解压一遍, 在小内存机器上白烧 CPU;
	//   b. 透明解压时 Transport 会从响应头里删掉 Content-Encoding 与 Content-Length,
	//      导致回写不出总长度, 播放器拿不到文件大小。
	//
	// 显式写入 identity 之后, Transport 既不会追加 gzip, 也不会透明解压。
	// 这一处修正完全在本包内完成, 不需要改动 internal/util/https 的共享 client。
	// 写死 identity 同时意味着配置项 request-header 也无法把它覆盖回 gzip。
	header.Set("Accept-Encoding", "identity")

	return header
}

// stripExcludedHeaders 剔除禁止转发给上游的请求头
//
// 按前缀剔除时必须大小写不敏感:
// http.Header 的键会被 textproto.CanonicalMIMEHeaderKey 规范化
// (连字符后只大写首字母), 因此 X-MediaBrowser-Token 实际存成 X-Mediabrowser-Token。
// 用字面量 "X-MediaBrowser-" 做前缀判断永远匹配不上, 该条剔除会静默失效,
// 凭据照常发给上游。
func stripExcludedHeaders(header http.Header) {
	for _, key := range upstreamExcludedHeaders {
		header.Del(key)
	}
	for key := range header {
		lowerKey := strings.ToLower(key)
		for _, prefix := range upstreamExcludedHeaderPrefixes {
			if strings.HasPrefix(lowerKey, prefix) {
				header.Del(key)
				break
			}
		}
	}
}

// resolveRedirectLocation 解析上游重定向的 Location 头
//
// 支持绝对地址、以 '/' 开头的相对地址与普通相对地址
func resolveRedirectLocation(currentURL, location string) (string, error) {
	location = strings.TrimSpace(location)
	if location == "" {
		return "", fmt.Errorf("上游重定向响应缺少 Location 头: %s", currentURL)
	}

	lower := strings.ToLower(location)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return location, nil
	}

	base, err := url.Parse(currentURL)
	if err != nil {
		return "", fmt.Errorf("解析当前上游地址失败: %w", err)
	}
	ref, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("解析上游重定向地址失败: %w", err)
	}
	return base.ResolveReference(ref).String(), nil
}

// requestUpstream 向上游发起一次不自动重定向的 GET 请求
//
// 手动跟随重定向是为了拿到中间跳转信息并控制跳数上限;
// 请求绑定了调用方上下文, 客户端断连时上游连接会一并释放
func requestUpstream(ctx context.Context, target string, clientHeader http.Header, cfg *config.StrmProxy) (*http.Response, error) {
	resp, err := https.Get(target).
		Header(buildUpstreamHeader(clientHeader, cfg)).
		Context(ctx).
		DoSingle()
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %s, %w", target, err)
	}
	return resp, nil
}

// resolveLink 请求上游并解析出可直接流式读取的最终响应
//
// 返回的 resp 已经带有本次请求的 Range 头, 调用方负责关闭 Body。
// 未写出任何响应时返回错误, 调用方可以按既有策略回退。
func resolveLink(ctx context.Context, normalizedURL string, clientHeader http.Header) (*http.Response, error) {
	cfg := proxyConfig()
	if cfg == nil {
		return nil, errors.New("strm 直链代理未配置")
	}

	target := normalizedURL
	if cached, ok := getCachedLink(normalizedURL); ok {
		logInfof("直链缓存命中: %s", cached)
		target = cached
	} else {
		logInfof("直链缓存未命中, 开始请求上游")
	}

	hops := 0
	retried := false
	for {
		resp, err := requestUpstream(ctx, target, clientHeader, cfg)
		if err != nil {
			logErrorf("%v", err)
			return nil, err
		}

		logInfof("上游响应: status=%d, content-type=%s, content-length=%s, accept-ranges=%q",
			resp.StatusCode, resp.Header.Get("Content-Type"),
			resp.Header.Get("Content-Length"), resp.Header.Get("Accept-Ranges"))

		// 1 跟随重定向
		if https.IsRedirectCode(resp.StatusCode) {
			location := resp.Header.Get("Location")
			resp.Body.Close()
			next, err := resolveRedirectLocation(target, location)
			if err != nil {
				return nil, err
			}
			hops++
			if hops > cfg.MaxRedirectDepth {
				return nil, fmt.Errorf("上游重定向次数超过上限 %d: %s", cfg.MaxRedirectDepth, normalizedURL)
			}
			logInfof("跟随重定向 (第 %d 跳): %s -> %s", hops, target, next)
			target = next
			continue
		}

		// 2 直链失效: 清除缓存并重试一次
		if _, invalid := cfg.RetryCodes()[resp.StatusCode]; invalid {
			resp.Body.Close()
			if retried {
				return nil, fmt.Errorf("直链失效重试后仍然失败: %d, %s", resp.StatusCode, target)
			}
			retried = true
			logWarnf("上游返回 %d, 判定直链失效, 清除缓存并重试一次", resp.StatusCode)
			removeCachedLink(normalizedURL)
			target = normalizedURL
			hops = 0
			continue
		}

		// 3 上游服务端错误: 此时尚未写出任何响应, 交给调用方回退
		if resp.StatusCode >= http.StatusInternalServerError {
			resp.Body.Close()
			return nil, fmt.Errorf("上游返回错误状态: %d, %s", resp.StatusCode, target)
		}

		// 4 只有真正发生过跳转才值得缓存:
		//   上游直接 2xx 时 "直链" 就是原地址本身, 写缓存只会白白占内存
		if hops > 0 {
			ttl := cfg.LinkCacheExpire()
			putCachedLink(normalizedURL, target, ttl)
			logSuccessf("解析到直链: %s, 已缓存 %s", target, ttl)
		}

		return resp, nil
	}
}
