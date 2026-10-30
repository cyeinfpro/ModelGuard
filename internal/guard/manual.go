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
	e.mu.Lock()
	defer e.mu.Unlock()
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
	if !e.ManualBusy.CompareAndSwap(false, true) {
		return errors.New("已有检测任务正在执行")
	}
	e.lastManual = id
	e.ManualQueued.Store(true)
	e.manualQueue <- id
	return nil
}

func (e *Engine) runManual(ctx context.Context, id string) {
	e.ManualQueued.Store(false)
	defer e.ManualBusy.Store(false)
	run := &ManualRun{ID: id, Status: "running", StartedAt: time.Now().UTC()}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.stateMu.Lock()
	for _, t := range e.Config.Targets {
		if t.Enabled {
			run.Total++
		}
	}
	e.Store.State.Manual = run
	if err := e.save(); err != nil {
		run.Status = "failed"
		run.Message = err.Error()
		e.publishLocked(err.Error())
		e.stateMu.Unlock()
		return
	}
	e.publishLocked("")
	e.stateMu.Unlock()
	e.runTargets(ctx, time.Now().UTC(), e.Config.Targets, true, func(t Target) {
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
		e.publishLocked(run.Message)
	})
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	run.CurrentID = 0
	run.CurrentIDs = nil
	run.FinishedAt = time.Now().UTC()
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
	e.publishLocked(run.Message)
}
