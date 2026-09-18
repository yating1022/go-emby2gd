package emby

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/gdrive"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/openlist"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/path"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/streamproxy"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/https"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/strs"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/trys"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/urls"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/web/cache"

	"github.com/gin-gonic/gin"
)

// Redirect2Transcode 将 master 请求重定向到本地 ts 代理
func Redirect2Transcode(c *gin.Context) {
	templateId := c.Query("template_id")
	if strs.AnyEmpty(templateId) {
		// 尝试从 mediaSourceId 中获取 templateId
		itemInfo, err := resolveItemInfo(c, RouteTranscode)
		if checkErr(c, err) {
			return
		}
		templateId = itemInfo.MsInfo.TemplateId
	}

	apiKey := c.Query(QueryApiKeyName)
	openlistPath := c.Query("openlist_path")
	if strs.AnyEmpty(templateId) {
		ProxyOrigin(c)
		return
	}

	// 只有 template id 时, 需要先获取 openlist path
	if strs.AnyEmpty(openlistPath) {
		Redirect2OpenlistLink(c)
		return
	}

	tu, _ := url.Parse(https.ClientRequestHost(c.Request) + "/videos/proxy_playlist")
	q := tu.Query()
	q.Set("openlist_path", openlistPath)
	q.Set(QueryApiKeyName, apiKey)
	q.Set("template_id", templateId)
	tu.RawQuery = q.Encode()
	c.Redirect(http.StatusTemporaryRedirect, tu.String())
}

