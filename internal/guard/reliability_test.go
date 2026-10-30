package guard

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyeinfpro/ModelGuard/internal/fingerprint"
)

type slowControlAdmin struct {
	Admin
	operation string
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
	mu        sync.Mutex
	reads     int
}

func (a *slowControlAdmin) wait(ctx context.Context, operation string, id int64) {
	if id != 1 || operation != a.operation {
		return
	}
	a.once.Do(func() { close(a.entered) })
	select {
	case <-a.release:
	case <-ctx.Done():
	}
}
func (a *slowControlAdmin) Account(ctx context.Context, id int64) (Account, error) {
	if id == 1 {
		a.mu.Lock()
		a.reads++
		recheck := a.reads == 2
		a.mu.Unlock()
		if recheck {
			a.wait(ctx, "account", id)
		}
	}
	return a.Admin.Account(ctx, id)
}
func (a *slowControlAdmin) Pause(ctx context.Context, id int64) (Account, error) {
	a.wait(ctx, "pause", id)
	return a.Admin.Pause(ctx, id)
}
func (a *slowControlAdmin) Resume(ctx context.Context, id int64) (Account, error) {
	a.wait(ctx, "resume", id)
	return a.Admin.Resume(ctx, id)
}

func TestSlowPauseAndRecoveryDoNotBlockOtherAccountProgress(t *testing.T) {
	for _, test := range []struct {
		name, operation string
		recovery        bool
	}{
		{"pause-recheck", "account", false}, {"pause-write", "pause", false},
		{"recovery-recheck", "account", true}, {"recovery-write", "resume", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := parallelFixture(t, 2, 2)
			base := e.Admin.(*parallelAdmin)
			if test.recovery {
				a := base.accounts[1]
				a.Schedulable, a.UpdatedAt = false, "owned"
				base.accounts[1] = a
				e.Store.State.Records[1] = &Record{AccountID: 1, Model: e.Config.Targets[0].Model, Expected: e.Config.Targets[0].Expected, Hold: true, PauseUpdatedAt: "owned", RecoveryPasses: 1}
			}
			a := &slowControlAdmin{Admin: base, operation: test.operation, entered: make(chan struct{}), release: make(chan struct{})}
			e.Admin = a
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.Evaluate = func(ctx context.Context, account Account, _ Target) (*fingerprint.Analysis, string, error) {
				if account.ID == 2 {
					select {
					case <-a.entered:
					case <-ctx.Done():
						return nil, "", ctx.Err()
					}
				}
				verdict := "match"
				if account.ID == 1 && !test.recovery {
					verdict = "mismatch"
				}
				return &fingerprint.Analysis{Decision: verdict, Used: 3}, verdict, nil
			}
			done := make(chan struct{})
			go func() { _ = e.Tick(ctx, time.Now()); close(done) }()
			select {
			case <-a.entered:
			case <-time.After(3 * time.Second):
				cancel()
				joinBatch(t, done)
				t.Fatal("control request did not start")
			}
			completed := false
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				var snapshot struct {
					State State      `json:"state"`
					Batch BatchState `json:"batch"`
				}
				if json.Unmarshal([]byte(e.Snapshot()), &snapshot) == nil {
					if r := snapshot.State.Records[2]; r != nil && r.Status == "passed" && snapshot.Batch.Completed == 1 {
						completed = true
						break
					}
				}
				time.Sleep(time.Millisecond)
			}
			close(a.release)
			joinBatch(t, done)
			if !completed {
				t.Fatal("slow control-plane request blocked unrelated completion/publication")
			}
		})
	}
}

