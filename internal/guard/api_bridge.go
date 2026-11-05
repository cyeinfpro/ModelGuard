package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Local socket and per-installation token never leave the server. A scoped
// account read must succeed before this supplementary credential lookup.
func ReadAPIAccount(ctx context.Context, c Config, id int64) (Account, error) {
	var out Account
	if !IsLoopbackAdminURL(c.AdminURL) {
		return out, errors.New("API 账号桥接仅支持本机 Sub2API 管理地址")
	}
	token, err := os.ReadFile(filepath.Join(c.DataDir, "api-bridge.token"))
	if err != nil {
		return out, errors.New("API 凭据桥接尚未安装")
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(c.DataDir, "api-bridge.sock"))
	}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 40 * time.Second, CheckRedirect: NoRedirect}
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://local/accounts/%d", id), nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("x-bridge-token", strings.TrimSpace(string(token)))
	req.Header.Set("x-admin-key", c.AdminKey)
	r, err := client.Do(req)
	if err != nil {
		return out, errors.New("API 凭据桥接暂时不可用")
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return out, fmt.Errorf("API 账号授权或桥接读取失败（HTTP %d）", r.StatusCode)
	}
	var payload struct {
		Account
		Proxy *struct {
			Protocol, Host, Username, Password string
			Port                               int
		}
	}
	if json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&payload) != nil || payload.ID != id || payload.Type != "apikey" || payload.Platform != "openai" || payload.UpdatedAt == "" {
		return out, errors.New("API 账号桥接数据无效")
	}
	out = payload.Account
	if payload.Proxy != nil {
		p := payload.Proxy
		u := url.URL{Scheme: p.Protocol, Host: net.JoinHostPort(p.Host, strconv.Itoa(p.Port))}
		if p.Username != "" {
			u.User = url.UserPassword(p.Username, p.Password)
		}
		out.OutboundProxy = u.String()
	}
	if credential(out, "api_key") == "" {
		return out, errors.New("API 账号没有配置上游 Key")
	}
	return out, nil
}
func apiResponsesURL(base string) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("API 上游地址无效")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("API 上游端口无效")
		}
	}
	ip := net.ParseIP(u.Hostname())
	if strings.Contains(u.Hostname(), ":") && ip == nil {
		return "", errors.New("API 上游主机无效")
	}
	loop := u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && loop) {
		return "", errors.New("API 上游地址必须使用 HTTPS 或本机 HTTP")
	}
	if !strings.HasSuffix(u.Path, "/v1") {
		u.Path = strings.TrimRight(u.Path, "/") + "/v1"
	}
	u.Path += "/responses"
	return u.String(), nil
}

// IsLoopbackAdminURL is shared with bridge readiness. The port is deployment
// specific; credentials must never be sent to a non-loopback admin endpoint.
func IsLoopbackAdminURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return false
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return true
}
