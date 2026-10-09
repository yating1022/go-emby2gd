package constant

const (
	CurrentVersion = "v2.8.2"
	RepoAddr       = "https://github.com/AmbitiousJun/go-emby2openlist"
)

const (
	Reg_Socket       = `(?i)^/.*(socket|embywebsocket)`
	Reg_PlaybackInfo = `(?i)^/.*items/.*/playbackinfo\??`

	Reg_PlayingStopped  = `(?i)^/.*sessions/playing/stopped`
	Reg_PlayingProgress = `(?i)^/.*sessions/playing/progress`

	Reg_UserItems                = `(?i)^/.*users/.*/items/\d+($|\?)`
	Reg_UserEpisodeItems         = `(?i)^/.*users/.*/items\?.*includeitemtypes=(episode|movie)`
	Reg_UserItemsRandomResort    = `(?i)^/.*users/.*/items\?.*SortBy=Random`
	Reg_UserItemsRandomWithLimit = `(?i)^/.*users/.*/items/with_limit\?.*SortBy=Random`
	Reg_UserPlayedItems          = `(?i)^/.*users/.*/playeditems/(\d+)($|\?|/.*)?`
	Reg_UserLatestItems          = `(?i)^/.*users/.*/items/latest($|\?)`

	Reg_ShowEpisodes   = `(?i)^/.*shows/.*/episodes\??`
	Reg_VideoSubtitles = `(?i)^/.*videos/.*/subtitles`

	Reg_ResourceStream   = `(?i)^/.*(videos|audio)/.*/(stream|universal)(\.\w+)?\??`
	Reg_ResourceMaster   = `(?i)^/.*(videos|audio)/.*/(master)(\.\w+)?\??`
	Reg_ResourceMain     = `(?i)^/.*(videos|audio)/.*/main.m3u8\??`
	Reg_ResourceOriginal = `(?i)^/.*(videos|audio)/.*/original(\.\w+)?\??`

	Reg_ProxyPlaylist = `(?i)^/.*videos/proxy_playlist\??`
	Reg_ProxyTs       = `(?i)^/.*videos/proxy_ts\??`
	Reg_ProxySubtitle = `(?i)^/.*videos/proxy_subtitle\??`

	Reg_ItemDownload     = `(?i)^/.*items/\d+/download($|\?)`
	Reg_ItemSyncDownload = `(?i)^/.*sync/jobitems/\d+/file($|\?)`

	Reg_Images             = `(?i)^/.*images`
	Reg_VideoModWebDefined = `(?i)^/web/modules/htmlvideoplayer/plugin.js`
	Reg_Proxy2Origin       = `^/$|(?i)^.*(/web|/users|/artists|/genres|/similar|/shows|/system|/remote|/scheduledtasks)`

	Reg_Root      = `(?i)^/$`
	Reg_IndexHtml = `(?i)^/web/index\.html`

	Reg_OpenlistLocalTreeUpdatePrefix = `^(/[^/\\]+)+$`

	// agent 代理网络: 节点侧调用的三个接口(协议见 internal/service/agentnet)
	Reg_AgentEnroll       = `^/api/agent/enroll($|\?)`
	Reg_AgentHeartbeat    = `^/api/agent/heartbeat($|\?)`
	Reg_AgentDownloadLink = `^/api/agent/download-link($|\?)`

	// agent 节点安装脚本(脚本本身不含密钥, 无需鉴权)
	Reg_InstallScript = `^/install\.sh($|\?)`

	Reg_All = `.*`
)

const (
	Route_SelfBase                = "/ge2o"
	Route_CustomJs                = Route_SelfBase + "/custom.js"
	Route_CustomCss               = Route_SelfBase + "/custom.css"
	Route_UpdateOpenlistLocalTree = Route_SelfBase + "/openlist/local_tree/update"
	Route_Web                     = Route_SelfBase + "/web"
	Route_ValidateApiSecret       = Route_SelfBase + "/secret/validate"
	Route_SyncServerLog           = Route_SelfBase + "/ws/log/sync"
	Route_InstallScript           = "/install.sh"

	// agent 代理网络管理接口(管理员, 沿用 /ge2o 惯例)
	//
	// 注意规则表的匹配顺序: /agents/update 与 /agents/delete 必须排在 /agents 之前
	// (匹配是未锚定的子串查找, 否则会被前缀规则截胡)。
	Route_AgentNetworkAgents         = Route_SelfBase + "/agent-network/agents"
	Route_AgentNetworkAgentsUpdate   = Route_SelfBase + "/agent-network/agents/update"
	Route_AgentNetworkAgentsDelete   = Route_SelfBase + "/agent-network/agents/delete"
	Route_AgentNetworkInstallCommand = Route_SelfBase + "/agent-network/install-command"
)

const (
	RouteSubMatchGinKey = "routeSubMatches" // 路由匹配成功时, 会将匹配的正则结果存放到 Gin 上下文

	CustomJsDirName  = "custom-js"  // 自定义脚本存放目录
	CustomCssDirName = "custom-css" // 自定义样式存放目录

	CommonDlUserAgent = "libmpv" // 通用的下载 UA
)
