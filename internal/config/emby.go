package config

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/https"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/maps"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/strs"

	"gopkg.in/yaml.v3"
)

// PeStrategy 代理异常策略类型
type PeStrategy string

const (
	PeStrategyOrigin PeStrategy = "origin" // 回源
	PeStrategyReject PeStrategy = "reject" // 拒绝请求
)

// DlStrategy 下载策略类型
type DlStrategy string

const (
	DlStrategyOrigin DlStrategy = "origin" // 代理到源服务器
	DlStrategyDirect DlStrategy = "direct" // 获取并重定向到直链
	DlStrategy403    DlStrategy = "403"    // 拒绝响应
)

// validPeStrategy 用于校验用户配置的策略是否合法
var validPeStrategy = map[PeStrategy]struct{}{
	PeStrategyOrigin: {}, PeStrategyReject: {},
}

// validDlStrategy 用于校验用户配置的下载策略是否合法
var validDlStrategy = map[DlStrategy]struct{}{
	DlStrategyOrigin: {}, DlStrategyDirect: {}, DlStrategy403: {},
}

// Emby 相关配置
type Emby struct {
	// Emby 源服务器地址
	Host string `yaml:"host"`
	// rclone 或者 cd 的挂载目录
	MountPath string `yaml:"mount-path"`
	// EpisodesUnplayPrior 在获取剧集列表时是否将未播资源优先展示
	EpisodesUnplayPrior bool `yaml:"episodes-unplay-prior"`
	// ResortRandomItems 是否对随机的 items 进行重排序
	ResortRandomItems bool `yaml:"resort-random-items"`
	// ProxyErrorStrategy 代理错误时的处理策略
	ProxyErrorStrategy PeStrategy `yaml:"proxy-error-strategy"`
	// ImagesQuality 图片质量
	ImagesQuality int `yaml:"images-quality"`
	// ImagesOriginal 是否返回原图
	ImagesOriginal bool `yaml:"images-original"`
	// Strm strm 配置
	Strm *Strm `yaml:"strm"`
	// DownloadStrategy 下载接口响应策略
	DownloadStrategy DlStrategy `yaml:"download-strategy"`
	// LocalMediaRoots 本地媒体根路径
	LocalMediaRoots []string `yaml:"local-media-roots"`
	// CustomCssJs 自定义 css js 配置
	CustomCssJs *CustomCssJs `yaml:"custom-css-js"`
}

func (e *Emby) Init() error {
	if strs.AnyEmpty(e.Host) {
		return errors.New("emby.host 配置不能为空")
	}
	if strs.AnyEmpty(string(e.ProxyErrorStrategy)) {
		// 失败默认回源
		e.ProxyErrorStrategy = PeStrategyOrigin
	}
	if strs.AnyEmpty(string(e.DownloadStrategy)) {
		// 默认响应直链
		e.DownloadStrategy = DlStrategyDirect
	}

	e.ProxyErrorStrategy = PeStrategy(strings.TrimSpace(string(e.ProxyErrorStrategy)))
	if _, ok := validPeStrategy[e.ProxyErrorStrategy]; !ok {
		return fmt.Errorf("emby.proxy-error-strategy 配置错误, 有效值: %v", maps.Keys(validPeStrategy))
	}

	if e.ImagesQuality == 0 {
		// 不允许配置零值
		e.ImagesQuality = 70
	}
	if e.ImagesQuality < 0 || e.ImagesQuality > 100 {
		return fmt.Errorf("emby.images-quality 配置错误: %d, 允许配置范围: [1, 100]", e.ImagesQuality)
	}

	if e.Strm == nil {
		e.Strm = new(Strm)
	}
	if err := e.Strm.Init(); err != nil {
		return fmt.Errorf("emby.strm 配置错误: %v", err)
	}

	e.DownloadStrategy = DlStrategy(strings.TrimSpace(string(e.DownloadStrategy)))
	if _, ok := validDlStrategy[e.DownloadStrategy]; !ok {
		return fmt.Errorf("emby.download-strategy 配置错误, 有效值: %v", maps.Keys(validDlStrategy))
	}

	if e.CustomCssJs == nil {
		e.CustomCssJs = new(CustomCssJs)
	}
	if err := e.CustomCssJs.Init(); err != nil {
		return fmt.Errorf("emby.custom-css-js 配置错误: %v", err)
	}

	return nil
}

