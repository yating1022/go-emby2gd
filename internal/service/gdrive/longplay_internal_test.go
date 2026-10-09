package gdrive

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// 本文件覆盖「长播放不中断」这条核心约束(PRD A10.3 / A16 / A17 / A18)。
//
// 前提: 面板返回的凭据约 1 小时过期, 而一次播放可以持续数小时。
// 令牌到期必须由本项目透明地重新取头解决, 不得表现为播放失败。

// TestFetchStream_RefetchesAfterCacheExpiry 模拟播放跨过令牌有效期边界
//
// 缓存被判定过期后, 下一次取流必须重新调面板拿到新头, 而不是拿旧头去请求 Google。
func TestFetchStream_RefetchesAfterCacheExpiry(t *testing.T) {
	panel, drive := newPanelAndDrive(t, futureRFC3339(time.Hour))

	// 1 第一次取流: 打一次面板, 凭据进入缓存
	resp, err := FetchStream(context.Background(), testPath, "bytes=0-1023")
	if err != nil {
		t.Fatalf("第一次 FetchStream() 返回错误: %v", err)
	}
	drainAndClose(t, resp)

	if panel.callCount() != 1 {
		t.Fatalf("第一次取流后面板调用次数 = %d, want 1", panel.callCount())
	}

	// 2 把令牌槽改成已过期, 等价于播放中途跨过了 1 小时边界;
	//   URL 缓存保持不动 —— 直链是长期有效的, 只有凭据会过期
	forceTokenExpired()

	// 3 第二次取流: 必须重新调面板
	resp, err = FetchStream(context.Background(), testPath, "bytes=2048-4095")
	if err != nil {
		t.Fatalf("过期后 FetchStream() 返回错误: %v", err)
	}
	body := drainAndClose(t, resp)

	if body != "media-bytes" {
		t.Errorf("响应体 = %q, want media-bytes", body)
	}
	if panel.callCount() != 2 {
		t.Errorf("凭据过期后面板调用次数 = %d, want 2 (必须重新取头)", panel.callCount())
	}
	if drive.requestCount() != 2 {
		t.Errorf("下载端点请求次数 = %d, want 2", drive.requestCount())
	}
}

// TestFetchStream_RetriesOnceAfter401 凭据被提前吊销(缓存仍以为有效)时自愈
//
// 这是第 3 层保护: 撞上失效也能重新取一次, 而不是让播放失败。
func TestFetchStream_RetriesOnceAfter401(t *testing.T) {
	drive := newFakeDrive(t, func(r *http.Request) (int, string) {
		// 旧凭据一律 401, 新凭据才放行 —— 模拟 Google 对过期凭据的响应
		if r.Header.Get("Authorization") == "Bearer stale-token" {
			return http.StatusUnauthorized, "Invalid Credentials"
		}
		return http.StatusOK, "media-bytes"
	})
	panel := newFakePanel(t, func(_, _ string) (int, string) {
		return http.StatusOK, successBody(drive.url(), futureRFC3339(time.Hour))
	})
	withTestConfig(t, panel.url())

	// 预置一份"缓存以为有效、Google 却已拒绝"的凭据与直链
	generation := putToken(map[string]string{"Authorization": "Bearer stale-token"}, "", time.Now().Add(time.Hour), time.Now())
	putCachedURL(testPath, drive.url(), generation)

	resp, err := FetchStream(context.Background(), testPath, "")
	if err != nil {
		t.Fatalf("失效重试后仍应成功, 实际错误: %v", err)
	}
	body := drainAndClose(t, resp)

	if body != "media-bytes" {
		t.Errorf("响应体 = %q, want media-bytes", body)
	}
	if panel.callCount() != 1 {
		t.Errorf("面板调用次数 = %d, want 1 (重试时刷新一次)", panel.callCount())
	}
	if drive.requestCount() != 2 {
		t.Errorf("下载端点请求次数 = %d, want 2 (一次失败 + 一次重试)", drive.requestCount())
	}
}

