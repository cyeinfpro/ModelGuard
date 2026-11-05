package guard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cyeinfpro/ModelGuard/internal/fingerprint"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"strings"
)

type Target struct {
	AccountID int64  `json:"account_id"`
	Model     string `json:"model"`
	Expected  string `json:"expected"`
	Enabled   bool   `json:"enabled"`
}
type Config struct {
	GroupIDs               []int64  `json:"group_ids,omitempty"`
	GroupModel             string   `json:"group_model,omitempty"`
	GroupExpected          string   `json:"group_expected,omitempty"`
	Concurrency            int      `json:"concurrency"`
	TestKey                string   `json:"test_key,omitempty"`
	ManualRequestID        string   `json:"manual_request_id,omitempty"`
	StopRequestID          string   `json:"stop_request_id,omitempty"`
	RecoverySpacingSeconds int      `json:"recovery_spacing_seconds"`
	FamilyMatch            bool     `json:"family_match"`
	Enabled                bool     `json:"enabled"`
	AutoPause              bool     `json:"auto_pause"`
	AutoRecover            bool     `json:"auto_recover"`
	PauseUnavailable       bool     `json:"pause_unavailable"`
	AdminURL               string   `json:"admin_url"`
	AdminKey               string   `json:"admin_key"`
	DataDir                string   `json:"data_dir"`
	NormalSeconds          int      `json:"normal_seconds"`
	RetrySeconds           int      `json:"retry_seconds"`
	QuarantineSeconds      int      `json:"quarantine_seconds"`
	TimeoutSeconds         int      `json:"timeout_seconds"`
	DailyRequests          int      `json:"daily_requests"`
	Targets                []Target `json:"targets"`
}

func DefaultConfig() Config {
	return Config{Concurrency: 3, AutoRecover: true, AdminURL: "http://127.0.0.1:8080", DataDir: "/app/data/sub2api-account-guard", NormalSeconds: 1800, RetrySeconds: 120, QuarantineSeconds: 21600, TimeoutSeconds: 180, DailyRequests: 2000, Targets: []Target{}}
}
func ParseConfig(raw []byte, detector *fingerprint.Detector) (Config, error) {
	c := DefaultConfig()
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, errors.New("配置格式错误或含未知字段")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, errors.New("配置必须只有一个 JSON 对象")
	}
	u, err := url.Parse(c.AdminURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return c, errors.New("管理地址必须为站点根地址")
	}
	ip := net.ParseIP(u.Hostname())
	loop := u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && loop) {
		return c, errors.New("管理地址只允许 HTTPS 或本机 HTTP")
	}
	c.PauseUnavailable = false // Retained only to read old configurations; failures never isolate.
	c.AdminURL = strings.TrimRight(c.AdminURL, "/")
	if !filepath.IsAbs(c.DataDir) || filepath.Clean(c.DataDir) == "/" {
		return c, errors.New("数据目录必须为绝对路径且不能为根目录")
	}
	if c.Concurrency < 1 || c.Concurrency > 8 {
		return c, errors.New("并行账号数必须为 1 到 8")
	}
	if c.NormalSeconds < 300 || c.NormalSeconds > 86400 || c.RetrySeconds < 60 || c.RetrySeconds > 86400 || c.QuarantineSeconds < 300 || c.QuarantineSeconds > 604800 || c.TimeoutSeconds < 10 || c.TimeoutSeconds > 600 || c.DailyRequests < 3 || c.DailyRequests > 10000 {
		return c, errors.New("检测间隔、超时或每日请求预算超出允许范围")
	}
	if c.RecoverySpacingSeconds < 0 || c.RecoverySpacingSeconds > 604800 {
		return c, errors.New("恢复确认间隔必须为 0 到 604800 秒")
	}
	if len(c.ManualRequestID) > 64 || len(c.StopRequestID) > 64 {
		return c, errors.New("检测请求标识过长")
	}
	if len(c.Targets) > 100 {
		return c, errors.New("最多配置 100 个账号")
	}
	if len(c.GroupIDs) > 100 {
		return c, errors.New("最多配置 100 个分组")
	}
	groups := map[int64]bool{}
	for _, id := range c.GroupIDs {
		if id <= 0 || groups[id] {
			return c, errors.New("分组 ID 必须为唯一正整数")
		}
		groups[id] = true
	}
	if (c.GroupModel == "") != (c.GroupExpected == "") {
		return c, errors.New("分组新成员模型和预期模型必须同时配置")
	}
	if c.GroupModel != "" {
		if strings.TrimSpace(c.GroupModel) == "" || len(c.GroupModel) > 160 {
			return c, errors.New("分组模型无效")
		}
		if _, ok := detector.ResolveExpected(c.GroupExpected); !ok {
			return c, errors.New("分组预期模型不在指纹库中")
		}
	}
	seen := map[int64]bool{}
	for _, t := range c.Targets {
		if t.AccountID <= 0 || seen[t.AccountID] || strings.TrimSpace(t.Model) == "" || len(t.Model) > 160 {
			return c, errors.New("账号 ID 必须唯一且模型不能为空")
		}
		seen[t.AccountID] = true
		if _, ok := detector.ResolveExpected(t.Expected); !ok {
			return c, fmt.Errorf("账号 %d 的预期模型不在指纹库中", t.AccountID)
		}
	}
	if c.Enabled && strings.TrimSpace(c.AdminKey) == "" {
		return c, errors.New("启用监测需要 Sub2API 管理 API Key")
	}
	return c, nil
}
