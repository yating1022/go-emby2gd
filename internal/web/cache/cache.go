package cache

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/constant"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/encrypts"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/https"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs/colors"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/strs"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/urls"

	"github.com/gin-gonic/gin"
)

// CacheKeyIgnoreParams 忽略的请求头或者参数
//
// 如果请求地址包含列表中的请求头或者参数, 则不参与 cacheKey 运算
var CacheKeyIgnoreParams = map[string]struct{}{
	// Fileball
	"StartTimeTicks": {}, "X-Playback-Session-Id": {},

	// Emby
	"PlaySessionId": {},

	// Common
	"Range": {}, "Host": {}, "Referrer": {}, "Connection": {},
	"Accept": {}, "Accept-Encoding": {}, "Accept-Language": {}, "Cache-Control": {},
	"Upgrade-Insecure-Requests": {}, "Referer": {}, "Origin": {},

	// StreamMusic
	"X-Streammusic-Audioid": {}, "X-Streammusic-Savepath": {},

	// IP
	"X-Forwarded-For": {}, "X-Real-IP": {}, "X-Real-Ip": {}, "Forwarded": {}, "Client-IP": {},
	"True-Client-IP": {}, "CF-Connecting-IP": {}, "X-Cluster-Client-IP": {},
	"Fastly-Client-IP": {}, "X-Client-IP": {}, "X-ProxyUser-IP": {},
	"Via": {}, "Forwarded-For": {}, "X-From-Cdn": {},
}

// CacheableRouteMarker 缓存白名单
// 只有匹配上正则表达式的路由才会被缓存
//
// 注意: 媒体/文件字节流类路由 (stream / download / sync download) 不在白名单内。
// 响应缓存会把整个响应体缓冲进内存, 而这类响应体可能是数 GB 的媒体数据,
// 必须由 handler 直接流式写回客户端。
func CacheableRouteMarker() gin.HandlerFunc {
	cacheablePatterns := []*regexp.Regexp{
		regexp.MustCompile(constant.Reg_PlaybackInfo),
		regexp.MustCompile(constant.Reg_VideoSubtitles),
		regexp.MustCompile(constant.Reg_UserItemsRandomWithLimit),
	}

	return func(c *gin.Context) {
		for _, pattern := range cacheablePatterns {
			if pattern.MatchString(c.Request.RequestURI) {
				return
			}
		}
		c.Header(HeaderKeyExpired, "-1")
	}
}

// RequestCacher 请求缓存中间件
func RequestCacher() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1 判断请求是否需要缓存
		if c.Writer.Header().Get(HeaderKeyExpired) == "-1" {
			return
		}

		// 2 计算 cache key
		cacheKey, err := calcCacheKey(c)
		if err != nil {
			logs.Warn("cache key 计算异常: %v, 跳过缓存", err)
			// 如果没有调用 Abort, Gin 会自动继续调用处理器链
			return
		}

		// 3 尝试获取缓存
		if rc, ok := getCache(cacheKey); ok {
			if https.IsRedirectCode(rc.code) {
				// 适配重定向请求
				c.Redirect(rc.code, rc.header.header.Get("Location"))
			} else {
				c.Status(rc.code)
				https.CloneHeader(c.Writer, rc.header.header)
				c.Writer.Write(rc.body)
			}
			c.Abort()
			return
		}

		// 4 使用自定义的响应器
		customWriter := &respCacheWriter{body: &bytes.Buffer{}, ResponseWriter: c.Writer}
		c.Writer = customWriter

		// 5 执行请求处理器
		c.Next()

		// 6 响应头清理, 必须放在从这里开始的任何提前返回之前
		//
		// 覆盖范围: 只覆盖从这一步往后走的所有返回路径 (超限放弃缓存、错误响应、
		// 以及正常写入缓存), 这些路径都不会把缓存中间件的三个内部控制字段
		// (Expired / Space / Space-Key) 发给客户端。
		//
		// 注意: 本清理并不覆盖 RequestCacher 的全部返回路径 ——
		// 第 1 步 (Expired 为 "-1" 时直接返回) 与第 3 步中的缓存命中回放
		// (用 https.CloneHeader 整份回放缓存过的响应头) 都不经过这里,
		// 这两条路径仍会把这些内部头写回客户端。
		// 该行为在本次改动之前就已存在, 按 prd R4 "清理时机保持不变" 未作处理。
		header := c.Writer.Header()
		defer header.Del(HeaderKeyExpired)
		defer header.Del(HeaderKeySpace)
		defer header.Del(HeaderKeySpaceKey)

		// 7 响应体超过缓冲上限, 已放弃缓存, 直接返回
		//
		// 这是纵深防御: 超限时 body 已被清空, putCache 拿到 nil 响应体后本身就会直接返回
		// (holder.go 的空响应体判断)。保留该守卫是为了在调用点显式表达意图,
		// 并省掉一次必然空转的 putCache 调用。因此删掉它不会有可观测的行为差异。
		if customWriter.disabled {
			return
		}

		// 8 不缓存错误请求
		if https.IsErrorStatus(c.Writer.Status()) {
			return
		}

		// 9 刷新缓存
		respHeader := respHeader{
			expired:  header.Get(HeaderKeyExpired),
			space:    header.Get(HeaderKeySpace),
			spaceKey: header.Get(HeaderKeySpaceKey),
			header:   header.Clone(),
		}

		go putCache(cacheKey, c, append([]byte(nil), customWriter.body.Bytes()...), respHeader)
	}
}

// Duration 将一个标准的时间转换成适用于缓存时间的字符串
func Duration(d time.Duration) string {
	expired := d.Milliseconds() + time.Now().UnixMilli()
	return fmt.Sprintf("%v", expired)
}

// WaitingForHandleChan 等待预缓存通道被处理完毕
func WaitingForHandleChan() {
	cacheHandleWaitGroup.Wait()
}

// calcCacheKey 计算缓存 key
//
// 计算方式: 取出 请求方法, 请求路径, 请求体, 请求头 转换成字符串之后字典排序,
// 再进行 Md5Hash
func calcCacheKey(c *gin.Context) (string, error) {
	method := c.Request.Method

	q := c.Request.URL.Query()
	for key := range CacheKeyIgnoreParams {
		q.Del(key)
	}
	c.Request.URL.RawQuery = q.Encode()
	uri := c.Request.URL.String()

	body := ""
	if c.Request.Body != nil {
		bodyBytes, err := io.ReadAll(c.Request.Body)
		if err != nil {
			return "", fmt.Errorf("读取请求体失败: %v", err)
		}
		body = string(bodyBytes)
		c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
	}
	header := strings.Builder{}
	for key, values := range c.Request.Header {
		if _, ok := CacheKeyIgnoreParams[key]; ok {
			continue
		}
		header.WriteString(key)
		header.WriteString("=")
		header.WriteString(strings.Join(values, "|"))
		header.WriteString(";")
	}

	headerStr := header.String()
	preEnc := strs.Sort(c.Request.URL.RawQuery + body + headerStr)
	if headerStr != "" {
		logs.Tip("headers to encode cacheKey: %s", colors.ToYellow(headerStr))
	}

	// 为防止字典排序后, 不同的 uri 冲突, 这里在排序完的字符串前再加上原始的 uri
	uriNoArgs := urls.ReplaceAll(
		uri,
		"?"+c.Request.URL.RawQuery, "",
		c.Request.URL.RawQuery, "",
	)

	hash := encrypts.Md5Hash(method + uriNoArgs + preEnc)
	return hash, nil
}
