// relay.go 沙箱网关 relay bridge 通道：
//
//	POST {relay}/api/electron/agent/send 发起 agent 对话
//	GET  {relay}/api/events（SSE）消费事件流
//
// 把 AutoClaw 网关事件（chat / agent 两类）转换为 OpenAI 流式 chunk，
// 与官方 web 客户端归一化逻辑对齐：文本增量、thinking、tool_calls、usage、done。
package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"autoclaw2api/internal/auth"
)

// SSEWriter 写 OpenAI SSE 行并 flush（stream 场景用 ResponseWriter）。
type SSEWriter struct {
	w  io.Writer
	fl http.Flusher
}

// NewSSEWriter 包装 writer；若实现 http.Flusher 则每行 flush。
func NewSSEWriter(w io.Writer) *SSEWriter {
	fl, _ := w.(http.Flusher)
	return &SSEWriter{w: w, fl: fl}
}

// WriteData 写一行 "data: <json>\n\n"。
func (s *SSEWriter) WriteData(payload string) error {
	if _, err := io.WriteString(s.w, "data: "+payload+"\n\n"); err != nil {
		return err
	}
	if s.fl != nil {
		s.fl.Flush()
	}
	return nil
}

// relayChatReq relay agent/send 请求参数。
// 真实网关校验（已实测）：message 必须是「字符串」，
// 匿名对象 {role,content} 会被拒绝：message is required for agent.send。
type relayChatReq struct {
	SessionKey string `json:"sessionKey"`
	Message    string `json:"message"`
	Thinking   string `json:"thinking,omitempty"`
}

// buildRelayChatReq 由 OpenAI 请求体构造 relay 请求：
//   - sessionKey 取 user 字段（缺省 "main"）
//   - message = 最后一条 user 消息的纯文本
//   - reasoning_effort → thinking
//
// 模型语义（已实测）：agent/send 的 model 字段只认 agent 目标名；把后端模型名
// （glm-5.3-flash / glm-5.2 / zai_auto 等）透传进去上游直接报 "LLM request failed"。
// 因此 relay 通道统一回落默认 agent（openclaw），后端模型选择仅由 OpenAI 兼容端点
// （x-openclaw-model 头）支持；客户端传任何模型名都不再导致上游失败。
func buildRelayChatReq(body []byte) (*relayChatReq, error) {
	var obj struct {
		Model           string `json:"model"`
		User            string `json:"user"`
		ReasoningEffort string `json:"reasoning_effort"`
		Messages        []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("parse body: %w", err)
	}
	req := &relayChatReq{SessionKey: strings.TrimSpace(obj.User)}
	if req.SessionKey == "" {
		req.SessionKey = "main"
	}
	// 取最后一条 user 文本（OpenAI 客户端通常把历史放 messages；session 上下文自行续聊）
	for i := len(obj.Messages) - 1; i >= 0; i-- {
		m := obj.Messages[i]
		if m.Role != "user" {
			continue
		}
		if t, ok := m.Content.(string); ok {
			req.Message = strings.TrimSpace(t)
			break
		}
		if parts, ok := m.Content.([]any); ok {
			req.Message = contentPartsText(parts)
			if req.Message != "" {
				break
			}
		}
	}
	if req.Message == "" {
		return nil, fmt.Errorf("no user text message found")
	}
	// model 不再透传（见上方注释）：任何客户端模型名都回落默认 agent。
	// 预留：若未来上游支持 model 字段传后端模型，可放开并加白名单校验。
	if strings.TrimSpace(obj.ReasoningEffort) != "" {
		req.Thinking = strings.TrimSpace(obj.ReasoningEffort)
	}
	return req, nil
}

// contentPartsText 把多模态 content 数组拼成纯文本。
func contentPartsText(parts []any) string {
	var sb strings.Builder
	for _, p := range parts {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		if typ == "text" {
			if t, ok := m["text"].(string); ok {
				sb.WriteString(t)
			}
		}
	}
	return strings.TrimSpace(sb.String())
}

