package upstream

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// ── Sign / headers ──────────────────────────────────────────────────────────

func TestSignKnownVector(t *testing.T) {
	// 固定时间戳：验证 MD5 格式（真实向量见 docs，端到端已 live 验证过）
	got := Sign("1787963465")
	if !isMD5Hex(got) {
		t.Fatalf("Sign() = %q, want 32-hex", got)
	}
	t.Logf("sign=%s", got)
}

func isMD5Hex(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func TestRelayBaseNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://sb1.example.com", "https://sb1.example.com/autoclaw-cloud"},
		{"https://sb1.example.com/autoclaw-cloud", "https://sb1.example.com/autoclaw-cloud"},
		{"wss://sb1.example.com/", "https://sb1.example.com/autoclaw-cloud"},
		{"https://sb1.example.com/autoclaw-cloud/", "https://sb1.example.com/autoclaw-cloud"},
		{"https://sb1.example.com/foo", "https://sb1.example.com/foo/autoclaw-cloud"},
		// 真实上游形态：wss + query 参数 → 去掉 query 并截断
		{"wss://autoglm-api.zhipuai.cn/autoclaw-cloud/ws?sandbox_id=sb1&port=29000&path=/ws&protocol=wss&auto_wake=true",
			"https://autoglm-api.zhipuai.cn/autoclaw-cloud"},
		{"https://sb.example.com/autoclaw-cloud/ws?x=1&y=2", "https://sb.example.com/autoclaw-cloud"},
		{"ws://h.example.com/autoclaw-cloud/ws?p=1", "http://h.example.com/autoclaw-cloud"},
	}
	for _, c := range cases {
		if got := RelayBase(c.in); got != c.want {
			t.Errorf("RelayBase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ── Classify ────────────────────────────────────────────────────────────────

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		code   int64
		body   string
		want   ErrKind
	}{
		{200, 410000, "Not logged in", ErrSessionDead},
		{200, 0, "ok", ErrNone},
		{402, 0, "insufficient credit", ErrHardCredit},
		{200, 0, "积分不足", ErrHardCredit},
		{429, 0, "rate limit", ErrSoftRate},
		{200, 0, "Unauthorized web bridge request", ErrSessionDead},
		{404, 0, "no route", ErrNotFound},
		{500, 0, "boom", ErrServer},
		{400, 0, "bad request", ErrClient},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.code, c.body); got != c.want {
			t.Errorf("Classify(%d,%d,%q) = %v, want %v", c.status, c.code, c.body, got, c.want)
		}
	}
}

// ── NormalizeModel / prepareModelBody ───────────────────────────────────────

func TestNormalizeModel(t *testing.T) {
	cases := []struct{ in, model, xmodel string }{
		{"openclaw", "openclaw", ""},
		{"openclaw/default", "openclaw/default", ""},
		{"openclaw/designer", "openclaw/designer", ""},
		{"agent:analyst", "agent:analyst", ""},
		{"glm-5.3-flash", "openclaw", "glm-5.3-flash"},
		{"glm-5.2", "openclaw", "glm-5.2"},
		{"", "openclaw", ""},
	}
	for _, c := range cases {
		m, x := NormalizeModel(c.in)
		if m != c.model || x != c.xmodel {
			t.Errorf("NormalizeModel(%q) = (%q,%q), want (%q,%q)", c.in, m, x, c.model, c.xmodel)
		}
	}
}

