package gdrive

import (
	"testing"
	"time"
)

// tokenSlotGeneration 读取当前令牌槽的代次
func tokenSlotGeneration() uint64 {
	tokenSlot.mu.RLock()
	defer tokenSlot.mu.RUnlock()
	return tokenSlot.generation
}

// TestLinkCacheSafetyMarginBelowPanelRefreshAhead
//
// ⚠️ 这条不变式是设计的核心约束之一, 单独钉住, 防止以后有人"顺手把余量调大一点":
//
// 面板在真实 expires_at 前 60 秒才发放新令牌。若本项目余量 ≥ 60 秒, 就会出现一段
// "本项目已判缓存失效、面板却仍在发放同一个旧令牌 + 同一个 expires_at" 的病态窗口 ——
// 该窗口内每个 Range 请求都重新打一次面板(各多 150~500ms, 拖进度条会卡),
// "一次 3 小时播放只调 3~4 次面板" 也随之不成立。
func TestLinkCacheSafetyMarginBelowPanelRefreshAhead(t *testing.T) {
	if linkCacheSafetyMargin >= panelTokenRefreshAhead {
		t.Fatalf("linkCacheSafetyMargin (%v) 必须严格小于 panelTokenRefreshAhead (%v), "+
			"否则会造出「缓存已作废但面板仍发同一个旧令牌」的窗口",
			linkCacheSafetyMargin, panelTokenRefreshAhead)
	}
}

