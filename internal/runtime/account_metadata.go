package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cyeinfpro/ModelGuard/internal/guard"
	v1 "github.com/cyeinfpro/ModelGuard/internal/pluginv1"
)

const accountMetadataTTL = 5 * time.Second

type accountMetadataEntry struct {
	accounts  map[int64]*v1.AccountInfo
	expires   time.Time
	loading   chan struct{}
	hostEpoch uint64
}

func (p *Plugin) invalidateMetadata() {
	p.metadataMu.Lock()
	p.metadataCache = nil
	p.metadataMu.Unlock()
}

// A forced read starts after any previous group read completes. A post-write
// verification therefore never joins a request that started before the write.
func (p *Plugin) accountMetadata(ctx context.Context, platform, accountType string, force bool) (map[int64]*v1.AccountInfo, bool, error) {
	key := platform + "\x00" + accountType
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		p.mu.RLock()
		host, epoch := p.host, p.hostEpoch
		if host == nil {
			p.mu.RUnlock()
			return nil, false, errors.New("宿主缺少账号元数据接口，无法校验隔离归属")
		}
		p.metadataMu.Lock()
		if p.metadataCache == nil {
			p.metadataCache = make(map[string]*accountMetadataEntry)
		}
		entry := p.metadataCache[key]
		if entry == nil || entry.hostEpoch != epoch {
			entry = &accountMetadataEntry{hostEpoch: epoch}
			p.metadataCache[key] = entry
		}
		if entry.loading != nil {
			ready := entry.loading
			p.metadataMu.Unlock()
			p.mu.RUnlock()
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case <-ready:
			}
			continue
		}
		if !force && entry.accounts != nil && time.Now().Before(entry.expires) {
			accounts := entry.accounts
			p.metadataMu.Unlock()
			p.mu.RUnlock()
			return accounts, true, nil
		}
		ready := make(chan struct{})
		entry.loading = ready
		p.metadataMu.Unlock()
		p.mu.RUnlock()
		readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		response, err := host.ListAccounts(readCtx, &v1.ListAccountsRequest{Platform: platform, AccountType: accountType})
		cancel()
		accounts := make(map[int64]*v1.AccountInfo)
		if err == nil && response != nil {
			for _, account := range response.Accounts {
				if account != nil {
					accounts[account.Id] = account
				}
			}
		} else {
			err = errors.New("读取宿主账号元数据失败，保留当前调度状态")
		}
		p.mu.RLock()
		if p.hostEpoch != epoch {
			err = errors.New("宿主连接已更新，请重新读取账号元数据")
		}
		p.metadataMu.Lock()
		if err == nil {
			entry.accounts = accounts
			entry.expires = time.Now().Add(accountMetadataTTL)
		} else {
			entry.expires = time.Time{}
		}
		entry.loading = nil
		close(ready)
		p.metadataMu.Unlock()
		p.mu.RUnlock()
		if err != nil {
			return nil, false, err
		}
		return accounts, false, nil
	}
}

// These reads only complement an already authorized scoped management read.
// The cache never expands the authorized account scope.
func (p *Plugin) enrichAccount(ctx context.Context, a *guard.Account) error {
	return p.enrichAccountWithFresh(ctx, a, false)
}
func (p *Plugin) enrichAccountFresh(ctx context.Context, a *guard.Account) error {
	return p.enrichAccountWithFresh(ctx, a, true)
}
func (p *Plugin) enrichAccountWithFresh(ctx context.Context, a *guard.Account, force bool) error {
	accounts, cached, err := p.accountMetadata(ctx, a.Platform, a.Type, force)
	if err != nil {
		return err
	}
	err = applyAccountMetadata(accounts[a.ID], a)
	if err != nil && cached {
		accounts, _, freshErr := p.accountMetadata(ctx, a.Platform, a.Type, true)
		if freshErr != nil {
			return freshErr
		}
		return applyAccountMetadata(accounts[a.ID], a)
	}
	return err
}
func applyAccountMetadata(info *v1.AccountInfo, a *guard.Account) error {
	if info == nil {
		return errors.New("宿主范围内未找到该账号，保留当前调度状态")
	}
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