// IsLocalMediaPath 判断路径是否为本地媒体路径
func (e *Emby) IsLocalMediaPath(p string) bool {
	// 根据配置文件中的本地文件进行判定
	for _, root := range e.LocalMediaRoots {
		if strings.HasPrefix(p, root) {
			return true
		}
	}

	// 根据路径前缀判定是否是网络共享
	sharedPrefixes := []string{"smb://", "//"}
	for _, prefix := range sharedPrefixes {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}

	return false
}

// StrmProxyEnabled 判断 strm 直链代理播放是否开启
func (e *Emby) StrmProxyEnabled() bool {
	if e == nil || e.Strm == nil || e.Strm.Proxy == nil {
		return false
	}
	return e.Strm.Proxy.Enable
}

// StrmProxyConfig 获取 strm 直链代理配置
//
// 未配置时返回零值, 不返回 nil
func (e *Emby) StrmProxyConfig() StrmProxy {
	if e == nil || e.Strm == nil || e.Strm.Proxy == nil {
		return StrmProxy{}
	}
	return *e.Strm.Proxy
}

// Strm strm 配置
type Strm struct {
	// PathMap 远程路径映射
	PathMap []string `yaml:"path-map"`
	// InternalRedirectEnable 是否启用 strm 内部重定向
	InternalRedirectEnable bool `yaml:"internal-redirect-enable"`
	// Proxy strm 直链代理播放配置
	Proxy *StrmProxy `yaml:"proxy"`

	// pathMap 配置初始化后转换为二维数组切片结构
	pathMap [][2]string
}

// Init 配置初始化
func (s *Strm) Init() error {
	s.pathMap = make([][2]string, 0, len(s.PathMap))
	for _, path := range s.PathMap {
		splits := strings.Split(path, "=>")
		if len(splits) != 2 {
			return fmt.Errorf("映射配置不规范: %s, 请使用 => 进行分割", path)
		}
		from, to := strings.TrimSpace(splits[0]), strings.TrimSpace(splits[1])
		s.pathMap = append(s.pathMap, [2]string{from, to})
	}

	if s.Proxy == nil {
		// 未配置时按零值构造, 等价于 enable: false
		s.Proxy = new(StrmProxy)
	}
	if err := s.Proxy.Init(); err != nil {
		return fmt.Errorf("proxy 配置错误: %w", err)
	}
	return nil
}

// 直链代理配置的默认值
const (
	// defaultStrmProxyLinkCacheExpired 直链缓存时长默认值
	defaultStrmProxyLinkCacheExpired = time.Minute * 10
	// defaultStrmProxyMaxRedirectDepth 上游重定向最大跟随跳数默认值
	defaultStrmProxyMaxRedirectDepth = 5
	// defaultStrmProxyMaxConcurrentStreams 并发代理传输上限默认值
	defaultStrmProxyMaxConcurrentStreams = 16
)

// defaultStrmProxyRetryStatusCodes 判定直链失效的默认状态码
var defaultStrmProxyRetryStatusCodes = []int{http.StatusForbidden, http.StatusNotFound, http.StatusGone}

