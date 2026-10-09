package gdrive

import (
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	// panelTokenRefreshAhead 面板侧提前多久开始发放新令牌
	//
	// 这是服务端行为, 本项目改不了; 在这里记下来只是作为下面那条不变式的依据。
	panelTokenRefreshAhead = 60 * time.Second

	// linkCacheSafetyMargin 本项目缓存提前多久作废
	//
	// ⚠️ 必须【严格小于】panelTokenRefreshAhead, 否则会出现一段病态窗口:
	// 本项目已判缓存失效 -> 重新调面板 -> 面板仍在发放【同一个旧令牌 + 同一个 expires_at】
	// -> 算出的有效期仍然不足 -> 继续不缓存 -> 该窗口内【每个 Range 请求都重新打一次面板】。
	// 余量取 5 分钟时窗口有 240 秒, 每个 Range 请求多 150~500ms, 拖进度条会卡。
	//
	// 也不能取 0: 那等于一直用到 expires_at 那一瞬间, 请求可能正好卡在过期边界上发出。
	linkCacheSafetyMargin = 30 * time.Second

	// maxLinkCacheTTL 缓存时长上限
	//
	// 防止面板给出离谱的远期 expires_at 之后, 同一份凭据被无限期复用。
	maxLinkCacheTTL = time.Hour
)

// tokenSlot 全局令牌槽
//
// 面板返回的 Authorization 是【账号级】凭据, 对共享盘内所有文件通用,
// 因此全局只存一份, 不按路径复制 N 份 —— 顺带让失效点也只有一个。
var tokenSlot struct {
	mu         sync.RWMutex
	entry      tokenEntry
	generation uint64
}

// urlCache 路径 -> 面板直链
//
// 直链长期有效, 因此条目不设自身过期时间, 只在凭据/直链失效时由重试逻辑刷新;
// 只存字符串, 不随媒体体积增长, 进程重启即清空。
var urlCache sync.Map

// refreshGroup 合并同一路径的并发刷新
//
// 一波 401 会让多个并发 Range 请求同时发现凭据失效,
// 若各自重取就会打出 N 次面板调用; 按路径合并后只真正刷新一次。
var refreshGroup singleflight.Group

// getToken 读取当前可用的令牌
//
// 过期条目在读时直接判为不可用(惰性清理), 不引入后台清理协程。
// expiresAtRaw 是面板给出的原始 RFC3339 串(原样透传给 agent, 见 ResolveTarget)。
func getToken() (headers map[string]string, expiresAtRaw string, generation uint64, ok bool) {
	tokenSlot.mu.RLock()
	defer tokenSlot.mu.RUnlock()

	entry := tokenSlot.entry
	if entry.headers == nil || !time.Now().Before(entry.deadline) {
		return nil, "", tokenSlot.generation, false
	}
	return entry.headers, entry.expiresAtRaw, tokenSlot.generation, true
}

// putToken 写入令牌槽并推进代次
//
// expiresAtRaw 是面板响应里的原始串, 只用于向下游(agent)原样透传;
// 本包不做任何加工, 也不据它做判断(判断一律用解析后的 expiresAt)。
//
// 即使 expires_at 不可用也照样写入: 本次请求已经拿到凭据, 用它完成即可。
// "能不能复用"由 deadline 决定 —— 算不出有效期的写入 deadline 即当下,
// 读侧立刻判为过期, 效果等价于没缓存; 但代次照常推进,
// 于是失效重试里"是否已被别的请求刷新过"的判断依然准确。
func putToken(headers map[string]string, expiresAtRaw string, expiresAt, now time.Time) uint64 {
	tokenSlot.mu.Lock()
	defer tokenSlot.mu.Unlock()

	tokenSlot.generation++
	tokenSlot.entry = tokenEntry{
		headers:      headers,
		expiresAtRaw: expiresAtRaw,
		deadline:     tokenDeadline(expiresAt, now),
	}
	return tokenSlot.generation
}

// tokenDeadline 由面板给出的过期时刻算出本项目复用到什么时刻为止
//
// 没有可用的过期信息时返回 now, 即"立即过期" —— 宁可每个请求都重新调一次面板,
// 也不拿一份来历不明的凭据去请求 Google。
func tokenDeadline(expiresAt, now time.Time) time.Time {
	if expiresAt.IsZero() {
		return now
	}

	// time.Time 没有实现有序比较(只有 Before/After), 因此这里不能用内置的 min()
	deadline := expiresAt.Add(-linkCacheSafetyMargin)
	// 封顶: 面板给出离谱的远期时间时, 不让一份凭据被无限期复用
	if limit := now.Add(maxLinkCacheTTL); deadline.After(limit) {
		deadline = limit
	}
	// 剩余寿命还不够扣掉安全余量时同样立即过期
	if !deadline.After(now) {
		return now
	}
	return deadline
}

