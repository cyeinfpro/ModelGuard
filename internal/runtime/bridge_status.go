package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const bridgeStatusTTL = 5 * time.Second

func (p *Plugin) bridgeStatus(ctx context.Context, dir string) bridgeInstallation {
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	if p.bridgeDir == dir && time.Now().Before(p.bridgeChecked.Add(bridgeStatusTTL)) {
		return p.bridgeCached
	}
	status := inspectBridgeContext(ctx, dir)
	if ctx.Err() == nil {
		p.bridgeDir = dir
		p.bridgeChecked = time.Now()
		p.bridgeCached = status
	}
	return status
}

func inspectBridge(dir string) bridgeInstallation {
	return inspectBridgeContext(context.Background(), dir)
}

// readyz checks local management and readonly database dependencies without
// reading account rows or making model requests. healthz is liveness only.
func inspectBridgeContext(ctx context.Context, dir string) bridgeInstallation {
	path := filepath.Join(dir, "api-bridge.sock")
	socket, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return bridgeInstallation{"missing", "尚未安装本机 API 桥接；OAuth 账号不受影响"}
	}
	if err != nil || socket.Mode()&os.ModeSocket == 0 {
		return bridgeInstallation{"incomplete", "本机 API 桥接 Socket 不可用；请检查桥接服务"}
	}
	tokenPath := filepath.Join(dir, "api-bridge.token")
	file, err := os.Lstat(tokenPath)
	if err != nil || !file.Mode().IsRegular() || file.Size() == 0 || file.Size() > 4096 || file.Mode().Perm()&0077 != 0 {
		return bridgeInstallation{"incomplete", "桥接令牌缺失或权限不正确；请按安装文档修复"}
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil || strings.TrimSpace(string(token)) == "" {
		return bridgeInstallation{"incomplete", "桥接令牌不可读取；请检查安装权限"}
	}
	checkCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(checkCtx, http.MethodGet, "http://bridge/readyz", nil)
	req.Header.Set("X-Bridge-Token", strings.TrimSpace(string(token)))
	response, err := client.Do(req)
	if err != nil {
		return bridgeInstallation{"offline", "桥接或依赖探测未及时响应；请检查桥接进程、宿主网络与数据库"}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return bridgeInstallation{"unauthorized", "桥接服务拒绝本机令牌；请重新同步桥接令牌"}
	}
	if response.StatusCode == http.StatusServiceUnavailable {
		var detail struct{ Code string }
		_ = json.NewDecoder(io.LimitReader(response.Body, 256)).Decode(&detail)
		switch detail.Code {
		case "checking":
			return bridgeInstallation{"checking", "桥接依赖检查中；后台正在验证管理接口与只读数据库，请稍后刷新"}
		case "admin_unreachable":
			return bridgeInstallation{"dependency_unavailable", "桥接无法连接宿主管理接口；宿主重启后请核对网络命名空间并重建桥接"}
		case "database_unavailable":
			return bridgeInstallation{"dependency_unavailable", "桥接只读数据库入口不可用；请检查网络、只读角色与视图"}
		default:
			return bridgeInstallation{"dependency_unavailable", "桥接依赖尚未就绪；请检查宿主管理接口与只读数据库"}
		}
	}
	if response.StatusCode != http.StatusOK {
		return bridgeInstallation{"incompatible", "桥接就绪接口不可用；请更新桥接服务"}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 257))
	var status map[string]bool
	if err != nil || len(body) > 256 || json.Unmarshal(body, &status) != nil || len(status) != 1 || !status["ok"] {
		return bridgeInstallation{"incompatible", "桥接就绪响应无效；请更新桥接服务"}
	}
	return bridgeInstallation{"installed", "本机 API 桥接及管理、只读数据库入口就绪；账号权限将在检测时验证"}
}
