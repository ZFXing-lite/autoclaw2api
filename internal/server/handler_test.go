package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"autoclaw2api/internal/auth"
	"autoclaw2api/internal/pool"
	"autoclaw2api/internal/upstream"
)

func serverAuth(uid string) *auth.Auth {
	return &auth.Auth{
		AccessToken: "at-" + uid,
		UserID:      uid,
		UserName:    uid,
		Region:      "cn",
		SandboxID:   "cloud-sb-" + uid,
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	}
}

// fakeGateway 模拟沙箱网关：
//   - wake/status 返回 ok，避免 prepareAccount 失败
//   - /v1/chat/completions 按 streaming 返回 SSE 或 JSON
func fakeGateway(t *testing.T, streaming bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.Contains(p, "/v1/sandboxes/") && strings.HasSuffix(p, "/wake"):
			_, _ = w.Write([]byte(`{"ok":true,"code":0}`))
		case strings.HasSuffix(p, "/v1/chat/completions"):
			if streaming {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: " + `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n"))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "c1", "object": "chat.completion", "model": "m",
				"choices": []any{map[string]any{"index": 0,
					"message":       map[string]any{"role": "assistant", "content": "hello"},
					"finish_reason": "stop"}},
			})
		case strings.HasSuffix(p, "/v1/models"):
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{
				map[string]any{"id": "openclaw"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
}

func buildHandler(gw *httptest.Server, mode string, streaming bool) *Handler {
	p := pool.New("")
	a := serverAuth("u1")
	a.SandboxEndpoint = gw.URL + "/autoclaw-cloud"
	p.Add(a)
	up := upstream.New()
	up.RelayBaseOverride = gw.URL + "/autoclaw-cloud"
	return NewHandler(Config{Pool: p, Upstream: up, Mode: mode})
}

func TestChatOpenAIStreamingPassThrough(t *testing.T) {
	gw := fakeGateway(t, true)
	defer gw.Close()
	h := buildHandler(gw, "openai", true)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"openclaw","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing DONE: %s", body)
	}
	if !strings.Contains(body, `"content":"hi"`) {
		t.Fatalf("missing content: %s", body)
	}
}

func TestChatNonStreamAggregates(t *testing.T) {
	gw := fakeGateway(t, false)
	defer gw.Close()
	h := buildHandler(gw, "openai", false)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"openclaw","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices=%v", choices)
	}
	if first, _ := choices[0].(map[string]any); first["message"] == nil {
		t.Fatalf("bad first choice: %v", first)
	}
}

// fakeRelayOnly 网关：OpenAI 端点 404（未启用），但 relay bridge 可用。
// mode=auto 时处理器应先试 openai → 失败 → 回退到 relay，仍返回 200 + [DONE]。
func fakeRelayOnly(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.Contains(p, "/v1/sandboxes/") && strings.HasSuffix(p, "/wake"):
			_, _ = w.Write([]byte(`{"ok":true,"code":0}`))
		case strings.HasSuffix(p, "/v1/chat/completions"):
			http.NotFound(w, r) // OpenAI 端点未启用
		case strings.HasSuffix(p, "/api/electron/agent/send"):
			_, _ = w.Write([]byte(`{"ok":true,"data":{"runId":"auto-fallback-run"}}`))
		case strings.HasSuffix(p, "/api/events"):
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: agent\n"))
			_, _ = w.Write([]byte("data: " + `{"payload":{"runId":"auto-fallback-run","stream":"assistant","data":{"delta":"relay-reply"}}}` + "\n\n"))
			_, _ = w.Write([]byte("event: chat\n"))
			_, _ = w.Write([]byte("data: " + `{"payload":{"runId":"auto-fallback-run","sessionKey":"main","state":"final","message":{"content":"relay-reply"}}}` + "\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestServerAutoFallsBackToRelay(t *testing.T) {
	gw := fakeRelayOnly(t)
	defer gw.Close()
	h := buildHandler(gw, "auto", false)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"openclaw","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "relay-reply") {
		t.Fatalf("relay reply missing: %s", rec.Body.String())
	}
}

func TestServerAuthRequired(t *testing.T) {
	p := pool.New("test")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "secret"})
	// healthz 是 liveness 不鉴权；用 /status（需鉴权）验证
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key status=%d want 401", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad key status=%d want 401", rec.Code)
	}
}

func TestServerHealthzNoAccounts(t *testing.T) {
	p := pool.New("test")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", rec.Code)
	}
}

// TestValidateChatBody 非法入参直接 400，不消耗池内账号。
func TestValidateChatBody(t *testing.T) {
	cases := []struct {
		body string
		want string // 期望错误子串；空串表示合法
	}{
		{`{"model":"x","messages":[{"role":"user","content":"hi"}]}`, ""},
		{`{bad json`, "invalid JSON body"},
		{`{"model":"x","messages":[]}`, "non-empty"},
		{`{"model":"x","messages":[{"role":"assistant","content":"hi"}]}`, "at least one user"},
		{`{"model":"x","messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}]}`, ""},
	}
	for _, c := range cases {
		got := validateChatBody([]byte(c.body))
		if c.want == "" {
			if got != "" {
				t.Errorf("validateChatBody(%s)=%q want empty", c.body, got)
			}
		} else if !strings.Contains(got, c.want) {
			t.Errorf("validateChatBody(%s)=%q want contain %q", c.body, got, c.want)
		}
	}
}
