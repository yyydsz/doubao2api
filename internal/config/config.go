// Package config 加载 config.json（豆包 /chat/completion 版）。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"doubao2api/internal/pool"
)

// Account 一个豆包账号（以 Cookie 为凭据）。
type Account struct {
	Name       string `json:"name"`
	Cookie     string `json:"cookie"`      // 完整 Cookie 串
	DeviceID   string `json:"device_id"`   // 可选，缺省取 cookie 派生
	WebID      string `json:"web_id"`      // 可选
	LocalAppID string `json:"local_app_id"`// 默认 582478
	Note       string `json:"note"`
}

// WorkMode 工作模式参数。
type WorkMode struct {
	Enabled         bool   `json:"enabled"`
	BotID           string `json:"bot_id"`
	ModeID          string `json:"mode_id"`
	ReasoningEffort string `json:"reasoning_effort"`
	MemoryProfile   string `json:"memory_profile"`
	TotalWindow     string `json:"total_window_size"`
	EnableSandbox   bool   `json:"enable_sandbox"`
	Workspace       string `json:"workspace"`
}

// Config 网关配置。
// CheckinConfig 每日自动签到配置。
type CheckinConfig struct {
	Enabled bool   `json:"enabled"`
	Hour    int    `json:"hour"`
	Scene   string `json:"scene"`
}

type Config struct {
	Listen   string   `json:"listen"`
	APIKeys  []string `json:"api_keys"`
	LogLevel string   `json:"log_level"`
	AdminToken string `json:"admin_token"` // 管理接口专用令牌（与业务密钥解耦）
	Checkin CheckinConfig `json:"checkin"`

	Upstream struct {
		ChatURL          string            `json:"chat_url"`
		Query            map[string]string `json:"query"`
		ABogus           string            `json:"a_bogus"`      // 调试用：固定的 a_bogus
		SignCommand      string            `json:"sign_command"` // 可选：外部签名命令，stdin=url，stdout=a_bogus
		TimeoutSeconds   int               `json:"timeout_seconds"`
		HeartbeatSeconds int               `json:"heartbeat_seconds"`
		UserAgent        string            `json:"user_agent"`
		FlattenHistory   bool              `json:"flatten_history"`
	} `json:"upstream"`

	Accounts []Account `json:"accounts"`

	DefaultModel string            `json:"default_model"`
	Models       []string          `json:"models"`
	ModelAliases map[string]string `json:"model_aliases"`
	WorkMode     WorkMode          `json:"work_mode"`

	QR struct {
		Enabled  bool              `json:"enabled"`
		GetURL   string            `json:"get_url"`
		CheckURL string            `json:"check_url"`
		Query    map[string]string `json:"query"`
	} `json:"qr"`

	Pool struct {
		RateLimitCooldown string `json:"rate_limit_cooldown"`
		OverloadCooldown  string `json:"overload_cooldown"`
		StickyTTL         string `json:"sticky_ttl"`
		LargeRequestBytes int    `json:"large_request_bytes"`
		LargeBudget       int    `json:"large_budget"`
		LargeWindow       string `json:"large_window"`
		Deterministic     bool   `json:"deterministic"`
	} `json:"pool"`

	rateLimitDur time.Duration
	overloadDur  time.Duration
	stickyTTL    time.Duration
	largeWindow  time.Duration
}

