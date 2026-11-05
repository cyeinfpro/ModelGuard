package guard

import (
	"container/heap"
	"context"
	"errors"
	"sort"
	"sync/atomic"
	"time"
)

type scheduledTarget struct {
	target   Target
	due      time.Time
	manualID string
}
type dueHeap []scheduledTarget

func (h dueHeap) Len() int { return len(h) }
func (h dueHeap) Less(i, j int) bool {
	if (h[i].manualID != "") != (h[j].manualID != "") {
		return h[i].manualID != ""
	}
	if h[i].due.Equal(h[j].due) {
		return h[i].target.AccountID < h[j].target.AccountID
	}
	return h[i].due.Before(h[j].due)
}
func (h dueHeap) Swap(i, j int)   { h[i], h[j] = h[j], h[i] }
func (h *dueHeap) Push(value any) { *h = append(*h, value.(scheduledTarget)) }
func (h *dueHeap) Pop() any {
	old := *h
	n := len(old)
	value := old[n-1]
	*h = old[:n-1]
	return value
}

type scheduledWork struct {
	done     atomic.Bool
	force    bool
	target   Target
	manualID string
	ctx      context.Context
	cancel   context.CancelFunc
	epoch    uint64
}
type workResult struct {
	work *scheduledWork
	err  error
}
type recentCheck struct {
	at           time.Time
	failed       bool
	inconclusive bool
}

