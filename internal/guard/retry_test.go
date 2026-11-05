package guard

import (
	"context"
	"errors"
	"github.com/cyeinfpro/ModelGuard/internal/fingerprint"
	"strings"
	"testing"
	"time"
)

// Keep tests deterministic and fast while recording requested retry delays.
type retryTestProbe struct {
	probeFunc
	delays  []time.Duration
	indices []int
}

func (p *retryTestProbe) Retry(_ context.Context, _ Account, index, attempt int, _ string, d time.Duration) error {
	p.delays = append(p.delays, d)
	p.indices = append(p.indices, index)
	return nil
}
func TestRetriesInvalidSampleAndTransportErrorWithoutRepeatingValidSamples(t *testing.T) {
	e, _, _, _ := fixture(t)
	calls := 0
	var prompts []string
	p := &retryTestProbe{probeFunc: func(_ context.Context, _ Account, _ Target, prompt string) (string, error) {
		calls++
		prompts = append(prompts, prompt)
		if calls == 2 {
			return "too short", nil
		}
		if calls == 3 {
			return "", errors.New("upstream timeout")
		}
		return strings.Repeat("42,", 340), nil
	}}
	var reserved []bool
	a, _, err := Evaluate(context.Background(), Account{}, e.Config.Targets[0], e.Config, p, e.Detector, func(retry bool) error { reserved = append(reserved, retry); return nil })
	if err != nil || a == nil || a.Used != 3 || calls != 5 {
		t.Fatal("did not recover complete samples", a, err, calls)
	}
	if prompts[1] != prompts[2] || prompts[2] != prompts[3] || len(p.delays) != 2 || p.indices[0] != 2 || p.indices[1] != 2 || reserved[0] || reserved[1] || !reserved[2] || !reserved[3] || reserved[4] {
		t.Fatal("retried successful sample or wrong budget category")
	}
}
func TestRetryExhaustionNeverTurnsIntoMismatch(t *testing.T) {
	e, _, _, _ := fixture(t)
	calls := 0
	p := &retryTestProbe{probeFunc: func(context.Context, Account, Target, string) (string, error) { calls++; return "42", nil }}
	a, reason, err := Evaluate(context.Background(), Account{}, e.Config.Targets[0], e.Config, p, e.Detector, func(bool) error { return nil })
	if err != nil || calls != 3 || a.Decision != "indeterminate" || !strings.Contains(reason, "重试 2 次") || !strings.Contains(reason, "有效数字") {
		t.Fatal(a, reason, err, calls)
	}
}
func TestExistingIsolationFailureRetriesSoonWithoutLosingOwnership(t *testing.T) {
	e, a, v, _ := fixture(t)
	tick(t, e)
	*v = "match"
	tick(t, e)
	*v = "indeterminate"
	tick(t, e)
	r := e.Store.State.Records[1]
	if !r.Hold || r.Status != "inconclusive" || r.RecoveryPasses != 0 || a.pauses != 1 || a.resumes != 0 || r.NextAt.Sub(r.CheckedAt) != time.Duration(e.Config.RetrySeconds)*time.Second {
		t.Fatal("failure misclassified or delayed six hours", r)
	}
	*v = "match"
	tick(t, e)
	if a.resumes != 0 {
		t.Fatal("failure did not break recovery streak")
	}
	tick(t, e)
	if a.resumes != 1 {
		t.Fatal("valid recovery no longer works")
	}
}
func TestRetryBudgetCannotConsumeReservedFutureSamples(t *testing.T) {
	e, a, _, _ := fixture(t)
	e.Config.DailyRequests = 3
	e.Evaluate = nil
	e.Probe = probeFunc(func(context.Context, Account, Target, string) (string, error) { return "short", nil })
	_ = e.Tick(context.Background(), time.Now())
	r := e.Store.State.Records[1]
	if e.Store.State.Requests != 1 || e.reservedRequests != 0 || a.pauses != 0 || r.Hold || r.Status != "blocked" {
		t.Fatal("retry consumed reserved budget or changed scheduling", r, e.Store.State.Requests, e.reservedRequests)
	}
}
func TestRetryCancellationHasNoLateProbe(t *testing.T) {
	e, _, _, _ := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	p := probeFunc(func(context.Context, Account, Target, string) (string, error) {
		calls++
		cancel()
		return "", errors.New("timeout")
	})
	_, _, err := Evaluate(ctx, Account{}, e.Config.Targets[0], e.Config, p, e.Detector, func(bool) error { return nil })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal("cancellation retried", calls, err)
	}
}
func TestLegacyFailurePauseIsDisabledOnLoad(t *testing.T) {
	d, _ := fingerprint.Load()
	c, err := ParseConfig([]byte(`{"pause_unavailable":true}`), d)
	if err != nil || c.PauseUnavailable {
		t.Fatal("legacy opt-in remained active", err)
	}
}
