package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cyeinfpro/ModelGuard/internal/guard"
)

// ConnectionStatus describes management API access independently of the
// monitoring switch. It deliberately contains no configuration credentials.
type ConnectionStatus struct {
	State         string    `json:"state"`
	Code          string    `json:"code"`
	Message       string    `json:"message"`
	NextAction    string    `json:"next_action"`
	CheckedAt     time.Time `json:"checked_at"`
	LastSuccessAt time.Time `json:"last_success_at"`
	AccountCount  int       `json:"account_count"`
}

type ReadinessCheck struct {
	Code       string  `json:"code"`
	State      string  `json:"state"`
	Label      string  `json:"label"`
	Message    string  `json:"message"`
	NextAction string  `json:"next_action"`
	AccountIDs []int64 `json:"account_ids,omitempty"`
}

type ReadinessStatus struct {
	State      string           `json:"state"`
	NextAction string           `json:"next_action"`
	Checks     []ReadinessCheck `json:"checks"`
}

type bridgeInstallation struct {
	State   string
	Message string
}

// Passive metadata checks cannot prove a live service or valid credentials.
// Only a real account probe can verify those, without an extra paid probe here.
func inspectBridge(dir string) bridgeInstallation {
	socket, err := os.Lstat(filepath.Join(dir, "api-bridge.sock"))
	if os.IsNotExist(err) {
		return bridgeInstallation{"missing", "尚未安装本机 API 桥接；OAuth 账号不受影响"}
	}
	if err != nil || socket.Mode()&os.ModeSocket == 0 {
		return bridgeInstallation{"incomplete", "本机 API 桥接 Socket 不可用；请检查桥接服务"}
	}
	token, err := os.Lstat(filepath.Join(dir, "api-bridge.token"))
	if err != nil || !token.Mode().IsRegular() || token.Size() == 0 || token.Mode().Perm()&0077 != 0 {
		return bridgeInstallation{"incomplete", "桥接令牌缺失或权限不正确；请按安装文档修复"}
	}
	return bridgeInstallation{"installed", "桥接文件已就绪；服务连通性和账号权限将在检测时验证"}
}

func connectionStatus(c guard.Config, count int, lastError string, loading bool, checked, updated, now time.Time) ConnectionStatus {
	s := ConnectionStatus{AccountCount: count, CheckedAt: checked, LastSuccessAt: updated}
	switch {
	case strings.TrimSpace(c.AdminKey) == "":
		s.State, s.Code = "needs_credentials", "credentials_required"
		s.Message, s.NextAction = "还未连接管理端", "填写管理地址和管理员访问密钥，然后保存连接并加载账号"
	case lastError != "":
		s.State, s.Code = "error", connectionErrorCode(lastError)
		s.Message, s.NextAction = lastError, connectionNextAction(s.Code)
	case loading && updated.IsZero():
		s.State, s.Code = "checking", "connection_checking"
		s.Message, s.NextAction = "正在连接管理端并读取授权账号", "等待连接结果，无需重复点击"
	case updated.IsZero():
		s.State, s.Code = "checking", "connection_unverified"
		s.Message, s.NextAction = "连接信息已保存，尚未验证", "加载账号以验证管理地址和读取权限"
	case now.Sub(updated) > 45*time.Second:
		s.State, s.Code = "stale", "accounts_stale"
		s.Message, s.NextAction = "显示最近一次账号列表，更新尚未完成", "可重新加载账号；现有列表会保留"
	default:
		s.State, s.Code = "ready", "connection_ready"
		s.Message = fmt.Sprintf("管理端已连接，已读取 %d 个授权账号", count)
		s.NextAction = "选择需要监测的账号"
	}
	return s
}

func connectionErrorCode(message string) string {
	switch {
	case strings.Contains(message, "HTTP 401"):
		return "credentials_invalid"
	case strings.Contains(message, "HTTP 403"):
		return "permission_denied"
	case strings.Contains(message, "HTTP 429"):
		return "rate_limited"
	case strings.Contains(message, "访问密钥") && (strings.Contains(message, "填写") || strings.Contains(message, "保存")):
		return "credentials_required"
	case strings.Contains(message, "格式") || strings.Contains(message, "分页") || strings.Contains(message, "响应无效") || strings.Contains(message, "列表不完整"):
		return "response_incompatible"
	default:
		return "connection_failed"
	}
}

func connectionNextAction(code string) string {
	switch code {
	case "credentials_required", "credentials_invalid":
		return "检查管理员访问密钥后，重新保存连接并加载账号"
	case "permission_denied":
		return "为管理密钥授权 accounts.read 和所需账号范围，再重新加载"
	case "rate_limited":
		return "管理接口请求受限，稍后自动重试"
	case "response_incompatible":
		return "检查 Sub2API 版本和管理接口地址，再重新加载账号"
	default:
		return "检查管理地址和服务是否可达；后台会自动重试"
	}
}

