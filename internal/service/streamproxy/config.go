package streamproxy

import "github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"

// proxyConfig 获取当前的 strm 直链代理配置
//
// 未配置时返回 nil;
// 这里不缓存配置对象: 启动后配置只读, 每次读取的开销可以忽略,
// 缓存反而会在配置被替换 (如测试注入) 之后持有失效的引用
func proxyConfig() *config.StrmProxy {
	if config.C == nil || config.C.Emby == nil || config.C.Emby.Strm == nil {
		return nil
	}
	return config.C.Emby.Strm.Proxy
}

// ProxyEnabled 判断 strm 直链代理播放是否开启
func ProxyEnabled() bool {
	if config.C == nil {
		return false
	}
	return config.C.Emby.StrmProxyEnabled()
}
