package guard

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// RequestManual durably queues one explicitly requested round. Idle slots start
// immediately, even when another scheduled account is still streaming.
func (e *Engine) RequestManual(id string) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if e.stopped.Load() || e.Suspended.Load() {
		return errors.New("监测正在应用配置或已停止，请稍后重试")
	}
	if id == "" || id == e.lastManual {
		return errors.New("此检测请求已处理，请重新点击立即检测")
	}
	targets := []Target{}
	for _, t := range e.targetsLocked() {
		if t.Enabled {
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 {
		return errors.New("请先选择至少一个检测账号并保存")
	}
	if len(e.manualQueue) > 0 || !e.ManualBusy.CompareAndSwap(false, true) {
		return errors.New("已有检测任务正在执行")
	}
	previous := e.Store.State.Manual
	e.Store.State.Manual = &ManualRun{ID: id, Status: "queued", Total: len(targets), RequestedAt: time.Now().UTC(), Message: "已排队；有空闲名额时立即开始"}
	if err := e.save(); err != nil {
		e.Store.State.Manual = previous
		e.ManualBusy.Store(false)
		e.publishLocked(err.Error())
		return err
	}
	e.lastManual = id
	e.manualTargets = targets
	e.ManualQueued.Store(true)
	e.manualQueue <- id
	e.wakeScheduler()
	e.publishLocked("")
	return nil
}

func (e *Engine) manualStartedLocked(run *ManualRun, id int64) {
	for _, current := range run.CurrentIDs {
		if current == id {
			return
		}
	}
	run.CurrentIDs = append(run.CurrentIDs, id)
	sort.Slice(run.CurrentIDs, func(i, j int) bool { return run.CurrentIDs[i] < run.CurrentIDs[j] })
	run.CurrentID = run.CurrentIDs[0]
	e.markStateDirtyLocked()
}
func (e *Engine) manualResultLocked(run *ManualRun, t Target, err error) {
	run.CurrentIDs = withoutID(run.CurrentIDs, t.AccountID)
	run.CurrentID = 0
	if len(run.CurrentIDs) > 0 {
		run.CurrentID = run.CurrentIDs[0]
	}
	run.Completed++
	r := e.Store.State.Records[t.AccountID]
	if err != nil || recordNeedsAttention(r) {
		run.Errors++
	}
	switch {
	case r == nil || r.Status == StatusSkipped || r.Status == StatusUnsupported || r.Status == StatusArchived || r.Status == "external_change" || r.Status == StatusPauseUncertain:
		run.Skipped++
	case r.Status == StatusInconclusive && err == nil:
		run.Inconclusive++
	case err != nil || r.Status == StatusUnavailable || r.Analysis == nil || r.Analysis.Used < 3 || r.CheckedAt.Before(run.RequestedAt):
		run.Unavailable++
	case r.Analysis.Decision == "match" || r.Analysis.Decision == "family_match":
		run.Passed++
	case r.Analysis.Decision == "mismatch":
		run.Mismatches++
	default:
		run.Inconclusive++
	}
	e.markStateDirtyLocked()
}
func (e *Engine) finishManualLocked(run *ManualRun, interrupted bool) {
	run.CurrentID = 0
	run.CurrentIDs = nil
	run.FinishedAt = time.Now().UTC()
	if run.Status != "failed" {
		run.Status = "completed"
		run.Message = fmt.Sprintf("通过 %d · 不符 %d · 不确定 %d · 采样失败 %d · 跳过 %d", run.Passed, run.Mismatches, run.Inconclusive, run.Unavailable, run.Skipped)
		if run.Errors > 0 {
			run.Status = "completed_with_errors"
		}
		if interrupted {
			run.Status = "interrupted"
			run.Message = "检测已中断，可重新发起"
		}
	}
	if err := e.save(); err != nil {
		run.Status = "failed"
		run.Message = err.Error()
	}
	e.ManualBusy.Store(false)
	e.ManualQueued.Store(false)
	if run.Status == "failed" {
		e.publishLocked(run.Message)
	} else {
		e.publishLocked("")
	}
}

// Synchronous entry point retained for command-line/tests. The long-lived Run
// uses the continuous scheduler, so this finite helper is never a batch barrier.
func (e *Engine) runManual(ctx context.Context, id string) {
	e.ManualQueued.Store(false)
	e.ManualBusy.Store(true)
	defer e.ManualBusy.Store(false)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.stateMu.Lock()
	run := &ManualRun{ID: id, Status: "running", StartedAt: time.Now().UTC(), RequestedAt: time.Now().UTC()}
	if queued := e.Store.State.Manual; queued != nil && queued.ID == id && queued.Status == "queued" {
		run.RequestedAt = queued.RequestedAt
	}
	targets := append([]Target(nil), e.targetsLocked()...)
	for _, t := range targets {
		if t.Enabled {
			run.Total++
		}
	}
	e.Store.State.Manual = run
	if err := e.save(); err != nil {
		run.Status = "failed"
		run.FinishedAt = time.Now().UTC()
		run.Message = err.Error()
		e.publishLocked(err.Error())
		e.stateMu.Unlock()
		return
	}
	e.publishLocked("")
	e.stateMu.Unlock()
	batchErr := e.runTargets(ctx, time.Now().UTC(), targets, true, func(t Target) {
		e.stateMu.Lock()
		defer e.stateMu.Unlock()
		e.manualStartedLocked(run, t.AccountID)
	}, func(t Target, err error) {
		e.stateMu.Lock()
		defer e.stateMu.Unlock()
		if ctx.Err() != nil || run.Status == "interrupted" {
			return
		}
		e.manualResultLocked(run, t, err)
		if saveErr := e.save(); saveErr != nil {
			run.Status = "failed"
			run.Message = saveErr.Error()
			cancel()
		}
		if run.Status == "failed" {
			e.publishLocked(run.Message)
		} else {
			e.publishLocked("")
		}
	})
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if batchErr != nil && ctx.Err() == nil && run.Errors == 0 {
		run.Errors++
	}
	e.finishManualLocked(run, ctx.Err() != nil || run.Status == "interrupted")
}
