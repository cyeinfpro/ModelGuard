package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cyeinfpro/ModelGuard/internal/fingerprint"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type Engine struct {
	stateMu          sync.Mutex // guards records, progress, budget reservations and durable writes
	batchMu          sync.Mutex // manual and scheduled batches share the same worker limit
	activeSamples    map[int64]*ActiveSample
	inFlight         map[int64]bool
	reservedRequests int

	activeSample *ActiveSample
	Config       Config
	Store        *Store
	Admin        Admin
	Probe        Probe
	Detector     *fingerprint.Detector
	mu           sync.RWMutex
	snapshot     []byte
	lastError    string
	Suspended    atomic.Bool
	ManualBusy   atomic.Bool
	ManualQueued atomic.Bool
	BatchBusy    atomic.Bool
	batch        *BatchState
	readyAt      time.Time
	manualQueue  chan string
	lastManual   string
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
	if s.State.Manual != nil && s.State.Manual.Status == "running" {
		s.State.Manual.Status = "interrupted"
		s.State.Manual.Message = "检测被重启或配置变更中断，请重新发起"
		if err := s.Save(); err != nil {
			s.Close()
			return nil, err
		}
	}
	e := &Engine{Config: c, Store: s, Admin: a, Probe: p, Detector: d, manualQueue: make(chan string, 1), activeSamples: map[int64]*ActiveSample{}, inFlight: map[int64]bool{}}
	if s.State.Manual != nil {
		e.lastManual = s.State.Manual.ID
	}
	e.readyAt = time.Now().UTC().Add(60 * time.Second)
	e.publish("")
	return e, nil
}
func (e *Engine) publish(message string) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.publishLocked(message)
}
func (e *Engine) publishLocked(message string) {
	active := make([]*ActiveSample, 0, len(e.activeSamples))
	for _, v := range e.activeSamples {
		active = append(active, v)
	}
	sort.Slice(active, func(i, j int) bool { return active[i].AccountID < active[j].AccountID })
	e.activeSample = nil
	if len(active) > 0 {
		e.activeSample = active[0]
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastError = message
	e.snapshot, _ = json.Marshal(map[string]any{"enabled": e.Config.Enabled, "auto_pause": e.Config.AutoPause, "auto_recover": e.Config.AutoRecover, "last_error": message, "state": e.Store.State, "active_sample": e.activeSample, "active_samples": active, "batch": e.batch, "startup_ready_at": e.readyAt, "budget": e.budgetLocked(), "concurrency": e.Config.Concurrency, "detector_version": e.Detector.Version(), "models": e.Detector.Models()})
}
func (e *Engine) Snapshot() string { e.mu.RLock(); defer e.mu.RUnlock(); return string(e.snapshot) }
func (e *Engine) Run(ctx context.Context) {
	defer e.Store.Close()
	// Host TestConfig may create a short-lived runtime. Do not launch probes at
	// ApplyConfig time: the persistent worker starts after this grace period.
	timer := time.NewTimer(time.Until(e.readyAt))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-e.manualQueue:
			e.runManual(ctx, id)
		case <-timer.C:
			if e.Suspended.Load() {
				timer.Reset(5 * time.Second)
				continue
			}
			err := e.Tick(ctx, time.Now().UTC())
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				e.publish(err.Error())
			} else {
				e.publish("")
			}
			timer.Reset(5 * time.Second)
		}
	}
}
func (e *Engine) save() error {
	if err := e.Store.Save(); err != nil {
		return errors.New("持久化失败，停止账号变更")
	}
	return nil
}
func (e *Engine) Tick(ctx context.Context, now time.Time) error {
	c := e.Config
	if !c.Enabled {
		return nil
	}
	return e.runTargets(ctx, now, c.Targets, false, nil, nil)
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
		if !t.Enabled {
			continue
		}
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
		defer delete(e.inFlight, t.AccountID)
		// Persist retry backoff even for control-plane failures; one broken account
		// must not starve the rest of the allowlist.
		r.NextAt = now.Add(time.Duration(c.RetrySeconds) * time.Second)
		if err := e.save(); err != nil {
			return err
		}
		e.stateMu.Unlock()
		a, err := e.Admin.Account(ctx, t.AccountID)
		e.stateMu.Lock()
		if err != nil {
			r.RecoveryPasses = 0
			r.Status = "blocked"
			r.Reason = err.Error()
			if saved := e.save(); saved != nil {
				return saved
			}
			return err
		}
		if a.Platform != "openai" || (a.Type != "oauth" && a.Type != "apikey") || a.ParentID != nil {
			r.Status = "unsupported"
			r.Reason = "仅支持独立 OpenAI OAuth 或 API Key 账号"
			return e.save()
		}
		if r.Pending {
			r.Pending = false
			if !a.Schedulable {
				r.Hold = true
				r.PauseUpdatedAt = ""
				r.Status = "pause_uncertain"
				r.Reason = "先前暂停回执不确定；禁止自动恢复以免覆盖外部停用"
			} else {
				r.Failures = 0
				r.Unavailable = 0
				r.Status = "unverified"
				r.Reason = "先前暂停未生效，重新采样"
			}
			e.Store.Event(a.ID, "reconcile", r.Reason, now)
			return e.save()
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
			r.Reason = "账号非 active 或已过期"
			return e.save()
		}
		if !a.Schedulable && !r.Hold && !force {
			r.Status = "skipped"
			r.Reason = "账号已被外部暂停，插件不接管"
			return e.save()
		}
		if r.Hold && (!sameRevision(a.UpdatedAt, r.PauseUpdatedAt)) {
			r.Status = "external_change"
			r.Reason = "隔离后账号被外部更新，停止自动恢复"
			r.NextAt = now.Add(time.Duration(c.QuarantineSeconds) * time.Second)
			return e.save()
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
			if err := ctx.Err(); err != nil {
				return err
			}
			if retry {
				used := 0
				if e.Store.State.Day == time.Now().UTC().Format("2006-01-02") {
					used = e.Store.State.Requests
				}
				if used+e.reservedRequests >= c.DailyRequests {
					return errors.New("重试预算不足，保留其他账号完整采样预算")
				}
			}
			if err := e.Store.ReserveRequest(time.Now(), c.DailyRequests); err != nil {
				return err
			}
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
		verdict := "indeterminate"
		if analysis != nil {
			verdict = analysis.Decision
		}
		r.Analysis = analysis
		done := time.Now().UTC()
		// An explicit manual run may observe an externally paused account.
		// It never adopts the hold, advances recovery, or changes scheduling.
		if !a.Schedulable && !r.Hold {
			r.CheckedAt = done
			r.Status = "paused_observation"
			r.Reason = "外部暂停，仅复测：" + reason
			r.NextAt = done.Add(time.Duration(c.NormalSeconds) * time.Second)
			e.Store.Event(a.ID, "observe_paused", fmt.Sprintf("%s: %s", verdict, r.Reason), done)
			return e.save()
		}
		pause := Transition(r, verdict, reason, done, c)
		e.Store.Event(a.ID, "check", fmt.Sprintf("%s: %s", verdict, reason), done)
		if err = e.save(); err != nil {
			return err
		}
		if pause {
			return e.pause(ctx, a, r, done)
		}
		if r.Hold && r.RecoveryPasses >= 2 && c.AutoRecover && c.AutoPause {
			return e.recover(ctx, r, done)
		}
		return nil
	}
	return nil
}
func (e *Engine) pause(ctx context.Context, before Account, r *Record, now time.Time) error {
	latest, err := e.Admin.Account(ctx, r.AccountID)
	if err != nil {
		return err
	}
	if !latest.Schedulable || latest.Status != "active" || !sameRevision(latest.UpdatedAt, before.UpdatedAt) {
		r.Status = "external_change"
		r.Reason = "采样期间账号已变更，本轮不执行暂停"
		return e.save()
	}
	r.Pending = true
	e.Store.Event(r.AccountID, "pause_intent", r.Reason, now)
	if err = e.save(); err != nil {
		return err
	}
	paused, err := e.Admin.Pause(ctx, r.AccountID)
	if err != nil {
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
	return e.save()
}
func (e *Engine) recover(ctx context.Context, r *Record, now time.Time) error {
	latest, err := e.Admin.Account(ctx, r.AccountID)
	if err != nil {
		return err
	}
	if latest.Schedulable || latest.Status != "active" || !sameRevision(latest.UpdatedAt, r.PauseUpdatedAt) || (latest.ExpiresAt != nil && !now.Before(*latest.ExpiresAt)) {
		r.Status = "external_change"
		r.Reason = "恢复前状态校验不通过，不覆盖外部操作"
		return e.save()
	}
	e.Store.Event(r.AccountID, "recovery_intent", "连续两轮有效通过，准备恢复本插件暂停的账号", now)
	if err = e.save(); err != nil {
		return err
	}
	resumed, err := e.Admin.Resume(ctx, r.AccountID)
	if err != nil {
		return err
	}
	if !resumed.Schedulable {
		return errors.New("恢复结果未确认")
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
	return e.save()
}
