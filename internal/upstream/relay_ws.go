// relay_ws.go AutoClaw 设备上线用 WebSocket 辅助。
//
// AutoClaw 云龙虾设备必须通过 WebSocket 连接才会真正上线；
// 仅用 HTTP relay bridge 会得到「云端设备未连接，请先唤醒云龙虾」。
// 经官方逆向（webElectronApi）：
//  1. dial  wss://{relay_base}/v1/client/ws?device_id=<sandbox_id>&access_token=<token>
//  2. 发   auth.inject（token/userId/clientMetadata），等 auth.inject.ok
//
// 设备上线后，对话经 HTTP bridge（agent/send + /api/events）完成，
// 此处只提供 WS 建连/鉴权的辅助函数（openDeviceWS 在 relay.go 中使用）。
package upstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"autoclaw2api/internal/auth"
)

// stripBearer 剥掉 token 的 Bearer 前缀（官方 me()）。
func stripBearer(t string) string {
	t = strings.TrimSpace(t)
	if strings.HasPrefix(t, "Bearer ") {
		return t[len("Bearer "):]
	}
	return t
}

// toWSBase 把 http(s) relay base 转成 ws(s)。
func toWSBase(base string) string {
	b := base
	if strings.HasPrefix(b, "https://") {
		b = "wss://" + b[len("https://"):]
	} else if strings.HasPrefix(b, "http://") {
		b = "ws://" + b[len("http://"):]
	}
	return b
}

// wsAuthInject 发送 auth.inject 并等待 auth.inject.ok。
func wsAuthInject(conn *wsConn, a *auth.Auth, token string) error {
	frame := map[string]any{
		"type":  "auth.inject",
		"token": token,
	}
	if a.UserID != "" {
		frame["userId"] = a.UserID
	}
	if a.UserName != "" {
		frame["userName"] = a.UserName
	}
	frame["clientMetadata"] = map[string]any{"client_type": "web"}
	if err := conn.wsWriteJSON(frame); err != nil {
		return err
	}
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		op, data, err := conn.NextMessage()
		if err != nil {
			return fmt.Errorf("auth wait: %w", err)
		}
		if op != wsOpText {
			continue
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m["type"] {
		case "auth.inject.ok":
			return nil
		case "auth.inject.error":
			return fmt.Errorf("auth.inject failed: %s", string(data))
		}
	}
	return errors.New("auth.inject timeout")
}
