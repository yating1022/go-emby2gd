package gdrive

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestFetchDirect_ForwardsHeadersAndRange 面板给的请求头与客户端的 Range 都要原样发出
func TestFetchDirect_ForwardsHeadersAndRange(t *testing.T) {
	drive := newFakeDrive(t, nil)

	tgt := &target{
		directURL: drive.url(),
		headers:   map[string]string{"Authorization": testProviderToken, "X-Extra": "v"},
	}

	resp, err := fetchDirect(context.Background(), tgt, "bytes=1024-2047")
	if err != nil {
		t.Fatalf("fetchDirect() 返回错误: %v", err)
	}
	defer resp.Body.Close()

	headers := drive.receivedHeaders()
	if len(headers) != 1 {
		t.Fatalf("下载端点被请求 %d 次, want 1", len(headers))
	}
	if got := headers[0].Get("Authorization"); got != testProviderToken {
		t.Errorf("Authorization = %q, want %q", got, testProviderToken)
	}
	if got := headers[0].Get("X-Extra"); got != "v" {
		t.Errorf("X-Extra = %q, want v", got)
	}
	if got := headers[0].Get("Range"); got != "bytes=1024-2047" {
		t.Errorf("Range = %q, want bytes=1024-2047", got)
	}
	if got := headers[0].Get("Accept-Encoding"); got != "identity" {
		t.Errorf("Accept-Encoding = %q, want identity", got)
	}
}

// TestFetchDirect_NoRangeWhenClientSendsNone 客户端没带 Range 时不应凭空造一个
func TestFetchDirect_NoRangeWhenClientSendsNone(t *testing.T) {
	drive := newFakeDrive(t, nil)

	tgt := &target{directURL: drive.url(), headers: map[string]string{"Authorization": testProviderToken}}
	resp, err := fetchDirect(context.Background(), tgt, "")
	if err != nil {
		t.Fatalf("fetchDirect() 返回错误: %v", err)
	}
	defer resp.Body.Close()

	if ranges := drive.receivedRanges(); len(ranges) != 1 || ranges[0] != "" {
		t.Errorf("未带 Range 时不应转发 Range 头, 实际: %q", ranges)
	}
}

// TestFetchDirect_DoesNotMutateSharedHeaders
//
// fetchDirect 必须复制一份请求头再交给底层: 底层会往传入的 map 里补字段,
// 直接交出缓存里那份会让并发请求同时写同一个 map。
func TestFetchDirect_DoesNotMutateSharedHeaders(t *testing.T) {
	drive := newFakeDrive(t, nil)

	shared := map[string]string{"Authorization": testProviderToken}
	tgt := &target{directURL: drive.url(), headers: shared}

	resp, err := fetchDirect(context.Background(), tgt, "bytes=0-")
	if err != nil {
		t.Fatalf("fetchDirect() 返回错误: %v", err)
	}
	defer resp.Body.Close()

	if len(shared) != 1 || shared["Authorization"] != testProviderToken {
		t.Errorf("共享的 headers map 被改动了: %v", shared)
	}
	if _, ok := shared["Range"]; ok {
		t.Error("Range 不应被写进共享的 headers map")
	}
}

// TestFetchDirect_AcceptsPartialContent 206 是正常的拖动进度响应
func TestFetchDirect_AcceptsPartialContent(t *testing.T) {
	drive := newFakeDrive(t, func(r *http.Request) (int, string) {
		return http.StatusPartialContent, "partial-bytes"
	})

	tgt := &target{directURL: drive.url(), headers: map[string]string{"Authorization": testProviderToken}}
	resp, err := fetchDirect(context.Background(), tgt, "bytes=0-9")
	if err != nil {
		t.Fatalf("206 应被接受, 实际: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("status = %d, want 206", resp.StatusCode)
	}
}

// TestFetchDirect_FailuresCarryStatusCode 失败必须带状态码, 否则上层无法判断该不该重试
func TestFetchDirect_FailuresCarryStatusCode(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusGone,
		http.StatusInternalServerError,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			drive := newFakeDrive(t, func(r *http.Request) (int, string) {
				return status, "upstream failure"
			})

			tgt := &target{directURL: drive.url(), headers: map[string]string{"Authorization": testProviderToken}}
			resp, err := fetchDirect(context.Background(), tgt, "")
			if err == nil {
				resp.Body.Close()
				t.Fatalf("HTTP %d 应返回错误", status)
			}

			var failure *fetchError
			if !errors.As(err, &failure) {
				t.Fatalf("错误类型应为 *fetchError, 实际: %T", err)
			}
			if failure.statusCode != status {
				t.Errorf("statusCode = %d, want %d", failure.statusCode, status)
			}
			// 上游正文内容不受本项目控制, 不进日志也不进错误信息
			if strings.Contains(err.Error(), "upstream failure") {
				t.Errorf("错误信息不应携带上游响应体, 实际: %v", err)
			}
		})
	}
}

// TestFetchDirect_EmptyTarget 空目标不应发起任何请求
func TestFetchDirect_EmptyTarget(t *testing.T) {
	drive := newFakeDrive(t, nil)

	if _, err := fetchDirect(context.Background(), nil, ""); err == nil {
		t.Error("nil target 应返回错误")
	}
	if _, err := fetchDirect(context.Background(), &target{directURL: "  "}, ""); err == nil {
		t.Error("空地址应返回错误")
	}
	if drive.requestCount() != 0 {
		t.Error("空目标不应发起任何上游请求")
	}
}

func TestRetryableStatus(t *testing.T) {
	tests := map[int]bool{
		http.StatusUnauthorized:        true,
		http.StatusForbidden:           true,
		http.StatusNotFound:            true,
		http.StatusGone:                true,
		http.StatusOK:                  false,
		http.StatusPartialContent:      false,
		http.StatusInternalServerError: false,
		http.StatusBadRequest:          false,
		http.StatusTooManyRequests:     false,
		http.StatusFound:               false,
	}

	for status, want := range tests {
		if got := retryableStatus(status); got != want {
			t.Errorf("retryableStatus(%d) = %v, want %v", status, got, want)
		}
	}
}

// TestPanelRetryStatusCodes_IncludesUnauthorized
//
// Google API 对过期/无效凭据返回 401 而不是 403。漏掉 401 的话,
// "令牌过期"这个最该自愈的情况反而会掉到回退分支上。
func TestPanelRetryStatusCodes_IncludesUnauthorized(t *testing.T) {
	if !retryableStatus(http.StatusUnauthorized) {
		t.Fatal("重试集必须包含 401 (Google 对过期凭据返回的是 401)")
	}
}
