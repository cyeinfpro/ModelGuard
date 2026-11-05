package runtime

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cyeinfpro/ModelGuard/internal/guard"
)

const (
	manualDiscoveryInterval = 120 * time.Second
	groupDiscoveryInterval  = 15 * time.Second
	discoveryMaxBackoff     = 300 * time.Second
)

func baseDiscoveryInterval(c guard.Config) time.Duration {
	if len(c.GroupIDs) > 0 {
		return groupDiscoveryInterval
	}
	return manualDiscoveryInterval
}

func discoveryDelay(c guard.Config, failures int, err error) (time.Duration, bool) {
	if err == nil {
		return baseDiscoveryInterval(c), false
	}
	var httpError *guard.AdminHTTPError
	if errors.As(err, &httpError) && (httpError.StatusCode == 401 || httpError.StatusCode == 403) {
		return baseDiscoveryInterval(c), true
	}
	// Compatibility with older permission errors and synthetic diagnostic tests.
	code := connectionErrorCode(err.Error())
	if code == "credentials_invalid" || code == "permission_denied" {
		return baseDiscoveryInterval(c), true
	}
	delay := 10 * time.Second
	for n := 1; n < failures && delay < discoveryMaxBackoff; n++ {
		delay *= 2
	}
	if delay > discoveryMaxBackoff {
		delay = discoveryMaxBackoff
	}
	if httpError != nil && httpError.RetryAfter > delay {
		delay = httpError.RetryAfter
	}
	return delay, false
}

func (p *Plugin) discoveryResultLocked(c guard.Config, err error) {
	if err == nil {
		p.discoveryFailures = 0
	} else {
		p.discoveryFailures++
	}
	p.discoveryInterval, p.discoverySuspended = discoveryDelay(c, p.discoveryFailures, err)
}

// The caller holds p.mu. A forced wake is coalesced but never downgraded by a
// later non-forced wake. Non-forced wakes only re-arm the current delay.
func (p *Plugin) wakeDiscoveryLocked(force bool) {
	if p.discoveryWake == nil {
		return
	}
	p.discoveryRefresh = p.discoveryRefresh || force
	select {
	case p.discoveryWake <- struct{}{}:
	default:
	}
}

func (p *Plugin) discoveryLoop(ctx context.Context, _ guard.Config) {
	p.mu.RLock()
	wake := p.discoveryWake
	p.mu.RUnlock()
	refresh := true
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		p.mu.Lock()
		c := p.config
		if p.discoveryRefresh {
			refresh = true
			p.discoveryRefresh = false
		}
		delay, suspended := p.discoveryInterval, p.discoverySuspended
		p.mu.Unlock()
		if delay <= 0 {
			delay = baseDiscoveryInterval(c)
		}
		if strings.TrimSpace(c.AdminKey) == "" {
			select {
			case <-ctx.Done():
				return
			case <-wake:
				refresh = true
				continue
			}
		}
		if refresh {
			_, _ = p.refreshAccounts(ctx, c)
			refresh = false
			continue
		}
		if suspended {
			select {
			case <-ctx.Done():
				return
			case <-wake:
				continue
			}
		} else {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-wake:
				timer.Stop()
			case <-timer.C:
				refresh = true
			}
		}
	}
}
