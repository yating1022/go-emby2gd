// hub 模式的端到端链路（全部 httptest，不经真实网络，从外部按 main.go 的
// 接线方式组装，保证导出面被真实使用）：
//
//	master（发 hub 直链、headers 为空）→ node /dl/ → hub /f/<fileID> → Google
//
// 覆盖：未 warm → 409；warm 区域集 → 块 0 本地供流 + 跨块 Range 字节一致；
// 透传同步落盘 → 全命中（hub 与 Google 断开后仍供流）；后缀区间 bytes=-N →
// node 原样透传、hub 收窄为 [size-N, size-1] 后本地全命中。
// 凭据流向对偶断言（S6 所在）：
//   - hub → Google 的每一条请求都带 warm 凭据（且至少发生过回源）；
//   - node → hub 的每一条请求都不带任何凭据（master 对 hub 链路下发空 headers）。
//
// 断言全部走 HTTP 行为（外部包拿不到 fileMeta 的小写字段），就绪类条件用
// 短周期轮询消除异步落盘的时间竞争。
package proxy_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yating1022/go-emby2gd/agent/internal/proxy"
)

// e2eBlock 是冻结的 4MiB 块尺寸（协议值，改它会破坏字节一致性）。
const e2eBlock = int64(4 << 20)

// hubRecorder 包住 hub handler，记录进入 hub 的请求头（S6 证据用；Range 用于
// ⑦ 的"node 原样透传后缀"断言）。
type hubRecorder struct {
	next   http.Handler
	mu     sync.Mutex
	auths  []string
	ranges []string
}

func (r *hubRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.auths = append(r.auths, req.Header.Get("Authorization"))
	r.ranges = append(r.ranges, req.Header.Get("Range"))
	r.mu.Unlock()
	r.next.ServeHTTP(w, req)
}

func (r *hubRecorder) snapshotAuth() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.auths...)
}

func (r *hubRecorder) snapshotRanges() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ranges...)
}