// Redirect2OpenlistLink 重定向资源到 openlist 网盘直链
func Redirect2OpenlistLink(c *gin.Context) {
	// 不处理字幕接口
	if strings.Contains(strings.ToLower(c.Request.RequestURI), "subtitles") {
		ProxyOrigin(c)
		return
	}

	// 1 解析要请求的资源信息
	itemInfo, err := resolveItemInfo(c, RouteStream)
	if checkErr(c, err) {
		return
	}
	logs.Info("解析到的 itemInfo: %v", itemInfo)

	// 2 如果请求的是转码资源, 重定向到本地的 m3u8 代理服务
	msInfo := itemInfo.MsInfo
	useTranscode := !msInfo.Empty && msInfo.Transcode
	if useTranscode && msInfo.OpenlistPath != "" {
		u, _ := url.Parse(strings.ReplaceAll(MasterM3U8UrlTemplate, "${itemId}", itemInfo.Id))
		q := u.Query()
		q.Set("template_id", itemInfo.MsInfo.TemplateId)
		q.Set(QueryApiKeyName, itemInfo.ApiKey)
		q.Set("openlist_path", itemInfo.MsInfo.OpenlistPath)
		u.RawQuery = q.Encode()
		logs.Success("重定向 playlist: %s", u.String())
		c.Redirect(http.StatusTemporaryRedirect, u.String())
		return
	}

	// 3 请求资源在 Emby 中的 Path 参数
	embyPath, err := getEmbyFileLocalPath(itemInfo)
	if checkErr(c, err) {
		return
	}

	// 4 Google Drive 直连: strm 内容是 rclone 挂载路径时, 去掉挂载前缀得到团队盘内的
	//   逻辑路径, 交给 GD 管理面板换取直链与请求头, 再由本项目把字节流代理给客户端
	//
	// 必须放在 urls.IsHttpRemote 判断【之外、之前】: 挂载路径是本地文件系统路径而非
	// http 地址, 放在那个分支里永远进不去(见 design §12)。
	if gdPath, ok := gdrive.MatchMountPath(embyPath); ok {
		logs.Info("[直链代理] 检测到 Google Drive 挂载路径: %s -> %s", embyPath, gdPath)

		// 异步发送一个播放 Playback 请求, 触发 emby 解析视频格式
		//
		// 必须在 ProxyGDrive 之前触发: ProxyGDrive 会阻塞到本次传输彻底结束,
		// 放在它之后触发要等整部片子播完才会发出, 等于没有触发。
		go sendOpenStreamPlaybackInfoReqToOrigin(itemInfo)

		written, proxyErr := streamproxy.ProxyGDrive(c.Writer, c.Request, gdPath)
		if written {
			// 响应已经开始写入, 此时不允许再做任何回退
			return
		}

		// 尚未写入任何响应 → 回源, 由 Emby 从挂载点直接读取(即改动前的行为)。
		//
		// 这里直接回源而不落回原有流程: 原有流程会走到 OpenList 分支并打出误导性的
		// "openlist.host 配置为空" 错误, 而最终结果同样是回源。
		logs.Warn("[直链代理] GD 面板取流失败, 回源处理: %v", proxyErr)
		ProxyOrigin(c)
		return
	}

	// 5 如果是远程地址 (strm), 重定向处理
	if urls.IsHttpRemote(embyPath) {
		finalPath := config.C.Emby.Strm.MapPath(embyPath)

		// 5.1 命中代理前缀时, 由本项目请求上游并把字节流代理给客户端, 不再 302
		//
		// 注意: 这里不需要处理响应缓存 —— 字节流路由已不在缓存白名单内,
		// c.Writer 就是原始 writer, 见子任务 09-16-cache-stream-passthrough
		if config.C.Emby.StrmProxyEnabled() {
			logs.Info("[直链代理] 检测到 strm 远程地址: %s", embyPath)

			if prefix, ok := streamproxy.MatchDomain(finalPath); ok {
				logs.Info("[直链代理] 命中代理前缀: %s", prefix)

				// 异步发送一个播放 Playback 请求, 触发 emby 解析 strm 视频格式
				//
				// 必须在 Proxy 之前触发: Proxy 会阻塞到本次传输彻底结束,
				// 放在它之后触发要等整部片子播完才会发出, 等于没有触发。
				//
				// 若代理失败回退到下面的 302 流程, 那里还会再触发一次 —— 重复触发是
				// 幂等的探测请求, 无害; 换来的好处是两条路径都保证在传输开始前触发。
				go sendOpenStreamPlaybackInfoReqToOrigin(itemInfo)

				written, proxyErr := streamproxy.Proxy(c.Writer, c.Request, finalPath)
				if written {
					// 响应已经开始写入, 此时不允许再做任何回退
					return
				}

				// 尚未写入任何响应, 回退到改动前的既定行为 (302):
				// 不叠加 checkErr, 否则客户端最终收到 302 还是回源响应将取决于运行时状态,
				// 排查时无法从日志一眼判定, 见父 design §8
				logs.Error("[直链代理] 代理失败, 回退原有 302 流程: %v", proxyErr)
			} else {
				logs.Info("[直链代理] 未命中任何代理前缀, 走原有 302 流程")
			}
		}

		// 5.2 原有 302 流程 (含 internal-redirect-enable)
		finalPath = getFinalRedirectLink(finalPath, c.Request.Header.Clone())
		logs.Success("重定向 strm: %s", finalPath)
		c.Header(cache.HeaderKeyExpired, cache.Duration(time.Minute*10))
		c.Redirect(http.StatusTemporaryRedirect, finalPath)

		// 异步发送一个播放 Playback 请求, 触发 emby 解析 strm 视频格式
		go sendOpenStreamPlaybackInfoReqToOrigin(itemInfo)

		return
	}

	// 6 如果是本地地址
	if config.C.Emby.IsLocalMediaPath(embyPath) {
		if shouldRedirectDirectly(c, config.C.Emby.Host) {
			logs.Info("本地媒体: %s, 检测到 Emby 服务端与 ge2o 同源: %s, 重定向处理", embyPath, config.C.Emby.Host)
			c.Redirect(http.StatusTemporaryRedirect, config.C.Emby.Host+c.Request.RequestURI)
			return
		}
		// 如果是外网，由于无法直连内网端口，我们返回原有的 original 路径进行代理回源播放
		// （因为有底层的 Context 机制保护，所以即使代理也不会发生内存暴涨）
		logs.Info("本地媒体: %s, 代理回源", embyPath)
		newUri := strings.Replace(c.Request.RequestURI, "stream", "original", 1)
		newUri = strings.Replace(newUri, "universal", "original", 1)
		c.Redirect(http.StatusTemporaryRedirect, newUri)
		return
	}

	// 7 请求 openlist 资源
	fi := openlist.FetchInfo{
		Header:       c.Request.Header.Clone(),
		UseTranscode: useTranscode,
		Format:       msInfo.TemplateId,
	}
	openlistPathRes := path.Emby2Openlist(embyPath)

	allErrors := strings.Builder{}
	// handleOpenlistResource 根据传递的 path 请求 openlist 资源
	handleOpenlistResource := func(path string) bool {
		logs.Info("尝试请求 Openlist 资源: %s", path)
		fi.Path = path
		res := openlist.FetchResource(fi)

		if res.Code != http.StatusOK {
			allErrors.WriteString(fmt.Sprintf("请求 Openlist 失败, code: %d, msg: %s, path: %s;", res.Code, res.Msg, path))
			return false
		}

		// 处理直链
		if !fi.UseTranscode {
			res.Data.Url = config.C.Emby.Strm.MapPath(res.Data.Url)
			logs.Success("请求成功, 重定向到: %s", res.Data.Url)
			c.Header(cache.HeaderKeyExpired, cache.Duration(time.Minute*10))
			c.Redirect(http.StatusTemporaryRedirect, res.Data.Url)
			return true
		}

		// 代理转码 m3u
		u, _ := url.Parse(https.ClientRequestHost(c.Request) + "/videos/proxy_playlist")
		q := u.Query()
		q.Set("template_id", itemInfo.MsInfo.TemplateId)
		q.Set(QueryApiKeyName, itemInfo.ApiKey)
		q.Set("openlist_path", openlist.PathEncode(path))
		u.RawQuery = q.Encode()
		c.Redirect(http.StatusTemporaryRedirect, u.String())
		return true
	}

	if openlistPathRes.Success && handleOpenlistResource(openlistPathRes.Path) {
		return
	}
	paths, err := openlistPathRes.Range()
	if checkErr(c, err) {
		return
	}
	if slices.ContainsFunc(paths, func(path string) bool {
		return handleOpenlistResource(path)
	}) {
		return
	}

	checkErr(c, fmt.Errorf("获取直链失败: %s", allErrors.String()))
}

