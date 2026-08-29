// Package upstream 封装 AutoClaw Cloud 上游：
//   - userapi（登录/刷新/积分/沙箱管理，带 X-Auth-Sign 签名头）
//   - 沙箱网关（OpenClaw gateway：OpenAI 兼容端点 + relay bridge + SSE 事件流）
package upstream

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"autoclaw2api/internal/auth"
)

// 浏览器端逆向得到的签名参数（webElectronApi 常量）：
//
//	lr = "100003"（X-Auth-Appid）
//	po = "38d2391985e2369a5fb8227d8e6cd5e5"（签名密钥）
//	X-Auth-Sign = MD5("{appid}&{unixSeconds}&{secret}")
const (
	AppID     = "100003"
	AppSecret = "38d2391985e2369a5fb8227d8e6cd5e5"
	ClientVer = "1.12.1" // X-Version（与 web 端一致）
)

// UserAPIBaseCN 大陆区 userapi 基址。
const UserAPIBaseCN = "https://autoglm-acceleration-api.zhipuai.cn"

// UserAPIBaseGlobal 海外区 userapi 基址。
const UserAPIBaseGlobal = "https://autoglm-api.autoglm.ai"

// Sign 计算 X-Auth-Sign = MD5("appid&ts&secret")。
func Sign(ts string) string {
	sum := md5.Sum([]byte(fmt.Sprintf("%s&%s&%s", AppID, ts, AppSecret)))
	return hex.EncodeToString(sum[:])
}

// TimeNowUnix 返回当前 Unix 秒（供 Sign 拼 ts；单独拆出便于测试）。
func TimeNowUnix() string {
	return fmt.Sprintf("%d", time.Now().Unix())
}

// UserAPIBase 返回指定 region 的 userapi 基址。
func UserAPIBase(region string) string {
	if region == "global" {
		return UserAPIBaseGlobal
	}
	return UserAPIBaseCN
}

// UserAPIHeaders 构造 userapi 请求头（含签名与可选的 Bearer token）。
// accessToken 为空时只带签名头（send-code 等未登录接口）。
func UserAPIHeaders(accessToken string, region string) http.Header {
	ts := TimeNowUnix()
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "*/*")
	h.Set("X-Version", ClientVer)
	h.Set("X-Tm", "web")
	h.Set("X-Product", "autoclaw")
	h.Set("X-Client-Type", "web")
	h.Set("X-Channel", "official")
	h.Set("X-Auth-Appid", AppID)
	h.Set("X-Auth-TimeStamp", ts)
	h.Set("X-Auth-Sign", Sign(ts))
	h.Set("X-Trace-Id", NewTraceID())
	h.Set("X-Lang", "en")
	if accessToken != "" {
		h.Set("Authorization", bearerValue(accessToken))
	}
	return h
}

// RelayHeaders 沙箱网关请求头（OpenClaw gateway 接受用户 access_token 作为 Bearer）。
func RelayHeaders(a *auth.Auth) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json, text/plain, */*")
	if a.AccessToken != "" {
		h.Set("Authorization", bearerValue(a.AccessToken))
	}
	return h
}

// bearerValue 生成 Authorization 头的完整值。
// AutoClaw 登录返回的 access_token 值本身已含 "Bearer " 前缀（真实凭证验证），
// 这里做幂等归一化：已带前缀则不重复加，避免 "Bearer Bearer ..."。
func bearerValue(token string) string {
	t := strings.TrimSpace(token)
	if len(t) >= 7 && strings.EqualFold(t[:7], "Bearer ") {
		return t
	}
	return "Bearer " + t
}

// RelayBase 把沙箱 endpoint 归一化为 relay 基址，与官方 web 端 L() 完全一致：
//   - ws:// -> http://、wss:// -> https://
//   - 解析 URL 后清空 query/hash
//   - 路径截断到 "/autoclaw-cloud"（保留该前缀），无该前缀则追加
//
// 注意：上游 sandbox_endpoint 形如
// "wss://autoglm-api.zhipuai.cn/autoclaw-cloud/ws?sandbox_id=..&port=..&path=/ws"
// 必须去掉 query 并截断，否则 http 调用会打到错误路径。
func RelayBase(endpoint string) string {
	t := strings.TrimSpace(endpoint)
	if strings.HasPrefix(strings.ToLower(t), "ws://") {
		t = "http://" + t[5:]
	} else if strings.HasPrefix(strings.ToLower(t), "wss://") {
		t = "https://" + t[6:]
	}
	suffix := "/autoclaw-cloud"
	// 不完整 URL（无 scheme）直接按旧逻辑兜底
	if !strings.Contains(t, "://") {
		t = strings.TrimRight(t, "/")
		if strings.HasSuffix(t, suffix) {
			return t
		}
		return t + suffix
	}
	u, err := url.Parse(t)
	if err != nil {
		t = strings.TrimRight(t, "/")
		if strings.HasSuffix(t, suffix) {
			return t
		}
		return t + suffix
	}
	u.RawQuery = ""
	u.Fragment = ""
	p := u.Path
	if i := strings.Index(p, suffix); i >= 0 {
		u.Path = p[:i+len(suffix)]
	} else {
		u.Path = strings.TrimRight(p, "/") + suffix
	}
	out := strings.TrimRight(u.String(), "/")
	if out == "" {
		return t + suffix
	}
	return out
}

// RelayProxyBase 返回沙箱网关（含 /proxy/<sandbox_id> 前缀）的 relay 基址。
// 官方 web 端通信（/health、relay bridge、OpenAI 端点）都在
//
//	{L(endpoint)}/proxy/{sandbox_id}
//
// 之下（见 webElectronApi 的 _e(sandboxEndpoint, sandboxId)）。
func RelayProxyBase(endpoint, sandboxID string) string {
	base := RelayBase(endpoint)
	if sandboxID == "" {
		return base
	}
	return strings.TrimRight(base, "/") + "/proxy/" + strings.TrimSpace(sandboxID)
}

// RelayBaseFromProxy 从 proxy 形式 base 反推裸 base（去掉 /proxy/<id> 后缀）。
// 官方把 wake/status 挂在裸 base 下：
//
//	{L(endpoint)}/v1/sandboxes/{deviceId}/wake|status
func RelayBaseFromProxy(proxyBase string) string {
	p := strings.TrimRight(proxyBase, "/")
	if i := strings.LastIndex(p, "/proxy/"); i >= 0 {
		return p[:i]
	}
	return p
}