func TestHubE2ENodeHubGoogle(t *testing.T) {
	size := e2eBlock + 65536 // 两块：块 0 整块 + 块 1 尾块
	content := e2eContent(int(size))

	// --- 假 Google：Range 语义 + 记录每条请求（Range/Authorization） ---
	type googleHit struct{ rng, auth string }
	var (
		googleMu   sync.Mutex
		googleHits []googleHit
		googleDown atomic.Bool
	)
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		googleMu.Lock()
		googleHits = append(googleHits, googleHit{rng: r.Header.Get("Range"), auth: r.Header.Get("Authorization")})
		googleMu.Unlock()
		if googleDown.Load() {
			http.Error(w, "google down", http.StatusBadGateway)
			return
		}
		http.ServeContent(w, r, "file.bin", time.Unix(1700000000, 0), bytes.NewReader(content))
	}))
	defer google.Close()
	googleSnapshot := func() (count int, hits []googleHit) {
		googleMu.Lock()
		defer googleMu.Unlock()
		return len(googleHits), append([]googleHit(nil), googleHits...)
	}

	// --- hub：磁盘缓存 + NewHub，外部形态与 main.go 接线一致 ---
	cache, err := proxy.NewDiskCache(proxy.DiskCacheConfig{
		Dir:         t.TempDir(),
		BudgetBytes: 1 << 30,
		MaxAge:      48 * time.Hour,
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	const (
		fileID = "e2e-drive-file"
		token  = "tok-e2e-drive-file"
	)
	directURL := google.URL + "/gfile"

	// 假 master 只服务 node 的 download-link（返回 hub 数据面地址、headers 为空）。
	var masterLinkCalls atomic.Int64
	var hubURLForLink string
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/download-link" {
			http.NotFound(w, r)
			return
		}
		masterLinkCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"url":        hubURLForLink,
			"headers":    map[string]string{}, // 冻结契约：hub 链路不带任何凭据
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	defer master.Close()

	hubLinks := proxy.NewLinkSource(proxy.LinkSourceConfig{
		MasterURL: master.URL,
		AgentID:   "hub-agent",
		Secret:    "hub-secret",
		Logger:    quietLogger(),
	})
	hub := proxy.NewHub(proxy.HubConfig{
		AllowIPs:              "127.0.0.1",
		Cache:                 cache,
		Links:                 hubLinks,
		Logger:                quietLogger(),
		WarmHeadBytes:         e2eBlock,
		WarmTailBytes:         0,
		WarmResumeWindowBytes: 0,
		WarmWait:              400 * time.Millisecond,
		WarmPollInterval:      5 * time.Millisecond,
		StallTimeout:          3 * time.Second,
	})
	rec := &hubRecorder{next: hub}
	hubServer := httptest.NewServer(rec)
	defer hubServer.Close()
	hubURLForLink = hubServer.URL + "/f/" + url.PathEscape(fileID)

	// --- node：与 main.go 的 node 接线一致（缓存关闭，纯转发 hub 响应） ---
	signKey := bytes.Repeat([]byte{0x5a}, 32)
	nodeLinks := proxy.NewLinkSource(proxy.LinkSourceConfig{
		MasterURL: master.URL,
		AgentID:   "node-agent",
		Secret:    "node-secret",
		Logger:    quietLogger(),
	})
	nodeHandler := proxy.NewHandler(proxy.Config{
		SignKey:       signKey,
		MaxConcurrent: 4,
		Links:         nodeLinks,
		Logger:        quietLogger(),
		Cache:         proxy.NewBlockCache(0), // 关闭读前缓存：纯转发
	})
	nodeServer := httptest.NewServer(nodeHandler)
	defer nodeServer.Close()

	nodeGET := func(rangeHeader string) (*http.Response, []byte) {
		t.Helper()
		expiry := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
		u := fmt.Sprintf("%s/dl/%s?e=%s&s=%s", nodeServer.URL, fileID, expiry, proxy.Sign(signKey, fileID, expiry))
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("node 请求失败：%v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, body
	}

	// --- ① 未 warm：node 透传 hub 的 409 not_warmed ---
	resp, body := nodeGET("bytes=0-99")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("未 warm 状态 = %d，应 409（%s）", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "not_warmed") {
		t.Fatalf("未 warm 响应体 = %s", body)
	}

	// --- ② warm：头段单流抓满块 0（master → hub 的 /warm，带账号级凭据） ---
	warmBody, _ := json.Marshal(map[string]any{
		"file_id":     fileID,
		"file_token":  token,
		"direct_link": directURL,
		"auth":        map[string]string{"Authorization": "Bearer warm-token"},
		"regions":     map[string]any{"head_bytes": e2eBlock, "tail_bytes": 0},
	})
	warmResp, err := http.Post(hubServer.URL+"/warm", "application/json", bytes.NewReader(warmBody))
	if err != nil {
		t.Fatalf("POST /warm 失败：%v", err)
	}
	warmRespBody, _ := io.ReadAll(warmResp.Body)
	_ = warmResp.Body.Close()
	if warmResp.StatusCode != http.StatusOK {
		t.Fatalf("/warm = %d（%s）", warmResp.StatusCode, warmRespBody)
	}

	// --- ③ 等块 0 就绪：块内小段探测直到**不再出网**（本地在供流的证据） ---
	deadline := time.Now().Add(5 * time.Second)
	for {
		before, _ := googleSnapshot()
		resp, body = nodeGET("bytes=100-199")
		if resp.StatusCode == http.StatusPartialContent && bytes.Equal(body, content[100:200]) {
			if after, _ := googleSnapshot(); after == before {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("warm 后块 0 未就绪：status=%d bytes=%d", resp.StatusCode, len(body))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// --- ④ 跨块 Range：本地前缀 + 上游续段拼出的字节必须整体一致 ---
	start, end := e2eBlock-100, e2eBlock+599
	resp, body = nodeGET(fmt.Sprintf("bytes=%d-%d", start, end))
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, content[start:end+1]) {
		t.Fatalf("跨块部分命中不符：status=%d bytes=%d", resp.StatusCode, len(body))
	}

	// --- ⑤ 整文件 GET（无 Range，按冻结语义直接透传）字节一致，tee 同步落盘 ---
	resp, body = nodeGET("")
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("整文件透传不符：status=%d bytes=%d", resp.StatusCode, len(body))
	}
	// 等尾块落盘：块 1 内小段探测直到本地全命中（不再出网）。
	probeStart, probeEnd := e2eBlock+123, e2eBlock+456
	deadline = time.Now().Add(5 * time.Second)
	for {
		before, _ := googleSnapshot()
		resp, body = nodeGET(fmt.Sprintf("bytes=%d-%d", probeStart, probeEnd))
		if resp.StatusCode == http.StatusPartialContent && bytes.Equal(body, content[probeStart:probeEnd+1]) {
			if after, _ := googleSnapshot(); after == before {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("尾块落盘后仍不能全命中供流")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// --- ⑥ hub 与 Google 断开：全命中仍供流（本地服务不依赖上游） ---
	googleDown.Store(true)
	resp, body = nodeGET("bytes=1000-1999")
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, content[1000:2000]) {
		t.Fatalf("断网命中不符：status=%d bytes=%d", resp.StatusCode, len(body))
	}

	// --- ⑦ 后缀区间 bytes=-64（F2）：node 原样透传、hub 收窄为 [size-64, size-1]
	//     并本地全命中。Google 仍断着：若 hub 试图回源必失败，能给出正确字节
	//     本身就证明"后缀由 hub 解析、本地供流"。
	resp, body = nodeGET("bytes=-64")
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, content[size-64:]) {
		t.Fatalf("后缀区间不符：status=%d bytes=%d", resp.StatusCode, len(body))
	}
	if got, want := resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-%d/%d", size-64, size-1, size); got != want {
		t.Fatalf("后缀区间 Content-Range = %q，应 %q", got, want)
	}
	// node → hub 的 Range 必须仍是原样的 `bytes=-64`：node 侧零改动（不展开、不
	// 改写成确定区间），后缀由 hub 解析——两者分工的 HTTP 边界证据。
	ranges := rec.snapshotRanges()
	if len(ranges) == 0 || ranges[len(ranges)-1] != "bytes=-64" {
		t.Fatalf("node→hub 的 Range 应为原样后缀 bytes=-64，实际 %v", ranges)
	}

	// --- 凭据流向（S6）：node→hub 全程无凭据；hub→Google 全程带 warm 凭据 ---
	hubAuths := rec.snapshotAuth()
	if len(hubAuths) == 0 {
		t.Fatal("hub 未收到任何 node 请求")
	}
	for _, auth := range hubAuths {
		if auth != "" {
			t.Fatalf("node 请求 hub 时带上了凭据：%q（master 对 hub 链路下发空 headers）", auth)
		}
	}
	if masterLinkCalls.Load() == 0 {
		t.Fatal("node 未向 master 拉过直链")
	}
	_, hits := googleSnapshot()
	if len(hits) == 0 {
		t.Fatal("hub 从未回源 Google")
	}
	for _, hit := range hits {
		if hit.auth != "Bearer warm-token" {
			t.Fatalf("hub 回源未带 warm 凭据：range=%q auth=%q", hit.rng, hit.auth)
		}
	}
}
