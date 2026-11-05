package guard

import (
	"errors"
	"reflect"
	"sort"
	"time"
)

// targetsLocked never mutates Config; discovery and workers share only stateMu-protected snapshots.
func (e *Engine) targetsLocked() []Target {
	if e.managedTargetsSet {
		return e.managedTargets
	}
	if len(e.Config.GroupIDs) > 0 {
		return nil
	} // Membership must be confirmed before the first sample.
	return e.Config.Targets
}
func (e *Engine) ManagedTargets() []Target {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	return append([]Target{}, e.targetsLocked()...)
}
func (e *Engine) targetEnabledLocked(id int64) bool {
	if len(e.Config.GroupIDs) > 0 && e.groupSyncBlocked {
		return false
	}
	for _, t := range e.targetsLocked() {
		if t.AccountID == id {
			return t.Enabled
		}
	}
	return false
}
func (e *Engine) targetGenerationLocked(id int64) uint64 { return e.targetGenerations[id] }

// UpdateManagedTargets only changes local monitoring membership. It never patches
// an upstream account or discards held-account evidence/history.
func (e *Engine) UpdateManagedTargets(targets []Target) error {
	next := append([]Target{}, targets...)
	sort.Slice(next, func(i, j int) bool { return next[i].AccountID < next[j].AccountID })
	if len(next) > 100 {
		return errors.New("分组成员超过 100 个；保留原监测目标")
	}
	for i, t := range next {
		if t.AccountID <= 0 || (i > 0 && next[i-1].AccountID == t.AccountID) {
			return errors.New("分组成员 ID 无效")
		}
	}
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	defer e.wakeScheduler()
	return e.updateManagedTargetsLocked(next)
}

func (e *Engine) updateManagedTargetsLocked(next []Target) error {
	if e.stopped.Load() {
		return errors.New("监测已停止")
	}
	if e.managedTargetsSet && reflect.DeepEqual(e.managedTargets, next) {
		return e.resumeGroupSyncLocked()
	}
	old := map[int64]Target{}
	for _, t := range e.targetsLocked() {
		old[t.AccountID] = t
	}
	current := map[int64]Target{}
	for _, t := range next {
		current[t.AccountID] = t
	}
	if e.targetGenerations == nil {
		e.targetGenerations = map[int64]uint64{}
	}
	for id, t := range old {
		if n, ok := current[id]; !ok || n != t {
			e.invalidateTargetLocked(id)
		}
	}
	for id := range current {
		if _, ok := old[id]; !ok {
			e.invalidateTargetLocked(id)
		}
	}
	e.managedTargets = next
	e.managedTargetsSet = true
	now := time.Now().UTC()
	changed := false
	for id, r := range e.Store.State.Records {
		t, present := current[id]
		if !present || !t.Enabled {
			if r.Status != "archived" {
				r.Status = "archived"
				r.Reason = "账号已删除、移出配置分组或取消监测；历史和隔离证据保留"
				r.RecoveryPasses = 0
				e.Store.Event(id, "target_archived", r.Reason, now)
				changed = true
			}
		} else if r.Status == "archived" {
			r.Status = "pending"
			r.Reason = "账号重新加入监测，重新验证"
			r.RecoveryPasses = 0
			r.NextAt = now
			e.Store.Event(id, "target_rejoined", r.Reason, now)
			changed = true
		}
	}
	if changed {
		if err := e.save(); err != nil {
			e.publishLocked(err.Error())
			return err
		}
	}
	return e.resumeGroupSyncLocked()
}

// TargetsForGroups requires a complete successful account discovery. Missing
// membership metadata is an error, never evidence that a group became empty.
func TargetsForGroups(c Config, accounts []AccountOption) ([]Target, []int64, error) {
	groups := map[int64]bool{}
	for _, id := range c.GroupIDs {
		if id <= 0 || groups[id] {
			return nil, nil, errors.New("分组 ID 必须为唯一正整数")
		}
		groups[id] = true
	}
	existing := map[int64]Target{}
	for _, t := range c.Targets {
		existing[t.AccountID] = t
	}
	targets := []Target{}
	pending := []int64{}
	seenAccounts := map[int64]bool{}
	for _, a := range accounts {
		if a.ID <= 0 || seenAccounts[a.ID] {
			return nil, nil, errors.New("账号列表包含无效或重复 ID；保留原监测目标")
		}
		seenAccounts[a.ID] = true
		if a.GroupIDs == nil {
			return nil, nil, errors.New("账号分组元数据缺失；保留原监测目标")
		}
		member := false
		for _, id := range a.GroupIDs {
			if id <= 0 {
				return nil, nil, errors.New("账号分组元数据无效")
			}
			if groups[id] {
				member = true
			}
		}
		if !member || !a.Supported {
			continue
		}
		if t, ok := existing[a.ID]; ok {
			targets = append(targets, t)
		} else if c.GroupModel != "" && c.GroupExpected != "" {
			targets = append(targets, Target{AccountID: a.ID, Model: c.GroupModel, Expected: c.GroupExpected, Enabled: true})
		} else {
			pending = append(pending, a.ID)
		}
	}
	if len(targets)+len(pending) > 100 {
		return nil, nil, errors.New("分组成员超过 100 个；保留原监测目标")
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].AccountID < targets[j].AccountID })
	sort.Slice(pending, func(i, j int) bool { return pending[i] < pending[j] })
	return targets, pending, nil
}

// SetGroupSyncBlocked retains membership/history but invalidates every in-flight
// round when its membership can no longer be verified. Same lock order as control.
func (e *Engine) SetGroupSyncBlocked(blocked bool) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if len(e.Config.GroupIDs) == 0 || e.stopped.Load() {
		return
	}
	if !blocked {
		return
	} // Only a complete successful UpdateManagedTargets may reopen.
	if e.groupSyncBlocked {
		return
	}
	e.groupSyncBlocked = true
	if e.targetGenerations == nil {
		e.targetGenerations = map[int64]uint64{}
	}
	changed := false
	for _, t := range e.targetsLocked() {
		e.invalidateTargetLocked(t.AccountID)
		if r := e.Store.State.Records[t.AccountID]; r != nil && r.RecoveryPasses != 0 {
			r.RecoveryPasses = 0
			changed = true
		}
	}
	if changed {
		_ = e.save()
	}
	e.publishLocked("")
}
func (e *Engine) resumeGroupSyncLocked() error {
	if e.groupSyncBlocked {
		now := time.Now().UTC()
		for _, t := range e.targetsLocked() {
			if r := e.Store.State.Records[t.AccountID]; r != nil && t.Enabled {
				r.NextAt = now
			}
		}
	}
	if e.groupSyncBlocked || e.storageError != "" {
		if err := e.save(); err != nil {
			e.publishLocked(err.Error())
			return err
		}
	}
	e.groupSyncBlocked = false
	e.wakeScheduler()
	e.publishLocked("")
	return nil
}