func TestTokenDeadline(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name      string
		expiresAt time.Time
		want      time.Time
	}{
		{
			name:      "正常情况: 过期时刻前 30 秒",
			expiresAt: now.Add(time.Hour),
			want:      now.Add(time.Hour - linkCacheSafetyMargin),
		},
		{
			name:      "没有过期信息: 立即过期",
			expiresAt: time.Time{},
			want:      now,
		},
		{
			name:      "剩余寿命不足安全余量: 立即过期",
			expiresAt: now.Add(linkCacheSafetyMargin / 2),
			want:      now,
		},
		{
			name:      "刚好等于安全余量: 立即过期",
			expiresAt: now.Add(linkCacheSafetyMargin),
			want:      now,
		},
		{
			name:      "已经过期: 立即过期",
			expiresAt: now.Add(-time.Hour),
			want:      now,
		},
		{
			name:      "过期时刻离谱地远: 封顶到 maxLinkCacheTTL",
			expiresAt: now.Add(30 * 24 * time.Hour),
			want:      now.Add(maxLinkCacheTTL),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tokenDeadline(tt.expiresAt, now); !got.Equal(tt.want) {
				t.Errorf("tokenDeadline() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestGetToken_UnusableDeadlineNeverServed deadline 落在当下即视为不可用
func TestGetToken_UnusableDeadlineNeverServed(t *testing.T) {
	withTestConfig(t, "")

	putToken(map[string]string{"Authorization": "Bearer x"}, time.Time{}, time.Now())

	if _, _, ok := getToken(); ok {
		t.Error("没有过期信息的条目不应被视为可用")
	}
}

// TestPutToken_AdvancesGenerationEvenWhenExpired 代次必须照常推进
//
// 否则失效重试里"是否已被别的请求刷新过"的判断会失效, 表现为并发 401 打出多次面板调用。
func TestPutToken_AdvancesGenerationEvenWhenExpired(t *testing.T) {
	withTestConfig(t, "")

	first := putToken(map[string]string{"Authorization": "Bearer x"}, time.Time{}, time.Now())
	second := putToken(map[string]string{"Authorization": "Bearer y"}, time.Time{}, time.Now())

	if second <= first {
		t.Errorf("代次应单调递增, first=%d, second=%d", first, second)
	}
	if _, _, ok := getToken(); ok {
		t.Error("立即过期的条目不应被视为可用")
	}
}

// TestTokenSlotIsGlobal 令牌是账号级的, 全局只存一份
//
// 对共享盘内所有文件通用, 因此不按路径复制 N 份 —— 失效点也只有一个。
func TestTokenSlotIsGlobal(t *testing.T) {
	withTestConfig(t, "")

	generation := putToken(map[string]string{"Authorization": "Bearer shared"},
		time.Now().Add(time.Hour), time.Now())

	for _, path := range []string{"/a/x.mkv", "/b/y.mkv", "/c/z.mkv"} {
		putCachedURL(path, "https://drive.example.com"+path, generation)
	}

	for _, path := range []string{"/a/x.mkv", "/b/y.mkv", "/c/z.mkv"} {
		cached, ok := cachedTarget(path, 0)
		if !ok {
			t.Fatalf("路径 %s 应命中缓存", path)
		}
		if got := cached.headers["Authorization"]; got != "Bearer shared" {
			t.Errorf("路径 %s 拿到的凭据 = %q, want Bearer shared", path, got)
		}
	}

	// 三个路径共用同一份凭据对象
	if got := tokenSlotGeneration(); got != 1 {
		t.Errorf("令牌槽代次 = %d, want 1 (三份 URL 不应各自写一次令牌)", got)
	}
}

// TestGetCachedURL 直链缓存的读写
func TestGetCachedURL(t *testing.T) {
	withTestConfig(t, "")

	if _, _, ok := getCachedURL("/a.mkv"); ok {
		t.Error("未写入的路径不应命中")
	}

	putCachedURL("/a.mkv", "https://drive.example.com/a", 7)
	got, generation, ok := getCachedURL("/a.mkv")
	if !ok || got != "https://drive.example.com/a" {
		t.Errorf("getCachedURL() = %q, %v", got, ok)
	}
	// 写入时的代次必须原样带出来: 失效重试靠它判断这条直链是不是重新取过的
	if generation != 7 {
		t.Errorf("getCachedURL() 代次 = %d, want 7", generation)
	}

	// 空值不写: 写入空直链会让后续请求拿到一个必然失败的地址
	putCachedURL("/b.mkv", "", 1)
	if _, _, ok := getCachedURL("/b.mkv"); ok {
		t.Error("空直链不应被写入缓存")
	}
}

// TestCachedTarget_RequiresBothCaches 两个缓存缺一不可
func TestCachedTarget_RequiresBothCaches(t *testing.T) {
	withTestConfig(t, "")

	// 只有令牌, 没有 URL
	putToken(map[string]string{"Authorization": "Bearer x"}, time.Now().Add(time.Hour), time.Now())
	if _, ok := cachedTarget(testPath, 0); ok {
		t.Error("缺少 URL 缓存时不应命中")
	}

	// 只有 URL, 令牌过期
	resetCache()
	putCachedURL(testPath, "https://drive.example.com/x", 1)
	forceTokenExpired()
	if _, ok := cachedTarget(testPath, 0); ok {
		t.Error("令牌不可用时不应命中, 哪怕 URL 还在")
	}
}

// TestCachedTarget_MinGeneration 代次不足时不算命中
//
// 这是失效重试的关键: 拿到的凭据代次不比失败时用的更新, 就必须重新取。
func TestCachedTarget_MinGeneration(t *testing.T) {
	withTestConfig(t, "")

	generation := putToken(map[string]string{"Authorization": "Bearer x"},
		time.Now().Add(time.Hour), time.Now())
	putCachedURL(testPath, "https://drive.example.com/x", generation)

	if _, ok := cachedTarget(testPath, generation); ok {
		t.Error("代次不比自己新时不应命中")
	}
	if _, ok := cachedTarget(testPath, generation-1); !ok {
		t.Error("代次比自己新时应命中")
	}
}

// TestResetCache 测试辅助本身要能真的清干净
func TestResetCache(t *testing.T) {
	withTestConfig(t, "")

	putCachedURL(testPath, "https://drive.example.com/x", 1)
	putToken(map[string]string{"Authorization": "Bearer x"}, time.Now().Add(time.Hour), time.Now())

	resetCache()

	if _, _, ok := getCachedURL(testPath); ok {
		t.Error("resetCache 之后 URL 缓存应为空")
	}
	if _, _, ok := getToken(); ok {
		t.Error("resetCache 之后令牌槽应为空")
	}
}
