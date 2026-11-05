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

// healthz checks the existing local service without querying credentials or
// making a model request. A socket file alone is not evidence of a live bridge.
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
	req, _ := http.NewRequestWithContext(checkCtx, http.MethodGet, "http://bridge/healthz", nil)
	req.Header.Set("X-Bridge-Token", strings.TrimSpace(string(token)))
	response, err := client.Do(req)
	if err != nil {
		return bridgeInstallation{"offline", "桥接文件存在，但本机桥接服务未响应；请检查服务进程"}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return bridgeInstallation{"unauthorized", "桥接服务拒绝本机令牌；请重新同步桥接令牌"}
	}
	if response.StatusCode != http.StatusOK {
		return bridgeInstallation{"incompatible", "桥接健康接口不可用；请更新桥接服务"}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 257))
	var status map[string]bool
	if err != nil || len(body) > 256 || json.Unmarshal(body, &status) != nil || len(status) != 1 || !status["ok"] {
		return bridgeInstallation{"incompatible", "桥接健康响应无效；请更新桥接服务"}
	}
	return bridgeInstallation{"installed", "本机 API 桥接在线，令牌验证通过；账号权限将在检测时验证"}
}