// Run continuously fills available slots from a due-time heap. A slow account
// occupies only its own slot; newly due and manual work can use every other slot.
// All state and persistence belongs to the engine, never to a caller's UI lifetime.
func (e *Engine) Run(ctx context.Context) {
	if !e.running.CompareAndSwap(false, true) {
		return
	}
	defer e.running.Store(false)
	results := make(chan workResult, 8)
	active := map[int64]*scheduledWork{}
	pendingManual := map[int64]Target{}
	manualID := ""
	recent := []recentCheck{}
	var lastStorageRetry time.Time
	ctxDone := ctx.Done()
	shuttingDown := false
	e.stateMu.Lock()
	epoch := e.stopEpoch
	e.stateMu.Unlock()
	defer func() {
		e.stateMu.Lock()
		defer e.stateMu.Unlock()
		e.stopped.Store(true)
		e.Suspended.Store(true)
		if run := e.Store.State.Manual; run != nil && (run.Status == "queued" || run.Status == "running") {
			run.Status = "interrupted"
			run.CurrentID = 0
			run.CurrentIDs = nil
			run.FinishedAt = time.Now().UTC()
			run.Message = "检测被重启或配置变更中断，请重新发起"
		}
		e.ManualQueued.Store(false)
		e.ManualBusy.Store(false)
		e.BatchBusy.Store(false)
		if err := e.save(); err != nil {
			e.publishLocked(err.Error())
		} else {
			e.publishLocked("")
		}
		e.Store.Close()
	}()

	for {
		now := time.Now().UTC()
		delay := 30 * time.Second
		e.stateMu.Lock()
		// Retry durability after a transient disk failure before releasing any
		// new upstream requests. Corrupt stores still fail closed at OpenStore.
		if e.storageError != "" && !shuttingDown {
			if lastStorageRetry.IsZero() || now.Sub(lastStorageRetry) >= 5*time.Second {
				lastStorageRetry = now
				if err := e.save(); err != nil {
					e.publishLocked(err.Error())
				} else {
					e.publishLocked("")
				}
			}
			if e.storageError != "" {
				delay = 5 * time.Second
			}
		}
		if epoch != e.stopEpoch {
			epoch = e.stopEpoch
			pendingManual = map[int64]Target{}
			manualID = ""
			for _, work := range active {
				work.cancel()
			}
		}
		run := e.Store.State.Manual
		if run == nil || run.ID != manualID || run.Status != "running" {
			pendingManual = map[int64]Target{}
			manualID = ""
		}
		// Keep rolling statistics bounded to the last hour.
		first := 0
		for first < len(recent) && now.Sub(recent[first].at) > time.Hour {
			first++
		}
		recent = recent[first:]
		queue := dueHeap{}
		current := map[int64]Target{}
		for _, target := range e.targetsLocked() {
			if target.Enabled && e.targetEnabledLocked(target.AccountID) {
				current[target.AccountID] = target
			}
		}
		manualChanged := false
		for id, target := range pendingManual {
			if latest, ok := current[id]; !ok || latest != target {
				delete(pendingManual, id)
				if run != nil && run.ID == manualID {
					run.Completed++
					run.Skipped++
					manualChanged = true
				}
			}
		}
		if manualChanged {
			e.markStateDirtyLocked()
			if err := e.save(); err != nil {
				e.publishLocked(err.Error())
			}
		}
		if !shuttingDown && !e.Suspended.Load() && e.storageError == "" {
			for id, target := range current {
				if active[id] != nil || e.inFlight[id] {
					continue
				}
				if manualTarget, ok := pendingManual[id]; ok {
					heap.Push(&queue, scheduledTarget{target: manualTarget, due: now, manualID: manualID})
					continue
				}
				if !e.Config.Enabled {
					continue
				}
				due := e.readyAt
				if record := e.Store.State.Records[id]; record != nil && record.NextAt.After(due) {
					due = record.NextAt
				}
				heap.Push(&queue, scheduledTarget{target: target, due: due})
			}
		}
		limit := e.Config.Concurrency
		if limit < 1 {
			limit = 1
		}
		if limit > 8 {
			limit = 8
		}
		for len(active) < limit && queue.Len() > 0 && !queue[0].due.After(now) {
			next := heap.Pop(&queue).(scheduledTarget)
			workCtx, cancel := context.WithCancel(ctx)
			work := &scheduledWork{force: next.manualID != "", target: next.target, manualID: next.manualID, ctx: workCtx, cancel: cancel, epoch: epoch}
			active[next.target.AccountID] = work
			e.activeCancels[next.target.AccountID] = cancel
			if next.manualID != "" {
				delete(pendingManual, next.target.AccountID)
				e.manualStartedLocked(run, next.target.AccountID)
			}
			go func(work *scheduledWork) {
				err := e.tickTargets(work.ctx, time.Now().UTC(), []Target{work.target}, work.force)
				work.done.Store(true)
				results <- workResult{work: work, err: err}
			}(work)
		}
		if queue.Len() > 0 && len(active) < limit {
			delay = time.Until(queue[0].due)
			if delay < time.Millisecond {
				delay = time.Millisecond
			}
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
		if manualID != "" && len(pendingManual) == 0 {
			outstanding := false
			for _, work := range active {
				if work.manualID == manualID {
					outstanding = true
					break
				}
			}
			if !outstanding {
				e.finishManualLocked(run, false)
				manualID = ""
			}
		}
		batch := &BatchState{Kind: "continuous", Status: "completed", WindowSeconds: 3600, StartedAt: now.Add(-time.Hour), QueuedIDs: []int64{}, ActiveIDs: []int64{}}
		for _, check := range recent {
			batch.Completed++
			if check.failed {
				batch.Errors++
			}
			if check.inconclusive {
				batch.Inconclusive++
			}
		}
		for _, target := range queue {
			if !target.due.After(now) {
				batch.QueuedIDs = append(batch.QueuedIDs, target.target.AccountID)
			}
		}
		for id := range active {
			batch.ActiveIDs = append(batch.ActiveIDs, id)
		}
		sort.Slice(batch.ActiveIDs, func(i, j int) bool { return batch.ActiveIDs[i] < batch.ActiveIDs[j] })
		sort.Slice(batch.QueuedIDs, func(i, j int) bool { return batch.QueuedIDs[i] < batch.QueuedIDs[j] })
		batch.Total = batch.Completed + len(batch.ActiveIDs) + len(batch.QueuedIDs)
		if len(active) > 0 {
			batch.Status = "running"
		} else if batch.Errors > 0 {
			batch.Status = "completed_with_errors"
		}
		e.batch = batch
		e.BatchBusy.Store(len(active) > 0)
		e.publishLocked("")
		e.stateMu.Unlock()

		if shuttingDown && len(active) == 0 {
			return
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctxDone:
			shuttingDown = true
			ctxDone = nil
			for _, work := range active {
				work.cancel()
			}
		case id := <-e.manualQueue:
			e.stateMu.Lock()
			run := e.Store.State.Manual
			if !shuttingDown && run != nil && run.ID == id && run.Status == "queued" {
				manualID = id
				pendingManual = map[int64]Target{}
				run.Status = "running"
				run.StartedAt = time.Now().UTC()
				run.CurrentID = 0
				run.CurrentIDs = nil
				run.Message = "正在检测；空闲名额持续处理队列"
				for _, target := range e.manualTargets {
					if work := active[target.AccountID]; work != nil && !work.done.Load() && work.ctx.Err() == nil && work.epoch == e.stopEpoch {
						work.manualID = id
						e.manualStartedLocked(run, target.AccountID)
					} else {
						pendingManual[target.AccountID] = target
					}
				}
				e.ManualQueued.Store(false)
				if err := e.save(); err != nil {
					run.Status = "failed"
					run.Message = err.Error()
					e.ManualBusy.Store(false)
					e.publishLocked(err.Error())
				}
				e.publishLocked("")
			}
			e.stateMu.Unlock()
		case result := <-results:
			work := result.work
			wasCancelled := work.ctx.Err() != nil
			work.cancel()
			delete(active, work.target.AccountID)
			e.stateMu.Lock()
			delete(e.activeCancels, work.target.AccountID)
			record := e.Store.State.Records[work.target.AccountID]
			interrupted := wasCancelled || errors.Is(result.err, context.Canceled) || work.epoch != e.stopEpoch || shuttingDown
			if !interrupted {
				recent = append(recent, recentCheck{at: time.Now().UTC(), failed: result.err != nil || recordNeedsAttention(record), inconclusive: record != nil && record.Status == StatusInconclusive})
				if result.err != nil {
					e.publishLocked(result.err.Error())
				} else {
					e.clearLastErrorLocked()
				}
			}
			if run := e.Store.State.Manual; run != nil && run.Status == "running" && run.ID == work.manualID && work.epoch == e.stopEpoch {
				if interrupted {
					run.Errors++
					run.Unavailable++
					run.Completed++
				} else {
					e.manualResultLocked(run, work.target, result.err)
				}
				run.CurrentIDs = withoutID(run.CurrentIDs, work.target.AccountID)
				run.CurrentID = 0
				if len(run.CurrentIDs) > 0 {
					run.CurrentID = run.CurrentIDs[0]
				}
				if err := e.save(); err != nil {
					run.Status = "failed"
					run.Message = err.Error()
					e.ManualBusy.Store(false)
					e.publishLocked(err.Error())
				}
			}
			e.publishLocked("")
			e.stateMu.Unlock()
		case <-e.wake:
		case <-timer.C:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}
