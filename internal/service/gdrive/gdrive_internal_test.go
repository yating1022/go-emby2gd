package gdrive

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// newPanelAndDrive 搭一套"假面板 + 假下载端点", 返回两者
//
// 面板总是返回成功响应, 直链指向假下载端点。
func newPanelAndDrive(t *testing.T, expiresAt string) (*fakePanel, *fakeDrive) {
	t.Helper()

	drive := newFakeDrive(t, nil)
	panel := newFakePanel(t, func(_, _ string) (int, string) {
		return http.StatusOK, successBody(drive.url(), expiresAt)
	})
	withTestConfig(t, panel.url())

	return panel, drive
}

func TestFetchStream_Success(t *testing.T) {
	panel, drive := newPanelAndDrive(t, futureRFC3339(time.Hour))

	resp, err := FetchStream(context.Background(), testPath, "bytes=0-1023")
	if err != nil {
		t.Fatalf("FetchStream() 返回错误: %v", err)
	}
	body := drainAndClose(t, resp)

	if body != "media-bytes" {
		t.Errorf("响应体 = %q, want media-bytes", body)
	}
	if panel.callCount() != 1 {
		t.Errorf("面板被调用 %d 次, want 1", panel.callCount())
	}
	if drive.requestCount() != 1 {
		t.Errorf("下载端点被请求 %d 次, want 1", drive.requestCount())
	}
	if got := drive.receivedRanges()[0]; got != "bytes=0-1023" {
		t.Errorf("Range = %q, want bytes=0-1023", got)
	}
}

// TestFetchStream_SecondRequestHitsCache 同一文件连续多次 Range 请求不应重复打面板
func TestFetchStream_SecondRequestHitsCache(t *testing.T) {
	panel, drive := newPanelAndDrive(t, futureRFC3339(time.Hour))

	for i := 0; i < 5; i++ {
		resp, err := FetchStream(context.Background(), testPath, "bytes=0-1023")
		if err != nil {
			t.Fatalf("第 %d 次 FetchStream() 返回错误: %v", i+1, err)
		}
		drainAndClose(t, resp)
	}

	if panel.callCount() != 1 {
		t.Errorf("面板被调用 %d 次, want 1 (缓存应吸收后续请求)", panel.callCount())
	}
	if drive.requestCount() != 5 {
		t.Errorf("下载端点被请求 %d 次, want 5", drive.requestCount())
	}
}

func TestFetchStream_Disabled(t *testing.T) {
	withDisabledConfig(t)

	if _, err := FetchStream(context.Background(), testPath, ""); err == nil {
		t.Error("未启用时应返回错误")
	}
}

func TestFetchStream_EmptyPath(t *testing.T) {
	withTestConfig(t, "")

	if _, err := FetchStream(context.Background(), "   ", ""); err == nil {
		t.Error("路径为空时应返回错误")
	}
}

func TestFetchStream_NilContext(t *testing.T) {
	panel, _ := newPanelAndDrive(t, futureRFC3339(time.Hour))

	resp, err := FetchStream(nil, testPath, "")
	if err != nil {
		t.Fatalf("nil context 应被归一化, 实际错误: %v", err)
	}
	drainAndClose(t, resp)

	if panel.callCount() != 1 {
		t.Errorf("面板被调用 %d 次, want 1", panel.callCount())
	}
}

// TestFetchStream_PanelErrorSkipsDownload 面板失败时不应再去请求下载地址
func TestFetchStream_PanelErrorSkipsDownload(t *testing.T) {
	drive := newFakeDrive(t, nil)
	panel := newFakePanel(t, func(_, _ string) (int, string) {
		return http.StatusNotFound, errorBody("PATH_NOT_IN_CACHE", "路径尚未缓存")
	})
	withTestConfig(t, panel.url())

	_, err := FetchStream(context.Background(), testPath, "")
	if err == nil {
		t.Fatal("面板失败时应返回错误")
	}
	if !strings.Contains(err.Error(), "路径尚未缓存") {
		t.Errorf("错误信息应保留面板中文文案, 实际: %v", err)
	}
	if drive.requestCount() != 0 {
		t.Error("面板失败时不应请求下载地址")
	}
}

// TestFetchStream_TokenIsNotLeakedInLogs
//
// 面板返回的 headers 是账号级凭据, 绝不能出现在日志里。
func TestFetchStream_TokenIsNotLeakedInLogs(t *testing.T) {
	logs := captureLogs(t)
	newPanelAndDrive(t, futureRFC3339(time.Hour))

	resp, err := FetchStream(context.Background(), testPath, "")
	if err != nil {
		t.Fatalf("FetchStream() 返回错误: %v", err)
	}
	drainAndClose(t, resp)

	content := logs.String()
	if strings.Contains(content, testProviderToken) {
		t.Errorf("日志不得包含 Google 凭据, 实际: %s", content)
	}
	if strings.Contains(content, "google-access-token") {
		t.Errorf("日志不得包含 Google 凭据片段, 实际: %s", content)
	}
	// 面板 Token 同样不得出现
	if strings.Contains(content, testApiToken) {
		t.Errorf("日志不得包含面板 Token, 实际: %s", content)
	}
}