// Load 读取并校验配置。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	if err := c.applyDefaults(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8850"
	}
	if c.AdminToken == "" {
		c.AdminToken = os.Getenv("DOUBAO2API_ADMIN_TOKEN")
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.Upstream.ChatURL == "" {
		c.Upstream.ChatURL = "https://www.doubao.com/chat/completion"
	}
	if c.Upstream.UserAgent == "" {
		c.Upstream.UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36 SamanthaDoubao/2.30.1"
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 300
	}
	if c.Upstream.HeartbeatSeconds <= 0 {
		c.Upstream.HeartbeatSeconds = 15
	}
	if c.DefaultModel == "" {
		c.DefaultModel = "doubao"
	}
	if c.QR.GetURL == "" {
		c.QR.GetURL = "https://accounts.doubao.com/passport/web/get_qrcode/"
	}
	if c.QR.CheckURL == "" {
		c.QR.CheckURL = "https://accounts.doubao.com/passport/web/check_qrconnect/"
	}
	if !c.QR.Enabled {
		c.QR.Enabled = true // 默认开启：参数已内置，开箱可用
	}
	if c.QR.Query == nil {
		// 实测可用组合：aid=497858 + language + next（带 real_aid 会被判「非法应用」）
		c.QR.Query = map[string]string{
			"aid":                    "582478",
			"language":               "zh",
			"next":                   "https://www.doubao.com/",
			"is_from_ttaccountsdk":   "1",
			"passport_jssdk_version": "4.4.6",
			"passport_jssdk_type":    "normal",
			"sdk_version":            "2.2.11-doubao.0",
			"pkg_type":               "release_version",
			"version_code":           "20800",
			"device_platform": "desktop",
		}
	}
	if c.WorkMode.BotID == "" {
		c.WorkMode.BotID = "7338286299411103781"
	}
	if c.WorkMode.ModeID == "" {
		c.WorkMode.ModeID = "3"
	}
	if c.WorkMode.ReasoningEffort == "" {
		c.WorkMode.ReasoningEffort = "4"
	}
	if c.WorkMode.MemoryProfile == "" {
		c.WorkMode.MemoryProfile = "300k_230"
	}
	if c.WorkMode.TotalWindow == "" {
		c.WorkMode.TotalWindow = "300000"
	}
	// 默认 query（缺失才补）
	def := map[string]string{
		"aid": "582478", "real_aid": "582478", "channel": "win",
		"client_platform": "pc_client", "device_platform": "desktop",
		"doubao_device_platform": "desktop", "doubao_pc_version": "2.30.1",
		"pc_version": "2.30.1", "pkg_type": "release_version", "language": "zh",
		"region": "CN", "sys_region": "CN", "runtime": "web",
		"runtime_version": "3.37.4", "samantha_web": "1",
		"use-olympus-account": "1", "version_code": "20800",
		"web_platform": "desktop", "chromium_version": "147.0.7727.149",
	}
	if c.Upstream.Query == nil {
		c.Upstream.Query = map[string]string{}
	}
	for k, v := range def {
		if _, ok := c.Upstream.Query[k]; !ok {
			c.Upstream.Query[k] = v
		}
	}

	for i := range c.Accounts {
		if c.Accounts[i].Name == "" {
			c.Accounts[i].Name = fmt.Sprintf("acct-%d", i+1)
		}
		if c.Accounts[i].Cookie == "" {
			return fmt.Errorf("accounts[%d] 缺少 cookie", i)
		}
		if c.Accounts[i].LocalAppID == "" {
			c.Accounts[i].LocalAppID = "582478"
		}
	}

	var err error
	if c.rateLimitDur, err = parseDur(c.Pool.RateLimitCooldown, 60*time.Second, "pool.rate_limit_cooldown"); err != nil {
		return err
	}
	if c.overloadDur, err = parseDur(c.Pool.OverloadCooldown, 15*time.Second, "pool.overload_cooldown"); err != nil {
		return err
	}
	if c.stickyTTL, err = parseDur(c.Pool.StickyTTL, 10*time.Minute, "pool.sticky_ttl"); err != nil {
		return err
	}
	if c.largeWindow, err = parseDur(c.Pool.LargeWindow, 2*time.Minute, "pool.large_window"); err != nil {
		return err
	}
	return nil
}

func parseDur(s string, def time.Duration, field string) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	if d <= 0 {
		return def, nil
	}
	return d, nil
}

// PoolOptions 转池参数。
func (c *Config) PoolOptions() pool.Options {
	return pool.Options{
		RateLimitCooldown: c.rateLimitDur,
		OverloadCooldown:  c.overloadDur,
		StickyTTL:         c.stickyTTL,
		LargeRequestBytes: c.Pool.LargeRequestBytes,
		LargeBudget:       c.Pool.LargeBudget,
		LargeWindow:       c.largeWindow,
		Deterministic:     c.Pool.Deterministic,
	}
}

// ResolveModel 客户端模型名 → model_item_key。
func (c *Config) ResolveModel(name string) string {
	if name == "" {
		name = c.DefaultModel
	}
	if m, ok := c.ModelAliases[name]; ok && m != "" {
		return m
	}
	return name
}

// ModelList /v1/models。
func (c *Config) ModelList() []string {
	if len(c.Models) > 0 {
		return c.Models
	}
	seen := map[string]bool{}
	var out []string
	add := func(m string) {
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	add(c.DefaultModel)
	for k := range c.ModelAliases {
		add(k)
	}
	return out
}

// AccountByName 查账号。
func (c *Config) AccountByName(name string) (Account, bool) {
	for _, a := range c.Accounts {
		if a.Name == name {
			return a, true
		}
	}
	return Account{}, false
}
