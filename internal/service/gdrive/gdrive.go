// Package gdrive 通过 GD 管理面板换取 Google Drive 直链并取流
//
// 本包把"一个 Drive 内的逻辑路径"变成"一条可读取的媒体响应":
// 调面板 /api/dl 换直链与请求头、带缓存地复用它们、失效时重新换一次。
// 字节流的转发、响应头回写、并发控制与回退语义由
// internal/service/streamproxy 负责, 本包不重复实现。
//
// 本包【不直接调用任何 Google API】: 路径换直链这一步完全由面板完成,
// 因此这里既没有 OAuth 授权流程, 也没有路径逐层解析。
//
// 安全约束: 面板返回的 headers 是【账号级】Google 凭据, 只用于本项目与 Google 之间 ——
// 绝不回写给客户端、绝不写进日志、绝不落盘。
//
// 依赖方向: gdrive -> config / util/https / util/logs。
// 本包不依赖 gin, 也不依赖 internal/web/...。
package gdrive

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
)

// panelRetryStatusCodes 判定"凭据或直链已失效、值得重新取一次"的状态码
//
// 刻意【不复用】 emby.strm.proxy.retry-status-codes: 那组码描述的是网关直链失效,
// 与用户怎么配网关绑定; 而这一组是 Google API 的性质, 两条链路本就不该互相牵连。
// 复用会造成"改了 strm-proxy 配置却悄悄改变了 GD 面板行为"的隐蔽耦合。
//
// 401 必须在列: Google API 对过期/无效凭据返回 401(Invalid Credentials) 而不是 403。
// 漏掉它, "令牌过期"这个最该自愈的情况反而会掉到回退分支上。
var panelRetryStatusCodes = []int{
	http.StatusUnauthorized,
	http.StatusForbidden,
	http.StatusNotFound,
	http.StatusGone,
}

// gdriveConfig 获取当前的 GD 面板配置
func gdriveConfig() *config.GDrive {
	if config.C == nil {
		return nil
	}
	return config.C.GDrive
}

// IsEnabled GD 面板直链是否启用
func IsEnabled() bool {
	return gdriveConfig().IsEnabled()
}

// FetchStream 换取直链并请求媒体数据
//
// gdPath 是团队盘内的文件路径(已去掉挂载前缀), 由调用方通过 MatchMountPath 解析得到。
// 返回的 http.Response 已带上本次请求的 Range, 调用方负责关闭 Body。
//
// 任何一步失败都返回错误, 由调用方回退到回源处理 ——
// 回退是硬性要求: 新数据源出问题不得让播放整体不可用。
func FetchStream(ctx context.Context, gdPath string, clientRange string) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !IsEnabled() {
		return nil, errors.New("Google Drive 直链未启用")
	}
	if strings.TrimSpace(gdPath) == "" {
		return nil, errors.New("Drive 路径为空")
	}

	// 最多重试一次: 第一次失败且判定为"凭据或直链已失效"时刷新后重试,
	// 第二次仍失败就交给上层回退, 不做循环重试, 也不做退避。
	//
	// 重试必须发生在写出任何响应之前 —— streamproxy.ProxyGDrive 拿到 resp 才回写,
	// 因此这里仍然保有完整的回退能力。
	var minGeneration uint64
	for attempt := 0; ; attempt++ {
		tgt, err := ensureTarget(ctx, gdPath, minGeneration)
		if err != nil {
			return nil, err
		}

		resp, err := fetchDirect(ctx, tgt, clientRange)
		if err == nil {
			return resp, nil
		}

		var failure *fetchError
		if attempt == 0 && errors.As(err, &failure) && retryableStatus(failure.statusCode) {
			logWarnf("直链失效 (HTTP %d), 刷新后重试一次: %s", failure.statusCode, gdPath)
			// 要求刷新到比本次使用的代次更新的凭据: 若并发的其它请求已经刷新过,
			// 这里会直接复用那一份, 不会再打一次面板
			minGeneration = tgt.generation
			continue
		}

		return nil, err
	}
}