// ProxyOriginalResource 拦截 original 接口
func ProxyOriginalResource(c *gin.Context) {
	if strings.Contains(strings.ToLower(c.Request.RequestURI), "subtitles") {
		ProxyOrigin(c)
		return
	}

	itemInfo, err := resolveItemInfo(c, RouteOriginal)
	if checkErr(c, err) {
		return
	}

	embyPath, err := getEmbyFileLocalPath(itemInfo)
	if checkErr(c, err) {
		return
	}

	// 如果是本地媒体
	if config.C.Emby.IsLocalMediaPath(embyPath) {
		if shouldRedirectDirectly(c, config.C.Emby.Host) {
			logs.Info("本地媒体: %s, 检测到 Emby 服务端与 ge2o 同源: %s, 重定向处理", embyPath, config.C.Emby.Host)
			c.Redirect(http.StatusTemporaryRedirect, config.C.Emby.Host+c.Request.RequestURI)
			return
		}
		// 外网请求，执行代理回源
		logs.Info("本地媒体: %s, 代理回源", embyPath)
		ProxyOrigin(c)
		return
	}
	Redirect2OpenlistLink(c)
}

// checkErr 检查 err 是否为空
// 不为空则根据错误处理策略返回响应
//
// 返回 true 表示请求已经被处理
func checkErr(c *gin.Context, err error) bool {
	if err == nil || c == nil {
		return false
	}

	// 异常接口, 不缓存
	c.Header(cache.HeaderKeyExpired, "-1")

	// 采用拒绝策略, 直接返回错误
	if config.C.Emby.ProxyErrorStrategy == config.PeStrategyReject {
		logs.Error("代理接口失败: %v", err)
		c.String(http.StatusInternalServerError, "代理接口失败, 请检查日志")
		return true
	}

	logs.Error("代理接口失败: %v, 回源处理", err)
	ProxyOrigin(c)
	return true
}

// getFinalRedirectLink 尝试对带有重定向的原始链接进行内部请求, 返回最终链接
//
// 检测到 internal-redirect-enable 配置未启用时, 直接返回原始链接
//
// 请求中途出现任何失败都会返回原始链接
func getFinalRedirectLink(originLink string, header http.Header) string {

	if !config.C.Emby.Strm.InternalRedirectEnable {
		logs.Info("internal-redirect-enable 未启用, 使用原始链接")
		return originLink
	}

	var finalLink string
	err := trys.Try(func() (err error) {
		logs.Info("正在尝试内部重定向, originLink: [%s]", originLink)
		fl, resp, e := https.Get(originLink).Header(header).DoRedirect()
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		finalLink = fl
		return nil
	}, 3, time.Second*2)

	if err != nil {
		logs.Warn("内部重定向失败: %v", err)
		return originLink
	}

	return finalLink
}

// shouldRedirectDirectly 判断客户端请求的主机名是否与配置的 Emby 服务器主机名一致
func shouldRedirectDirectly(c *gin.Context, rawEmbyHost string) bool {
	// 1. 提取客户端请求的 Hostname（优先处理反向代理头）
	clientHost := c.GetHeader("X-Forwarded-Host")
	if clientHost == "" {
		clientHost = c.Request.Host
	}

	// 清理客户端 Host 中的端口号（如 "192.168.1.100:8080" -> "192.168.1.100"）
	if idx := strings.Index(clientHost, ":"); idx != -1 {
		clientHost = clientHost[:idx]
	}
	clientHost = strings.TrimSpace(clientHost)

	// 2. 提取配置中 Emby Host 的 Hostname（如 "http://192.168.1.100:8096" -> "192.168.1.100"）
	embyHost := extractHostname(rawEmbyHost)

	if clientHost == "" || embyHost == "" {
		return false
	}

	// 3. 忽略大小写比对两者 Hostname
	return strings.EqualFold(clientHost, embyHost)
}

// extractHostname 安全解析 url 格式的主机名
func extractHostname(rawUrl string) string {
	if rawUrl == "" {
		return ""
	}

	// 若配置未带协议前缀（如仅填了 "192.168.1.100:8096"），补全便于 net/url 解析
	if !strings.HasPrefix(rawUrl, "http://") && !strings.HasPrefix(rawUrl, "https://") {
		rawUrl = "http://" + rawUrl
	}

	u, err := url.Parse(rawUrl)
	if err != nil {
		return ""
	}

	return u.Hostname()
}
