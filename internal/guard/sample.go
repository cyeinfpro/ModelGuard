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

// Each account worker records progress under the shared state lock. Persist only timing/outcome,
// never an upstream answer, identity, or request payload.
type sampleRecorder struct {
	engine *Engine
	probe  Probe
	index  int
}

func (p *sampleRecorder) Sample(ctx context.Context, a Account, t Target, prompt string) (string, error) {
	p.index++
	attempt := sampleAttempt{Index: p.index, Attempt: 1}
	if v, ok := ctx.Value(sampleAttemptKey{}).(sampleAttempt); ok {
		attempt = v
	}
	start := time.Now().UTC()
	e := p.engine
	e.stateMu.Lock()
	e.activeSamples[a.ID] = &ActiveSample{AccountID: a.ID, Index: attempt.Index, Attempt: attempt.Attempt, StartedAt: start, TimeoutSeconds: e.Config.TimeoutSeconds, SampleProgress: SampleProgress{Phase: "identity"}}
	e.Store.Event(a.ID, "sample_started", fmt.Sprintf("%s · 第 %d/3 份样本 · 尝试 %d/3", t.Model, attempt.Index, attempt.Attempt), start)
	if err := e.save(); err != nil {
		delete(e.activeSamples, a.ID)
		e.stateMu.Unlock()
		return "", configurationError("采样日志保存失败，停止检测")
	}
	e.publishLocked("")
	e.stateMu.Unlock()
	var answer string
	var err error
	if probe, ok := p.probe.(ProgressProbe); ok {
		answer, err = probe.SampleWithProgress(ctx, a, t, prompt, func(progress SampleProgress) {
			e.stateMu.Lock()
			defer e.stateMu.Unlock()
			e.activeSamples[a.ID].SampleProgress = progress
			e.publishLocked("")
		})
	} else {
		answer, err = p.probe.Sample(ctx, a, t, prompt)
	}
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	outcome := "完成"
	if err != nil {
		outcome = "未完成"
	}
	e.Store.Event(a.ID, "sample_finished", fmt.Sprintf("第 %d/3 份样本 · 尝试 %d/3 请求%s · %.1f 秒", attempt.Index, attempt.Attempt, outcome, time.Since(start).Seconds()), time.Now().UTC())
	delete(e.activeSamples, a.ID)
	if saveErr := e.save(); saveErr != nil {
		e.publishLocked(saveErr.Error())
		return "", configurationError("采样日志保存失败，停止判定")
	}
	e.publishLocked("")
	return answer, err
}
