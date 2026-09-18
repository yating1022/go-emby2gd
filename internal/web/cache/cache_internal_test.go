package cache

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/gin-gonic/gin"
)

// newTestRespCacheWriter 构造一个测试用的缓冲响应器
//
// 返回包装后的响应器以及底层记录器, 便于断言客户端实际收到的字节
func newTestRespCacheWriter(t *testing.T) (*respCacheWriter, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	return &respCacheWriter{body: &bytes.Buffer{}, ResponseWriter: c.Writer}, recorder
}

// TestRespCacheWriter_BufferWithinLimit 验证未超过缓冲上限时的行为
//
// 未超限时, 响应体应同步缓存进内存, 且原样写回客户端
func TestRespCacheWriter_BufferWithinLimit(t *testing.T) {
	tests := []struct {
		name   string
		writes [][]byte
	}{
		{
			name:   "单次写入",
			writes: [][]byte{[]byte("hello")},
		},
		{
			name:   "多次写入",
			writes: [][]byte{[]byte("ab"), []byte("cd"), []byte("ef")},
		},
		{
			name:   "空写入",
			writes: [][]byte{{}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rcw, recorder := newTestRespCacheWriter(t)

			want := make([]byte, 0)
			for _, b := range tt.writes {
				n, err := rcw.Write(b)
				if err != nil {
					t.Errorf("Write() err = %v, want nil", err)
				}
				if n != len(b) {
					t.Errorf("Write() n = %d, want %d", n, len(b))
				}
				want = append(want, b...)
			}

			if rcw.disabled {
				t.Errorf("disabled = true, want false")
			}
			if !bytes.Equal(rcw.body.Bytes(), want) {
				t.Errorf("缓存内容 = %q, want %q", rcw.body.Bytes(), want)
			}
			if !bytes.Equal(recorder.Body.Bytes(), want) {
				t.Errorf("客户端收到 = %q, want %q", recorder.Body.Bytes(), want)
			}
		})
	}
}

// TestRespCacheWriter_ExceedLimitDisablesCache 验证超过缓冲上限后的行为
//
// 超过上限时应关闭缓冲、丢弃已缓存内容, 但所有字节仍必须完整写回客户端
func TestRespCacheWriter_ExceedLimitDisablesCache(t *testing.T) {
	chunk := make([]byte, MaxBufferedRespSize/2)

	tests := []struct {
		name         string
		writes       [][]byte
		wantDisabled bool
	}{
		{
			name:         "单次写入超过上限",
			writes:       [][]byte{make([]byte, MaxBufferedRespSize+1)},
			wantDisabled: true,
		},
		{
			name: "累计写入超过上限",
			writes: [][]byte{
				chunk,
				chunk,
				[]byte("多出的一字节"),
			},
			wantDisabled: true,
		},
		{
			name: "恰好达到上限不算超限",
			writes: [][]byte{
				chunk,
				make([]byte, MaxBufferedRespSize-MaxBufferedRespSize/2),
			},
			wantDisabled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rcw, recorder := newTestRespCacheWriter(t)

			want := make([]byte, 0)
			for _, b := range tt.writes {
				n, err := rcw.Write(b)
				if err != nil {
					t.Errorf("Write() err = %v, want nil", err)
				}
				if n != len(b) {
					t.Errorf("Write() n = %d, want %d", n, len(b))
				}
				want = append(want, b...)
			}

			// 客户端收到的响应体必须完整无损
			if !bytes.Equal(recorder.Body.Bytes(), want) {
				t.Errorf("客户端收到 %d 字节, want %d 字节", recorder.Body.Len(), len(want))
			}

			if rcw.disabled != tt.wantDisabled {
				t.Errorf("disabled = %v, want %v", rcw.disabled, tt.wantDisabled)
			}
			if rcw.disabled && rcw.body.Len() != 0 {
				t.Errorf("禁用缓冲后 body.Len() = %d, want 0", rcw.body.Len())
			}
		})
	}
}

// TestRespCacheWriter_AlreadyDisabled 验证已关闭缓冲时的行为
//
// 已关闭缓冲后连续写入, 不应再缓存任何内容, 但字节仍需完整透传
func TestRespCacheWriter_AlreadyDisabled(t *testing.T) {
	rcw, recorder := newTestRespCacheWriter(t)
	rcw.disabled = true

	writes := [][]byte{[]byte("first"), []byte("second"), []byte("third")}
	want := make([]byte, 0)
	for _, b := range writes {
		n, err := rcw.Write(b)
		if err != nil {
			t.Errorf("Write() err = %v, want nil", err)
		}
		if n != len(b) {
			t.Errorf("Write() n = %d, want %d", n, len(b))
		}
		want = append(want, b...)
	}

	if !rcw.disabled {
		t.Errorf("disabled = false, want true")
	}
	if rcw.body.Len() != 0 {
		t.Errorf("body.Len() = %d, want 0", rcw.body.Len())
	}
	if !bytes.Equal(recorder.Body.Bytes(), want) {
		t.Errorf("客户端收到 = %q, want %q", recorder.Body.Bytes(), want)
	}
}

