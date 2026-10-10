package proxy

// F2（v0.4.1）：hub 的 /f/ 支持后缀区间 bytes=-N——
//   - size 已知：解析为 [size-N, size-1]（N>=size → 整个文件），此后与常规区间
//     一样走三态服务（本地命中即不出网）；
//   - N==0 / 非法：原样透传，由上游按其 416 语义处理；
//   - size 未知：维持现状，原样透传上游。
//
// node 侧零改动：后缀原样透传给 hub，由 hub 解析。

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"
)

// TestParseByteRangeSized 钉住"已知 size 时的区间解析"边界矩阵：常规/open-ended
// 与原解析完全一致（零回归），后缀按 RFC 9110 收窄，N=0/非法交上游。
func TestParseByteRangeSized(t *testing.T) {
	const size = int64(1000)
	cases := []struct {
		header string
		want   byteRange
		ok     bool
	}{
		// 常规形态：与 parseByteRange 完全一致。
		{"bytes=0-1023", byteRange{start: 0, end: 1023}, true},
		{"bytes=100-", byteRange{start: 100, end: -1}, true},
		{"bytes= 5 - 10 ", byteRange{start: 5, end: 10}, true},
		// 后缀：N<size / N==size / N>size。
		{"bytes=-100", byteRange{start: 900, end: 999}, true},
		{"bytes=-1", byteRange{start: 999, end: 999}, true},
		{"bytes=-1000", byteRange{start: 0, end: 999}, true},
		{"bytes=-1001", byteRange{start: 0, end: 999}, true},
		{"bytes=-007", byteRange{start: 993, end: 999}, true},
		{"bytes= -5", byteRange{start: 995, end: 999}, true},
		// 非法/坏形态：ok=false → 原样透传，由上游按其 416 语义处理。
		{"bytes=-0", byteRange{}, false},
		{"bytes=-", byteRange{}, false},
		{"bytes=--5", byteRange{}, false},
		{"bytes=-abc", byteRange{}, false},
		{"bytes=0-1023,2048-3071", byteRange{}, false},
		{"items=-5", byteRange{}, false},
		{"", byteRange{}, false},
	}
	for _, tc := range cases {
		got, ok := parseByteRangeSized(tc.header, size)
		if ok != tc.ok {
			t.Errorf("%q：ok 应为 %v，实际 %v", tc.header, tc.ok, ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("%q：解析为 %+v，期望 %+v", tc.header, got, tc.want)
		}
	}

	// size 未知（<=0）：后缀不展开（交调用方按现状透传），常规形态照旧。
	if _, ok := parseByteRangeSized("bytes=-100", 0); ok {
		t.Error("size 未知时后缀不应解析")
	}
	if _, ok := parseByteRangeSized("bytes=5-9", 0); !ok {
		t.Error("size 未知时常规区间应照常解析")
	}
}

// TestHubSuffixRangeMatrix 是后缀区间在 /f/ 三态服务上的边界矩阵：
// N<size（本地全命中不出网）/ N>=size（整个文件） / N=0（非法透传 416） /
// 常规与 open-ended 不回归。
func TestHubSuffixRangeMatrix(t *testing.T) {
	size := 2*blockSize + 12345
	hf := newHubFixture(t, makePayload(size), nil)
	fileID := hf.fileID
	identity := fmt.Sprintf("size:%d", size)
	hf.cache.Observe(fileID, fileMeta{size: int64(size)})
	if !hf.cache.Put(fileID, identity, 0, hf.payload[:blockSize]) {
		t.Fatal("预置块 0 失败")
	}
	if !hf.cache.Put(fileID, identity, 2, hf.payload[2*blockSize:]) {
		t.Fatal("预置尾块失败")
	}
	hf.installLink()

	// ① N<size 且完整落在尾块内：纯本地 206（不出网）。
	before := hf.up.count()
	rec := hf.request(http.MethodGet, "/f/"+fileID, "bytes=-8000", nil)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("后缀全命中状态 = %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), hf.payload[size-8000:]) {
		t.Fatal("后缀全命中字节与源不一致")
	}
	if got := rec.Header().Get("Content-Range"); got != fmt.Sprintf("bytes %d-%d/%d", size-8000, size-1, size) {
		t.Fatalf("后缀全命中 Content-Range = %q", got)
	}
	if got := rec.Header().Get("Content-Length"); got != "8000" {
		t.Fatalf("后缀全命中 Content-Length = %q", got)
	}
	if hf.up.count() != before {
		t.Fatalf("后缀全命中不该出网（新增 %d 次请求）", hf.up.count()-before)
	}

	// ② N>=size（N==size 与 N>size 两个边角）：映射为整个文件 [0, size-1] →
	//    本地前缀（块 0）+ 上游精确余段；上游**绝不**收到 `bytes=-N`。
	for _, n := range []int{size, size + 5000} {
		before = hf.up.count()
		rec = hf.request(http.MethodGet, "/f/"+fileID, fmt.Sprintf("bytes=-%d", n), nil)
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("N=%d 状态 = %d", n, rec.Code)
		}
		if !bytes.Equal(rec.Body.Bytes(), hf.payload) {
			t.Fatalf("N=%d 应给整个文件", n)
		}
		if got := rec.Header().Get("Content-Range"); got != fmt.Sprintf("bytes 0-%d/%d", size-1, size) {
			t.Fatalf("N=%d Content-Range = %q", n, got)
		}
		reqs := hf.up.snapshot()[before:]
		if len(reqs) != 1 {
			t.Fatalf("N=%d 混合应恰好 1 条上游请求，实际 %d", n, len(reqs))
		}
		wantRange := fmt.Sprintf("bytes=%d-%d", blockSize, size-1)
		if reqs[0].rangeHeader != wantRange {
			t.Fatalf("N=%d 上游 Range = %q，应 %q（后缀必须已被 hub 解析）", n, reqs[0].rangeHeader, wantRange)
		}
		if reqs[0].auth != "Bearer warm-token" {
			t.Fatalf("N=%d 混合余段未带 warm 凭据：%q", n, reqs[0].auth)
		}
	}

	// ③ N==0：非法 → 原样透传，上游收到 `bytes=-0` 并回 416。
	before = hf.up.count()
	rec = hf.request(http.MethodGet, "/f/"+fileID, "bytes=-0", nil)
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("N=0 状态 = %d，应 416", rec.Code)
	}
	reqs := hf.up.snapshot()[before:]
	if len(reqs) != 1 || reqs[0].rangeHeader != "bytes=-0" {
		t.Fatalf("N=0 应原样透传：%+v", reqs)
	}

	// ④ 常规与 open-ended 零回归：块 0 内小段与尾块 open-ended 都是纯本地。
	before = hf.up.count()
	rec = hf.request(http.MethodGet, "/f/"+fileID, "bytes=100-999", nil)
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), hf.payload[100:1000]) {
		t.Fatalf("常规全命中回归：status=%d", rec.Code)
	}
	rec = hf.request(http.MethodGet, "/f/"+fileID, fmt.Sprintf("bytes=%d-", 2*blockSize), nil)
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), hf.payload[2*blockSize:]) {
		t.Fatalf("open-ended 全命中回归：status=%d", rec.Code)
	}
	if hf.up.count() != before {
		t.Fatalf("常规/open-ended 全命中不该出网（新增 %d 次）", hf.up.count()-before)
	}
}

// TestHubSuffixRangeNoMetaPassthrough：size 未知（该文件从未观测到元数据）时，
// 后缀维持现状——原样透传给上游，hub 不做本地映射。
func TestHubSuffixRangeNoMetaPassthrough(t *testing.T) {
	size := 5000
	hf := newHubFixture(t, makePayload(size), nil)
	hf.installLink() // 只注入直链：没有任何本地元数据/块

	rec := hf.request(http.MethodGet, "/f/"+hf.fileID, "bytes=-100", nil)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("状态 = %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), hf.payload[size-100:]) {
		t.Fatal("后缀透传字节与源不一致")
	}
	if got := rec.Header().Get("Content-Range"); got != fmt.Sprintf("bytes %d-%d/%d", size-100, size-1, size) {
		t.Fatalf("Content-Range = %q", got)
	}
	reqs := hf.up.snapshot()
	if len(reqs) != 1 || reqs[0].rangeHeader != "bytes=-100" {
		t.Fatalf("size 未知时应原样透传后缀：%+v", reqs)
	}
}
