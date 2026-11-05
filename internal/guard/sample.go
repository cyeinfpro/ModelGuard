package guard

import (
	"context"
	"fmt"
	"time"
)

type ActiveSample struct {
	SampleProgress
	TimeoutSeconds int       `json:"timeout_seconds"`
	Attempt        int       `json:"attempt"`
	AccountID      int64     `json:"account_id"`
	Index          int       `json:"index"`
	StartedAt      time.Time `json:"started_at"`
}

// Activity is volatile and bounded. Only decisions, control intents/results,
// manual requests and budget leases have synchronous durability requirements.
type sampleRecorder struct {
	engine *Engine
	probe  Probe
	index  int
}
type recordedSample struct {
	recorder *sampleRecorder
	account  Account
	target   Target
	prepared PreparedSample
}
type fallbackSample struct {
	probe   Probe
	account Account
	target  Target
	prompt  string
}

func (p *fallbackSample) Sample(ctx context.Context, progress func(SampleProgress)) (string, error) {
	if probe, ok := p.probe.(ProgressProbe); ok {
		return probe.SampleWithProgress(ctx, p.account, p.target, p.prompt, progress)
	}
	return p.probe.Sample(ctx, p.account, p.target, p.prompt)
}
func (p *sampleRecorder) Prepare(ctx context.Context, a Account, t Target, prompt string) (PreparedSample, error) {
	var prepared PreparedSample
	var err error
	if probe, ok := p.probe.(PreparedProbe); ok {
		prepared, err = probe.Prepare(ctx, a, t, prompt)
		if err != nil {
			return nil, err
		}
	} else {
		prepared = &fallbackSample{probe: p.probe, account: a, target: t, prompt: prompt}
	}
	return &recordedSample{recorder: p, account: a, target: t, prepared: prepared}, nil
}
func (p *sampleRecorder) Sample(ctx context.Context, a Account, t Target, prompt string) (string, error) {
	prepared, err := p.Prepare(ctx, a, t, prompt)
	if err != nil {
		return "", err
	}
	return prepared.Sample(ctx, nil)
}
func (p *recordedSample) Sample(ctx context.Context, externalProgress func(SampleProgress)) (string, error) {
	recorder, a, t := p.recorder, p.account, p.target
	recorder.index++
	attempt := sampleAttempt{Index: recorder.index, Attempt: 1}
	if value, ok := ctx.Value(sampleAttemptKey{}).(sampleAttempt); ok {
		attempt = value
	}
	start := time.Now().UTC()
	e := recorder.engine
	e.stateMu.Lock()
	timeout := e.Config.TimeoutSeconds
	if deadline, ok := ctx.Deadline(); ok {
		timeout = int(time.Until(deadline).Seconds())
	}
	e.activeSamples[a.ID] = &ActiveSample{AccountID: a.ID, Index: attempt.Index, Attempt: attempt.Attempt, StartedAt: start, TimeoutSeconds: timeout, SampleProgress: SampleProgress{Phase: "connecting"}}
	e.Store.Event(a.ID, "sample_started", fmt.Sprintf("%s · 第 %d/3 份样本 · 尝试 %d/3", t.Model, attempt.Index, attempt.Attempt), start)
	e.markStateDirtyLocked()
	e.stateMu.Unlock()
	answer, err := p.prepared.Sample(ctx, func(progress SampleProgress) {
		e.stateMu.Lock()
		if active := e.activeSamples[a.ID]; active != nil {
			active.SampleProgress = progress
		}
		e.publishLocked("")
		e.stateMu.Unlock()
		if externalProgress != nil {
			externalProgress(progress)
		}
	})
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	outcome := "完成"
	if err != nil {
		outcome = "未完成"
	}
	e.Store.Event(a.ID, "sample_finished", fmt.Sprintf("第 %d/3 份样本 · 尝试 %d/3 请求%s · %.1f 秒", attempt.Index, attempt.Attempt, outcome, time.Since(start).Seconds()), time.Now().UTC())
	delete(e.activeSamples, a.ID)
	e.markStateDirtyLocked()
	return answer, err
}
