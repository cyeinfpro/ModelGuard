package guard

import (
	"context"
	"errors"
	"fmt"
	"github.com/cyeinfpro/ModelGuard/internal/fingerprint"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type Engine struct {
	stateMu           sync.Mutex // guards records, progress, budget reservations and durable writes
	batchMu           sync.Mutex // manual and scheduled batches share the same worker limit
	activeSamples     map[int64]*ActiveSample
	inFlight          map[int64]bool
	reservedRequests  int
	managedTargets    []Target
	managedTargetsSet bool
	targetGenerations map[int64]uint64
	groupSyncBlocked  bool

	Config                Config
	Store                 *Store
	Admin                 Admin
	Probe                 Probe
	Detector              *fingerprint.Detector
	snapshotMu            sync.Mutex
	snapshot              string
	snapshotRevision      uint64
	snapshotDay           string
	stateSnapshotRevision uint64
	stateSnapshot         []byte
	modelsJSON            []byte
	revision              uint64
	stateRevision         uint64
	wake                  chan struct{}
	activeCancels         map[int64]context.CancelFunc
	controlCancels        map[int64]context.CancelFunc
	running               atomic.Bool
	stopEpoch             uint64
	manualTargets         []Target
	lastError             string
	storageError          string // guarded by stateMu, separate from transient account errors
	stopped               atomic.Bool
	Suspended             atomic.Bool
	ManualBusy            atomic.Bool
	ManualQueued          atomic.Bool
	BatchBusy             atomic.Bool
	batch                 *BatchState
	readyAt               time.Time
	manualQueue           chan string
	lastManual            string
	// Injection point for deterministic end-to-end policy tests.
	Evaluate func(context.Context, Account, Target) (*fingerprint.Analysis, string, error)
}

func NewEngine(c Config, d *fingerprint.Detector, a Admin, p Probe) (*Engine, error) {
	s, err := OpenStore(c.DataDir)
	if err != nil {
		return nil, err
	}
	if s.State.AdminURL != "" && s.State.AdminURL != c.AdminURL {
		s.Close()
		return nil, errors.New("同一状态目录不能用于不同 Sub2API 实例")
	}
	s.State.AdminURL = c.AdminURL
	if err = s.Save(); err != nil {
		s.Close()
		return nil, err
	}
	if s.State.Manual != nil && (s.State.Manual.Status == "running" || s.State.Manual.Status == "queued") {
		s.State.Manual.Status = "interrupted"
		s.State.Manual.CurrentID = 0
		s.State.Manual.CurrentIDs = nil
		s.State.Manual.FinishedAt = time.Now().UTC()
		s.State.Manual.Message = "检测被重启或配置变更中断，请重新发起"
		if err := s.Save(); err != nil {
			s.Close()
			return nil, err
		}
	}
	e := &Engine{Config: c, Store: s, Admin: a, Probe: p, Detector: d, manualQueue: make(chan string, 1), activeSamples: map[int64]*ActiveSample{}, inFlight: map[int64]bool{}, wake: make(chan struct{}, 1), activeCancels: map[int64]context.CancelFunc{}, controlCancels: map[int64]context.CancelFunc{}}
	if s.State.Manual != nil {
		e.lastManual = s.State.Manual.ID
	}
	e.readyAt = time.Now().UTC().Add(60 * time.Second)
	e.initializeSnapshot()
	e.publish("")
	return e, nil
}
func (e *Engine) publish(message string) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.publishLocked(message)
}
func (e *Engine) publishLocked(message string) {
	if message != "" {
		e.lastError = message
	}
	e.revision++
}
func (e *Engine) markStateDirtyLocked() { e.stateRevision++; e.revision++ }

// TrySuspend closes the scheduling gate atomically with the idle check so a
// timer cannot start a batch between the runtime's busy check and cancellation.
func (e *Engine) TrySuspend() bool {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if e.ManualBusy.Load() || e.BatchBusy.Load() {
		return false
	}
	e.Suspended.Store(true)
	return true
}

