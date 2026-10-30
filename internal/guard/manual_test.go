package guard

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/cyeinfpro/ModelGuard/internal/fingerprint"
	"testing"
	"time"
)

func TestManualRoundIgnoresScheduleButKeepsPolicy(t *testing.T) {
	e, a, verdict, calls := fixture(t)
	e.Config.Enabled = false
	*verdict = "match"
	e.Store.State.Records[1] = &Record{AccountID: 1, Model: e.Config.Targets[0].Model, Expected: e.Config.Targets[0].Expected, NextAt: time.Now().Add(24 * time.Hour)}
	if err := e.RequestManual("one"); err != nil {
		t.Fatal(err)
	}
	if err := e.RequestManual("duplicate-click"); err == nil {
		t.Fatal("overlap accepted")
	}
	e.runManual(context.Background(), <-e.manualQueue)
	if *calls != 1 || a.pauses != 0 || e.Store.State.Manual.Completed != 1 || e.ManualBusy.Load() {
		t.Fatal("manual round did not complete safely")
	}
	if err := e.RequestManual("one"); err == nil {
		t.Fatal("same request replayed")
	}
}
func TestManualBatchContinuesAfterAccountError(t *testing.T) {
	e, a, _, _ := fixture(t)
	e.Config.Targets = append(e.Config.Targets, Target{2, "gpt-6.1-terra", "gpt-6.1-terra", true})
	e.Evaluate = func(_ context.Context, _ Account, target Target) (*fingerprint.Analysis, string, error) {
		if target.AccountID == 1 {
			return nil, "", errors.New("probe failed")
		}
		return &fingerprint.Analysis{Decision: "match"}, "pass", nil
	}
	e.runManual(context.Background(), "batch")
	if a.reads != 2 || e.Store.State.Manual.Completed != 2 || e.Store.State.Manual.Errors != 1 || e.Store.State.Manual.Status != "completed_with_errors" {
		t.Fatal(e.Store.State.Manual)
	}
}
func TestManualWorkerPublishesCompletionAndStops(t *testing.T) {
	e, _, _, _ := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	if err := e.RequestManual("async"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	complete := false
	for time.Now().Before(deadline) {
		var s struct {
			State State `json:"state"`
		}
		json.Unmarshal([]byte(e.Snapshot()), &s)
		if s.State.Manual != nil && s.State.Manual.Status == "completed" {
			complete = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if !complete {
		t.Fatal("manual work waited for scheduled grace period")
	}
}
func TestManualRestartMarksInterruptedWithoutReplay(t *testing.T) {
	e, a, _, _ := fixture(t)
	e.Store.State.Manual = &ManualRun{ID: "old", Status: "running", Total: 1}
	e.Store.Save()
	e.Store.Close()
	next, err := NewEngine(e.Config, e.Detector, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Store.Close()
	if next.Store.State.Manual.Status != "interrupted" {
		t.Fatal("interrupted run appears active")
	}
	if err := next.RequestManual("old"); err == nil {
		t.Fatal("restart replay accepted")
	}
}