func readinessStatus(c guard.Config, accounts []guard.AccountOption, connection ConnectionStatus, host bool, bridge bridgeInstallation, runtimeError string) ReadinessStatus {
	s := ReadinessStatus{State: "ready", NextAction: "保存账号与策略后开始首轮检测", Checks: []ReadinessCheck{}}
	add := func(check ReadinessCheck) {
		s.Checks = append(s.Checks, check)
		if check.State == "blocked" && s.State == "ready" {
			s.State, s.NextAction = "needs_attention", check.NextAction
		}
	}
	check := ReadinessCheck{Code: connection.Code, State: "ready", Label: "管理端连接", Message: connection.Message, NextAction: connection.NextAction}
	if connection.State != "ready" {
		check.State = "blocked"
		s.State, s.NextAction = "needs_connection", connection.NextAction
	}
	add(check)
	if runtimeError != "" {
		add(ReadinessCheck{Code: "runtime_unavailable", State: "blocked", Label: "监测服务", Message: runtimeError, NextAction: "修复状态目录后重新保存配置"})
	}
	selected := []int64{}
	for _, t := range c.Targets {
		if t.Enabled {
			selected = append(selected, t.AccountID)
		}
	}
	if len(selected) == 0 {
		add(ReadinessCheck{Code: "targets_required", State: "blocked", Label: "检测账号", Message: "尚未选择检测账号", NextAction: "勾选账号，确认请求模型与预期指纹"})
		if connection.State == "ready" && runtimeError == "" {
			s.State, s.NextAction = "choose_accounts", "勾选账号，确认请求模型与预期指纹"
		}
		return s
	}
	byID := make(map[int64]guard.AccountOption, len(accounts))
	for _, a := range accounts {
		byID[a.ID] = a
	}
	var missing, unsupported, oauth, api, inactive []int64
	for _, id := range selected {
		a, ok := byID[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		if !a.Supported {
			unsupported = append(unsupported, id)
		} else if a.Type == "oauth" {
			oauth = append(oauth, id)
		} else if a.Type == "apikey" {
			api = append(api, id)
		}
		if a.Status != "active" {
			inactive = append(inactive, id)
		}
	}
	// Failed or stale discovery is not evidence that an account was deleted.
	if len(missing) > 0 && connection.State == "ready" {
		add(ReadinessCheck{Code: "targets_not_visible", State: "blocked", Label: "账号授权范围", Message: "部分已选账号不在当前管理密钥的可见列表中", NextAction: "检查账号是否删除或密钥授权范围；重新选择后保存", AccountIDs: missing})
	}
	if len(unsupported) > 0 {
		add(ReadinessCheck{Code: "targets_unsupported", State: "blocked", Label: "账号类型", Message: "部分账号不支持独立检测", NextAction: "请选择独立 OpenAI OAuth 或 API Key 账号", AccountIDs: unsupported})
	}
	if len(oauth) > 0 && !host {
		add(ReadinessCheck{Code: "host_identity_unavailable", State: "blocked", Label: "OAuth 检测通道", Message: "宿主账号身份接口尚未连接", NextAction: "检查插件是否启用及宿主版本；接口恢复后可检测", AccountIDs: oauth})
	}
	if len(api) > 0 {
		switch {
		case c.AdminURL != "http://127.0.0.1:8080" && c.AdminURL != "http://localhost:8080":
			add(ReadinessCheck{Code: "api_bridge_address", State: "blocked", Label: "API 检测通道", Message: "API 桥接要求本机 Sub2API 管理地址", NextAction: "按桥接文档使用 http://127.0.0.1:8080 或 http://localhost:8080", AccountIDs: api})
		case bridge.State != "installed":
			add(ReadinessCheck{Code: "api_bridge_" + bridge.State, State: "blocked", Label: "API 检测通道", Message: bridge.Message, NextAction: "安装或修复本机 API 桥接，也可先只选择 OAuth 账号", AccountIDs: api})
		default:
			add(ReadinessCheck{Code: "api_bridge_installed", State: "warning", Label: "API 检测通道", Message: bridge.Message, NextAction: "首轮检测将验证桥接与上游 Responses API", AccountIDs: api})
		}
	}
	if len(inactive) > 0 {
		add(ReadinessCheck{Code: "accounts_inactive", State: "warning", Label: "账号状态", Message: "部分账号不是 active 状态，检测时会跳过", NextAction: "需要检测时先在 Sub2API 恢复账号有效状态", AccountIDs: inactive})
	}
	// Read access does not prove mutation permission. Do not mutate a real
	// account just to test that permission during onboarding.
	if c.AutoPause || c.AutoRecover {
		add(ReadinessCheck{Code: "status_permission_on_use", State: "warning", Label: "调度权限", Message: "accounts.status 权限将在需要暂停或恢复时验证", NextAction: "确保管理密钥包含 accounts.status；读取成功不代表可修改调度"})
	}
	return s
}
