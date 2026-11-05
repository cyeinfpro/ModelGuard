package guard

import (
	"context"
	"fmt"
	"time"
)

type sampleAttemptKey struct{}
type sampleAttempt struct{ Index, Attempt int }
type retryObserver interface {
	Retry(context.Context, Account, int, int, string, time.Duration) error
}

func waitRetry(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (p *sampleRecorder) Retry(ctx context.Context, a Account, index, attempt int, reason string, delay time.Duration) error {
	e := p.engine
	e.stateMu.Lock()
	e.activeSamples[a.ID] = &ActiveSample{AccountID: a.ID, Index: index, Attempt: attempt, StartedAt: time.Now().UTC(), TimeoutSeconds: int(delay.Seconds()), SampleProgress: SampleProgress{Phase: "retry_wait"}}
	e.Store.Event(a.ID, "sample_retry", fmt.Sprintf("第 %d/3 份样本：%s；%d 秒后重试（%d/2）", index, reason, int(delay.Seconds()), attempt-1), time.Now().UTC())
	e.markStateDirtyLocked()
	e.stateMu.Unlock()
	err := waitRetry(ctx, delay)
	e.stateMu.Lock()
	delete(e.activeSamples, a.ID)
	e.publishLocked("")
	e.stateMu.Unlock()
	return err
}
