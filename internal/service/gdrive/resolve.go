package gdrive

import (
	"context"
	"errors"
	"strings"
)

// ResolveTarget 换取一条 Google 直链与随行请求头(不取字节)
//
// 供 agent 网络的 download-link 端点使用: master 把直链下发给持 agent_secret
// 的节点, 由节点去请求 Google —— 字节不经过本网关, 也不经过 Emby。
// 缓存与刷新语义与 FetchStream 完全一致(同一份令牌槽与路径缓存,
// 包括 30s 安全余量、singleflight 合并、代次判据), 不另起一套。
//
// 安全约束(调用方必须遵守):
//
//   - headers 是【账号级】Google 凭据, 与令牌槽里的是同一份 map(只读, 不得修改);
//     只允许交给已鉴权的节点端点, 绝不进日志、绝不落盘、绝不下发给普通客户端;
//   - expiresAt 原样透传面板返回的 RFC3339 串(不做二次格式化, 避免丢掉小数秒),
//     面板未给出时为空串, 节点侧按"不可缓存"处理。
func ResolveTarget(ctx context.Context, gdPath string) (url string, headers map[string]string, expiresAt string, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !IsEnabled() {
		return "", nil, "", errors.New("Google Drive 直链未启用")
	}
	if strings.TrimSpace(gdPath) == "" {
		return "", nil, "", errors.New("Drive 路径为空")
	}

	tgt, err := ensureTarget(ctx, gdPath, 0)
	if err != nil {
		return "", nil, "", err
	}
	return tgt.directURL, tgt.headers, tgt.expiresAtRaw, nil
}