// ChatRelay 通过 relay bridge 完成一次 agent 对话，把事件流转换为 OpenAI chunk 写入 out。
// 流程：先开 SSE 事件流 → POST agent/send 拿 runId → 消费事件（按 runId/session 过滤）→ 写 [DONE]。
// 返回 nil 表示对话完成；错误发生在尚未写任何 chunk 时（调用方回 5xx）。
func (c *Client) ChatRelay(a *auth.Auth, body []byte, out *SSEWriter) error {
	base := a.SandboxEndpoint
	if c.RelayBaseOverride != "" {
		base = RelayBase(c.RelayBaseOverride)
	}
	if base == "" {
		return fmt.Errorf("no sandbox endpoint for account %s", a.UserID)
	}
	req, err := buildRelayChatReq(body)
	if err != nil {
		return err
	}

	// 0) 建立 WebSocket 让设备（云龙虾）真正上线。
	// AutoClaw 仅 HTTP bridge（agent/send）会得到「云端设备未连接」；
	// 必须先经 wss 把设备带上线，HTTP bridge 才能执行。
	// WS 建立最佳努力：失败时回退（单元测试的假网关无 WS / 真实设备暂不可达时
	// 由下方 HTTP agent/send 返回准确错误，不因 WS 阻塞整次请求）。
	wsConn, wsErr := c.openDeviceWS(a)
	if wsErr != nil {
		log.Printf("warn: device online ws: %v (fallback to http bridge)", wsErr)
	} else if wsConn != nil {
		defer wsConn.Close()
	}

	// 1) 打开事件流（先于 send，避免错过 start 事件）
	eventsRC, err := c.openRelayEvents(a, base)
	if err != nil {
		return err
	}
	defer eventsRC.Close()

	// 2) POST agent/send
	sendBody, _ := json.Marshal(map[string]any{"args": []any{req}})
	postReq, err := http.NewRequest(http.MethodPost, base+"/api/electron/agent/send", bytes.NewReader(sendBody))
	if err != nil {
		return err
	}
	for k, vs := range RelayHeaders(a) {
		for _, v := range vs {
			postReq.Header.Add(k, v)
		}
	}
	postReq.Header.Set("Content-Type", "application/json")
	data, err := c.doRelayJSON(postReq)
	if err != nil {
		return err
	}
	runID := extractRunID(data)

	// 3) 消费事件并转换
	cv := newRelayConverter(runID, req.SessionKey, body, out)
	return cv.run(eventsRC)
}

