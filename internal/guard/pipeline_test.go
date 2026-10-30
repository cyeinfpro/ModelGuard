package guard

import (
	"context"
	"encoding/json"
	"github.com/cyeinfpro/ModelGuard/internal/fingerprint"
	"testing"
	"time"
)

func TestBatchReportsActualQueueAndCompletion(t *testing.T) {
	e := parallelFixture(t, 5, 2)
	started := make(chan int64, 5)
	release := make(chan struct{})
	e.Evaluate = func(_ context.Context, a Account, _ Target) (*fingerprint.Analysis, string, error) {
		started <- a.ID
		<-release
		return &fingerprint.Analysis{Decision: "match", Used: 3}, "match", nil
	}
	done := make(chan struct{})
	go func() { e.Tick(context.Background(), time.Now()); close(done) }()
	receiveStarted(t, started, 2)
	var snap struct {
		Batch BatchState `json:"batch"`
	}
	json.Unmarshal([]byte(e.Snapshot()), &snap)
	if !e.BatchBusy.Load() || snap.Batch.Total != 5 || len(snap.Batch.ActiveIDs) != 2 || len(snap.Batch.QueuedIDs) != 3 {
		t.Fatal("queue state inaccurate", snap)
	}
	close(release)
	joinBatch(t, done)
	json.Unmarshal([]byte(e.Snapshot()), &snap)
	if e.BatchBusy.Load() || snap.Batch.Completed != 5 || snap.Batch.Status != "completed" || len(snap.Batch.ActiveIDs) > 0 {
		t.Fatal("batch failed to settle", snap)
	}
}
func TestBudgetExhaustionWaitsForUTCReset(t *testing.T) {
	e, _, _, _ := fixture(t)
	e.Config.DailyRequests = 3
	e.Store.State.Day = time.Now().UTC().Format("2006-01-02")
	e.Store.State.Requests = 3
	_ = e.Tick(context.Background(), time.Now())
	r := e.Store.State.Records[1]
	if !r.NextAt.Equal(nextBudgetReset(time.Now())) {
		t.Fatal("exhausted budget loops", r.NextAt)
	}
}
