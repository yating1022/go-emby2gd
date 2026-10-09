package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type enrollPayload struct {
	AgentID   string `json:"agent_id"`
	SignKey   string `json:"sign_key"`
	Heartbeat int    `json:"heartbeat_interval_seconds"`
}

func TestGetJSONDecodesFlatResponse(t *testing.T) {
	// 冻结稿 §2 冻结的裸对象形状。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agent_id":"a1","sign_key":"k1","heartbeat_interval_seconds":15}`))
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL}
	var out enrollPayload
	if err := client.GetJSON(context.Background(), "/api/agent/enroll", nil, &out); err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	if out.AgentID != "a1" || out.SignKey != "k1" || out.Heartbeat != 15 {
		t.Fatalf("解包结果不对：%+v", out)
	}
}

func TestGetJSONDecodesWrappedResponse(t *testing.T) {
	// 本项目既有约定（backend/app/core/response.py）的 {ok,data} 形状。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"data":{"agent_id":"a2","sign_key":"k2","heartbeat_interval_seconds":30}}`))
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL}
	var out enrollPayload
	if err := client.GetJSON(context.Background(), "/api/agent/enroll", nil, &out); err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	if out.AgentID != "a2" || out.Heartbeat != 30 {
		t.Fatalf("解包结果不对：%+v", out)
	}
}

func TestPostJSONSendsCredentialsAndQuery(t *testing.T) {
	var gotAuth, gotAgentID, gotQuery, gotBodyType string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAgentID = r.Header.Get("X-Agent-Id")
		gotQuery = r.URL.RawQuery
		gotBodyType = r.Header.Get("Content-Type")
		gotBody = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(gotBody)
		_, _ = w.Write([]byte(`{"ok":true,"enabled":true}`))
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL + "/", AgentID: "agent-1", Secret: "sec-1"}
	if err := client.PostJSON(context.Background(), "/api/agent/heartbeat", map[string]any{"active_streams": 3}, nil); err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	if gotAuth != "Bearer sec-1" {
		t.Fatalf("Authorization 不对：%q", gotAuth)
	}
	if gotAgentID != "agent-1" {
		t.Fatalf("X-Agent-Id 不对：%q", gotAgentID)
	}
	if gotBodyType != "application/json" {
		t.Fatalf("Content-Type 不对：%q", gotBodyType)
	}
	if !strings.Contains(string(gotBody), `"active_streams":3`) {
		t.Fatalf("请求体不对：%s", gotBody)
	}
	if gotQuery != "" {
		t.Fatalf("未传 query 时不应有查询串：%q", gotQuery)
	}
}

func TestGetJSONEncodesQuery(t *testing.T) {
	var rawQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"url":"http://x"}`))
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL}
	err := client.GetJSON(context.Background(), "/api/agent/download-link",
		url.Values{"file_id": {"abc/def+g=="}}, nil)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	if !strings.Contains(rawQuery, "file_id=abc%2Fdef%2Bg%3D%3D") {
		t.Fatalf("file_id 未正确转义：%q", rawQuery)
	}
}

func TestErrorWrappedEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"ok":false,"error":{"code":"GD_QUOTA_EXCEEDED","message":"Google 配额已用尽"}}`))
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL}
	err := client.GetJSON(context.Background(), "/api/agent/download-link", nil, nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("应返回 *api.Error，实际 %T", err)
	}
	if apiErr.Status != 502 || apiErr.Code != "GD_QUOTA_EXCEEDED" || apiErr.Message != "Google 配额已用尽" {
		t.Fatalf("错误解析不对：%+v", apiErr)
	}
	if !strings.Contains(apiErr.Error(), "502") || !strings.Contains(apiErr.Error(), "GD_QUOTA_EXCEEDED") {
		t.Fatalf("Error() 应含状态码与 code：%s", apiErr.Error())
	}
}

func TestErrorFlatEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"ENROLL_TOKEN_INVALID","message":"注册 Token 无效"}`))
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL}
	err := client.PostJSON(context.Background(), "/api/agent/enroll", map[string]any{}, nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("应返回 *api.Error，实际 %T", err)
	}
	if apiErr.Status != 401 || apiErr.Code != "ENROLL_TOKEN_INVALID" || apiErr.Message != "注册 Token 无效" {
		t.Fatalf("错误解析不对：%+v", apiErr)
	}
}

func TestErrorNonJSONBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL}
	err := client.GetJSON(context.Background(), "/api/agent/download-link", nil, nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("应返回 *api.Error，实际 %T", err)
	}
	if apiErr.Status != 502 || !strings.Contains(apiErr.Message, "502") {
		t.Fatalf("非 JSON 错误应给中文兜底并含状态码：%+v", apiErr)
	}
}

func TestNetworkFailureIsChinese(t *testing.T) {
	// 127.0.0.1:0 不会有人监听（httptest 已关掉的地址）。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := server.URL
	server.Close()

	client := &Client{BaseURL: addr}
	err := client.GetJSON(context.Background(), "/api/agent/enroll", nil, nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("应返回 *api.Error，实际 %T", err)
	}
	if apiErr.Status != 0 || !strings.Contains(apiErr.Message, "无法连接 master") {
		t.Fatalf("网络失败应有中文提示：%+v", apiErr)
	}
}

func TestContextCanceledReturnsContextError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &Client{BaseURL: server.URL}
	err := client.GetJSON(ctx, "/api/agent/enroll", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应透出 context.Canceled，实际 %v", err)
	}
}

func TestInvalidResponseShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"url":123}`))
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL}
	var out struct {
		URL string `json:"url"`
	}
	err := client.GetJSON(context.Background(), "/api/agent/download-link", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "不符合预期") {
		t.Fatalf("类型不符应报中文错误，实际 %v", err)
	}
}