// discardResponseWriter 丢弃所有响应字节的响应器
//
// 用于观测"写响应"本身的内存占用, 避免记录器把响应体缓存起来干扰观测
type discardResponseWriter struct {
	header http.Header
}

func (d *discardResponseWriter) Header() http.Header {
	if d.header == nil {
		d.header = http.Header{}
	}
	return d.header
}

func (d *discardResponseWriter) Write(b []byte) (int, error) { return len(b), nil }

func (d *discardResponseWriter) WriteHeader(int) {}

// TestRequestCacher_StreamRouteHeapIndependentOfBodySize 验证字节流路由的堆内存增量与响应体大小无关
//
// 这是本任务的核心目标: 部署机器内存不足 1GB, 代理数 GB 的媒体流时不得把响应体缓冲进内存
func TestRequestCacher_StreamRouteHeapIndependentOfBodySize(t *testing.T) {
	const bodySize = 64 << 20 // 64MB

	tests := []struct {
		name string
		uri  string
	}{
		{name: "播放流", uri: "/emby/Videos/123/stream"},
		{name: "条目下载", uri: "/emby/Items/123/Download"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)

			// 复用同一块缓冲, 保证 handler 自身不随响应体大小分配内存
			chunk := make([]byte, 32*1024)

			engine := gin.New()
			engine.Use(CacheableRouteMarker(), RequestCacher())
			engine.Any("/*vars", func(c *gin.Context) {
				for remaining := bodySize; remaining > 0; remaining -= len(chunk) {
					if _, err := c.Writer.Write(chunk); err != nil {
						t.Errorf("Write() err = %v, want nil", err)
						return
					}
				}
			})

			req := httptest.NewRequest(http.MethodGet, tt.uri, nil)

			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)

			engine.ServeHTTP(&discardResponseWriter{}, req)

			runtime.ReadMemStats(&after)

			// 阈值取 8MB, 远小于 64MB 的响应体:
			// 只要响应体被整体缓冲, 增量就会明显超过该阈值
			const toleratedGrowth = 8 << 20
			growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
			if growth > toleratedGrowth {
				t.Errorf("写入 %d 字节响应体后堆内存增长 %d 字节, 超过容忍上限 %d 字节", bodySize, growth, toleratedGrowth)
			}
		})
	}
}

// TestCacheableRouteMarker_StreamRoutesNotCacheable 验证字节流类路由已从缓存白名单移除
//
// 媒体/文件字节流响应可能高达数 GB, 必须走直通写回, 不得进入内存缓冲
func TestCacheableRouteMarker_StreamRoutesNotCacheable(t *testing.T) {
	tests := []struct {
		name       string
		uri        string
		wantCached bool
	}{
		{
			name:       "播放流",
			uri:        "/emby/Videos/123/stream",
			wantCached: false,
		},
		{
			name:       "通用播放流",
			uri:        "/emby/Videos/123/universal.mp4",
			wantCached: false,
		},
		{
			name:       "音频流",
			uri:        "/emby/Audio/456/stream",
			wantCached: false,
		},
		{
			name:       "条目下载",
			uri:        "/emby/Items/123/Download",
			wantCached: false,
		},
		{
			name:       "同步任务下载",
			uri:        "/emby/Sync/JobItems/123/File",
			wantCached: false,
		},
		{
			name:       "播放信息",
			uri:        "/emby/Items/123/PlaybackInfo",
			wantCached: true,
		},
		{
			name:       "视频字幕",
			uri:        "/emby/Videos/123/subtitles",
			wantCached: true,
		},
		{
			name:       "随机条目列表",
			uri:        "/emby/Users/1/Items/with_limit?SortBy=Random",
			wantCached: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)

			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, tt.uri, nil)

			CacheableRouteMarker()(c)

			gotCached := c.Writer.Header().Get(HeaderKeyExpired) != "-1"
			if gotCached != tt.wantCached {
				t.Errorf("可缓存 = %v, want %v", gotCached, tt.wantCached)
			}
		})
	}
}

// TestRequestCacher_StreamRouteNotWrapped 验证字节流类路由不会被换成缓冲响应器
func TestRequestCacher_StreamRouteNotWrapped(t *testing.T) {
	tests := []struct {
		name string
		uri  string
	}{
		{name: "播放流", uri: "/emby/Videos/123/stream"},
		{name: "条目下载", uri: "/emby/Items/123/Download"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)

			wrapped := false
			engine := gin.New()
			engine.Use(CacheableRouteMarker(), RequestCacher())
			engine.Any("/*vars", func(c *gin.Context) {
				_, wrapped = c.Writer.(*respCacheWriter)
				c.String(http.StatusOK, "ok")
			})

			engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, tt.uri, nil))

			if wrapped {
				t.Errorf("c.Writer 被包装为缓冲响应器, want 原始响应器")
			}
		})
	}
}

