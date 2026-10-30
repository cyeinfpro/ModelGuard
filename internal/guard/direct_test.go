package guard

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type probeFunc func(context.Context, Account, Target, string) (string, error)

func (f probeFunc) Sample(c context.Context, a Account, t Target, s string) (string, error) {
	return f(c, a, t, s)
}
func TestSampleAuditExcludesPayloadAndRecordsProgress(t *testing.T) {
	e, _, _, _ := fixture(t)
	p := &sampleRecorder{engine: e, probe: probeFunc(func(_ context.Context, a Account, _ Target, prompt string) (string, error) {
		var state struct {
			Active *ActiveSample `json:"active_sample"`
		}
		if json.Unmarshal([]byte(e.Snapshot()), &state) != nil || state.Active == nil || state.Active.AccountID != 1 || state.Active.Index != 1 {
			t.Fatal("missing live sample progress")
		}
		return "PRIVATE_ANSWER", nil
	})}
	if _, err := p.Sample(context.Background(), Account{ID: 1}, Target{Model: "terra"}, "PRIVATE_PROMPT"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(e.Store.State)
	if strings.Contains(string(raw), "PRIVATE_") {
		t.Fatal("sample contents leaked")
	}
	if len(e.Store.State.Events) != 2 || e.activeSample != nil {
		t.Fatal("missing sample audit or stale progress")
	}
}
func TestIdentityFailureDoesNotCountTowardPause(t *testing.T) {
	e, a, _, _ := fixture(t)
	e.Evaluate = nil
	e.Config.PauseUnavailable = true
	e.Probe = probeFunc(func(context.Context, Account, Target, string) (string, error) {
		return "", configurationError("identity unavailable")
	})
	for i := 0; i < 4; i++ {
		if r := e.Store.State.Records[1]; r != nil {
			r.NextAt = r.NextAt.AddDate(-1, 0, 0)
		}
		_ = e.Tick(context.Background(), time.Now())
	}
	r := e.Store.State.Records[1]
	if r.Unavailable != 0 || a.pauses != 0 || r.Status != "blocked" {
		t.Fatal("identity failure counted as account failure")
	}
}
func TestUnsupportedAccountNeverProbedOrPaused(t *testing.T) {
	e, a, _, calls := fixture(t)
	a.account.Type = "unsupported"
	for i := 0; i < 4; i++ {
		tick(t, e)
	}
	if *calls != 0 || a.pauses != 0 || e.Store.State.Records[1].Status != "unsupported" {
		t.Fatal("unsupported account was tested")
	}
}

func TestManualObservesExternalPauseWithoutAdopting(t *testing.T) {
	for _, verdict := range []string{"match", "mismatch", "indeterminate"} {
		t.Run(verdict, func(t *testing.T) {
			e, a, v, calls := fixture(t)
			*v = verdict
			a.account.Schedulable = false
			for i := 0; i < 4; i++ {
				if err := e.tickTargets(context.Background(), time.Now(), e.Config.Targets, true); err != nil {
					t.Fatal(err)
				}
			}
			r := e.Store.State.Records[1]
			if *calls != 4 || a.pauses != 0 || a.resumes != 0 || a.account.Schedulable || r.Hold || r.RecoveryPasses != 0 || r.Failures != 0 || r.Unavailable != 0 || r.CheckedAt.IsZero() || r.Status != "paused_observation" {
				t.Fatal("manual observation changed scheduling or failed to sample")
			}
		})
	}
}
func TestManualReportsUnavailableInsteadOfSuccessfulBatch(t *testing.T) {
	e, _, v, _ := fixture(t)
	*v = "indeterminate"
	e.runManual(context.Background(), "audit-unavailable")
	if e.Store.State.Manual.Status != "completed_with_errors" || e.Store.State.Manual.Errors != 1 {
		t.Fatal("unavailable batch reported success")
	}
}

func TestCompletedOutputOverridesPartialDeltaAndRejectsRefusal(t *testing.T) {
	complete := `data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n" + `data: {"type":"response.completed","response":{"status":"completed","output":[{"content":[{"type":"output_text","text":"full sample"}]}]}}` + "\n\n"
	got, err := ReadResponses(strings.NewReader(complete))
	if err != nil || got != "full sample" {
		t.Fatal(got, err)
	}
	for _, s := range []string{
		`data: {"type":"response.refusal.delta","delta":"no"}` + "\n\n" + complete,
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"content":[{"type":"output_text","text":"1,2,3"},{"type":"refusal"}]}]}}` + "\n\n",
	} {
		if _, err := ReadResponses(strings.NewReader(s)); err == nil {
			t.Fatal("refusal scored as valid output")
		}
	}
}
func TestProgressContainsNoRawResponse(t *testing.T) {
	data := `data: {"type":"response.output_text.delta","delta":"SECRET_OUTPUT"}` + "\n\n" + `data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n"
	var progress []SampleProgress
	_, err := readResponsesProgress(strings.NewReader(data), func(p SampleProgress) { progress = append(progress, p) })
	raw, _ := json.Marshal(progress)
	if err != nil || len(progress) == 0 || strings.Contains(string(raw), "SECRET_OUTPUT") {
		t.Fatal("invalid or unsafe progress", err)
	}
}

func TestInsufficientRoundBudgetDoesNotSpendPartialSamples(t *testing.T) {
	e, _, _, calls := fixture(t)
	e.Store.State.Day = time.Now().UTC().Format("2006-01-02")
	e.Store.State.Requests = e.Config.DailyRequests - 2
	if err := e.Tick(context.Background(), time.Now()); err == nil {
		t.Fatal("partial round allowed")
	}
	if *calls != 0 || e.Store.State.Requests != e.Config.DailyRequests-2 {
		t.Fatal("remaining budget wasted on incomplete round")
	}
}
