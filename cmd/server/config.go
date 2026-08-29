// config.go 加载 JSON 配置 + 环境变量覆盖（AC2A_* 前缀）。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7865"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json
	Region    string `json:"region"`     // "cn" / "global"
	Mode      string `json:"mode"`       // auto / openai / relay

	Cooldown struct {
		HardCredit  string `json:"hard_credit"`   // "12h"
		SoftRate    string `json:"soft_rate"`     // "60s"
		ErrThresh   int    `json:"err_threshold"` // 默认 3
		ErrCooldown string `json:"err_cooldown"`  // "10m"
	} `json:"cooldown"`

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"` // 默认 120
		// UserAPIBaseOverride 自定义 userapi 基址（调试用）。
		UserAPIBaseOverride string `json:"user_api_base_override,omitempty"`
		// RelayBaseOverride 自定义 relay 基址（调试用）。
		RelayBaseOverride string `json:"relay_base_override,omitempty"`
	} `json:"upstream"`

	Schedule struct {
		MaintenanceMinutes int `json:"maintenance_minutes"` // 默认 30
	} `json:"schedule"`

	// 解析后
	HardCreditDur  time.Duration `json:"-"`
	SoftRateDur    time.Duration `json:"-"`
	ErrCooldownDur time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7865",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
		Region:    "cn",
		Mode:      "auto",
	}
	c.Cooldown.HardCredit = "12h"
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.ErrThresh = 3
	c.Cooldown.ErrCooldown = "10m"
	c.Upstream.TimeoutSeconds = 120
	c.Schedule.MaintenanceMinutes = 30
	return c
}

// Load 从文件读，再用 AC2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	get := func(k string) string { return os.Getenv("AC2A_" + k) }
	if v := get("LISTEN"); v != "" {
		c.Listen = v
	}
	if v := get("API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := get("AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := get("STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := get("REGION"); v != "" {
		c.Region = v
	}
	if v := get("MODE"); v != "" {
		c.Mode = v
	}
	if v := get("HARD_CREDIT"); v != "" {
		c.Cooldown.HardCredit = v
	}
	if v := get("SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := get("ERR_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Cooldown.ErrThresh = n
		}
	}
	if v := get("ERR_COOLDOWN"); v != "" {
		c.Cooldown.ErrCooldown = v
	}
	if v := get("TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := get("MAINTENANCE_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Schedule.MaintenanceMinutes = n
		}
	}
	if v := get("USER_API_BASE"); v != "" {
		c.Upstream.UserAPIBaseOverride = v
	}
	if v := get("RELAY_BASE"); v != "" {
		c.Upstream.RelayBaseOverride = v
	}
}

func (c *Config) normalize() error {
	var err error
	if c.HardCreditDur, err = time.ParseDuration(c.Cooldown.HardCredit); err != nil {
		return fmt.Errorf("cooldown.hard_credit: %w", err)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.ErrCooldownDur, err = time.ParseDuration(c.Cooldown.ErrCooldown); err != nil {
		return fmt.Errorf("cooldown.err_cooldown: %w", err)
	}
	if c.Cooldown.ErrThresh <= 0 {
		c.Cooldown.ErrThresh = 3
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.Schedule.MaintenanceMinutes <= 0 {
		c.Schedule.MaintenanceMinutes = 30
	}
	if c.Region == "" {
		c.Region = "cn"
	}
	c.Region = strings.ToLower(c.Region)
	if c.Region != "cn" && c.Region != "global" {
		return fmt.Errorf("region must be cn or global, got %q", c.Region)
	}
	if c.Mode == "" {
		c.Mode = "auto"
	}
	c.Mode = strings.ToLower(c.Mode)
	if c.Mode != "auto" && c.Mode != "openai" && c.Mode != "relay" {
		return fmt.Errorf("mode must be auto|openai|relay, got %q", c.Mode)
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	return nil
}