// TestRequestCacher_OversizeResponseNotCached 验证超过缓冲上限的响应不会被写入缓存
//
// 覆盖链路: 命中白名单的路由 + 响应体超过 MaxBufferedRespSize -> 放弃缓存并直通写回。
//
// 断言方式: 同一请求连发两次。若第一次的超限响应被写入了缓存, 第二次会直接命中缓存
// 而不进入处理器, 因此 "处理器被调用两次" 即可证明超限响应没有被缓存。
//
// 覆盖边界: 本用例锁定的是"超限响应不会被写入缓存"这一外部可观测行为,
// 无法区分 cache.go 中的 `customWriter.disabled` 守卫与 putCache 的空响应体提前返回
// —— 超限时 customWriter.body 已被清空, putCache 收到 nil 响应体后本来就会直接返回,
// 两者对缓存状态的影响完全一致 (详见本次审查报告)。
func TestRequestCacher_OversizeResponseNotCached(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 白名单路由 (Reg_PlaybackInfo), 保证请求确实会走到缓冲响应器
	// item id 为本用例专属, 避免与其他用例的缓存 key 相互干扰
	const uri = "/emby/Items/2026/PlaybackInfo"

	// 分块写入, 避免处理器自己额外分配一整块超限大小的数据
	chunk := make([]byte, 1<<20)

	handlerCalls := 0
	wrapped := false

	engine := gin.New()
	engine.Use(CacheableRouteMarker(), RequestCacher())
	engine.Any("/*vars", func(c *gin.Context) {
		handlerCalls++

		// 前提校验: 请求必须已被缓存中间件接管,
		// 否则该路由根本没进缓存链路, 本用例会变成无意义的空跑
		if _, ok := c.Writer.(*respCacheWriter); !ok {
			t.Fatalf("c.Writer 未被包装为缓冲响应器, 用例前提不成立")
			return
		}
		wrapped = true

		for remaining := MaxBufferedRespSize + 1; remaining > 0; {
			n := int64(len(chunk))
			if n > remaining {
				n = remaining
			}
			written, err := c.Writer.Write(chunk[:n])
			if err != nil {
				t.Errorf("Write() err = %v, want nil", err)
				return
			}
			if written != int(n) {
				t.Errorf("Write() n = %d, want %d", written, n)
				return
			}
			remaining -= n
		}
	})

	send := func() {
		engine.ServeHTTP(&discardResponseWriter{}, httptest.NewRequest(http.MethodGet, uri, nil))
	}

	// 第一次请求: 写入超限响应体, 应放弃缓存
	send()
	if !wrapped {
		t.Fatalf("请求未被缓存中间件接管, 用例前提不成立")
	}
	if handlerCalls != 1 {
		t.Fatalf("第一次请求后 handlerCalls = %d, want 1", handlerCalls)
	}

	// 第二次请求: 若超限响应体被缓存, 这里会命中缓存而不再调用处理器
	send()
	if handlerCalls != 2 {
		t.Errorf("超限响应被写入了缓存: handler 只被调用 %d 次, want 2", handlerCalls)
	}
}

// TestRespCacheWriter_CanBufferBoundary 验证缓冲上限的边界判定
//
// 恰好达到上限时允许继续缓冲, 超出 1 字节即关闭缓冲
func TestRespCacheWriter_CanBufferBoundary(t *testing.T) {
	tests := []struct {
		name          string
		prefilledBody int64
		n             int
		wantCanBuffer bool
		wantDisabled  bool
	}{
		{
			name:          "空缓冲写入恰好等于上限",
			prefilledBody: 0,
			n:             int(MaxBufferedRespSize),
			wantCanBuffer: true,
			wantDisabled:  false,
		},
		{
			name:          "接近上限时写入剩余空间",
			prefilledBody: MaxBufferedRespSize - 1,
			n:             1,
			wantCanBuffer: true,
			wantDisabled:  false,
		},
		{
			name:          "接近上限时超出 1 字节",
			prefilledBody: MaxBufferedRespSize - 1,
			n:             2,
			wantCanBuffer: false,
			wantDisabled:  true,
		},
		{
			name:          "空缓冲一次写入超过上限",
			prefilledBody: 0,
			n:             int(MaxBufferedRespSize) + 1,
			wantCanBuffer: false,
			wantDisabled:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rcw, _ := newTestRespCacheWriter(t)
			if tt.prefilledBody > 0 {
				rcw.body.Grow(int(tt.prefilledBody))
				rcw.body.Write(make([]byte, tt.prefilledBody))
			}

			if got := rcw.canBuffer(tt.n); got != tt.wantCanBuffer {
				t.Errorf("canBuffer(%d) = %v, want %v", tt.n, got, tt.wantCanBuffer)
			}
			if rcw.disabled != tt.wantDisabled {
				t.Errorf("disabled = %v, want %v", rcw.disabled, tt.wantDisabled)
			}
			if tt.wantDisabled && rcw.body.Len() != 0 {
				t.Errorf("禁用缓冲后 body.Len() = %d, want 0", rcw.body.Len())
			}
			if !tt.wantDisabled {
				wantLen := int(tt.prefilledBody)
				if rcw.body.Len() != wantLen {
					t.Errorf("body.Len() = %d, want %d", rcw.body.Len(), wantLen)
				}
			}
		})
	}
}
