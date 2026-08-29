// Package auth 解析/写回 AutoClaw 账号凭证文件（auths/autoclaw-*.json），
// 提供 token 过期判定与刷新后的原子写回。
//
// 磁盘格式（与 cmd/login 落盘一致）：
//
//	{
//	  "accessToken":  "...",
//	  "refreshToken": "...",
//	  "expiresAt":    1787963465,        // Unix 秒
//	  "userId":       "...",
//	  "userName":     "...",
//	  "phone":        "138****8000",
//	  "deviceId":     "<用户 API 侧设备 UUID>",
//	  "region":       "cn" | "global",
//	  "sandboxId":    "cloud-sb-xxx",      // 可选，缓存
//	  "sandboxEndpoint": "https://...",    // 可选，缓存
//	  "endTimestamp": 1789000000           // 可选，sandbox 到期时间
//	}
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Auth 是归一化后的 AutoClaw 账号凭证。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken     string
	RefreshToken    string
	ExpiresAt       int64 // Unix 秒；<=0 表示未知
	UserID          string
	UserName        string
	Phone           string
	DeviceID        string // userapi 侧设备 UUID（login 时生成，refresh 需要一致）
	Region          string // "cn" / "global"
	SandboxID       string // 缓存：云沙箱 id（relay 侧 device_id）
	SandboxEndpoint string // 缓存：沙箱网关 base（含 /autoclaw-cloud 归一化）
	EndTimestamp    int64  // 缓存：沙箱到期时间（Unix 秒）

	FilePath string // 来源文件；refresh 后原子写回此处
}

// Lock 供同进程内其他包（upstream）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse 解析磁盘凭证文件。
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var f struct {
		AccessToken     string `json:"accessToken"`
		RefreshToken    string `json:"refreshToken"`
		ExpiresAt       int64  `json:"expiresAt"`
		UserID          string `json:"userId"`
		UserName        string `json:"userName"`
		Phone           string `json:"phone"`
		DeviceID        string `json:"deviceId"`
		Region          string `json:"region"`
		SandboxID       string `json:"sandboxId"`
		SandboxEndpoint string `json:"sandboxEndpoint"`
		EndTimestamp    int64  `json:"endTimestamp"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	a := &Auth{
		AccessToken:     f.AccessToken,
		RefreshToken:    f.RefreshToken,
		ExpiresAt:       f.ExpiresAt,
		UserID:          f.UserID,
		UserName:        f.UserName,
		Phone:           f.Phone,
		DeviceID:        f.DeviceID,
		Region:          f.Region,
		SandboxID:       f.SandboxID,
		SandboxEndpoint: f.SandboxEndpoint,
		EndTimestamp:    f.EndTimestamp,
	}
	if a.Region == "" {
		a.Region = "cn"
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return a, nil
}

// SaveAtomic 以原子方式写回 FilePath（tmp + rename）。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UserID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"accessToken":     a.AccessToken,
		"refreshToken":    a.RefreshToken,
		"expiresAt":       a.ExpiresAt,
		"userId":          a.UserID,
		"userName":        a.UserName,
		"phone":           a.Phone,
		"deviceId":        a.DeviceID,
		"region":          a.Region,
		"sandboxId":       a.SandboxID,
		"sandboxEndpoint": a.SandboxEndpoint,
		"endTimestamp":    a.EndTimestamp,
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// LoadDir 扫描 dir 下 autoclaw-*.json，只收 wantRegion（"cn"/"global"）。
// 解析失败与 region 不符的文件静默跳过（启动日志由调用方统计）。
func LoadDir(dir, wantRegion string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "autoclaw-*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		if a.Region != wantRegion {
			continue
		}
		a.FilePath = f
		out = append(out, a)
	}
	return out, nil
}