// TestFetchStream_RetriesAtMostOnce 只重试一次, 不做循环重试
func TestFetchStream_RetriesAtMostOnce(t *testing.T) {
	drive := newFakeDrive(t, func(r *http.Request) (int, string) {
		return http.StatusUnauthorized, "Invalid Credentials"
	})
	panel := newFakePanel(t, func(_, _ string) (int, string) {
		return http.StatusOK, successBody(drive.url(), futureRFC3339(time.Hour))
	})
	withTestConfig(t, panel.url())

	generation := putToken(map[string]string{"Authorization": "Bearer stale-token"}, "", time.Now().Add(time.Hour), time.Now())
	putCachedURL(testPath, drive.url(), generation)

	_, err := FetchStream(context.Background(), testPath, "")
	if err == nil {
		t.Fatal("持续 401 时应返回错误, 交由上层回退")
	}

	if drive.requestCount() != 2 {
		t.Errorf("下载端点请求次数 = %d, want 2 (初次 + 一次重试, 不得更多)", drive.requestCount())
	}
	// 初次取缓存不算面板调用, 重试时刷新一次
	if panel.callCount() != 1 {
		t.Errorf("面板调用次数 = %d, want 1", panel.callCount())
	}
}

// TestFetchStream_Concurrent401SharesSingleRefresh
//
// 一波 401 打来时, 多个并发 Range 请求会同时发现凭据失效。
// 若各自重取就会打出 N 次面板调用, 因此刷新必须被合并成一次。
func TestFetchStream_Concurrent401SharesSingleRefresh(t *testing.T) {
	const concurrency = 8

	drive := newFakeDrive(t, func(r *http.Request) (int, string) {
		if r.Header.Get("Authorization") == "Bearer stale-token" {
			return http.StatusUnauthorized, "Invalid Credentials"
		}
		return http.StatusOK, "media-bytes"
	})
	panel := newFakePanel(t, func(_, _ string) (int, string) {
		return http.StatusOK, successBody(drive.url(), futureRFC3339(time.Hour))
	})
	withTestConfig(t, panel.url())

	generation := putToken(map[string]string{"Authorization": "Bearer stale-token"}, "", time.Now().Add(time.Hour), time.Now())
	putCachedURL(testPath, drive.url(), generation)

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		errs   []error
		bodies []string
	)
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()

			resp, err := FetchStream(context.Background(), testPath, "bytes=0-1023")
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}

			var sb []byte
			buf := make([]byte, 64)
			for {
				n, readErr := resp.Body.Read(buf)
				sb = append(sb, buf[:n]...)
				if readErr != nil {
					break
				}
			}
			resp.Body.Close()

			mu.Lock()
			bodies = append(bodies, string(sb))
			mu.Unlock()
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()

	if len(errs) != 0 {
		t.Fatalf("%d 个并发请求失败, 首个错误: %v", len(errs), errs[0])
	}
	if len(bodies) != concurrency {
		t.Fatalf("成功请求数 = %d, want %d", len(bodies), concurrency)
	}
	for _, body := range bodies {
		if body != "media-bytes" {
			t.Errorf("响应体 = %q, want media-bytes", body)
		}
	}
	if panel.callCount() != 1 {
		t.Errorf("面板调用次数 = %d, want 1 (并发刷新必须被合并, 不得打出 %d 次)",
			panel.callCount(), concurrency)
	}
}

// TestFetchStream_UnusableExpiresAtIsNotCached 算不出有效期时不缓存
//
// 宁可每个请求都重新调一次面板, 也不拿一份来历不明的凭据去请求 Google。
func TestFetchStream_UnusableExpiresAtIsNotCached(t *testing.T) {
	panel, _ := newPanelAndDrive(t, "not-a-timestamp")

	for i := 0; i < 3; i++ {
		resp, err := FetchStream(context.Background(), testPath, "")
		if err != nil {
			t.Fatalf("第 %d 次 FetchStream() 返回错误: %v", i+1, err)
		}
		drainAndClose(t, resp)
	}

	if panel.callCount() != 3 {
		t.Errorf("面板调用次数 = %d, want 3 (有效期不可用就不该复用凭据)", panel.callCount())
	}
}

// TestFetchStream_TokenAboutToExpireIsNotCached 剩余寿命不足安全余量时不缓存
func TestFetchStream_TokenAboutToExpireIsNotCached(t *testing.T) {
	// 剩余寿命 10 秒, 小于 30 秒的安全余量
	panel, _ := newPanelAndDrive(t, futureRFC3339(10*time.Second))

	for i := 0; i < 2; i++ {
		resp, err := FetchStream(context.Background(), testPath, "")
		if err != nil {
			t.Fatalf("第 %d 次 FetchStream() 返回错误: %v", i+1, err)
		}
		drainAndClose(t, resp)
	}

	if panel.callCount() != 2 {
		t.Errorf("面板调用次数 = %d, want 2 (余量不足时不复用)", panel.callCount())
	}
}
