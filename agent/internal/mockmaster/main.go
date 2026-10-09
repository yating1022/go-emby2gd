// mockmaster 是一个**本地联调用的假 master**（不参与发布）：
// 实现冻结稿 §2 的三个 agent 接口 + 一个假 Google（支持 Range/HEAD），
// 让 agent 在没有真实 master、也不出网的情况下跑通全链路。
//
// 典型用法（详见本任务 notes.md 的冒烟步骤）：
//
//	go run ./internal/mockmaster --addr 127.0.0.1:18990 --token dev-token --agent-base http://127.0.0.1:8790
//	go run . enroll --master http://127.0.0.1:18990 --token dev-token --port 8790 --config /tmp/gd-agent/config.env
//	go run . serve  --config /tmp/gd-agent/config.env
//	curl -r 0-1023 "<启动时打印的签名 URL>" -o chunk.bin
//
// 它打印的签名 URL 用 proxy.Sign 生成——签名格式只有一个实现，
// 这里不会和 agent 的验签逻辑漂移。
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/yating1022/go-emby2gd/agent/internal/proxy"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18990", "mock master 监听地址")
	token := flag.String("token", "dev-token", "注册 Token（enroll 时校验）")
	agentBase := flag.String("agent-base", "http://127.0.0.1:8790", "agent 数据面地址（用于生成签名 URL）")
	size := flag.Int("size", 1<<20, "假 Google 返回的文件字节数")
	flag.Parse()

	content := make([]byte, *size)
	for i := range content {
		content[i] = byte((i * 7) % 251)
	}

	// 与真实 master 一致：agent_id 固定，secret/sign_key 在每次 enroll 时轮换
	// （冻结稿 §2.1 的幂等语义——重跑安装脚本复用记录但换新凭据）。
	state := &agentState{agentID: "mock-agent-" + randomHex(4)}
	state.rotate()
	printDemoURL := func() {
		expiry := time.Now().Add(24 * time.Hour)
		url := fmt.Sprintf("%s/dl/%s?e=%d&s=%s",
			strings.TrimRight(*agentBase, "/"), demoFile, expiry.Unix(),
			proxy.Sign(mustHex(state.signKey()), demoFile, fmt.Sprint(expiry.Unix())))
		fmt.Fprintf(os.Stdout, "可直接 curl 的测试 URL（24h 有效，字节数 %d）：\n  %s\n", *size, url)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/enroll", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			EnrollToken   string `json:"enroll_token"`
			MachineID     string `json:"machine_id"`
			Hostname      string `json:"hostname"`
			Version       string `json:"version"`
			ListenPort    int    `json:"listen_port"`
			PublicBaseURL any    `json:"public_base_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, `{"ok":false,"error":{"code":"VALIDATION_ERROR","message":"请求体不是合法 JSON"}}`)
			return
		}
		if body.EnrollToken != *token {
			writeJSON(w, http.StatusUnauthorized, `{"ok":false,"error":{"code":"ENROLL_TOKEN_INVALID","message":"注册 Token 无效"}}`)
			return
		}
		log.Printf("enroll：machine_id=%s hostname=%s version=%s listen_port=%d public_base_url=%v",
			body.MachineID, body.Hostname, body.Version, body.ListenPort, body.PublicBaseURL)
		state.rotate() // 幂等复用 agent_id，但轮换 secret/sign_key
		log.Printf("enroll：已轮换凭据（agent_id=%s，旧 secret/sign_key 立即失效）", state.id())
		printDemoURL()
		// 冻结稿的裸对象形状（agent 兼容两种信封）。
		writeJSON(w, http.StatusOK, fmt.Sprintf(
			`{"agent_id":%q,"agent_secret":%q,"sign_key":%q,"heartbeat_interval_seconds":15}`,
			state.id(), state.secret(), state.signKey()))
	})
	mux.HandleFunc("/api/agent/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+state.secret() || r.Header.Get("X-Agent-Id") != state.id() {
			writeJSON(w, http.StatusUnauthorized, `{"ok":false,"error":{"code":"UNAUTHORIZED","message":"agent 凭据无效"}}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		log.Printf("heartbeat：active_streams=%v version=%v uptime=%vs listen_port=%v",
			body["active_streams"], body["version"], body["uptime_seconds"], body["listen_port"])
		writeJSON(w, http.StatusOK, `{"ok":true,"enabled":true,"heartbeat_interval_seconds":15}`)
	})
	mux.HandleFunc("/api/agent/download-link", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+state.secret() || r.Header.Get("X-Agent-Id") != state.id() {
			writeJSON(w, http.StatusUnauthorized, `{"ok":false,"error":{"code":"UNAUTHORIZED","message":"agent 凭据无效"}}`)
			return
		}
		fileID := r.URL.Query().Get("file_id")
		if fileID == "" {
			writeJSON(w, http.StatusBadRequest, `{"ok":false,"error":{"code":"VALIDATION_ERROR","message":"缺少 file_id"}}`)
			return
		}
		log.Printf("download-link：file_id=%s", fileID)
		scheme := "http"
		writeJSON(w, http.StatusOK, fmt.Sprintf(
			`{"url":%q,"headers":{"Authorization":"Bearer mock-google-token"},"expires_at":%q}`,
			fmt.Sprintf("%s://%s/gdrive/%s", scheme, *addr, fileID),
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339)))
	})
	mux.HandleFunc("/gdrive/", func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer mock-google-token" {
			log.Printf("假 Google：收到缺少/错误的凭据头 %q（agent 应注入 master 下发的头）", auth)
		}
		http.ServeContent(w, r, "file.bin", time.Unix(1700000000, 0), bytes.NewReader(content))
	})

	go func() {
		if err := http.ListenAndServe(*addr, mux); err != nil {
			log.Fatalf("mock master 退出：%v", err)
		}
	}()

	fmt.Fprintf(os.Stdout, "mock master 监听 http://%s\n", *addr)
	fmt.Fprintf(os.Stdout, "注册 Token：%s\n", *token)
	fmt.Fprintf(os.Stdout, "安装命令：gd-agent enroll --master http://%s --token %s --port <端口> --config <路径>\n", *addr, *token)
	printDemoURL()
	select {}
}

// demoFile 是本地联调用的固定 file_id。
const demoFile = "demo-file"

// agentState 持有当前有效的 agent 凭据（enroll 轮换）。
type agentState struct {
	mu        sync.Mutex
	agentID   string
	agentSec  string
	agentSign string
}

func (s *agentState) rotate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agentSec = randomHex(32)
	s.agentSign = randomHex(32)
}

func (s *agentState) id() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentID
}

func (s *agentState) secret() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentSec
}

func (s *agentState) signKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentSign
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("生成随机数失败：%v", err)
	}
	return hex.EncodeToString(buf)
}

func mustHex(s string) []byte {
	buf, err := hex.DecodeString(s)
	if err != nil {
		log.Fatalf("内部错误：%v", err)
	}
	return buf
}
