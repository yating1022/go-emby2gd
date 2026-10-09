package gdrive

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fractionalExpiresAt 构造一个带小数秒的 RFC3339 串
//
// 面板可能给出 "…T12:34:56.789Z" 这类时间戳; 透传时【不得】二次格式化,
// 否则小数秒会被丢掉, 两侧算出的过期时刻就不再是同一个。
func fractionalExpiresAt(offset time.Duration) string {
	return time.Now().Add(offset).UTC().Format("2006-01-02T15:04:05") + ".123456Z"
}

func TestResolveTarget_ReturnsPanelTarget(t *testing.T) {
	const gdPath = "/影视库/resolve/直链透传.mkv"
	expiresAt := fractionalExpiresAt(time.Hour)

	panel := newFakePanel(t, func(_, _ string) (int, string) {
		return http.StatusOK, successBody("https://drive.example.com/direct", expiresAt)
	})
	withTestConfig(t, panel.url())

	url, headers, gotExpiresAt, err := ResolveTarget(context.Background(), gdPath)
	if err != nil {
		t.Fatalf("换取直链失败: %v", err)
	}
	if url != "https://drive.example.com/direct" {
		t.Errorf("直链 = %q, want https://drive.example.com/direct", url)
	}
	if headers["Authorization"] != testProviderToken {
		t.Errorf("请求头应带上面板下发的凭据, 实际: %v", headers)
	}
	if gotExpiresAt != expiresAt {
		t.Errorf("expires_at 必须原样透传(含小数秒): 实际 %q, want %q", gotExpiresAt, expiresAt)
	}
	if panel.callCount() != 1 {
		t.Errorf("面板调用次数 = %d, want 1", panel.callCount())
	}
	if paths := panel.receivedPaths(); len(paths) != 1 || paths[0] != gdPath {
		t.Errorf("面板收到的 path 参数 = %v, want [%s]", paths, gdPath)
	}
}

func TestResolveTarget_ServesFromCache(t *testing.T) {
	const gdPath = "/影视库/resolve/缓存命中.mkv"
	expiresAt := fractionalExpiresAt(time.Hour)

	panel := newFakePanel(t, func(_, _ string) (int, string) {
		return http.StatusOK, successBody("https://drive.example.com/cached", expiresAt)
	})
	withTestConfig(t, panel.url())

	for i := 0; i < 3; i++ {
		url, _, gotExpiresAt, err := ResolveTarget(context.Background(), gdPath)
		if err != nil {
			t.Fatalf("第 %d 次换取直链失败: %v", i+1, err)
		}
		if url != "https://drive.example.com/cached" || gotExpiresAt != expiresAt {
			t.Fatalf("第 %d 次结果不一致: url=%q expires_at=%q", i+1, url, gotExpiresAt)
		}
	}
	if panel.callCount() != 1 {
		t.Errorf("命中缓存时不应重复调面板, 调用次数 = %d", panel.callCount())
	}

	// 凭据过期后必须重新调面板
	forceTokenExpired()
	if _, _, _, err := ResolveTarget(context.Background(), gdPath); err != nil {
		t.Fatalf("凭据过期后换取直链失败: %v", err)
	}
	if panel.callCount() != 2 {
		t.Errorf("凭据过期后应重新调面板, 调用次数 = %d", panel.callCount())
	}
}

