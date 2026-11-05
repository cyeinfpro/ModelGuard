package guard

import (
	"context"
	"errors"
	"net"
	"slices"
	"sort"
	"time"
)

func cloneConfig(c Config) Config {
	c.Targets = append([]Target(nil), c.Targets...)
	c.GroupIDs = append([]int64(nil), c.GroupIDs...)
	return c
}
func (e *Engine) CurrentConfig() Config {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	return cloneConfig(e.Config)
}
func (e *Engine) wakeScheduler() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func (e *Engine) invalidateTargetLocked(id int64) {
	if e.targetGenerations == nil {
		e.targetGenerations = map[int64]uint64{}
	}
	e.targetGenerations[id]++
	if cancel := e.controlCancels[id]; cancel != nil {
		cancel()
	}
}

// UpdateConfig atomically replaces soft settings under the same lock used by
// decisions. Active samples retain their captured timeout; their next decision
// reads the current switches, and changed target generations discard old results.
func (e *Engine) UpdateConfig(c Config) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	defer e.wakeScheduler()
	if e.stopped.Load() || e.Suspended.Load() {
		return errors.New("监测正在应用配置或已停止")
	}
	old := e.Config
	if old.AdminURL != c.AdminURL || old.AdminKey != c.AdminKey || old.DataDir != c.DataDir {
		return errors.New("连接或状态目录变更需要重新启动监测")
	}
	if c.Concurrency < 1 || c.Concurrency > 8 || c.DailyRequests < 3 || c.TimeoutSeconds < 10 {
		return errors.New("配置值超出允许范围")
	}
	c = cloneConfig(c)
	groupChanged := !slices.Equal(old.GroupIDs, c.GroupIDs) || old.GroupModel != c.GroupModel || old.GroupExpected != c.GroupExpected || !slices.Equal(old.Targets, c.Targets)
	// Preserve old membership long enough to invalidate removed generations.
	oldTargets := append([]Target(nil), e.targetsLocked()...)
	e.Config = c
	if len(c.GroupIDs) == 0 {
		next := append([]Target{}, c.Targets...)
		sort.Slice(next, func(i, j int) bool { return next[i].AccountID < next[j].AccountID })
		if !e.managedTargetsSet {
			e.managedTargets, e.managedTargetsSet = oldTargets, true
		}
		if err := e.updateManagedTargetsLocked(next); err != nil {
			return err
		}
		e.groupSyncBlocked = false
	} else if groupChanged {
		for _, target := range oldTargets {
			e.invalidateTargetLocked(target.AccountID)
		}
		e.groupSyncBlocked = true
		e.managedTargets, e.managedTargetsSet = oldTargets, true
	}
	if old.AutoPause != c.AutoPause || old.AutoRecover != c.AutoRecover {
		for _, cancel := range e.controlCancels {
			cancel()
		}
	}
	now := time.Now().UTC()
	for id, r := range e.Store.State.Records {
		if e.inFlight[id] || r.CheckedAt.IsZero() || r.Status == StatusArchived {
			continue
		}
		interval := c.RetrySeconds
		switch r.Status {
		case StatusPassed:
			interval = c.NormalSeconds
		case StatusQuarantined, StatusRecoveryReady:
			interval = c.QuarantineSeconds
		}
		// A settings-only save must not reset completed-round timing to now.
		r.NextAt = r.CheckedAt.Add(time.Duration(interval) * time.Second)
		if r.Hold && r.RecoveryPasses == 1 && c.RecoverySpacingSeconds > 0 {
			if spacing := r.RecoveryAt.Add(time.Duration(c.RecoverySpacingSeconds) * time.Second); spacing.After(r.NextAt) {
				r.NextAt = spacing
			}
		}
		if r.Status == StatusBlocked && e.Store.State.Day == now.Format("2006-01-02") && e.Store.State.Requests+e.reservedRequests >= c.DailyRequests {
			r.NextAt = nextBudgetReset(now)
		}
	}
	e.Store.Event(0, "config_applied", "检测策略已热更新；进行中的采样继续，后续决策使用新配置", now)
	if err := e.save(); err != nil {
		e.publishLocked(err.Error())
		return err
	}
	e.publishLocked("")
	return nil
}

// StopCurrent cancels requests and pending manual work without changing any
// existing account pause. Automatic monitoring resumes at its next retry time.
func (e *Engine) StopCurrent() bool {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	defer e.wakeScheduler()
	stopped := len(e.activeCancels) > 0 || e.ManualBusy.Load() || e.BatchBusy.Load()
	e.stopEpoch++
	now := time.Now().UTC()
	for id, cancel := range e.activeCancels {
		e.invalidateTargetLocked(id)
		cancel()
	}
	for _, cancel := range e.controlCancels {
		cancel()
	}
	for _, t := range e.targetsLocked() {
		if !t.Enabled {
			continue
		}
		r := e.Store.State.Records[t.AccountID]
		if r == nil {
			r = &Record{AccountID: t.AccountID, Model: t.Model, Expected: t.Expected, Status: StatusPending}
			e.Store.State.Records[t.AccountID] = r
		}
		if !r.NextAt.After(now) || e.inFlight[t.AccountID] {
			r.NextAt = now.Add(time.Duration(e.Config.RetrySeconds) * time.Second)
		}
	}
	if run := e.Store.State.Manual; run != nil && (run.Status == "queued" || run.Status == "running") {
		run.Status = "interrupted"
		run.FinishedAt = now
		run.CurrentID = 0
		run.CurrentIDs = nil
		run.Message = "当前检测已停止；已有暂停保持不变"
	}
	for len(e.manualQueue) > 0 {
		<-e.manualQueue
	}
	e.ManualQueued.Store(false)
	e.ManualBusy.Store(false)
	e.manualTargets = nil
	e.Store.Event(0, "checks_stopped", "用户停止当前检测；已有暂停保持不变", now)
	if err := e.save(); err != nil {
		e.publishLocked(err.Error())
	} else {
		e.publishLocked("")
	}
	return stopped
}

func transientControlError(err error) bool {
	var connection *AdminConnectionError
	if errors.As(err, &connection) {
		return true
	}
	var response *AdminHTTPError
	if errors.As(err, &response) {
		return response.StatusCode == 429 || response.StatusCode >= 500
	}
	var network net.Error
	return errors.As(err, &network) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
