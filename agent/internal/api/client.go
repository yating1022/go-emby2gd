// Package api 是 agent → master 的最小 HTTP 客户端：统一拼 URL、带 agent 凭据头、
// 解包响应、把失败转成中文错误。
//
// 它是 master 响应契约的**唯一解密处**（enroll / heartbeat / download-link 共用）：
// 信封兼容两种形状，避免契约细节散落到各个调用点：
//
//	① 冻结稿 §2 冻结的裸对象：        {"agent_id": "...", ...}
//	② 本项目既有约定（core/response.py）：   {"ok": true, "data": {...}}
//	                                          {"ok": false, "error": {"code": "...", "message": "中文"}}
//
// 两种都接受：子任务 A 无论按哪种实现，agent 都能对接；错误响应优先取
// `error.message`（中文原因），没有则退回裸 `message` 字段。
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 响应体上限：agent 接口的响应都是小 JSON，1 MiB 足够；
// 避免异常上游把 agent 内存拖垮。
const maxResponseBytes = 1 << 20

// Error 是一次 master 调用的失败描述（HTTP 错误或网络错误）。
//
// Status == 0 表示请求根本没拿到响应（连接/DNS/超时），Message 仍是中文。
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return e.Message
	}
	if e.Code != "" {
		return fmt.Sprintf("%s（HTTP %d，code=%s）", e.Message, e.Status, e.Code)
	}
	return fmt.Sprintf("%s（HTTP %d）", e.Message, e.Status)
}

// Client 面向单个 master 地址。
//
// AgentID / Secret 为空时只用于 enroll（那时还没有凭据）。
type Client struct {
	BaseURL string
	AgentID string
	Secret  string
	HTTP    *http.Client
}

// GetJSON 发 GET 并把成功响应解到 out（out 可为 nil）。
func (c *Client) GetJSON(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, out)
}

// PostJSON 发 POST（body 为 JSON，可为 nil）并把成功响应解到 out。
func (c *Client) PostJSON(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, nil, body, out)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	if strings.TrimSpace(c.BaseURL) == "" {
		return &Error{Message: "master 地址为空"}
	}
	endpoint := strings.TrimRight(c.BaseURL, "/") + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return &Error{Message: fmt.Sprintf("构造请求失败：%v", err)}
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return &Error{Message: fmt.Sprintf("构造请求失败：%v", err)}
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.Secret)
	}
	if c.AgentID != "" {
		req.Header.Set("X-Agent-Id", c.AgentID)
	}

	client := c.HTTP
	if client == nil {
		client = defaultHTTPClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{Message: fmt.Sprintf("无法连接 master：%v", err)}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return &Error{Status: resp.StatusCode, Message: fmt.Sprintf("读取 master 响应失败：%v", err)}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseError(resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	if err := decodeData(raw, out); err != nil {
		return &Error{Status: resp.StatusCode, Message: err.Error()}
	}
	return nil
}

// envelope 用于探测 `{"ok":..., "data":..., "error":...}` 形状；
// 冻结稿的裸对象没有 data/error 字段，探测结果为空即按裸对象解。
type envelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeData(raw []byte, out any) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err == nil {
		if env.Error != nil {
			// 200 但带 error 信封：按失败处理（契约异常，不静默吞掉）。
			msg := env.Error.Message
			if msg == "" {
				msg = "master 返回了错误信封"
			}
			return &Error{Status: http.StatusOK, Code: env.Error.Code, Message: msg}
		}
		if len(env.Data) > 0 && !bytes.Equal(bytes.TrimSpace(env.Data), []byte("null")) {
			if err := json.Unmarshal(env.Data, out); err != nil {
				return &Error{Message: fmt.Sprintf("master 响应结构不符合预期：%v", err)}
			}
			return nil
		}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		if json.Valid(raw) {
			return &Error{Message: fmt.Sprintf("master 响应结构不符合预期：%v", err)}
		}
		return &Error{Message: fmt.Sprintf("master 响应不是合法 JSON：%v", err)}
	}
	return nil
}

func parseError(status int, raw []byte) *Error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err == nil && env.Error != nil && env.Error.Message != "" {
		return &Error{Status: status, Code: env.Error.Code, Message: env.Error.Message}
	}
	// 裸对象错误：{"code": "...", "message": "中文原因"}
	var flat struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &flat); err == nil && flat.Message != "" {
		return &Error{Status: status, Code: flat.Code, Message: flat.Message}
	}
	return &Error{Status: status, Message: fmt.Sprintf("master 返回 HTTP %d（响应不是可识别的错误 JSON）", status)}
}

// defaultHTTPClient 用于控制面调用：必须有整体超时（都是小 JSON 往返），
// 与数据面（大文件流、不设超时）刻意不同。
var defaultHTTPClient = &http.Client{Timeout: 30 * time.Second}