// StrmProxy strm 直链代理播放配置
type StrmProxy struct {
	// Enable 是否启用 strm 直链代理播放
	Enable bool `yaml:"enable"`
	// Domains 命中代理模式的地址前缀列表
	Domains []string `yaml:"domains"`
	// LinkCacheExpired 直链缓存时长, 支持 s/m/h/d 单位, 默认 10m
	LinkCacheExpired string `yaml:"link-cache-expired"`
	// MaxRedirectDepth 上游重定向最大跟随跳数, 默认 5
	MaxRedirectDepth int `yaml:"max-redirect-depth"`
	// RetryStatusCodes 判定直链失效并触发重试的状态码, 默认 [403, 404, 410]
	RetryStatusCodes []int `yaml:"retry-status-codes"`
	// MaxConcurrentStreams 并发代理传输上限, 0 表示不限制, 默认 16
	MaxConcurrentStreams int `yaml:"max-concurrent-streams"`
	// RequestHeader 固定添加到上游请求的请求头
	RequestHeader map[string]string `yaml:"request-header"`

	// maxConcurrentStreamsSet 记录配置中是否显式出现 max-concurrent-streams
	//
	// 用于区分"完全缺省"(取默认值 16) 与"显式配置 0"(表示不限制)
	maxConcurrentStreamsSet bool
	// domains 初始化后归一化(去首尾空白 + scheme 小写 + 去尾部斜杠)的前缀列表
	domains []string
	// linkCacheExpire 初始化后的直链缓存时长
	linkCacheExpire time.Duration
	// retryCodes 初始化后的失效状态码集合
	retryCodes map[int]struct{}
	// requestHeader 初始化后的固定请求头
	requestHeader http.Header
}

// UnmarshalYAML 自定义解析 strm 直链代理配置
//
// 唯一的目的是识别 max-concurrent-streams 是否被显式配置:
// 该字段的 "0" 有独立语义(不限制), 与"未配置"不能共用零值。
func (p *StrmProxy) UnmarshalYAML(value *yaml.Node) error {
	type plainStrmProxy struct {
		Enable               bool              `yaml:"enable"`
		Domains              []string          `yaml:"domains"`
		LinkCacheExpired     string            `yaml:"link-cache-expired"`
		MaxRedirectDepth     int               `yaml:"max-redirect-depth"`
		RetryStatusCodes     []int             `yaml:"retry-status-codes"`
		MaxConcurrentStreams *int              `yaml:"max-concurrent-streams"`
		RequestHeader        map[string]string `yaml:"request-header"`
	}

	var v plainStrmProxy
	if err := value.Decode(&v); err != nil {
		return err
	}

	p.Enable = v.Enable
	p.Domains = v.Domains
	p.LinkCacheExpired = v.LinkCacheExpired
	p.MaxRedirectDepth = v.MaxRedirectDepth
	p.RetryStatusCodes = v.RetryStatusCodes
	p.RequestHeader = v.RequestHeader
	if v.MaxConcurrentStreams != nil {
		p.MaxConcurrentStreams = *v.MaxConcurrentStreams
		p.maxConcurrentStreamsSet = true
	}
	return nil
}

// Init 配置初始化
func (p *StrmProxy) Init() error {
	// 1 上游重定向最大跟随跳数
	if p.MaxRedirectDepth == 0 {
		p.MaxRedirectDepth = defaultStrmProxyMaxRedirectDepth
	}
	if p.MaxRedirectDepth < 1 {
		return fmt.Errorf("max-redirect-depth 配置错误: %d, 值需大于 0", p.MaxRedirectDepth)
	}
	if p.MaxRedirectDepth > https.MaxRedirectDepth {
		return fmt.Errorf("max-redirect-depth 配置错误: %d, 不能大于 %d", p.MaxRedirectDepth, https.MaxRedirectDepth)
	}

	// 2 直链缓存时长
	if strs.AnyEmpty(p.LinkCacheExpired) {
		p.linkCacheExpire = defaultStrmProxyLinkCacheExpired
	} else {
		expire, err := parseDuration(p.LinkCacheExpired)
		if err != nil {
			return fmt.Errorf("link-cache-expired 配置错误: %w", err)
		}
		p.linkCacheExpire = expire
	}

	// 3 并发传输上限: 完全缺省取默认值, 显式配置 0 表示不限制
	if !p.maxConcurrentStreamsSet {
		p.MaxConcurrentStreams = defaultStrmProxyMaxConcurrentStreams
	}
	if p.MaxConcurrentStreams < 0 {
		return fmt.Errorf("max-concurrent-streams 配置错误: %d, 值不能小于 0", p.MaxConcurrentStreams)
	}

	// 4 失效状态码
	if len(p.RetryStatusCodes) == 0 {
		p.RetryStatusCodes = defaultStrmProxyRetryStatusCodes
	}
	p.retryCodes = make(map[int]struct{}, len(p.RetryStatusCodes))
	for _, code := range p.RetryStatusCodes {
		if code < 100 || code > 599 {
			return fmt.Errorf("retry-status-codes 配置错误: %d, 有效范围: [100, 599]", code)
		}
		p.retryCodes[code] = struct{}{}
	}

	// 5 固定请求头
	p.requestHeader = make(http.Header, len(p.RequestHeader))
	for key, value := range p.RequestHeader {
		if strs.AnyEmpty(key) {
			return errors.New("request-header 配置错误: 请求头名称不能为空")
		}
		p.requestHeader.Set(key, value)
	}

	// 6 代理前缀: 归一化后按配置顺序保留
	p.domains = make([]string, 0, len(p.Domains))
	for _, domain := range p.Domains {
		normalized := normalizeProxyDomain(domain)
		if normalized == "" {
			return fmt.Errorf("domains 配置错误: %q 不是合法的 http/https 地址", domain)
		}
		p.domains = append(p.domains, normalized)
	}
	if p.Enable && len(p.domains) == 0 {
		return errors.New("domains 配置不能为空")
	}

	return nil
}

