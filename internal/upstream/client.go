// client.go AutoClaw Cloud userapi 客户端：
// 登录/刷新/用户信息/积分/沙箱管理，全部走签名请求头 + 统一业务信封。
package upstream

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"autoclaw2api/internal/auth"
)

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code  int64           `json:"code"`
	Msg   string          `json:"msg"`
	Time  int64           `json:"time"`
	Trace string          `json:"trace"`
	Data  json.RawMessage `json:"data"`
}

// NewTraceID 生成 X-Trace-Id（16 字节随机 hex）。
func NewTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client
	// UserAPIBaseOverride 非空时覆盖 region 默认基址（测试/自定义网关）。
	UserAPIBaseOverride string
	// RelayBaseOverride 非空时覆盖沙箱 endpoint 归一化后的 relay 基址（测试）。
	RelayBaseOverride string
	// AutoRelayFallback 为 true 时，OpenAI 端点不可用自动回退 relay bridge（默认 true）。
	AutoRelayFallback bool
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		HTTP:              &http.Client{Timeout: 120 * time.Second, Transport: tr},
		AutoRelayFallback: true,
	}
}

func (c *Client) userAPIBase(a *auth.Auth) string {
	if c.UserAPIBaseOverride != "" {
		return strings.TrimRight(c.UserAPIBaseOverride, "/")
	}
	return UserAPIBase(a.Region)
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, 0, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Code, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Code: env.Code, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// authPayload 通用 source_id/device_id 包体。
func authPayload(a *auth.Auth, extra map[string]any) map[string]any {
	p := map[string]any{
		"source_id": "autoclaw",
		"device_id": a.DeviceID,
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// ---------------------------------------------------------------------------
// 登录（供 cmd/login 使用；无既有凭证）
// ---------------------------------------------------------------------------

// LoginResult 登录返回的凭证。
type LoginResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	UserID       string `json:"user_id"`
	UserName     string `json:"user_name"`
	Phone        string `json:"phone"`
	FirstLogin   bool   `json:"first_login"`
	WebFirst     bool   `json:"web_first_login"`
}

// SendCode 发送短信验证码。region 决定 userapi 基址；deviceID 为调用方生成的新设备 UUID。
func (c *Client) SendCode(region, deviceID, phone string) error {
	var holder auth.Auth // 仅取 Region/DeviceID 用于拼包体
	holder.Region = region
	holder.DeviceID = deviceID
	req, err := c.newUserAPIRequest(region, http.MethodPost, "/userapi/v1/agent-send-code", authPayload(&holder, map[string]any{"phone": phone}))
	if err != nil {
		return err
	}
	_, err = c.doJSON(req)
	return err
}

// Login 用手机号+验证码换取凭证。成功后返回 LoginResult，调用方落盘。
func (c *Client) Login(region, deviceID, phone, code string) (*LoginResult, error) {
	var holder auth.Auth
	holder.Region = region
	holder.DeviceID = deviceID
	// code 必须为数字：官方 web 用 Number(String(code).trim()) 转数字后发送，
	// 字符串形式会被后端以 400001 拒绝。
	codeNum := int64(0)
	if n, err := parseCode(code); err != nil {
		return nil, fmt.Errorf("invalid code %q: %w", code, err)
	} else {
		codeNum = n
	}
	req, err := c.newUserAPIRequestToken(region, "", http.MethodPost, "/userapi/v1/agent-login/", authPayload(&holder, map[string]any{
		"phone":    phone,
		"code":     codeNum,
		"platform": "web",
	}))
	if err != nil {
		return nil, err
	}
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var lr LoginResult
	if err := json.Unmarshal(data, &lr); err != nil {
		return nil, fmt.Errorf("login parse: %w", err)
	}
	if lr.AccessToken == "" {
		return nil, fmt.Errorf("login_failed: no access_token in response")
	}
	return &lr, nil
}

// parseCode 把验证码解析为 int64（兼容 "457950" 与 457950）。
func parseCode(code string) (int64, error) {
	s := strings.TrimSpace(code)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// 凭证期（持锁改写 Auth 字段；调用方负责 SaveAtomic）
// ---------------------------------------------------------------------------

// RefreshToken 用 refresh_token 刷新 access token；成功时更新 a 的字段。
// 全程持 a 锁，防止并发 SaveAtomic 读半更新 token。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	req, err := c.newUserAPIRequestToken(a.Region, a.AccessToken, http.MethodPost, "/userapi/v1/refresh", authPayload(a, map[string]any{
		"refresh_token": a.RefreshToken,
	}))
	if err != nil {
		return err
	}
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no access_token in response — re-login required")
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	return nil
}

