package streamproxy

import "time"

// linkEntry 直链缓存条目
//
// 缓存只保存地址字符串, 不保存任何响应体, 因此不存在按媒体体积增长的内存占用。
type linkEntry struct {
	// finalURL 跟随重定向之后解析出的最终地址
	finalURL string
	// expireAt 缓存过期时间
	expireAt time.Time
}