// LinkCacheExpire 获取直链缓存时长
func (p *StrmProxy) LinkCacheExpire() time.Duration {
	if p == nil {
		return 0
	}
	return p.linkCacheExpire
}

// RetryCodes 获取判定直链失效的状态码集合
func (p *StrmProxy) RetryCodes() map[int]struct{} {
	if p == nil {
		return nil
	}
	return p.retryCodes
}

// DomainsNormalized 获取归一化之后的代理前缀列表
func (p *StrmProxy) DomainsNormalized() []string {
	if p == nil {
		return nil
	}
	return p.domains
}

// RequestHeaderValues 获取固定添加到上游请求的请求头
func (p *StrmProxy) RequestHeaderValues() http.Header {
	if p == nil {
		return nil
	}
	return p.requestHeader
}

// normalizeProxyDomain 归一化代理前缀配置
//
// 处理: 去首尾空白 + scheme 小写 + 去尾部斜杠;
// 非 http/https 地址返回空字符串
func normalizeProxyDomain(domain string) string {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return ""
	}
	domain = strings.TrimSuffix(domain, "/")
	lower := strings.ToLower(domain)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return ""
	}
	u, err := url.Parse(domain)
	if err != nil || u.Host == "" {
		return ""
	}
	u.Scheme = strings.ToLower(u.Scheme)
	return strings.TrimSuffix(u.String(), "/")
}

// parseDuration 解析形如 10m / 1h / 30s / 1d 的时长配置
func parseDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, errors.New("时长不能为空")
	}
	timeFlag := raw[len(raw)-1:]
	duration, ok := durationMap[timeFlag]
	if !ok {
		return 0, fmt.Errorf("%s, 支持的时间单位: s, m, h, d", timeFlag)
	}
	base, err := strconv.Atoi(raw[:len(raw)-1])
	if err != nil {
		return 0, err
	}
	if base < 1 {
		return 0, fmt.Errorf("%d, 值需大于 0", base)
	}
	return time.Duration(base) * duration, nil
}

// MapPath 将传入路径按照预配置的映射关系从上到下按顺序进行映射,
// 至多成功映射一次
func (s *Strm) MapPath(path string) string {
	for _, m := range s.pathMap {
		from, to := m[0], m[1]
		if strings.Contains(path, from) {
			logs.Tip("映射路径: [%s] => [%s]", from, to)
			return strings.Replace(path, from, to, 1)
		}
	}
	return path
}

// CustomCssJs 自定义 css js 配置
type CustomCssJs struct {

	// DebugMode 调试模式开关
	DebugMode bool `yaml:"debug-mode"`
}

// Init 配置初始化
func (c *CustomCssJs) Init() error {
	return nil
}