func TestQueuedManualIsDurableAndRestartDoesNotReplay(t *testing.T) {
	e, admin, _, _ := fixture(t)
	if err := e.RequestManual("durable-queued"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(e.Config.DataDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.Manual == nil || state.Manual.Status != "queued" || state.Manual.RequestedAt.IsZero() || !e.ManualQueued.Load() {
		t.Fatal("successful receipt is not durably queued")
	}
	e.Store.Close()
	next, err := NewEngine(e.Config, e.Detector, admin, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Store.Close()
	if next.Store.State.Manual.Status != "interrupted" || next.Store.State.Manual.FinishedAt.IsZero() || next.ManualBusy.Load() {
		t.Fatal("restart left a phantom queued job")
	}
	if err := next.RequestManual("durable-queued"); err == nil {
		t.Fatal("restart replayed old manual command")
	}
}

func TestFailedQueueSaveDoesNotAcceptOrConsumeRequest(t *testing.T) {
	e, _, _, _ := fixture(t)
	dir := e.Store.dir
	e.Store.dir = filepath.Join(t.TempDir(), "missing-parent")
	if err := e.RequestManual("retry-me"); err == nil {
		t.Fatal("accepted a non-durable manual request")
	}
	if e.ManualBusy.Load() || e.ManualQueued.Load() || len(e.manualQueue) != 0 || e.lastManual == "retry-me" {
		t.Fatal("failed persistence consumed the request")
	}
	var snapshot struct {
		StorageError string `json:"storage_error"`
	}
	_ = json.Unmarshal([]byte(e.Snapshot()), &snapshot)
	if snapshot.StorageError == "" {
		t.Fatal("storage failure was not exposed")
	}
	e.Store.dir = dir
	if err := e.RequestManual("retry-me"); err != nil {
		t.Fatal("same request could not retry after storage recovery", err)
	}
	_ = json.Unmarshal([]byte(e.Snapshot()), &snapshot)
	if snapshot.StorageError != "" {
		t.Fatal("storage recovery was not reflected")
	}
}

func TestBatchErrorsSurviveProgressAndIdleUntilSuccessfulRound(t *testing.T) {
	e, admin, verdict, _ := fixture(t)
	admin.err = errors.New("admin unavailable")
	if err := e.Tick(context.Background(), time.Now()); err == nil {
		t.Fatal("expected failed batch")
	}
	var snapshot struct {
		LastError string     `json:"last_error"`
		Batch     BatchState `json:"batch"`
	}
	_ = json.Unmarshal([]byte(e.Snapshot()), &snapshot)
	if snapshot.LastError == "" || snapshot.Batch.Status != "completed_with_errors" || snapshot.Batch.Errors != 1 {
		t.Fatal("failed batch looked successful", snapshot)
	}
	e.publish("")
	_ = e.Tick(context.Background(), time.Now()) // no account due yet
	_ = json.Unmarshal([]byte(e.Snapshot()), &snapshot)
	if snapshot.LastError == "" {
		t.Fatal("idle tick erased the outstanding error")
	}
	admin.err = nil
	*verdict = "match"
	e.Store.State.Records[1].NextAt = time.Time{}
	if err := e.Tick(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(e.Snapshot()), &snapshot)
	if snapshot.LastError != "" || snapshot.Batch.Errors != 0 || snapshot.Batch.Status != "completed" {
		t.Fatal("successful retry did not resolve the error", snapshot)
	}
	e.runManual(context.Background(), "successful-summary")
	_ = json.Unmarshal([]byte(e.Snapshot()), &snapshot)
	if snapshot.LastError != "" || !strings.Contains(e.Store.State.Manual.Message, "通过 1") {
		t.Fatal("successful manual summary leaked into error field")
	}
}

func TestIndependentRecoveryPolicyStillOwnsItsPause(t *testing.T) {
	e, admin, verdict, _ := fixture(t)
	tick(t, e)
	e.Config.AutoPause = false
	*verdict = "match"
	tick(t, e)
	tick(t, e)
	if admin.resumes != 1 || e.Store.State.Records[1].Hold {
		t.Fatal("disabling new pauses disabled separately enabled recovery")
	}
}

func TestSuccessfulOtherAccountDoesNotClearUnresolvedFailure(t *testing.T) {
	e := parallelFixture(t, 2, 2)
	failFirst := true
	e.Evaluate = func(_ context.Context, account Account, _ Target) (*fingerprint.Analysis, string, error) {
		if account.ID == 1 && failFirst {
			return nil, "", errors.New("account one unavailable")
		}
		return &fingerprint.Analysis{Decision: "match", Used: 3}, "match", nil
	}
	if err := e.Tick(context.Background(), time.Now()); err == nil {
		t.Fatal("expected first account failure")
	}
	e.Store.State.Records[2].NextAt = time.Time{}
	if err := e.Tick(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		LastError string `json:"last_error"`
	}
	_ = json.Unmarshal([]byte(e.Snapshot()), &snapshot)
	if snapshot.LastError == "" {
		t.Fatal("unrelated successful account cleared an unresolved error")
	}
	failFirst = false
	e.Store.State.Records[1].NextAt = time.Time{}
	if err := e.Tick(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(e.Snapshot()), &snapshot)
	if snapshot.LastError != "" {
		t.Fatal("recovered account did not resolve the error")
	}
}

func TestSuspendGateExcludesScheduledAndManualWork(t *testing.T) {
	e, _, _, calls := fixture(t)
	if !e.TrySuspend() {
		t.Fatal("idle engine could not suspend")
	}
	if err := e.Tick(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if *calls != 0 || e.BatchBusy.Load() {
		t.Fatal("scheduled batch crossed suspension gate")
	}
	if err := e.RequestManual("suspended"); err == nil {
		t.Fatal("manual request crossed suspension gate")
	}
	e.Resume()
	if err := e.RequestManual("resumed"); err != nil {
		t.Fatal(err)
	}
	if e.TrySuspend() {
		t.Fatal("suspended a queued manual request")
	}
	e.runManual(context.Background(), <-e.manualQueue)
	if *calls != 1 || !e.TrySuspend() {
		t.Fatal("gate did not settle after completion")
	}
}

func TestSuspendAndTickHaveNoCheckThenStartWindow(t *testing.T) {
	for i := 0; i < 20; i++ {
		e := parallelFixture(t, 1, 1)
		var calls atomic.Int32
		started, release := make(chan struct{}), make(chan struct{})
		e.Evaluate = func(ctx context.Context, _ Account, _ Target) (*fingerprint.Analysis, string, error) {
			calls.Add(1)
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, "", ctx.Err()
			}
			return &fingerprint.Analysis{Decision: "match", Used: 3}, "match", nil
		}
		barrier, done := make(chan struct{}), make(chan struct{})
		go func() { <-barrier; _ = e.Tick(context.Background(), time.Now()); close(done) }()
		close(barrier)
		suspended := e.TrySuspend()
		close(release)
		joinBatch(t, done)
		if suspended && calls.Load() != 0 {
			t.Fatal("batch started after successful idle suspension")
		}
	}
}
