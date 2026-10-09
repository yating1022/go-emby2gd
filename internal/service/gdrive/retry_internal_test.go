package gdrive

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestFetchStream_RetryRefreshesStaleDirectLink
//
// 令牌有效但**直链本身**失效(404)时, 重试必须重新向面板取直链。
//
// 反例(本次回归的成因): 重试的判据只看令牌代次。若在两次尝试之间, 别的并发请求
// 恰好刷新过令牌, 代次就"看起来更新了", 于是重试直接复用该路径上那份**失效的旧直链**,
// 拿着同一个地址再打一次 —— 仍然 404, 白白回退到回源。
//
// 直链与令牌是两件事: 令牌是账号级的、全局一份; 直链是按路径的、可能单独失效。
// 因此缓存命中必须同时要求"这条路径的直链"也是在失败之后重新取过的。
func TestFetchStream_RetryRefreshesStaleDirectLink(t *testing.T) {
	fresh := newFakeDrive(t, nil)

	stale := newFakeDrive(t, func(r *http.Request) (int, string) {
		// 在第一次请求期间模拟"另一个并发请求刷新了令牌":
		// 令牌代次被推进, 但这条路径的直链没有跟着变。
		putToken(map[string]string{"Authorization": "Bearer concurrent-refresh"}, "", time.Now().Add(time.Hour), time.Now())
		return http.StatusNotFound, "stale direct link"
	})

	panel := newFakePanel(t, func(_, _ string) (int, string) {
		return http.StatusOK, successBody(fresh.url(), futureRFC3339(time.Hour))
	})
	withTestConfig(t, panel.url())

	// 预置: 令牌看起来有效, 但这条路径缓存的是失效直链
	generation := putToken(map[string]string{"Authorization": "Bearer stale-token"}, "", time.Now().Add(time.Hour), time.Now())
	putCachedURL(testPath, stale.url(), generation)

	resp, err := FetchStream(context.Background(), testPath, "")
	if err != nil {
		t.Fatalf("直链失效后重试应重新取直链并成功, 实际错误: %v", err)
	}
	body := drainAndClose(t, resp)

	if body != "media-bytes" {
		t.Errorf("响应体 = %q, want media-bytes", body)
	}
	if panel.callCount() != 1 {
		t.Errorf("面板调用次数 = %d, want 1 (重试必须重新取直链)", panel.callCount())
	}
	if stale.requestCount() != 1 {
		t.Errorf("失效直链被请求 %d 次, want 1 (只应尝试一次)", stale.requestCount())
	}
	if fresh.requestCount() != 1 {
		t.Errorf("新直链被请求 %d 次, want 1", fresh.requestCount())
	}
}

// TestFetchStream_SecondRequestReusesURLAfterUnrelatedTokenRefresh
//
// 上一条修复不能走过头: 令牌被**无关的**请求刷新之后, 已有的直链仍然可用,
// 不该因此被丢掉、也不该再打一次面板。
//
// 直链是长期有效的, 只有令牌会到期; 两者的新鲜度必须分开判断。
func TestFetchStream_SecondRequestReusesURLAfterUnrelatedTokenRefresh(t *testing.T) {
	panel, drive := newPanelAndDrive(t, futureRFC3339(time.Hour))

	resp, err := FetchStream(context.Background(), testPath, "")
	if err != nil {
		t.Fatalf("第一次 FetchStream() 返回错误: %v", err)
	}
	drainAndClose(t, resp)

	if panel.callCount() != 1 {
		t.Fatalf("第一次取流后面板调用次数 = %d, want 1", panel.callCount())
	}

	// 别的路径的刷新推进了全局令牌槽 —— 本路径的直链并没有失效
	putToken(map[string]string{"Authorization": "Bearer refreshed-elsewhere"}, "", time.Now().Add(time.Hour), time.Now())

	resp, err = FetchStream(context.Background(), testPath, "")
	if err != nil {
		t.Fatalf("第二次 FetchStream() 返回错误: %v", err)
	}
	drainAndClose(t, resp)

	if panel.callCount() != 1 {
		t.Errorf("面板调用次数 = %d, want 1 (直链仍有效, 不该因令牌变动而重取)", panel.callCount())
	}
	if drive.requestCount() != 2 {
		t.Errorf("下载端点请求次数 = %d, want 2", drive.requestCount())
	}
}
