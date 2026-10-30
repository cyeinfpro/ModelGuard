package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/cyeinfpro/ModelGuard/internal/guard"
	v1 "github.com/cyeinfpro/ModelGuard/internal/pluginv1"
	"time"
)

// Called only after the scoped management key successfully reads/writes this
// exact account. The host fills missing non-secret safety fields; it never
// expands the account list or bypasses a failed management permission check.
func (p *Plugin) enrichAccount(ctx context.Context, a *guard.Account) error {
	p.mu.RLock()
	h := p.host
	p.mu.RUnlock()
	if h == nil {
		return errors.New("宿主缺少账号元数据接口，无法校验隔离归属")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	r, err := h.ListAccounts(ctx, &v1.ListAccountsRequest{Platform: a.Platform, AccountType: a.Type})
	if err != nil || r == nil {
		return errors.New("读取宿主账号元数据失败，保留当前调度状态")
	}
	for _, info := range r.Accounts {
		if info == nil || info.Id != a.ID {
			continue
		}
		// Host metadata uses the Go account model's field names, not API DTO names.
		var m struct {
			ID              int64
			UpdatedAt       time.Time
			ExpiresAt       *time.Time
			ParentAccountID *int64
			Schedulable     *bool
		}
		if json.Unmarshal(info.MetadataJson, &m) != nil || m.ID != a.ID || m.UpdatedAt.IsZero() || m.Schedulable == nil {
			return errors.New("宿主账号元数据缺少有效更新时间，保留当前调度状态")
		}
		if info.Platform != a.Platform || info.AccountType != a.Type || info.Status != a.Status || *m.Schedulable != a.Schedulable || info.IsShadow {
			return errors.New("账号在读取期间已变更，等待重新检测")
		}
		a.UpdatedAt = m.UpdatedAt.UTC().Format(time.RFC3339Nano)
		a.ExpiresAt = m.ExpiresAt
		a.ParentID = m.ParentAccountID
		return nil
	}
	return errors.New("宿主范围内未找到该账号，保留当前调度状态")
}