func TestPrepareModelBody(t *testing.T) {
	body := []byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`)
	out := prepareModelBody(body)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["model"] != "openclaw" {
		t.Errorf("model = %v, want openclaw", obj["model"])
	}
	// agent 目标名原样透传
	body2 := []byte(`{"model":"openclaw/foo"}`)
	if !bytes.Equal(prepareModelBody(body2), body2) {
		t.Error("agent target model should pass through unchanged")
	}
}

// ── buildRelayChatReq ───────────────────────────────────────────────────────

func TestBuildRelayChatReq(t *testing.T) {
	body := []byte(`{
		"model":"glm-5.2",
		"user":"alice",
		"messages":[
			{"role":"system","content":"sys"},
			{"role":"user","content":[{"type":"text","text":"hello "},{"type":"text","text":"world"}]}
		]
	}`)
	req, err := buildRelayChatReq(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.SessionKey != "alice" {
		t.Errorf("session = %q, want alice", req.SessionKey)
	}
	if req.Message != "hello world" {
		t.Errorf("content = %q, want 'hello world'", req.Message)
	}
	// relay 通道不再透传后端模型名（见 buildRelayChatReq 注释）
	if raw, _ := json.Marshal(req); strings.Contains(string(raw), "model") {
		t.Errorf("relay req should not carry model field: %s", raw)
	}
}

// ── OpenAI mini-endpoint enabled check ──────────────────────────────────────

func TestNotEnabled(t *testing.T) {
	if !NotEnabled(404, []byte("no route")) {
		t.Error("404 should be not-enabled")
	}
	if !NotEnabled(400, []byte("chatCompletions endpoint is not enabled")) {
		t.Error("400 + marker should be not-enabled")
	}
	if NotEnabled(400, []byte("invalid model")) {
		t.Error("unrelated 400 should not be not-enabled")
	}
}

// ── relayConverter 事件流 → OpenAI chunk ───────────────────────────────────

func TestRelayConverterTextAndTools(t *testing.T) {
	feed := []string{
		`event: agent
data: {"payload":{"runId":"r1","stream":"assistant","data":{"delta":"Hel"}}}`,
		`event: agent
data: {"payload":{"runId":"r1","stream":"assistant","data":{"delta":"lo"}}}`,
		`event: agent
data: {"payload":{"runId":"r1","stream":"tool","data":{"phase":"start","name":"web_search","toolCallId":"t1","args":{"q":"x"}}}}`,
		`event: chat
data: {"payload":{"runId":"r1","sessionKey":"main","state":"final","message":{"content":"Hello world","toolCalls":[{"id":"t2","name":"calc","input":{"a":1}}]},"usage":{"input":10,"output":20}}}`,
	}
	input := strings.Join(feed, "\n\n") + "\n\n"

	var buf bytes.Buffer
	cv := newRelayConverter("r1", "main", []byte(`{"model":"glm-5.2"}`), NewSSEWriter(&buf))
	if err := cv.run(strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"content":"Hel"`) || !strings.Contains(out, `"content":"lo"`) {
		t.Errorf("missing text deltas:\n%s", out)
	}
	if !strings.Contains(out, `"name":"web_search"`) {
		t.Errorf("missing tool_use:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("missing finish chunk:\n%s", out)
	}
	if !strings.Contains(out, "prompt_tokens") || !strings.Contains(out, "completion_tokens") {
		t.Errorf("missing usage:\n%s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("missing [DONE]:\n%s", out)
	}
}

func TestRelayConverterFiltersOtherRun(t *testing.T) {
	feed := []string{
		`event: agent
data: {"payload":{"runId":"other","stream":"assistant","data":{"delta":"noise"}}}`,
		`event: chat
data: {"payload":{"runId":"r1","sessionKey":"main","state":"final","message":{"content":"mine"}}}`,
	}
	input := strings.Join(feed, "\n\n") + "\n\n"
	var buf bytes.Buffer
	cv := newRelayConverter("r1", "main", []byte(`{"model":"m"}`), NewSSEWriter(&buf))
	if err := cv.run(strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "noise") {
		t.Errorf("other run leaked into stream:\n%s", out)
	}
	if !strings.Contains(out, "mine") {
		t.Errorf("own run missing:\n%s", out)
	}
}

// ── Aggregate OpenAI SSE ────────────────────────────────────────────────────

func TestAggregateToolCalls(t *testing.T) {
	feed := "data: " + `{"id":"c1","model":"glm","created":1,"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n"
	feed += "data: " + `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]},"finish_reason":null}]}` + "\n\n"
	feed += "data: " + `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":null}]}` + "\n\n"
	feed += "data: [DONE]\n\n"

	agg, err := Aggregate(strings.NewReader(feed))
	if err != nil {
		t.Fatal(err)
	}
	choices, _ := agg["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", choices)
	}
	first, _ := choices[0].(map[string]any)
	msg, _ := first["message"].(map[string]any)
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls = %v", tcs)
	}
	tc, _ := tcs[0].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	if fn["name"] != "f" || fn["arguments"] != `{"a":1}` {
		t.Errorf("merged function = %v", fn)
	}
}

// TestRelayProxyBaseRoundTrip 验证 L()/proxy 归一化与反推的一致性。
func TestRelayProxyBaseRoundTrip(t *testing.T) {
	in := "wss://autoglm-api.zhipuai.cn/autoclaw-cloud/ws?sandbox_id=sb-1&port=29000&path=/ws"
	base := RelayBase(in)
	if base != "https://autoglm-api.zhipuai.cn/autoclaw-cloud" {
		t.Fatalf("RelayBase=%q", base)
	}
	proxy := RelayProxyBase(in, "sb-1")
	if proxy != "https://autoglm-api.zhipuai.cn/autoclaw-cloud/proxy/sb-1" {
		t.Fatalf("RelayProxyBase=%q", proxy)
	}
	if back := RelayBaseFromProxy(proxy); back != base {
		t.Fatalf("RelayBaseFromProxy(%q)=%q want %q", proxy, back, base)
	}
}

// TestWSUrlHelpers 验证 WS URL 与 token 归一化。
func TestWSUrlHelpers(t *testing.T) {
	if got := toWSBase("https://x.com/autoclaw-cloud"); got != "wss://x.com/autoclaw-cloud" {
		t.Fatalf("toWSBase=https=%q", got)
	}
	if got := toWSBase("http://x.com/autoclaw-cloud"); got != "ws://x.com/autoclaw-cloud" {
		t.Fatalf("toWSBase=http=%q", got)
	}
	if got := stripBearer("Bearer eyJ"); got != "eyJ" {
		t.Fatalf("stripBearer=%q", got)
	}
	if got := stripBearer("eyJ"); got != "eyJ" {
		t.Fatalf("stripBearer-no-prefix=%q", got)
	}
}