func TestResolveTarget_SharesTokenMap(t *testing.T) {
	const gdPath = "/影视库/resolve/共享凭据.mkv"

	panel := newFakePanel(t, func(_, _ string) (int, string) {
		return http.StatusOK, successBody("https://drive.example.com/shared", fractionalExpiresAt(time.Hour))
	})
	withTestConfig(t, panel.url())

	// 预置令牌与直链, 让缓存直接命中
	shared := map[string]string{"Authorization": "Bearer shared-header"}
	generation := putToken(shared, "2030-01-01T00:00:00Z", time.Now().Add(time.Hour), time.Now())
	putCachedURL(gdPath, "https://drive.example.com/shared", generation)

	_, headers, _, err := ResolveTarget(context.Background(), gdPath)
	if err != nil {
		t.Fatalf("换取直链失败: %v", err)
	}
	// 与令牌槽里是同一份 map: 账号级凭据全局只存一份, 不按请求复制
	if reflect.ValueOf(headers).Pointer() != reflect.ValueOf(shared).Pointer() {
		t.Error("返回的请求头应是令牌槽里的同一份 map(只读共享)")
	}
	if panel.callCount() != 0 {
		t.Errorf("缓存命中时不应调面板, 调用次数 = %d", panel.callCount())
	}
}

func TestResolveTarget_Errors(t *testing.T) {
	t.Run("未启用", func(t *testing.T) {
		withDisabledConfig(t)

		_, _, _, err := ResolveTarget(context.Background(), "/影视库/x.mkv")
		if err == nil || !strings.Contains(err.Error(), "未启用") {
			t.Fatalf("未启用时应返回中文错误, 实际: %v", err)
		}
	})

	t.Run("路径为空", func(t *testing.T) {
		withTestConfig(t, "")

		if _, _, _, err := ResolveTarget(context.Background(), "  "); err == nil {
			t.Fatal("空路径应返回错误")
		}
	})

	t.Run("面板失败", func(t *testing.T) {
		panel := newFakePanel(t, func(_, _ string) (int, string) {
			return http.StatusForbidden, errorBody("PATH_NOT_IN_CACHE", "路径不在缓存中, 请稍后重试")
		})
		withTestConfig(t, panel.url())

		url, headers, expiresAt, err := ResolveTarget(context.Background(), "/影视库/resolve/面板失败.mkv")
		if err == nil {
			t.Fatal("面板失败时应返回错误")
		}
		if !strings.Contains(err.Error(), "路径不在缓存中") {
			t.Errorf("错误消息应透传面板的中文原因, 实际: %q", err.Error())
		}
		if !strings.Contains(err.Error(), "PATH_NOT_IN_CACHE") {
			t.Errorf("错误消息应带上面板错误码, 实际: %q", err.Error())
		}
		if url != "" || headers != nil || expiresAt != "" {
			t.Errorf("失败时不应返回任何部分结果: url=%q headers=%v expires_at=%q", url, headers, expiresAt)
		}
		// 失败信息里绝不能出现面板令牌
		if strings.Contains(err.Error(), testApiToken) {
			t.Errorf("错误消息回显了面板令牌: %q", err.Error())
		}
	})

	t.Run("nil context", func(t *testing.T) {
		const gdPath = "/影视库/resolve/nil-ctx.mkv"
		panel := newFakePanel(t, func(_, _ string) (int, string) {
			return http.StatusOK, successBody("https://drive.example.com/nilctx", fractionalExpiresAt(time.Hour))
		})
		withTestConfig(t, panel.url())

		//nolint:staticcheck // 显式测试 nil context 的兜底分支
		if _, _, _, err := ResolveTarget(nil, gdPath); err != nil {
			t.Fatalf("nil context 应被兜底成 Background, 实际: %v", err)
		}
	})
}

func TestParseExpiresAt_AcceptsFractionalSeconds(t *testing.T) {
	raw := fractionalExpiresAt(time.Hour)
	parsed := parseExpiresAt(raw)
	if parsed.IsZero() {
		t.Fatalf("带小数秒的 RFC3339 串应能解析: %q", raw)
	}
	if !strings.HasSuffix(raw, ".123456Z") {
		t.Fatalf("测试串未带小数秒, 用例失效: %q", raw)
	}
	// 解析结果应保留小数秒(微秒)
	if parsed.Nanosecond() != 123456000 {
		t.Errorf("小数秒解析结果 = %d ns, want 123456000 ns", parsed.Nanosecond())
	}
}
