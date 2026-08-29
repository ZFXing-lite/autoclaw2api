// openai.go 沙箱网关的 OpenAI 兼容端点（/v1/chat/completions、/v1/models）。
// AutoClaw 云端沙箱即 OpenClaw gateway，若其配置启用了 chatCompletions 端点，
// 则直接透传标准 OpenAI 协议；未启用时回退 relay bridge（见 relay.go）。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"autoclaw2api/internal/auth"
)

// ModelInfo 模型信息（来自上游 /v1/models，或静态表）。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64
	MaxTokens     int64
}

// ModelTarget 是否 OpenClaw agent 目标名（openclaw / openclaw/default / openclaw/<id> / agent:<id>）。
func ModelTarget(model string) bool {
	m := strings.TrimSpace(strings.ToLower(model))
	if m == "openclaw" || m == "openclaw/default" {
		return true
	}
	return strings.HasPrefix(m, "openclaw/") || strings.HasPrefix(m, "openclaw:") || strings.HasPrefix(m, "agent:")
}

// NormalizeModel 归一化模型名：
//   - agent 目标名 → 原样
//   - 其他（如 glm-5.3-flash）→ 视为后端模型：model=openclaw + x-openclaw-model=原值
//
// 返回 (model 字段值, x-openclaw-model 头值或空)。
func NormalizeModel(model string) (string, string) {
	if ModelTarget(model) {
		return model, ""
	}
	return "openclaw", model
}

// ChatOpenAI 调沙箱网关 OpenAI 兼容端点。
// 返回 (响应体流, status, 非 2xx 响应体, Content-Type, 传输错误)。
// 2xx 时 rc 非空（调用方负责 Close）；非 2xx 时 rc=nil、respBody=上游响应体。
func (c *Client) ChatOpenAI(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, contentType string, err error) {
	base := a.SandboxEndpoint
	if c.RelayBaseOverride != "" {
		base = RelayBase(c.RelayBaseOverride)
	}
	if base == "" {
		return nil, 0, nil, "", fmt.Errorf("no sandbox endpoint for account %s", a.UserID)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(prepareModelBody(body)))
	if err != nil {
		return nil, 0, nil, "", err
	}
	headers := RelayHeaders(a)
	if headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", "application/json")
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	applyOpenAIHeaders(req, body)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, nil, "", err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		ct := resp.Header.Get("Content-Type")
		resp.Body.Close()
		return nil, resp.StatusCode, raw, ct, nil
	}
	return resp.Body, resp.StatusCode, nil, resp.Header.Get("Content-Type"), nil
}

// applyOpenAIHeaders 依据请求体设置可选的 x-openclaw-* 头。
func applyOpenAIHeaders(req *http.Request, body []byte) {
	var peek struct {
		Model string `json:"model"`
		User  string `json:"user"`
	}
	if json.Unmarshal(body, &peek) != nil {
		return
	}
	if _, xmodel := NormalizeModel(peek.Model); xmodel != "" {
		req.Header.Set("x-openclaw-model", xmodel)
	}
	if u := strings.TrimSpace(peek.User); u != "" && !reservedSessionKey(u) {
		req.Header.Set("x-openclaw-session-key", "webchat:autoclaw2api:"+u)
	}
}

// prepareModelBody 把非 agent 目标模型改写为 model=openclaw + x-openclaw-model 头；
// 已是 agent 目标名则原样返回。
func prepareModelBody(body []byte) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	m, _ := obj["model"].(string)
	if _, xmodel := NormalizeModel(m); xmodel == "" {
		return body
	}
	obj["model"] = "openclaw"
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// reservedSessionKey OpenClaw 保留的内部 session 命名空间。
func reservedSessionKey(k string) bool {
	k = strings.TrimSpace(strings.ToLower(k))
	return strings.HasPrefix(k, "subagent:") || strings.HasPrefix(k, "cron:") || strings.HasPrefix(k, "acp:")
}

// NotEnabled 判断 OpenAI 兼容端点是否未启用（网关未开 chatCompletions）。
// 命中时调用方可回退 relay bridge。
func NotEnabled(status int, body []byte) bool {
	switch status {
	case http.StatusNotFound, http.StatusNotImplemented:
		return true
	case http.StatusBadRequest:
		lower := strings.ToLower(string(body))
		for _, m := range []string{"chatcompletions", "endpoint", "not enabled", "disabled"} {
			if strings.Contains(lower, m) {
				return true
			}
		}
	}
	return false
}

// FetchModels 拉取沙箱 /v1/models（agent 目标列表）。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	base := a.SandboxEndpoint
	if c.RelayBaseOverride != "" {
		base = RelayBase(c.RelayBaseOverride)
	}
	if base == "" {
		return nil, fmt.Errorf("no sandbox endpoint for account %s", a.UserID)
	}
	req, err := http.NewRequest(http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range RelayHeaders(a) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	var out []ModelInfo
	for _, m := range env.Data {
		if m.ID != "" {
			out = append(out, ModelInfo{ID: m.ID})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}
