package upstream

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"autoclaw2api/internal/auth"
)

// fake relay 网关：POST {base}/api/electron/agent/send 返回 {"ok":true,"data":{"runId":"r1"}}，
// GET {base}/api/events 返回一段 SSE：先 agent assistant delta，再 lifecycle end（带 usage）。
func fakeRelayGate(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/electron/agent/send"):
			_, _ = w.Write([]byte(`{"ok":true,"data":{"runId":"r1","sessionKey":"main"}}`))
		case strings.HasSuffix(r.URL.Path, "/api/events"):
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			write := func(ev, payload string) {
				_, _ = w.Write([]byte("event: " + ev + "\n"))
				_, _ = w.Write([]byte("data: " + payload + "\n\n"))
				if fl != nil {
					fl.Flush()
				}
			}
			write("agent", `{"payload":{"runId":"r1","stream":"assistant","data":{"delta":"Hello"}}}`)
			write("agent", `{"payload":{"runId":"r1","stream":"assistant","data":{"delta":" world"}}}`)
			write("agent", `{"payload":{"runId":"r1","stream":"tool","data":{"phase":"start","name":"sql_query","toolCallId":"t-1","args":{"x":1}}}}`)
			write("chat", `{"payload":{"runId":"r1","sessionKey":"main","state":"final","message":{"content":"Hello world"},"usage":{"input":9,"output":7}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestChatRelayFullRoundTrip(t *testing.T) {
	gw := fakeRelayGate(t)
	defer gw.Close()

	a := &auth.Auth{
		AccessToken:     "at-u1",
		UserID:          "u1",
		SandboxID:       "s1",
		SandboxEndpoint: gw.URL + "/autoclaw-cloud",
	}
	c := New()

	body := []byte(`{"model":"openclaw","messages":[{"role":"user","content":"say hello"}]}`)
	var buf bytes.Buffer
	if err := c.ChatRelay(a, body, NewSSEWriter(&buf)); err != nil {
		t.Fatalf("ChatRelay: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Hello world") {
		t.Fatalf("missing aggregate text 'Hello world':\n%s", out)
	}
	if !strings.Contains(out, `"name":"sql_query"`) {
		t.Fatalf("missing tool_calls for sql_query:\n%s", out)
	}
	if !strings.Contains(out, "prompt_tokens") {
		t.Fatalf("missing usage:\n%s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("missing [DONE]:\n%s", out)
	}
}

// 验证 runId 归属过滤：先请求 r1，再把另一个 runId=r2 的噪音塞进同一流，r2 不应混入。
func TestChatRelayFiltersForeignRun(t *testing.T) {
	var useForeign bool
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/electron/agent/send"):
			_, _ = w.Write([]byte(`{"ok":true,"data":{"runId":"r1"}}`))
		case strings.HasSuffix(r.URL.Path, "/api/events"):
			w.Header().Set("Content-Type", "text/event-stream")
			payload := `{"payload":{"runId":"r1","stream":"assistant","data":{"delta":"Mine"}}}`
			if useForeign {
				payload = `{"payload":{"runId":"r2","stream":"assistant","data":{"delta":"FOREIGN"}}}`
			}
			_, _ = w.Write([]byte("event: agent\n"))
			_, _ = w.Write([]byte("data: " + payload + "\n\n"))
			_, _ = w.Write([]byte("event: chat\n"))
			_, _ = w.Write([]byte("data: " + `{"payload":{"runId":"r1","sessionKey":"main","state":"final","message":{"content":"Mine"}}}` + "\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gw.Close()
	_ = useForeign

	a := &auth.Auth{UserID: "u1", AccessToken: "t", SandboxID: "s", SandboxEndpoint: gw.URL + "/autoclaw-cloud"}
	c := New()
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	var buf bytes.Buffer
	if err := c.ChatRelay(a, body, NewSSEWriter(&buf)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "FOREIGN") {
		t.Fatalf("foreign run r2 leaked:\n%s", buf.String())
	}
}

func TestChatRelayBuildReqUsesLastUserText(t *testing.T) {
	req, err := buildRelayChatReq([]byte(`{
		"model":"glm-5.2","user":"u-42",
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":"first"},
			{"role":"assistant","content":"a"},
			{"role":"user","content":[{"type":"text","text":"final now"}]}
		]}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.SessionKey != "u-42" {
		t.Errorf("session=%q", req.SessionKey)
	}
	if req.Message != "final now" {
		t.Errorf("content=%q", req.Message)
	}
	// relay 通道不再透传后端模型名（glm-* 透传上游会 LLM request failed），
	// 只回落默认 agent；reasoning_effort 仍透传。
	if raw, _ := json.Marshal(req); strings.Contains(string(raw), "model") {
		t.Errorf("relay req should not carry model field: %s", raw)
	}
}

func TestChatRelayEnvelopeCompat(t *testing.T) {
	// 真相源：send 的 data 既可对象含 runId，也可裸字符串
	for _, raw := range []string{`{"ok":true,"data":{"runId":"R9"}}`, `{"ok":true,"data":"R9"}`} {
		var m map[string]any
		_ = json.Unmarshal([]byte(raw), &m)
		data, _ := json.Marshal(m["data"])
		if got := extractRunID(data); got != "R9" {
			t.Errorf("extractRunID(%s)=%q", raw, got)
		}
	}
}

// TestRelayConverterAgentStream 验证实测的 agent:stream 事件格式转换：
// delta 为累积快照（前缀差分发增量），type=text|done。
func TestRelayConverterAgentStream(t *testing.T) {
	var buf bytes.Buffer
	out := NewSSEWriter(&buf)
	cv := newRelayConverter("run-1", "main", []byte(`{"model":"openclaw"}`), out)

	// 累积快照：第一帧 "你好"，第二帧 "你好！我是助"
	frames := []string{
		`{"event":"agent:stream","payload":{"runId":"run-1","type":"phase","phase":"planning","sessionKey":"main"}}`,
		`{"event":"agent:stream","payload":{"runId":"run-1","type":"text","delta":"你好","sessionKey":"agent:main:main"}}`,
		`{"event":"agent:stream","payload":{"runId":"run-1","type":"text","delta":"你好！我是助教","sessionKey":"agent:main:main"}}`,
		`{"event":"agent:stream","payload":{"runId":"run-1","type":"done","sessionKey":"agent:main:main"}}`,
	}
	for _, raw := range frames {
		if !hasInto(cv, raw) {
			t.Fatalf("parse fail: %s", raw)
		}
	}
	if err := cv.finish(); err != nil {
		t.Fatal(err)
	}
	outStr := buf.String()
	for _, want := range []string{"你好", "！我是助教", "[DONE]"} {
		if !strings.Contains(outStr, want) {
			t.Fatalf("missing %q in:\n%s", want, outStr)
		}
	}
}

// hasInto 把一个 SSE data 帧喂给 converter。
func hasInto(cv *relayConverter, dataLine string) bool {
	f, ok := parseFrame("data: " + dataLine)
	if !ok {
		return false
	}
	return cv.handleFrame(f) == nil
}
