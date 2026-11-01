package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Account struct {
	OutboundProxy string         `json:"-"`
	ID            int64          `json:"id"`
	Name          string         `json:"name"`
	Platform      string         `json:"platform"`
	Type          string         `json:"type"`
	Status        string         `json:"status"`
	Schedulable   bool           `json:"schedulable"`
	UpdatedAt     string         `json:"updated_at"`
	ExpiresAt     *time.Time     `json:"expires_at"`
	ParentID      *int64         `json:"parent_account_id"`
	ProxyID       *int64         `json:"proxy_id"`
	Credentials   map[string]any `json:"credentials"`
	Extra         map[string]any `json:"extra"`
}
type Admin interface {
	Account(context.Context, int64) (Account, error)
	Pause(context.Context, int64) (Account, error)
	Resume(context.Context, int64) (Account, error)
	Proxy(context.Context, int64) (string, error)
}
type AdminClient struct {
	Base, Key string
	HTTP      *http.Client
	Enrich    func(context.Context, *Account) error
}

func NewAdmin(c Config) *AdminClient {
	return &AdminClient{Base: c.AdminURL, Key: c.AdminKey, HTTP: &http.Client{Timeout: 15 * time.Second, CheckRedirect: NoRedirect}}
}
func NoRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
func (a *AdminClient) call(ctx context.Context, method, path string, body any, out any) error {
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			return errors.New("管理请求编码失败")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, a.Base+path, bytes.NewReader(b))
	if err != nil {
		return errors.New("管理地址无效")
	}
	req.Header.Set("x-api-key", a.Key)
	req.Header.Set("Content-Type", "application/json")
	res, err := a.HTTP.Do(req)
	if err != nil {
		return errors.New("Sub2API 管理接口连接失败")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		if res.StatusCode == 401 {
			return errors.New("管理员访问密钥无效或未填写（HTTP 401）")
		}
		if res.StatusCode == 403 {
			return errors.New("管理员访问密钥缺少该操作或账号范围的权限（HTTP 403）")
		}
		return fmt.Errorf("Sub2API 管理接口 HTTP %d", res.StatusCode)
	}
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&env) != nil || env.Code != 0 || len(env.Data) == 0 {
		return errors.New("Sub2API 管理接口响应无效")
	}
	if json.Unmarshal(env.Data, out) != nil {
		return errors.New("Sub2API 管理接口数据格式不兼容")
	}
	return nil
}
func (a *AdminClient) Account(ctx context.Context, id int64) (Account, error) {
	var x Account
	err := a.call(ctx, "GET", fmt.Sprintf("/api/v1/admin/accounts/%d", id), nil, &x)
	if err == nil && x.ID != id {
		err = errors.New("管理接口返回了不同的账号")
	}
	if err == nil && (x.UpdatedAt == "" || x.Type == "apikey") && a.Enrich != nil {
		err = a.Enrich(ctx, &x)
	}
	return x, err
}
func (a *AdminClient) Pause(ctx context.Context, id int64) (Account, error) {
	var receipt Account
	if err := a.call(ctx, "POST", fmt.Sprintf("/api/v1/admin/accounts/%d/schedulable", id), map[string]bool{"schedulable": false}, &receipt); err != nil {
		return receipt, err
	}
	if receipt.ID != id {
		return receipt, errors.New("调度写入回执未确认")
	}
	// Scoped mutation receipts contain only id/status. Resolve the account via
	// the scoped read endpoint, never infer OAuth from an empty receipt type.
	// A failed read is an uncertain result, not permission to repeat the write.
	x, err := a.Account(ctx, id)
	if err == nil && x.Schedulable != false {
		err = errors.New("调度写入后的状态未确认")
	}
	return x, err
}
func (a *AdminClient) Proxy(ctx context.Context, id int64) (string, error) {
	var p struct {
		Protocol string `json:"protocol"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := a.call(ctx, "GET", fmt.Sprintf("/api/v1/admin/proxies/%d", id), nil, &p); err != nil {
		return "", err
	}
	if p.Protocol != "http" && p.Protocol != "https" && p.Protocol != "socks5" && p.Protocol != "socks5h" {
		return "", errors.New("不支持账号代理协议")
	}
	u := url.URL{Scheme: p.Protocol, Host: strings.Trim(p.Host, "[]") + ":" + strconv.Itoa(p.Port)}
	if strings.Contains(p.Host, ":") {
		u.Host = "[" + strings.Trim(p.Host, "[]") + "]:" + strconv.Itoa(p.Port)
	}
	if p.Username != "" {
		u.User = url.UserPassword(p.Username, p.Password)
	}
	return u.String(), nil
}
func credential(a Account, k string) string { v, _ := a.Credentials[k].(string); return v }
func mappedModel(a Account, model string) string {
	if m, ok := a.Credentials["model_mapping"].(map[string]any); ok {
		if v, ok := m[model].(string); ok && v != "" {
			return v
		}
	}
	return model
}

func (a *AdminClient) Resume(ctx context.Context, id int64) (Account, error) {
	var receipt Account
	if err := a.call(ctx, "POST", fmt.Sprintf("/api/v1/admin/accounts/%d/schedulable", id), map[string]bool{"schedulable": true}, &receipt); err != nil {
		return receipt, err
	}
	if receipt.ID != id {
		return receipt, errors.New("调度写入回执未确认")
	}
	// Scoped mutation receipts contain only id/status. Resolve the account via
	// the scoped read endpoint, never infer OAuth from an empty receipt type.
	// A failed read is an uncertain result, not permission to repeat the write.
	x, err := a.Account(ctx, id)
	if err == nil && x.Schedulable != true {
		err = errors.New("调度写入后的状态未确认")
	}
	return x, err
}
