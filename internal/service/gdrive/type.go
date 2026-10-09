package gdrive

import "time"

// panelEnvelope 面板直链接口的响应信封
type panelEnvelope struct {
	// OK 是否成功
	OK bool `json:"ok"`
	// Data 成功时的数据
	Data panelDirectLink `json:"data"`
	// Error 失败时的错误详情
	Error panelError `json:"error"`
}

// panelDirectLink /api/dl 成功响应里的 data 对象
type panelDirectLink struct {
	// URL Google 的下载地址
	URL string `json:"url"`
	// Headers 请求 URL 时必须带上的请求头, 至少含 Authorization
	Headers map[string]string `json:"headers"`
	// ExpiresAt 令牌的过期时刻, RFC3339 带 Z
	//
	// 刻意声明成 string 而不是 time.Time: 一个格式不对的时间戳不该让整个响应
	// 反序列化失败。解析失败的语义是"没有过期信息", 由调用方按立即过期处理。
	ExpiresAt string `json:"expires_at"`
	// ScopeNote 面板给出的权限范围说明
	//
	// 内容较长且只在排查凭据范围时有用, 不参与逻辑, 也不进日志。
	ScopeNote string `json:"scope_note"`
	// File 文件元信息
	File panelFile `json:"file"`
}

// panelFile 面板返回的文件元信息
type panelFile struct {
	// ID 文件 ID, 仅用于日志
	ID string `json:"id"`
	// Name 文件名
	Name string `json:"name"`
	// Path Drive 内的逻辑路径
	Path string `json:"path"`
	// Size 文件大小, 单位: 字节
	Size int64 `json:"size"`
	// MimeType 文件类型
	MimeType string `json:"mime_type"`
}

// panelError 面板返回的错误详情
//
// 只取 code 与 message, 不保留响应体原文。
type panelError struct {
	// Code 错误码, 如 PATH_NOT_IN_CACHE
	Code string `json:"code"`
	// Message 中文错误描述, 由面板专门撰写, 本项目直接沿用
	Message string `json:"message"`
}

// tokenEntry 全局令牌槽的条目
type tokenEntry struct {
	// headers 请求下载地址时要带上的请求头
	headers map[string]string
	// expiresAtRaw 面板给出的原始 RFC3339 过期串
	//
	// 只用于原样透传(ResolveTarget -> agent 的 download-link 响应):
	// agent 侧按它排直链刷新, 原样传才能保证两侧算的是同一个时刻。
	expiresAtRaw string
	// deadline 本条目视为过期的时刻
	//
	// 已经扣掉 linkCacheSafetyMargin 并封顶 maxLinkCacheTTL,
	// 因此读侧只需要与当前时间比较即可。
	deadline time.Time
}

// urlEntry URL 缓存的条目
//
// 直链按路径缓存且长期有效, 但"长期有效"不等于"永远有效": 文件被替换、面板改了
// 直链格式都会让它失效。因此条目上记一个写入时的令牌代次, 供失效重试判断
// "这条路径的直链是不是在失败之后重新取过的"。
type urlEntry struct {
	// directURL 面板给出的下载地址
	directURL string
	// generation 写入本条时的令牌代次
	generation uint64
}

// target 一次取流所需的完整信息
type target struct {
	// directURL 面板给出的下载地址
	directURL string
	// headers 请求 directURL 时必须带上的请求头(含账号级 Authorization)
	//
	// 与令牌槽里的是同一份 map, 只读, 任何调用方都不得修改。
	headers map[string]string
	// expiresAtRaw 面板给出的原始 RFC3339 过期串(可空)
	//
	// 只透传给 agent, 本包不据它做任何判断。
	expiresAtRaw string
	// generation 这份凭据对应的令牌代次
	//
	// 失效重试用它判断"是否已经有别的请求刷新过": 代次比自己拿到的更新时,
	// 说明当前缓存就是新凭据, 不需要再打一次面板。
	generation uint64
}

// fetchError 拉取媒体数据失败时携带的上游状态码
//
// 单独一个类型是为了让上层能按状态码判断"是不是刚才用的凭据/直链失效了",
// 而不是把所有错误一律重试。
type fetchError struct {
	// statusCode 上游返回的状态码
	statusCode int
	// directURL 出错的下载地址(不含凭据)
	directURL string
}
