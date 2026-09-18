package streamproxy_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/streamproxy"
)

// withGDriveForTest 在已注入的配置上挂一份 Google Drive 配置
//
// 必须在 proxyConfigWith 之后调用: 后者会整体替换 config.C。
// ProxyGDrive 的取流实现被替换为本地假 Drive 服务端, 因此这里不需要真实凭据。
func withGDriveForTest(t *testing.T, enable bool) {
	t.Helper()
	config.C.GDrive = &config.GDrive{Enable: enable, MountPrefix: "/home/googleDrive"}
}

// proxyGDriveRequest 构造一个模拟客户端发往本项目的请求
func proxyGDriveRequest(t *testing.T, clientRange string) *http.Request {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "http://project.local/stream", nil)
	if clientRange != "" {
		req.Header.Set("Range", clientRange)
	}
	return req
}

// gdriveFetchFrom 构造一个指向本地假下载端点的取流实现
//
// 它替换的是 gdrive.FetchStream 整个函数 —— 面板换直链、带凭据请求下载地址
// 这些都在 gdrive 包内部完成, 因此这里的假实现只需要"按 Range 返回一段响应",
// 不必模仿任何 Google 侧的地址形状。
func gdriveFetchFrom(t *testing.T, base string) func(ctx context.Context, gdPath, clientRange string) (*http.Response, error) {
	t.Helper()

	return func(ctx context.Context, gdPath, clientRange string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/direct-link", nil)
		if err != nil {
			return nil, err
		}
		if clientRange != "" {
			req.Header.Set("Range", clientRange)
		}
		return http.DefaultClient.Do(req)
	}
}

func TestProxyGDrive_StreamsResponseFromDrive(t *testing.T) {
	var gotRange atomic.Value
	drive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange.Store(r.Header.Get("Range"))
		w.Header().Set("Content-Type", "video/x-matroska")
		w.Header().Set("Content-Range", "bytes 0-10/100")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("drive-bytes"))
	}))
	defer drive.Close()

	proxyConfigWith(t, nil)
	withGDriveForTest(t, true)

	restore := streamproxy.SetGDriveFetchForTest(gdriveFetchFrom(t, drive.URL))
	defer restore()

	recorder := httptest.NewRecorder()
	written, err := streamproxy.ProxyGDrive(recorder, proxyGDriveRequest(t, "bytes=0-10"), "/影视库/最新电影/72小时 (2026).mkv")
	if err != nil {
		t.Fatalf("ProxyGDrive() 返回错误: %v", err)
	}
	if !written {
		t.Fatal("ProxyGDrive() written = false, 期望 true")
	}
	if recorder.Code != http.StatusPartialContent {
		t.Errorf("响应码 = %d, want %d", recorder.Code, http.StatusPartialContent)
	}
	if got := recorder.Body.String(); got != "drive-bytes" {
		t.Errorf("客户端收到的响应体 = %q, want drive-bytes", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "video/x-matroska" {
		t.Errorf("Content-Type = %q, want video/x-matroska", got)
	}
	if got := recorder.Header().Get("Content-Range"); got != "bytes 0-10/100" {
		t.Errorf("Content-Range = %q, want bytes 0-10/100", got)
	}
	// 206 缺 Accept-Ranges 时由 relay 内部的 writeResponseHeader 补写:
	// 删掉 relay 的这段实现时本断言会变红
	if got := recorder.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	if got := gotRange.Load(); got != "bytes=0-10" {
		t.Errorf("假 Drive 服务端收到的 Range = %v, want bytes=0-10", got)
	}
}

func TestProxyGDrive_FetchFailureNotWritten(t *testing.T) {
	proxyConfigWith(t, nil)
	withGDriveForTest(t, true)

	restore := streamproxy.SetGDriveFetchForTest(func(ctx context.Context, gdPath, clientRange string) (*http.Response, error) {
		return nil, errors.New("模拟 Drive 取流失败")
	})
	defer restore()

	recorder := httptest.NewRecorder()
	written, err := streamproxy.ProxyGDrive(recorder, proxyGDriveRequest(t, ""), "/影视库/x.mkv")
	if err == nil {
		t.Fatal("取流失败时应返回错误, 以便调用方回源")
	}
	if written {
		t.Fatal("尚未写出任何响应时 written 应为 false")
	}
	if recorder.Body.Len() != 0 {
		t.Errorf("失败时不应写出响应体, 实际写出 %d 字节", recorder.Body.Len())
	}
	if got := recorder.Header().Get("Content-Type"); got != "" {
		t.Errorf("失败时不应回写响应头, 实际 Content-Type = %q", got)
	}
}

func TestProxyGDrive_DisabledOrEmptyPathNoRequest(t *testing.T) {
	tests := []struct {
		name   string
		enable bool
		gdPath string
	}{
		{"未启用", false, "/影视库/x.mkv"},
		{"空路径", true, ""},
		{"仅空白路径", true, "   "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var called atomic.Int64

			proxyConfigWith(t, nil)
			withGDriveForTest(t, tt.enable)

			restore := streamproxy.SetGDriveFetchForTest(func(ctx context.Context, gdPath, clientRange string) (*http.Response, error) {
				called.Add(1)
				return nil, errors.New("不应被调用")
			})
			defer restore()

			recorder := httptest.NewRecorder()
			written, err := streamproxy.ProxyGDrive(recorder, proxyGDriveRequest(t, ""), tt.gdPath)
			if err == nil {
				t.Fatal("未启用或路径为空时应返回错误")
			}
			if written {
				t.Fatal("未写出任何响应时 written 应为 false")
			}
			if got := called.Load(); got != 0 {
				t.Errorf("未启用或路径为空时不应发起任何取流请求, 实际 %d 次", got)
			}
		})
	}
}

func TestProxyGDrive_NilArgs(t *testing.T) {
	proxyConfigWith(t, nil)
	withGDriveForTest(t, true)

	var called atomic.Int64
	restore := streamproxy.SetGDriveFetchForTest(func(ctx context.Context, gdPath, clientRange string) (*http.Response, error) {
		called.Add(1)
		return nil, errors.New("不应被调用")
	})
	defer restore()

	if written, err := streamproxy.ProxyGDrive(nil, proxyGDriveRequest(t, ""), "/x.mkv"); err == nil || written {
		t.Errorf("w 为空时应返回错误且 written=false, 实际 written=%v, err=%v", written, err)
	}
	if written, err := streamproxy.ProxyGDrive(httptest.NewRecorder(), nil, "/x.mkv"); err == nil || written {
		t.Errorf("r 为空时应返回错误且 written=false, 实际 written=%v, err=%v", written, err)
	}
	if got := called.Load(); got != 0 {
		t.Errorf("参数为空时不应发起任何取流请求, 实际 %d 次", got)
	}
}
