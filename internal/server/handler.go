// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
// chat 通道：默认先试沙箱 OpenAI 兼容端点，未启用时自动回退 relay bridge（SSE）。
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"autoclaw2api/internal/auth"
	"autoclaw2api/internal/pool"
	"autoclaw2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	APIKey       string        // 空 = 不鉴权
	MaxRotate    int           // 单请求最多换号次数，默认 3
	HardCooldown time.Duration // 余额不足冷却，默认 12h
	SoftCooldown time.Duration // 429 冷却，默认 60s
	ErrThreshold int           // 连续其他错误冷却阈值，默认 3
	ErrCooldown  time.Duration // 错误冷却时长，默认 10m
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m
	// Mode 通道模式：auto（默认，OpenAI 端点失败回退 relay）/ openai / relay。
	Mode string
}

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.HardCooldown <= 0 {
		cfg.HardCooldown = 12 * time.Hour
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.Mode == "" {
		cfg.Mode = "auto"
	}
	// 仅在 auto 模式允许从 OpenAI 端点回退到 relay；openai / relay 单通道模式禁回退。
	if cfg.Upstream != nil {
		cfg.Upstream.AutoRelayFallback = cfg.Mode == "auto"
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") || strings.TrimPrefix(authz, "Bearer ") != h.cfg.APIKey {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy := h.cfg.Pool.Counts()
	status := http.StatusOK
	if healthy == 0 {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"healthy": healthy, "total": total})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled := h.cfg.Pool.CountsDetailed()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": h.cfg.Pool.List(),
		"total":    total,
		"healthy":  healthy,
		"cooling":  cooling,
		"disabled": disabled,
		"mode":     h.cfg.Mode,
	})
}

// ---------------------------------------------------------------------------
// 模型
// ---------------------------------------------------------------------------

// 静态模型表（动态接口失败时的回退）。
// openclaw 系列 = agent 目标名(默认/指定 agent)；glm-* / zai_auto = 后端模型，
// 经 x-openclaw-model 头在 OpenAI 端点与 relay 通道均可指定(已实测)。
var staticModels = []map[string]any{
	{"id": "openclaw", "object": "model", "created": 1787000000, "owned_by": "autoclaw", "context_length": 1000000, "description": "configured default agent"},
	{"id": "openclaw/default", "object": "model", "created": 1787000000, "owned_by": "autoclaw", "context_length": 1000000, "description": "configured default agent (stable alias)"},
	{"id": "glm-5.3-flash", "object": "model", "created": 1787000000, "owned_by": "autoclaw", "context_length": 1000000, "description": "backend model via x-openclaw-model"},
	{"id": "glm-5.2", "object": "model", "created": 1787000000, "owned_by": "autoclaw", "context_length": 1000000, "description": "backend model via x-openclaw-model"},
	{"id": "zai_auto", "object": "model", "created": 1787000000, "owned_by": "autoclaw", "context_length": 1000000, "description": "zai auto backend model via x-openclaw-model"},
}

var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

func (h *Handler) modelList() []map[string]any {
	if infos := h.fetchDynamicModels(); len(infos) > 0 {
		out := make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			out = append(out, map[string]any{
				"id":             mi.ID,
				"object":         "model",
				"created":        1787000000,
				"owned_by":       "autoclaw",
				"context_length": mi.ContextWindow,
			})
		}
		return out
	}
	return staticModels
}

