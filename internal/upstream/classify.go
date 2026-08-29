// classify.go 上游错误分类，驱动 pool 冷却状态机。
package upstream

import (
	"fmt"
	"net/http"
	"strings"
)

// ErrKind 错误分类。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrHardCredit                 // 余额/配额不足 → 长冷却（等积分恢复）
	ErrSoftRate                   // 429 软限流 → 短冷却
	ErrSessionDead                // token 失效 / 未登录（410000）→ 禁用
	ErrNotFound                   // 404 上游偶发（含 OpenAI 端点未启用）→ 短冷却不累计
	ErrServer                     // 5xx 上游故障
	ErrClient                     // 其他 4xx / 业务错误
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Code   int64 // userapi 业务 code（可为 0）
	Msg    string
}

func (e *Error) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("upstream %s (http %d code %d): %s", e.Kind, e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// userapi 业务错误码。
const (
	CodeNotLoggedIn = 410000 // "This feature is available after logging in."
)

// hardMarkers 余额/配额不足关键词（大小写 + 中文双通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "not enough credit",
	"insufficient balance", "balance not enough", "limit reached", "usage limit",
	"余额不足", "额度不足", "积分不足", "积分用完", "额度用尽", "没有积分", "配额不足", "可用额度",
}

var sessionDeadMarkers = []string{
	"not logged in", "unauthorized", "invalid token", "token expired",
	"login required", "session expired", "offline user session", "auth.inject failed",
	"未登录", "登录已过期", "请先登录", "token 失效",
}

// Classify 按 HTTP 状态码 + body/业务 code 判定错误类别。
// code 为 userapi 业务 code（0 表示无）；body 为响应体或错误信息。
func Classify(status int, code int64, body string) ErrKind {
	if code == CodeNotLoggedIn {
		return ErrSessionDead
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		l := strings.ToLower(m)
		if strings.Contains(lower, l) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return ErrSessionDead
		}
	}
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
