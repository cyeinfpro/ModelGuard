package guard

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// RequestManual queues one bounded parallel round. The caller commits configuration first;
// passive health, startup, and ordinary diagnostics never call this method.
func (e *Engine) RequestManual(id string) error {
	// Keep the global lock order stateMu -> mu (publishLocked). Holding mu
	// while persisting would deadlock a worker publishing its progress.
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if e.stopped.Load() || e.Suspended.Load() {
		return errors.New("监测正在应用配置或已停止，请稍后重试")
	}
	if id == "" || id == e.lastManual {
		return errors.New("此检测请求已处理，请重新点击立即检测")
	}
	count := 0
	for _, t := range e.Config.Targets {
		if t.Enabled {
			count++
		}
	}
	if count == 0 {
		return errors.New("请先选择至少一个检测账号并保存")
	}
	if len(e.manualQueue) > 0 || !e.ManualBusy.CompareAndSwap(false, true) {
		return errors.New("已有检测任务正在执行")
	}
	previous := e.Store.State.Manual
	e.Store.State.Manual = &ManualRun{ID: id, Status: "queued", Total: count, RequestedAt: time.Now().UTC(), Message: "已排队；当前巡检结束后开始"}
	if err := e.save(); err != nil {
		e.Store.State.Manual = previous
		e.ManualBusy.Store(false)
		e.publishLocked(err.Error())
		return err
	}
	e.lastManual = id
	e.ManualQueued.Store(true)
	e.manualQueue <- id
	e.publishLocked("")
	return nil
}

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
	for _, t := range e.Config.Targets {
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
	batchErr := e.runTargets(ctx, time.Now().UTC(), e.Config.Targets, true, func(t Target) {
		e.stateMu.Lock()
		defer e.stateMu.Unlock()
		run.CurrentIDs = append(run.CurrentIDs, t.AccountID)
		sort.Slice(run.CurrentIDs, func(i, j int) bool { return run.CurrentIDs[i] < run.CurrentIDs[j] })
		run.CurrentID = run.CurrentIDs[0]
		e.publishLocked("")
	}, func(t Target, err error) {
		e.stateMu.Lock()
		defer e.stateMu.Unlock()
		for i, id := range run.CurrentIDs {
			if id == t.AccountID {
				run.CurrentIDs = append(run.CurrentIDs[:i], run.CurrentIDs[i+1:]...)
				break
			}
		}
		run.CurrentID = 0
		if len(run.CurrentIDs) > 0 {
			run.CurrentID = run.CurrentIDs[0]
		}
		if ctx.Err() != nil {
			return
		}
		run.Completed++
		r := e.Store.State.Records[t.AccountID]
		if err != nil || r == nil {
			run.Errors++
		} else {
			switch r.Status {
			case "unavailable", "skipped", "unsupported", "blocked", "external_change", "pause_uncertain":
				run.Errors++
			case "paused_observation":
				if r.Analysis == nil || r.Analysis.Decision == "indeterminate" {
					run.Errors++
				}
			}
		}
		if r == nil || r.Status == "skipped" || r.Status == "unsupported" || r.Status == "external_change" || r.Status == "pause_uncertain" {
			run.Skipped++
		} else if err != nil || r.Analysis == nil || r.CheckedAt.Before(run.StartedAt) || r.Analysis.Decision == "indeterminate" {
			run.Unavailable++
		} else if r.Analysis.Decision == "match" {
			run.Passed++
		} else {
			run.Mismatches++
		}
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
	run.CurrentID = 0
	run.CurrentIDs = nil
	run.FinishedAt = time.Now().UTC()
	if batchErr != nil && ctx.Err() == nil && run.Errors == 0 {
		run.Errors++
	}
	if run.Status != "failed" {
		run.Status = "completed"
		run.Message = fmt.Sprintf("通过 %d · 不符 %d · 不可用 %d · 跳过 %d", run.Passed, run.Mismatches, run.Unavailable, run.Skipped)
		if run.Errors > 0 {
			run.Status = "completed_with_errors"
		}
		if ctx.Err() != nil {
			run.Status = "interrupted"
			run.Message = "检测已中断，可重新发起"
		}
	}
	if err := e.save(); err != nil {
		run.Status = "failed"
		run.Message = err.Error()
	}
	if run.Status == "failed" {
		e.publishLocked(run.Message)
	} else {
		// Completion statistics belong to manual.message, never last_error.
		e.publishLocked("")
	}
}