// getCachedURL 读取缓存的直链, 同时返回写入它时的令牌代次
func getCachedURL(gdPath string) (directURL string, generation uint64, ok bool) {
	value, loaded := urlCache.Load(gdPath)
	if !loaded {
		return "", 0, false
	}

	entry, isEntry := value.(urlEntry)
	if !isEntry || entry.directURL == "" {
		urlCache.Delete(gdPath)
		return "", 0, false
	}
	return entry.directURL, entry.generation, true
}

// putCachedURL 缓存直链
//
// generation 传写入本次直链时的令牌代次, 供失效重试判断新鲜度。
func putCachedURL(gdPath, directURL string, generation uint64) {
	if gdPath == "" || directURL == "" {
		return
	}
	urlCache.Store(gdPath, urlEntry{directURL: directURL, generation: generation})
}

// cachedTarget 两个缓存都命中时返回可用目标, 不发起任何请求
//
// minGeneration 非 0 时要求**令牌与直链**的代次都比它更新: 这是失效重试的判据。
//
// 必须两边都判, 只判令牌是不够的 —— 令牌是账号级的、全局一份, 直链是按路径的、
// 可能单独失效。若只判令牌, 那么在两次尝试之间恰好有别的请求刷新过令牌时,
// 代次就"看起来更新了", 重试会直接复用本路径上那份**失效的旧直链**,
// 拿着同一个地址再打一次, 白白回退到回源。
//
// 反过来, 两个都判也不会让并发的同路径请求各打一次面板: 首个刷新会同时推进
// 令牌代次与这条路径的直链代次, 其余请求两个判据都能通过, 直接复用同一结果。
func cachedTarget(gdPath string, minGeneration uint64) (*target, bool) {
	headers, expiresAtRaw, generation, ok := getToken()
	if !ok || generation <= minGeneration {
		return nil, false
	}

	directURL, urlGeneration, ok := getCachedURL(gdPath)
	if !ok || urlGeneration <= minGeneration {
		return nil, false
	}

	return &target{
		directURL:    directURL,
		headers:      headers,
		expiresAtRaw: expiresAtRaw,
		generation:   generation,
	}, true
}

// ensureTarget 取回一个可用的取流目标
//
// 两个缓存都命中就直接返回; 否则调面板刷新。
// 同一路径的并发刷新由 singleflight 合并成一次面板调用。
func ensureTarget(ctx context.Context, gdPath string, minGeneration uint64) (*target, error) {
	if cached, ok := cachedTarget(gdPath, minGeneration); ok {
		return cached, nil
	}

	value, err, _ := refreshGroup.Do(gdPath, func() (any, error) {
		// 二次检查: 排队等待期间可能已被先到的请求刷新过
		if cached, ok := cachedTarget(gdPath, minGeneration); ok {
			return cached, nil
		}

		// 刷新结果会被所有等待方复用, 因此不能绑在任何一个发起方的取消信号上:
		// 否则发起方一断开, 等待方会一起拿到 context.Canceled 并回退到回源,
		// 而"不再绕道 Emby"正是本功能存在的意义。
		// 剥掉取消信号之后没有任何人能终止它, 所以必须自带超时上限。
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), panelRequestTimeout)
		defer cancel()

		link, err := fetchDirectLink(refreshCtx, gdPath)
		if err != nil {
			return nil, err
		}

		now := time.Now()
		generation := putToken(link.Headers, link.ExpiresAt, parseExpiresAt(link.ExpiresAt), now)
		putCachedURL(gdPath, link.URL, generation)

		logInfof("已换取直链: %s, expires_at=%s", gdPath, link.ExpiresAt)
		return &target{
			directURL:    link.URL,
			headers:      link.Headers,
			expiresAtRaw: link.ExpiresAt,
			generation:   generation,
		}, nil
	})
	if err != nil {
		return nil, err
	}

	cached, ok := value.(*target)
	if !ok || cached == nil {
		return nil, errors.New("刷新直链失败")
	}
	return cached, nil
}

// resetCache 清空全部缓存, 仅供测试使用
func resetCache() {
	tokenSlot.mu.Lock()
	tokenSlot.entry = tokenEntry{}
	tokenSlot.generation = 0
	tokenSlot.mu.Unlock()

	urlCache.Range(func(key, _ any) bool {
		urlCache.Delete(key)
		return true
	})
}