// fetchDynamicModels 从池中任一健康账号拉沙箱 /v1/models，缓存 1h；失败 5min 负缓存。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	acct := h.cfg.Pool.Pick()
	if acct == nil {
		return nil
	}
	if st, _ := h.cfg.Pool.Status(acct.UserID); !st.Cooling && !st.Disabled {
		if err := h.prepareAccount(acct); err != nil {
			h.cfg.Pool.NoteError(acct.UserID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
		} else {
			infos, err := h.cfg.Upstream.FetchModels(acct)
			if err == nil && len(infos) > 0 {
				dynamicModelsCache.Lock()
				dynamicModelsCache.ids = infos
				dynamicModelsCache.fetched = time.Now()
				dynamicModelsCache.lastFail = time.Time{}
				dynamicModelsCache.Unlock()
				return infos
			}
			h.cfg.Pool.NoteError(acct.UserID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
		}
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.lastFail = time.Now()
	dynamicModelsCache.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// 聊天
// ---------------------------------------------------------------------------

// chatStat 请求级统计日志。
type chatStat struct {
	seq    int64
	start  time.Time
	uid    string
	status int
	stream bool
}

var chatSeq = new(atomic.Int64)

// validateChatBody 轻量校验请求体：返回错误消息（空串表示合法）。
// 结构校验：messages 非空、且至少一条 user 消息（补齐 text 由 buildRelayChatReq 处理）。
func validateChatBody(body []byte) string {
	var req struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "invalid JSON body"
	}
	if len(req.Messages) == 0 {
		return "messages must be a non-empty array"
	}
	for _, m := range req.Messages {
		if m.Role == "user" {
			return ""
		}
	}
	return "messages must contain at least one user message"
}

func newChatStat(start time.Time, stream bool) *chatStat {
	return &chatStat{seq: chatSeq.Add(1), start: start, stream: stream}
}

func (st *chatStat) done() {
	if st.uid == "" {
		st.uid = "-"
	}
	model := "-"
	log.Printf("| #%03d | %s | %s | %s | %d | uid=%s | total=%s |",
		st.seq, st.start.Format("15:04:05"), model, boolStr(st.stream), st.status, st.uid, time.Since(st.start).Round(time.Millisecond))
}

func boolStr(b bool) string {
	if b {
		return "stream"
	}
	return "sync"
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)

	// 客户端入参校验：非法请求（无 messages / 无 user 文本）直接 400，
	// 不要占用池内账号判断再 503，也避免消耗账号的冷却额度。
	if msgErr := validateChatBody(body); msgErr != "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", msgErr)
		return
	}

	st := newChatStat(time.Now(), peek.Stream)
	defer st.done()

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcluding(tried)
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UserID
		tried[acct.UserID] = true

		if err := h.prepareAccount(acct); err != nil {
			lastErr = err
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				h.cfg.Pool.Disable(acct.UserID, "session dead")
			} else if errors.As(err, &ue) && ue.Kind == upstream.ErrHardCredit {
				h.cfg.Pool.Cooldown(acct.UserID, pool.CoolHard, h.cfg.HardCooldown, "余额不足")
			} else if errors.As(err, &ue) && ue.Kind == upstream.ErrSoftRate {
				h.cfg.Pool.Cooldown(acct.UserID, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
			} else {
				h.cfg.Pool.NoteError(acct.UserID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			}
			continue
		}

		// 通道 1：OpenAI 兼容端点
		if h.cfg.Mode != "relay" {
			handled, s, terr := h.chatViaOpenAI(w, r, acct, body, peek.Stream)
			if handled {
				st.status = s
				return
			}
			if terr != nil {
				lastErr = terr
				var ue *upstream.Error
				if errors.As(terr, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UserID, "session dead")
				} else if errors.As(terr, &ue) && ue.Kind == upstream.ErrHardCredit {
					h.cfg.Pool.Cooldown(acct.UserID, pool.CoolHard, h.cfg.HardCooldown, "余额不足")
				} else if errors.As(terr, &ue) && ue.Kind == upstream.ErrSoftRate {
					h.cfg.Pool.Cooldown(acct.UserID, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
				} else {
					h.cfg.Pool.NoteError(acct.UserID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
				}
				continue // 轮转换号
			}
			// 未处理（端点未启用 → 回退 relay）
		}

		// 通道 2：relay bridge
		if err := h.chatViaRelay(w, acct, body, peek.Stream); err != nil {
			lastErr = err
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				h.cfg.Pool.Disable(acct.UserID, "session dead")
			} else if errors.As(err, &ue) && ue.Kind == upstream.ErrHardCredit {
				h.cfg.Pool.Cooldown(acct.UserID, pool.CoolHard, h.cfg.HardCooldown, "余额不足")
			} else if errors.As(err, &ue) && ue.Kind == upstream.ErrSoftRate {
				h.cfg.Pool.Cooldown(acct.UserID, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
			} else {
				h.cfg.Pool.NoteError(acct.UserID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			}
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UserID)
		st.status = http.StatusOK
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	st.status = http.StatusServiceUnavailable
}

// chatViaOpenAI 走 OpenAI 端点。handled=true 表示已写出响应（成功或失败分类）。
// 端点未启用时 handled=false、terr=nil，由调用方回退 relay。
func (h *Handler) chatViaOpenAI(w http.ResponseWriter, r *http.Request, a *auth.Auth, body []byte, wantStream bool) (handled bool, status int, terr error) {
	rc, st, respBody, ct, err := h.cfg.Upstream.ChatOpenAI(a, body)
	if err != nil {
		return false, 0, err // 传输层：换号重试
	}
	if st >= 400 {
		if h.cfg.Upstream.AutoRelayFallback && upstream.NotEnabled(st, respBody) {
			return false, 0, nil // 回退 relay
		}
		kind := upstream.Classify(st, 0, string(respBody))
		return true, st, &upstream.Error{Kind: kind, Status: st, Msg: string(respBody)}
	}
	defer rc.Close()
	h.cfg.Pool.NoteSuccess(a.UserID)

	if isEventStream(ct) {
		if wantStream {
			if serr := upstream.Stream(w, rc); serr != nil {
				log.Printf("chat stream uid=%s: write: %v", a.UserID, serr)
			}
			return true, http.StatusOK, nil
		}
		agg, aerr := upstream.Aggregate(rc)
		if aerr != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", aerr.Error())
			return true, http.StatusBadGateway, nil
		}
		writeJSON(w, http.StatusOK, agg)
		return true, http.StatusOK, nil
	}

	// 上游返回完整 JSON（非流式）
	raw, rerr := io.ReadAll(io.LimitReader(rc, 16<<20))
	if rerr != nil {
		return true, http.StatusBadGateway, rerr
	}
	if !wantStream {
		var resp map[string]any
		if json.Unmarshal(raw, &resp) != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", "invalid JSON from upstream")
			return true, http.StatusBadGateway, nil
		}
		writeJSON(w, http.StatusOK, resp)
		return true, http.StatusOK, nil
	}
	// 客户端要流式但上游给完整响应：包成单 chunk + [DONE]
	writeSSEHeaders(w)
	_ = upstream.WriteSingleChunkSSE(w, raw)
	return true, http.StatusOK, nil
}