func (e *Engine) Resume() {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if !e.stopped.Load() {
		e.Suspended.Store(false)
		e.wakeScheduler()
	}
}

func (e *Engine) clearLastErrorLocked() {
	if e.storageError != "" {
		return
	}
	for _, target := range e.targetsLocked() {
		if !target.Enabled {
			continue
		}
		r := e.Store.State.Records[target.AccountID]
		if r != nil && (recordNeedsAttention(r) || (e.Config.AutoRecover && r.Hold && r.Status == "recovery_ready")) {
			// A different account finishing successfully does not resolve this
			// account's outstanding control/probe failure.
			return
		}
	}
	e.lastError = ""
	e.revision++
}

func (e *Engine) save() error {
	e.markStateDirtyLocked()
	if err := e.Store.Save(); err != nil {
		e.storageError = "持久化失败，停止账号变更；请检查数据目录可写性和磁盘空间"
		e.wakeScheduler()
		return errors.New(e.storageError)
	}
	e.storageError = ""
	return nil
}
func (e *Engine) Tick(ctx context.Context, now time.Time) error {
	if e.running.Load() {
		e.wakeScheduler()
		return nil
	}
	c := e.CurrentConfig()
	if !c.Enabled {
		return nil
	}
	return e.runTargets(ctx, now, e.ManagedTargets(), false, nil, nil)
}
func (e *Engine) tickTargets(ctx context.Context, now time.Time, selected []Target, force bool) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()

	c := e.Config
	targets := append([]Target(nil), selected...)
	sort.SliceStable(targets, func(i, j int) bool {
		a, b := e.Store.State.Records[targets[i].AccountID], e.Store.State.Records[targets[j].AccountID]
		if a == nil {
			return b != nil
		}
		if b == nil {
			return false
		}
		return a.NextAt.Before(b.NextAt)
	})
	for _, t := range targets {
		if !t.Enabled || !e.targetEnabledLocked(t.AccountID) {
			continue
		}
		currentTarget := false
		for _, current := range e.targetsLocked() {
			if current == t {
				currentTarget = true
				break
			}
		}
		if !currentTarget {
			continue
		}
		generation := e.targetGenerationLocked(t.AccountID)
		r := e.Store.State.Records[t.AccountID]
		if r == nil {
			r = &Record{AccountID: t.AccountID, Model: t.Model, Expected: t.Expected, Status: "unverified"}
			e.Store.State.Records[t.AccountID] = r
		}
		if r.Model != t.Model || r.Expected != t.Expected {
			if r.Hold || r.Pending {
				r.Status = "blocked"
				r.Reason = "隔离期间不能更换检测模型；请先处理原隔离记录"
				r.NextAt = now.Add(time.Hour)
				return e.save()
			}
			*r = Record{AccountID: t.AccountID, Model: t.Model, Expected: t.Expected, Status: "unverified"}
		}
		if !force && now.Before(r.NextAt) {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e.inFlight[t.AccountID] {
			continue
		}
		e.inFlight[t.AccountID] = true
		ctx, cancel := context.WithCancel(ctx)
		e.activeCancels[t.AccountID] = cancel
		defer func() {
			cancel()
			delete(e.inFlight, t.AccountID)
			delete(e.activeCancels, t.AccountID)
		}()
		// Persist retry backoff even for control-plane failures; one broken account
		// must not starve the rest of the allowlist.
		r.NextAt = now.Add(time.Duration(c.RetrySeconds) * time.Second)
		if err := e.save(); err != nil {
			return err
		}
		e.stateMu.Unlock()
		a, err := e.Admin.Account(ctx, t.AccountID)
		e.stateMu.Lock()
		if !e.targetEnabledLocked(t.AccountID) || generation != e.targetGenerationLocked(t.AccountID) {
			return e.save()
		}
		if err != nil {
			if !transientControlError(err) {
				r.RecoveryPasses = 0
				r.RecoveryAt = time.Time{}
			}
			r.Status = "blocked"
			r.Reason = err.Error()
			if saved := e.save(); saved != nil {
				return saved
			}
			return err
		}
		if a.Platform != "openai" || (a.Type != "oauth" && a.Type != "apikey") || a.ParentID != nil {
			r.Status = "unsupported"
			r.RecoveryPasses = 0
			r.Reason = "仅支持独立 OpenAI OAuth 或 API Key 账号"
			return e.save()
		}
		if r.Pending {
			r.Pending = false
			r.RecoveryPasses = 0
			r.Reason = "已重新读取调度状态，继续由插件管理"
			e.Store.Event(a.ID, "reconcile", r.Reason, now)
		}
		if r.Hold && a.Schedulable {
			r.Hold = false
			r.Failures = 0
			r.Unavailable = 0
			r.RecoveryPasses = 0
			r.PauseUpdatedAt = ""
			e.Store.Event(a.ID, "external_resume", "账号已由外部恢复，清除本插件隔离计数", now)
		}
		if a.Status != "active" || (a.ExpiresAt != nil && !now.Before(*a.ExpiresAt)) {
			r.Status = "skipped"
			r.RecoveryPasses = 0
			r.Reason = "账号非 active 或已过期"
			return e.save()
		}
		// Enabled targets are explicitly managed by the plugin. General account
		// updates and external scheduling pauses do not revoke that authority.
		// Legacy uncertain/external-change holds require two fresh valid rounds.
		if !a.Schedulable && (!r.Hold || r.Status == "external_change" || r.Status == "pause_uncertain") {
			r.Hold = true
			r.RecoveryPasses = 0
			r.Status = "quarantined"
			r.Reason = "账号已纳入插件暂停管理，连续两轮有效通过后恢复"
			e.Store.Event(a.ID, "managed_pause", r.Reason, now)
			if err := e.save(); err != nil {
				return err
			}
		}
		// Do not spend the remainder on a round that cannot collect all three samples.
		used := 0
		if e.Store.State.Day == time.Now().UTC().Format("2006-01-02") {
			used = e.Store.State.Requests
		}
		if c.DailyRequests-used-e.reservedRequests < 3 {
			r.RecoveryPasses = 0
			r.Status = "blocked"
			r.Reason = "今日剩余请求预算不足一轮（需要 3 次），等待次日或调整预算"
			r.NextAt = nextBudgetReset(time.Now())
			if err := e.save(); err != nil {
				return err
			}
			return errors.New(r.Reason)
		}
		var analysis *fingerprint.Analysis
		var reason string
		remaining := 3
		e.reservedRequests += remaining
		reserve := func(retry bool) error {
			e.stateMu.Lock()
			defer e.stateMu.Unlock()
			if !e.targetEnabledLocked(t.AccountID) || generation != e.targetGenerationLocked(t.AccountID) {
				return errors.New("账号已移出监测范围，停止后续采样")
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			budgetLimit := e.Config.DailyRequests
			if retry {
				used := 0
				if e.Store.State.Day == time.Now().UTC().Format("2006-01-02") {
					used = e.Store.State.Requests
				}
				if used+e.reservedRequests >= budgetLimit {
					return errors.New("重试预算不足，保留其他账号完整采样预算")
				}
			}
			if err := e.Store.ReserveRequest(time.Now(), budgetLimit); err != nil {
				return err
			}
			e.markStateDirtyLocked()
			if !retry {
				remaining--
				e.reservedRequests--
			}
			return nil
		}
		e.stateMu.Unlock()
		if e.Evaluate != nil {
			analysis, reason, err = e.Evaluate(ctx, a, t)
		} else {
			analysis, reason, err = Evaluate(ctx, a, t, c, &sampleRecorder{engine: e, probe: e.Probe}, e.Detector, reserve)
		}
		e.stateMu.Lock()
		e.reservedRequests -= remaining
		if !e.targetEnabledLocked(t.AccountID) || generation != e.targetGenerationLocked(t.AccountID) {
			return e.save()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			r.RecoveryPasses = 0
			r.Analysis = nil
			r.CheckedAt = time.Now().UTC()
			r.NextAt = r.CheckedAt.Add(time.Duration(c.RetrySeconds) * time.Second)
			r.Status = "blocked"
			r.Reason = err.Error()
			if saved := e.save(); saved != nil {
				return saved
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Policy switches and intervals take effect at the next decision, while
		// the already-started sampling timeout remains stable for that round.
		c = e.Config
		verdict := "unavailable"
		if analysis != nil {
			verdict = analysis.Decision
			if analysis.Used < 3 {
				verdict = "unavailable"
			} else if analysis.MatchScope == "family" && !c.FamilyMatch {
				copy := *analysis
				copy.Decision = "indeterminate"
				copy.MatchScope = ""
				analysis = &copy
				verdict = "indeterminate"
				reason = "家族匹配已关闭；本轮完整采样无法确认精确参考类"
			}
		}
		r.Analysis = analysis
		done := time.Now().UTC()
		pause := Transition(r, verdict, reason, done, c)
		e.Store.Event(a.ID, "check", fmt.Sprintf("%s: %s", verdict, reason), done)
		if err = e.save(); err != nil {
			return err
		}
		if pause {
			return e.pause(ctx, a, r, done, generation)
		}
		if r.Hold && r.RecoveryPasses >= 2 && c.AutoRecover {
			return e.recover(ctx, r, done, generation)
		}
		return nil
	}
	return nil
}

// The caller holds stateMu. inFlight excludes another worker for this account;
// control-plane I/O releases the global lock so unrelated accounts keep moving.
func (e *Engine) pause(ctx context.Context, before Account, r *Record, now time.Time, generation uint64) error {
	if !e.targetEnabledLocked(r.AccountID) || generation != e.targetGenerationLocked(r.AccountID) {
		return e.save()
	}
	e.stateMu.Unlock()
	latest, err := e.Admin.Account(ctx, r.AccountID)
	e.stateMu.Lock()
	if !e.targetEnabledLocked(r.AccountID) || generation != e.targetGenerationLocked(r.AccountID) {
		return e.save()
	}
	if err != nil {
		r.Status = "blocked"
		r.Reason = "暂停前校验失败：" + err.Error()
		r.NextAt = now.Add(time.Duration(e.Config.RetrySeconds) * time.Second)
		if saved := e.save(); saved != nil {
			return saved
		}
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !latest.Schedulable {
		r.Hold = true
		r.Pending = false
		r.RecoveryPasses = 0
		r.Status = "quarantined"
		r.Reason = "账号已暂停，纳入插件管理，无需重复暂停"
		e.Store.Event(r.AccountID, "managed_pause", r.Reason, now)
		return e.save()
	}
	if latest.Status != "active" || (latest.ExpiresAt != nil && !time.Now().Before(*latest.ExpiresAt)) {
		r.Status = "blocked"
		r.Reason = "暂停前账号状态不可用，本轮不执行暂停"
		return e.save()
	}
	if !e.Config.AutoPause {
		return nil
	}
	controlCtx, cancel := context.WithCancel(ctx)
	e.controlCancels[r.AccountID] = cancel
	defer func() { cancel(); delete(e.controlCancels, r.AccountID) }()
	r.Pending = true
	e.Store.Event(r.AccountID, "pause_intent", r.Reason, now)
	if err = e.save(); err != nil {
		return err
	}
	e.publishLocked("")
	e.stateMu.Unlock()
	paused, err := e.Admin.Pause(controlCtx, r.AccountID)
	e.stateMu.Lock()
	if err != nil {
		// A lost response does not prove the server failed to pause. Preserve
		// intent for reconciliation before any further scheduling write.
		r.Status = "pause_uncertain"
		r.Reason = "暂停回执未确认；下次检测会核对调度状态：" + err.Error()
		r.NextAt = time.Now().UTC().Add(time.Duration(e.Config.RetrySeconds) * time.Second)
		e.Store.Event(r.AccountID, "pause_unconfirmed", r.Reason, time.Now().UTC())
		if !e.targetEnabledLocked(r.AccountID) {
			r.Status = StatusArchived
		}
		if saved := e.save(); saved != nil {
			return saved
		}
		return err
	}
	r.Pending = false
	r.Hold = true
	r.PausedAt = now
	r.PauseUpdatedAt = paused.UpdatedAt
	r.RecoveryPasses = 0
	r.Status = "quarantined"
	r.NextAt = now.Add(time.Duration(e.Config.QuarantineSeconds) * time.Second)
	e.Store.Event(r.AccountID, "paused", r.Reason, now)
	if !e.targetEnabledLocked(r.AccountID) {
		r.Status = StatusArchived
		r.Reason = "暂停回执已记录；账号已移出监测范围，不再自动处理"
	}
	return e.save()
}
func (e *Engine) recover(ctx context.Context, r *Record, now time.Time, generation uint64) error {
	if !e.targetEnabledLocked(r.AccountID) || generation != e.targetGenerationLocked(r.AccountID) {
		return e.save()
	}
	e.stateMu.Unlock()
	latest, err := e.Admin.Account(ctx, r.AccountID)
	e.stateMu.Lock()
	if !e.targetEnabledLocked(r.AccountID) || generation != e.targetGenerationLocked(r.AccountID) {
		return e.save()
	}
	if err != nil {
		r.Status = "recovery_ready"
		r.Reason = "恢复前校验失败，保留现有暂停并稍后重试：" + err.Error()
		r.NextAt = now.Add(time.Duration(e.Config.RetrySeconds) * time.Second)
		if saved := e.save(); saved != nil {
			return saved
		}
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if latest.Schedulable {
		r.Hold = false
		r.Pending = false
		r.RecoveryPasses = 0
		r.Status = "passed"
		r.Reason = "读取确认账号已可调度，无需重复恢复"
		e.Store.Event(r.AccountID, "resume_reconciled", r.Reason, now)
		return e.save()
	}
	if latest.Status != "active" || (latest.ExpiresAt != nil && !time.Now().Before(*latest.ExpiresAt)) {
		r.Status = "blocked"
		r.RecoveryPasses = 0
		r.Reason = "恢复前账号状态不可用，保留暂停"
		return e.save()
	}
	if !e.Config.AutoRecover {
		return nil
	}
	controlCtx, cancel := context.WithCancel(ctx)
	e.controlCancels[r.AccountID] = cancel
	defer func() { cancel(); delete(e.controlCancels, r.AccountID) }()
	e.Store.Event(r.AccountID, "recovery_intent", "连续两轮有效通过，准备恢复本插件暂停的账号", now)
	if err = e.save(); err != nil {
		return err
	}
	e.publishLocked("")
	e.stateMu.Unlock()
	resumed, err := e.Admin.Resume(controlCtx, r.AccountID)
	e.stateMu.Lock()
	if err == nil && !resumed.Schedulable {
		err = errors.New("恢复结果未确认")
	}
	if err != nil {
		r.Status = "recovery_ready"
		r.Reason = "自动恢复暂未完成，保留暂停记录并稍后核对：" + err.Error()
		r.NextAt = time.Now().UTC().Add(time.Duration(e.Config.RetrySeconds) * time.Second)
		e.Store.Event(r.AccountID, "recovery_unconfirmed", r.Reason, time.Now().UTC())
		if !e.targetEnabledLocked(r.AccountID) {
			r.Status = StatusArchived
		}
		if saved := e.save(); saved != nil {
			return saved
		}
		return err
	}
	r.Hold = false
	r.Status = "passed"
	r.RecoveryPasses = 0
	r.PauseUpdatedAt = ""
	r.Failures = 0
	r.Unavailable = 0
	r.NextAt = now.Add(time.Duration(e.Config.NormalSeconds) * time.Second)
	r.Reason = "连续两轮复测通过，已自动恢复调度"
	e.Store.Event(r.AccountID, "recovered", r.Reason, now)
	if !e.targetEnabledLocked(r.AccountID) {
		r.Status = StatusArchived
		r.Reason = "恢复回执已记录；账号已移出监测范围，不再自动处理"
	}
	return e.save()
}
