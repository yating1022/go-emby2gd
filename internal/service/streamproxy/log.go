package streamproxy

import (
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs"
)

// logPrefix 直链代理日志的固定前缀
//
// 排查一次播放时, grep '[直链代理]' 应能还原完整的代理流程
const logPrefix = "[直链代理] "

// logInfof 输出 info 级别的直链代理日志
func logInfof(format string, v ...any) {
	logs.Info(logPrefix+format, v...)
}

// logSuccessf 输出 success 级别的直链代理日志
func logSuccessf(format string, v ...any) {
	logs.Success(logPrefix+format, v...)
}

// logWarnf 输出 warn 级别的直链代理日志
func logWarnf(format string, v ...any) {
	logs.Warn(logPrefix+format, v...)
}

// logErrorf 输出 error 级别的直链代理日志
func logErrorf(format string, v ...any) {
	logs.Error(logPrefix+format, v...)
}