// chatViaRelay 走 relay bridge（事件流 → OpenAI chunk）。
func (h *Handler) chatViaRelay(w http.ResponseWriter, a *auth.Auth, body []byte, wantStream bool) error {
	if wantStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		return h.cfg.Upstream.ChatRelay(a, body, upstream.NewSSEWriter(w))
	}
	// 非流式：本地转换后聚合
	var buf bytes.Buffer
	if err := h.cfg.Upstream.ChatRelay(a, body, upstream.NewSSEWriter(&buf)); err != nil {
		return err
	}
	if buf.Len() == 0 {
		return errors.New("relay returned empty stream")
	}
	agg, err := upstream.Aggregate(&buf)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, agg)
	return nil
}

func isEventStream(ct string) bool {
	return strings.Contains(ct, "text/event-stream")
}

// prepareAccount token 预刷新 + 沙箱确保 + 唤醒；失败返回分类错误。
func (h *Handler) prepareAccount(a *auth.Auth) error {
	if a.NeedsRefresh(h.cfg.RefreshSkew) {
		if a.RefreshToken == "" {
			return &upstream.Error{Kind: upstream.ErrSessionDead, Status: 401, Msg: "no refreshToken — re-login required"}
		}
		if err := h.cfg.Upstream.RefreshToken(a); err != nil {
			return err
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("prepare uid=%s: save auth failed: %v", a.UserID, err)
		}
	}
	_, _, err := h.cfg.Upstream.EnsureSandbox(a)
	if err != nil {
		return err
	}
	_ = a.SaveAtomic() // 缓存 sandboxId/endpoint
	if err := h.cfg.Upstream.WakeSandbox(a, a.SandboxID); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