// openDeviceWS 建立并鉴权 WebSocket，使设备上线。
// 只建立连接并保持打开（auth.inject 完成即视为设备接入），
// 事件回复仍从 /api/events（SSE）读取，故不在此消费 ws 帧。
// 当 c.RelayBaseOverride 非空（单元测试指向假 HTTP relay）时不建 WS，
// 返回 noop 连接即可，避免对测试网关发起真实 WS 握手阻塞。
func (c *Client) openDeviceWS(a *auth.Auth) (*wsConn, error) {
	if c.RelayBaseOverride != "" {
		return &wsConn{conn: nopNetConn{}}, nil
	}
	base := a.SandboxEndpoint
	// 真实设备 endpoint 恒为 "…/autoclaw-cloud/proxy/<sandbox_id>"；
	// 旧式裸测试 endpoint（无 /proxy/）跳过 WS，避免对假网关发起无谓握手。
	if !strings.Contains(base, "/proxy/") {
		return nil, nil
	}
	bare := RelayBaseFromProxy(base)
	if bare == "" || a.SandboxID == "" {
		return nil, fmt.Errorf("no sandbox endpoint/device for account %s", a.UserID)
	}
	token := stripBearer(a.AccessToken)
	wsURL := fmt.Sprintf("%s/v1/client/ws?device_id=%s&access_token=%s", toWSBase(bare), a.SandboxID, token)
	conn, err := dialWS(wsURL, wsDialOptions{Timeout: 30 * time.Second})
	if err != nil {
		return nil, err
	}
	if err := wsAuthInject(conn, a, token); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// openRelayEvents GET {relay}/api/events（SSE 长连接）。
func (c *Client) openRelayEvents(a *auth.Auth, base string) (io.ReadCloser, error) {
	req, err := http.NewRequest(http.MethodGet, base+"/api/events", nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range RelayHeaders(a) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, 0, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	return resp.Body, nil
}

// extractRunID 从 agent/send 响应提取 runId（兼容 runId / run_id / 裸字符串）。
func extractRunID(data json.RawMessage) string {
	if len(data) == 0 || string(data) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(data, &s) == nil {
		return strings.TrimSpace(s)
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	for _, k := range []string{"runId", "run_id"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 事件 → OpenAI chunk 转换
// ---------------------------------------------------------------------------

type relayConverter struct {
	runID   string
	session string
	model   string
	out     *SSEWriter

	id        string // chat completion id
	created   int64
	lastText  string
	toolSeq   int
	seenTools map[string]bool
	done      bool
}

func newRelayConverter(runID, session string, body []byte, out *SSEWriter) *relayConverter {
	var peek struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)
	return &relayConverter{
		runID:     runID,
		session:   session,
		model:     peek.Model,
		out:       out,
		id:        fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		created:   time.Now().Unix(),
		seenTools: map[string]bool{},
	}
}

// run 读取 SSE 事件流直到结束，写完必发一个 [DONE]。
func (cv *relayConverter) run(rc io.Reader) error {
	br := bufio.NewReaderSize(rc, 64*1024)
	buf := &strings.Builder{}
	frames := []frame{}
	var err error
	writeErr := error(nil)
	for {
		line, rerr := br.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "" {
				// 帧结束
				if f, ok := parseFrame(buf.String()); ok {
					frames = append(frames, f)
					if writeErr == nil {
						writeErr = cv.handleFrame(f)
					}
				}
				buf.Reset()
			} else {
				buf.WriteString(trimmed)
				buf.WriteString("\n")
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				err = rerr
			}
			break
		}
	}
	// 收尾
	if writeErr == nil {
		writeErr = cv.finish()
	}
	_ = err // 事件流 EOF 属正常结束；传输错误已在 handleFrame/finish 暴露
	return writeErr
}

type frame struct {
	event string
	data  string
}

func parseFrame(raw string) (frame, bool) {
	var f frame
	saw := false
	for _, line := range strings.Split(raw, "\n") {
		switch {
		case strings.HasPrefix(line, "event:"):
			f.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			saw = true
		case strings.HasPrefix(line, "data:"):
			f.data += strings.TrimPrefix(line, "data:")
			// 多行 data 为列表；SSE 规范多行拼接
			saw = true
		case line == "":
			continue
		default:
			// 注释/未知行忽略
			saw = true
		}
	}
	return f, saw
}

// handleFrame 解析单帧 JSON 并分发。
func (cv *relayConverter) handleFrame(f frame) error {
	if cv.done {
		return nil
	}
	var ev struct {
		Event   string          `json:"event"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(f.data), &ev); err != nil {
		return nil // 非事件帧忽略
	}
	if ev.Event == "" && f.event != "" {
		ev.Event = f.event
	}
	var payload map[string]any
	if len(ev.Payload) > 0 {
		_ = json.Unmarshal(ev.Payload, &payload)
	}
	if payload == nil {
		return nil
	}
	return cv.handle(ev.Event, payload)
}

// handle 判断事件归属（runId / session）并转换。
func (cv *relayConverter) handle(event string, payload map[string]any) error {
	runID, _ := payload["runId"].(string)
	sk, _ := payload["sessionKey"].(string)
	to, _ := payload["to"].(string)

	sameRun := runID != "" && cv.runID != "" && runID == cv.runID
	sameSession := (sk != "" && sk == cv.session) || (to != "" && to == cv.session)
	if !sameRun && !(sameSession && runID == "") {
		return nil
	}
	switch event {
	case "chat":
		return cv.handleChat(payload)
	case "agent":
		return cv.handleAgent(payload)
	case "agent:stream", "stream":
		return cv.handleAgentStream(payload)
	case "chat:message", "chat.delta":
		return cv.handleAgentStream(payload)
	}
	return nil
}

// handleAgentStream 处理 agent:stream 事件（实时看到的实测格式）：
//
//		payload: {runId, type:"phase"|"text"|"tool_call"|"tool_result"|"done"|"error", delta?, phase?, sessionKey}
//
//	  - text     delta 为累积快照（含前缀），emitText 做前缀差分发增量
//	  - phase    （"planning" 等）忽略，或作为 reasoning 阶段提示
//	  - done     会话结束，写 usage + [DONE]
//	  - error    出错
func (cv *relayConverter) handleAgentStream(payload map[string]any) error {
	if cv.done {
		return nil
	}
	typ, _ := payload["type"].(string)
	switch typ {
	case "error":
		return cv.emitError(payloadMsg(payload))
	case "done":
		return cv.emitUsageAndDone(extractUsage(payload))
	case "text":
		delta, _ := payload["delta"].(string)
		if delta == "" {
			txt, _ := payload["text"].(string)
			delta = txt
		}
		return cv.emitText(delta)
	case "thinking":
		th, _ := payload["text"].(string)
		if th == "" {
			th, _ = payload["delta"].(string)
		}
		return cv.emitThinking(th)
	case "phase":
		// 阶段提示（planning 等）；无文本增量则忽略
		return nil
	case "tool_call", "tool_use":
		name, _ := payload["name"].(string)
		id, _ := payload["id"].(string)
		if name == "" {
			name, _ = payload["tool"].(string)
		}
		if name != "" {
			return cv.emitToolUse(id, name, firstOf(payload["input"], payload["arguments"]))
		}
	case "tool_result":
		return nil
	}
	// 兼容：对象 message 里的 content
	if msg, ok := payload["message"].(map[string]any); ok {
		switch c := msg["content"].(type) {
		case string:
			return cv.emitText(c)
		case []any:
			return cv.emitContentParts(c)
		}
	}
	return nil
}

// emitText 发文本增量（前缀 diff，兼容快照式/增量式两种上游）。
func (cv *relayConverter) emitText(text string) error {
	if text == "" || text == cv.lastText {
		return nil
	}
	var delta string
	if strings.HasPrefix(text, cv.lastText) {
		delta = text[len(cv.lastText):]
	} else {
		delta = text
	}
	cv.lastText = text
	if delta == "" {
		return nil
	}
	return cv.emitChunk(map[string]any{
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"content": delta}, "finish_reason": nil,
		}},
	})
}

func (cv *relayConverter) emitThinking(text string) error {
	if text == "" {
		return nil
	}
	return cv.emitChunk(map[string]any{
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"reasoning_content": text}, "finish_reason": nil,
		}},
	})
}

// emitToolUse 发 tool_calls 增量（按 id 去重，index 递增）。
func (cv *relayConverter) emitToolUse(id, name string, input any) error {
	if id == "" {
		id = name + "-" + fmt.Sprint(cv.toolSeq)
	}
	if cv.seenTools[id] {
		return nil
	}
	cv.seenTools[id] = true
	args := ""
	if input != nil {
		raw, err := json.Marshal(input)
		if err == nil {
			args = string(raw)
		}
	}
	idx := cv.toolSeq
	cv.toolSeq++
	return cv.emitChunk(map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index": idx, "id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": args},
				}},
			},
			"finish_reason": nil,
		}},
	})
}

// emitUsageAndDone 发 usage（若有）与 done。
func (cv *relayConverter) emitUsageAndDone(usage map[string]any) error {
	var usageOut map[string]any
	if usage != nil {
		usageOut = map[string]any{
			"prompt_tokens":     numOr(usage["input"], usage["prompt_tokens"]),
			"completion_tokens": numOr(usage["output"], usage["completion_tokens"]),
			"total_tokens":      numOr(usage["total"], usage["total_tokens"]),
		}
	}
	chunk := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}
	if usageOut != nil {
		chunk["usage"] = usageOut
	}
	if err := cv.emitChunk(chunk); err != nil {
		return err
	}
	return cv.finish()
}

func (cv *relayConverter) emitError(msg string) error {
	// OpenAI 流式无法中途报错：把错误文本作为内容增量输出后正常收尾
	if msg != "" {
		_ = cv.emitText(msg)
	}
	return cv.emitChunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
}

// emitChunk 写一条 OpenAI chunk。
func (cv *relayConverter) emitChunk(chunk map[string]any) error {
	chunk["id"] = cv.id
	chunk["object"] = "chat.completion.chunk"
	chunk["created"] = cv.created
	chunk["model"] = cv.model
	raw, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	return cv.out.WriteData(string(raw))
}

// finish 保证写完 [DONE]（幂等）。
func (cv *relayConverter) finish() error {
	if cv.done {
		return nil
	}
	cv.done = true
	_, err := io.WriteString(cv.out.w, "data: [DONE]\n\n")
	if fl, ok := cv.out.w.(http.Flusher); ok {
		fl.Flush()
	}
	return err
}

// handleChat 处理 chat 事件（状态快照 + 增量内容）。
func (cv *relayConverter) handleChat(payload map[string]any) error {
	state, _ := payload["state"].(string)
	if state == "error" {
		return cv.emitError(payloadMsg(payload))
	}
	msg, _ := payload["message"].(map[string]any)
	if msg == nil {
		if state == "final" && isUsageEnd(payload) {
			return cv.emitUsageAndDone(extractUsage(payload))
		}
		return nil
	}
	switch content := msg["content"].(type) {
	case string:
		if err := cv.emitText(content); err != nil {
			return err
		}
	case []any:
		if err := cv.emitContentParts(content); err != nil {
			return err
		}
	}
	if tcs, ok := msg["toolCalls"].([]any); ok {
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			id, _ := tc["id"].(string)
			name, _ := tc["name"].(string)
			if name == "" {
				if fn, ok := tc["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
			}
			if name == "" {
				continue
			}
			if err := cv.emitToolUse(id, name, firstOf(tc["input"], tc["arguments"])); err != nil {
				return err
			}
		}
	}
	if state == "final" {
		return cv.emitUsageAndDone(extractUsage(firstObj(payload["usage"], payload)))
	}
	return nil
}

// emitContentParts 处理 content 数组（text/thinking/tool_use/tool_result）。
func (cv *relayConverter) emitContentParts(parts []any) error {
	for _, p := range parts {
		part, _ := p.(map[string]any)
		if part == nil {
			continue
		}
		typ, _ := part["type"].(string)
		switch typ {
		case "text":
			if t, ok := part["text"].(string); ok {
				if err := cv.emitText(t); err != nil {
					return err
				}
			}
		case "thinking":
			if t, ok := part["thinking"].(string); ok {
				if err := cv.emitThinking(t); err != nil {
					return err
				}
			}
		case "tool_use", "toolCall":
			name, _ := part["name"].(string)
			id, _ := part["id"].(string)
			if name != "" {
				if err := cv.emitToolUse(id, name, firstOf(part["input"], part["arguments"])); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// handleAgent 处理 agent 事件（流式分片：assistant/tool/lifecycle/error/compaction）。
func (cv *relayConverter) handleAgent(payload map[string]any) error {
	stream, _ := payload["stream"].(string)
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		data = payload
	}
	switch stream {
	case "assistant":
		if parts, ok := data["content"].([]any); ok {
			return cv.emitContentParts(parts)
		}
		if blocks, ok := data["blocks"].([]any); ok {
			return cv.emitContentParts(blocks)
		}
		text := firstStr(data["text"], data["delta"])
		return cv.emitText(text)
	case "tool":
		phase, _ := data["phase"].(string)
		switch phase {
		case "start":
			name, _ := data["name"].(string)
			if name == "" {
				return nil
			}
			id, _ := data["toolCallId"].(string)
			return cv.emitToolUse(id, name, firstOf(data["args"], data["input"]))
		}
		return nil
	case "lifecycle":
		phase, _ := data["phase"].(string)
		switch phase {
		case "end":
			return cv.emitUsageAndDone(extractUsage(firstObj(data["usage"], payload)))
		case "error":
			return cv.emitError(payloadMsg(data))
		}
		return nil
	case "error":
		return cv.emitError(payloadMsg(data))
	}
	return nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func payloadMsg(m map[string]any) string {
	if m == nil {
		return "agent request failed (upstream error event, no detail)"
	}
	for _, k := range []string{"message", "error", "msg", "detail", "text", "delta"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	// 兜底：把事件类型与 runId 拼进文案，便于定位是哪个请求/哪类失败
	typ, _ := m["type"].(string)
	phase, _ := m["phase"].(string)
	runID, _ := m["runId"].(string)
	code, _ := m["code"].(string)
	if typ == "" {
		typ = "error"
	}
	parts := []string{"agent request failed"}
	if code != "" {
		parts = append(parts, "code="+code)
	}
	if phase != "" {
		parts = append(parts, "phase="+phase)
	}
	if runID != "" {
		parts = append(parts, "run="+runID)
	}
	return strings.Join(parts, " ")
}

func firstObj(a any, b map[string]any) map[string]any {
	if m, ok := a.(map[string]any); ok {
		return m
	}
	return b
}

func firstOf(a, b any) any {
	if a != nil {
		return a
	}
	return b
}

func firstStr(a, b any) string {
	if s, ok := a.(string); ok && s != "" {
		return s
	}
	if s, ok := b.(string); ok {
		return s
	}
	return ""
}

func numOr(a, b any) int64 {
	if n, ok := a.(float64); ok {
		return int64(n)
	}
	if n, ok := b.(float64); ok {
		return int64(n)
	}
	return 0
}

// extractUsage 从事件/数据中提取 token 用量。
func extractUsage(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	if u, ok := m["usage"].(map[string]any); ok {
		if hasAnyUsage(u) {
			return u
		}
	}
	if hasAnyUsage(m) {
		return m
	}
	if r, ok := m["result"].(map[string]any); ok {
		if hasAnyUsage(r) {
			return r
		}
		if u, ok := r["usage"].(map[string]any); ok {
			return u
		}
	}
	return nil
}

func hasAnyUsage(m map[string]any) bool {
	for _, k := range []string{"input", "output", "total", "inputTokens", "outputTokens", "totalTokens", "prompt_tokens", "completion_tokens", "total_tokens"} {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// isUsageEnd 无 message 但 state=final 的 chat 事件是否携带 usage。
func isUsageEnd(payload map[string]any) bool {
	return extractUsage(payload) != nil
}
