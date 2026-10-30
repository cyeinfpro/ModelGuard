package guard

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// Both scheduled and manual detection use bounded account workers. A worker
// finishes all samples for its account before taking another account.
func (e *Engine) runTargets(ctx context.Context, now time.Time, targets []Target, force bool, onStart func(Target), onDone func(Target, error)) error {
	e.batchMu.Lock()
	defer e.batchMu.Unlock()
	e.stateMu.Lock()
	pending := make([]Target, 0, len(targets))
	seen := map[int64]bool{}
	for _, t := range targets {
		r := e.Store.State.Records[t.AccountID]
		if t.Enabled && !seen[t.AccountID] && (force || r == nil || !now.Before(r.NextAt)) {
			pending = append(pending, t)
			seen[t.AccountID] = true
		}
	}
	sort.SliceStable(pending, func(i, j int) bool {
		a, b := e.Store.State.Records[pending[i].AccountID], e.Store.State.Records[pending[j].AccountID]
		if a == nil {
			return b != nil
		}
		if b == nil {
			return false
		}
		return a.NextAt.Before(b.NextAt)
	})
	if len(pending) == 0 {
		e.stateMu.Unlock()
		return ctx.Err()
	}
	if ctx.Err() != nil {
		e.stateMu.Unlock()
		return ctx.Err()
	}
	kind := "scheduled"
	if force {
		kind = "manual"
	}
	batch := &BatchState{Kind: kind, Status: "running", StartedAt: time.Now().UTC(), Total: len(pending), QueuedIDs: []int64{}, ActiveIDs: []int64{}}
	for _, t := range pending {
		batch.QueuedIDs = append(batch.QueuedIDs, t.AccountID)
	}
	e.batch = batch
	e.BatchBusy.Store(true)
	e.publishLocked("")
	e.stateMu.Unlock()
	defer func() {
		e.stateMu.Lock()
		batch.FinishedAt = time.Now().UTC()
		batch.Status = "completed"
		if ctx.Err() != nil {
			batch.Status = "interrupted"
		}
		batch.QueuedIDs = nil
		batch.ActiveIDs = nil
		e.BatchBusy.Store(false)
		e.publishLocked("")
		e.stateMu.Unlock()
	}()
	limit := e.Config.Concurrency
	if limit < 1 {
		limit = 1
	}
	if limit > 8 {
		limit = 8
	}
	if limit > len(pending) {
		limit = len(pending)
	}
	jobs := make(chan Target)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var errs []error
	for i := 0; i < limit; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				if ctx.Err() != nil {
					return
				}
				e.stateMu.Lock()
				batch.QueuedIDs = withoutID(batch.QueuedIDs, t.AccountID)
				batch.ActiveIDs = append(batch.ActiveIDs, t.AccountID)
				e.publishLocked("")
				e.stateMu.Unlock()
				if onStart != nil {
					onStart(t)
				}
				checkAt := now
				if force {
					checkAt = time.Now().UTC()
				}
				err := e.tickTargets(ctx, checkAt, []Target{t}, force)
				if err != nil {
					errMu.Lock()
					errs = append(errs, err)
					errMu.Unlock()
				}
				e.stateMu.Lock()
				batch.ActiveIDs = withoutID(batch.ActiveIDs, t.AccountID)
				batch.Completed++
				e.publishLocked("")
				e.stateMu.Unlock()
				if onDone != nil {
					onDone(t, err)
				} else {
					e.publish("")
				}
			}
		}()
	}
dispatch:
	for _, t := range pending {
		select {
		case <-ctx.Done():
			break dispatch
		case jobs <- t:
		}
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.Join(errs...)
}