// UserProfile 拉取用户信息（用于校验 token 有效性）。
func (c *Client) UserProfile(a *auth.Auth) (map[string]any, error) {
	req, err := c.newUserAPIRequestToken(a.Region, a.AccessToken, http.MethodPost, "/userapi/v1/user-profile", authPayload(a, nil))
	if err != nil {
		return nil, err
	}
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("user-profile parse: %w", err)
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// 积分
// ---------------------------------------------------------------------------

// CheckPoints 查询账号积分余额（v2 wallets 聚合 total_balance）。
func (c *Client) CheckPoints(a *auth.Auth) (int64, error) {
	req, err := c.newUserAPIRequestToken(a.Region, a.AccessToken, http.MethodGet, "/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw", nil)
	if err != nil {
		return 0, err
	}
	data, err := c.doJSON(req)
	if err != nil {
		return 0, err
	}
	var resp struct {
		TotalBalance json.RawMessage `json:"total_balance"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("wallets parse: %w", err)
	}
	return rawBalance(resp.TotalBalance), nil
}

// WalletScope 钱包分类快照（分 scope 的余额）。
type WalletScope struct {
	Scope   string `json:"scope"`
	Balance int64  `json:"balance"`
}

// WalletScopes 查询按用途分类的钱包余额，用于确认「每日赠送额度（daily）」情况。
// 端点：GET /agent-assetmgr/api/v1/wallet-instances?wallet_type=all&wallet_scope=all
// 真实响应为对象：{"next_page_token":"","total_balance":..,"wallet_instances":[{wallet_type,wallet_scope,balance,...}]}
func (c *Client) WalletScopes(a *auth.Auth) ([]WalletScope, error) {
	req, err := c.newUserAPIRequestToken(a.Region, a.AccessToken, http.MethodGet,
		"/agent-assetmgr/api/v1/wallet-instances?wallet_type=all&wallet_scope=all", nil)
	if err != nil {
		return nil, err
	}
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	// 兼容顶层数组（上游可能变化）
	if data[0] == '[' {
		var list []struct {
			Scope      string          `json:"scope"`
			WalletType string          `json:"wallet_type"`
			BalanceAmt json.RawMessage `json:"balance_amount"`
		}
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, fmt.Errorf("wallet-instances parse: %w", err)
		}
		out := make([]WalletScope, 0, len(list))
		for _, it := range list {
			out = append(out, WalletScope{Scope: it.WalletType, Balance: rawBalance(it.BalanceAmt)})
		}
		return out, nil
	}
	var env struct {
		TotalBalance json.RawMessage `json:"total_balance"`
		Instances    []struct {
			WalletType  string          `json:"wallet_type"`
			WalletScope string          `json:"wallet_scope"`
			Balance     json.RawMessage `json:"balance"`
		} `json:"wallet_instances"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("wallet-instances parse: %w", err)
	}
	out := make([]WalletScope, 0, len(env.Instances))
	for _, it := range env.Instances {
		out = append(out, WalletScope{Scope: it.WalletType, Balance: rawBalance(it.Balance)})
	}
	return out, nil
}

// rawBalance 兼容 number/字符串的数字解析。
func rawBalance(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return int64(n)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		var n2 float64
		if _, err := fmt.Sscanf(s, "%f", &n2); err == nil {
			return int64(n2)
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// 沙箱
// ---------------------------------------------------------------------------

// Sandbox 沙箱信息（字段与 /agentdr/v2/assistant/sandbox/list 对齐）。
type Sandbox struct {
	SandboxID       string `json:"sandbox_id"`
	SandboxName     string `json:"sandbox_name"`
	SandboxStatus   string `json:"sandbox_status"`
	RuntimeStatus   string `json:"runtime_status"`
	SandboxEndpoint string `json:"sandbox_endpoint"`
	EndTimestamp    int64  `json:"end_timestamp"`
	UpdateTimestamp int64  `json:"update_timestamp"`
}

// ListSandboxes 列出账号沙箱。
func (c *Client) ListSandboxes(a *auth.Auth) ([]Sandbox, error) {
	req, err := c.newUserAPIRequestToken(a.Region, a.AccessToken, http.MethodGet, "/agentdr/v2/assistant/sandbox/list", nil)
	if err != nil {
		return nil, err
	}
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		SandboxList []Sandbox `json:"sandbox_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		// 兼容直接数组
		var list []Sandbox
		if json.Unmarshal(data, &list) == nil {
			return list, nil
		}
		return nil, fmt.Errorf("sandbox list parse: %w", err)
	}
	return resp.SandboxList, nil
}

// ApplySandbox 申请沙箱（sandbox_name 用官方默认值）。
func (c *Client) ApplySandbox(a *auth.Auth) error {
	body := map[string]any{"sandbox_name": "default"}
	req, err := c.newUserAPIRequestToken(a.Region, a.AccessToken, http.MethodPost, "/agentdr/v2/assistant/sandbox/apply", body)
	if err != nil {
		return err
	}
	_, err = c.doJSON(req)
	return err
}

// EnsureSandbox 确保账号存在沙箱并与预期一致：
//  1. 缓存可用（SandboxID 非空且未过期）→ 用缓存 sandboxId 推导 relay base 返回
//  2. 拉取 sandbox list；非空 → 取第一个可用
//  3. 空 → apply 后重拉
//
// SandboxEndpoint 以「含 /proxy/<sandbox_id> 前缀」的完整 relay base 保存
// （官方 web 端 _e() 逻辑）；旧版缓存只有裸 base 时也会按 sandboxId 重算。
// 成功时把 sandboxId/endpoint/endTimestamp 写回 a（不负责落盘，调用方 SaveAtomic）。
func (c *Client) EnsureSandbox(a *auth.Auth) (string, string, error) {
	now := time.Now().Unix()
	if a.SandboxID != "" && a.SandboxEndpoint != "" && (a.EndTimestamp == 0 || a.EndTimestamp > now) {
		// 缓存命中：RelayBase 会把任何 endpoint（含旧版裸 base 或 proxy 形式）归一为
		// 裸 /autoclaw-cloud base，再按 sandboxId 拼回 /proxy/<id>，保证格式一致。
		a.SandboxEndpoint = RelayProxyBase(RelayBase(a.SandboxEndpoint), a.SandboxID)
		return a.SandboxID, a.SandboxEndpoint, nil
	}
	list, err := c.ListSandboxes(a)
	if err != nil {
		return "", "", err
	}
	if len(list) == 0 {
		if err := c.ApplySandbox(a); err != nil {
			return "", "", err
		}
		list, err = c.ListSandboxes(a)
		if err != nil {
			return "", "", err
		}
	}
	if len(list) == 0 {
		return "", "", fmt.Errorf("no sandbox available after apply")
	}
	pick := list[0]
	for _, s := range list {
		if !sandboxExpired(s.EndTimestamp) && (pick.SandboxID == "" || sandboxExpired(pick.EndTimestamp)) {
			pick = s
		}
	}
	a.SandboxID = pick.SandboxID
	a.SandboxEndpoint = RelayProxyBase(pick.SandboxEndpoint, pick.SandboxID)
	a.EndTimestamp = pick.EndTimestamp
	if a.SandboxID == "" {
		return "", "", fmt.Errorf("sandbox response missing sandbox_id")
	}
	return a.SandboxID, a.SandboxEndpoint, nil
}

func sandboxExpired(ts int64) bool {
	return ts != 0 && ts <= time.Now().Unix()
}

// SandboxStatus 查询沙箱运行时状态。
type SandboxStatus struct {
	RuntimeStatus string `json:"runtimeStatus"`
	SandboxStatus string `json:"sandboxStatus"`
}

// SandboxStatusOf 查询沙箱状态（用于 wake 前判断），挂在裸 base 下。
func (c *Client) SandboxStatusOf(a *auth.Auth, sandboxID string) (*SandboxStatus, error) {
	req, err := c.newBareRelayRequest(a, http.MethodGet, "/v1/sandboxes/"+sandboxID+"/status", nil)
	if err != nil {
		return nil, err
	}
	data, err := c.doRelayJSON(req)
	if err != nil {
		return nil, err
	}
	var out SandboxStatus
	_ = json.Unmarshal(data, &out)
	return &out, nil
}

// WakeSandbox 唤醒沙箱（autoscaling 场景下沙箱可能休眠），挂在裸 base 下。
func (c *Client) WakeSandbox(a *auth.Auth, sandboxID string) error {
	body := map[string]any{
		"access_hold_ms":               600000,
		"reason":                       "autoclaw2api chat request",
		"apply_business_busy_cooldown": true,
	}
	req, err := c.newBareRelayRequest(a, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/wake", body)
	if err != nil {
		return err
	}
	_, err = c.doRelayJSON(req)
	return err
}

// ---------------------------------------------------------------------------
// 请求构造
// ---------------------------------------------------------------------------

// newUserAPIRequest 构造 userapi 请求（+签名头）。
func (c *Client) newUserAPIRequest(region, method, path string, body any) (*http.Request, error) {
	return c.newUserAPIRequestToken(region, "", method, path, body)
}

// newUserAPIRequestToken 同上，但允许单独提供 Bearer token 覆盖
// （登录前无凭证的接口用空 token 即可，Login 响应后由调用方落盘）。
func (c *Client) newUserAPIRequestToken(region, token, method, path string, body any) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(raw)
	}
	url := UserAPIBase(region) + path
	if c.UserAPIBaseOverride != "" {
		url = strings.TrimRight(c.UserAPIBaseOverride, "/") + path
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return nil, err
	}
	for k, vs := range UserAPIHeaders(token, region) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return req, nil
}

// doRelayJSON 发向沙箱网关的 JSON 请求。
// 兼容两种信封：{ok,data,error} 与 {code,msg,data}；无 data 字段时返回整个 body。
func (c *Client) doRelayJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, 0, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env struct {
		Ok    *bool           `json:"ok"`
		Code  *int64          `json:"code"`
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
		Msg   string          `json:"msg"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("relay parse: %w (body: %s)", err, truncate(string(raw), 120))
	}
	msg := env.Error
	if msg == "" {
		msg = env.Msg
	}
	if (env.Ok != nil && !*env.Ok) || (env.Code != nil && *env.Code != 0) {
		kind := Classify(resp.StatusCode, derefCode(env.Code), msg)
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Code: derefCode(env.Code), Msg: msg}
	}
	if len(env.Data) > 0 && string(env.Data) != "null" {
		return env.Data, nil
	}
	return raw, nil
}

func derefCode(c *int64) int64 {
	if c == nil {
		return 0
	}
	return *c
}

// newRelayRequest 构造沙箱网关请求，base 取 a.SandboxEndpoint（含 /proxy/<id> 前缀）。
func (c *Client) newRelayRequest(a *auth.Auth, method, path string, body any) (*http.Request, error) {
	return c.newRelayRequestBase(a, a.SandboxEndpoint, method, path, body)
}

// newBareRelayRequest 构造挂载在「裸 base」下的沙箱网关请求
// （官方 wake/status 在 {L(endpoint)}/v1/sandboxes/... 下，不含 /proxy/<id>）。
func (c *Client) newBareRelayRequest(a *auth.Auth, method, path string, body any) (*http.Request, error) {
	base := a.SandboxEndpoint
	if c.RelayBaseOverride != "" {
		base = RelayBase(c.RelayBaseOverride)
	}
	return c.newRelayRequestBase(a, RelayBaseFromProxy(base), method, path, body)
}

// newRelayRequestBase 构造沙箱网关请求。
func (c *Client) newRelayRequestBase(a *auth.Auth, base, method, path string, body any) (*http.Request, error) {
	if c.RelayBaseOverride != "" {
		base = RelayBase(c.RelayBaseOverride)
	}
	base = strings.TrimRight(base, "/")
	if base == "" {
		return nil, fmt.Errorf("no sandbox endpoint for account %s", a.UserID)
	}
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		return nil, err
	}
	for k, vs := range RelayHeaders(a) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// logEnv 调试用：打印上游错误（保留 trace 便于排查）。
func logEnv(uid string, err error, extra string) {
	log.Printf("upstream uid=%s %s: %v", uid, extra, err)
}
