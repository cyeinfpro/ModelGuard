package guard

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyeinfpro/ModelGuard/internal/fingerprint"
)

func waitEngine(t *testing.T, e *Engine, condition func(State, *BatchState) bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var snap struct {
			State State       `json:"state"`
			Batch *BatchState `json:"batch"`
		}
		if json.Unmarshal([]byte(e.Snapshot()), &snap) == nil && condition(snap.State, snap.Batch) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("engine state did not settle")
}
func TestContinuousSchedulerFillsIdleSlotWhileAnotherAccountIsSlow(t *testing.T) {
	e := parallelFixture(t, 3, 2)
	e.readyAt = time.Now().Add(-time.Second)
	targets := e.CurrentConfig().Targets
	e.Store.State.Records[3] = &Record{AccountID: 3, Model: targets[2].Model, Expected: targets[2].Expected, NextAt: time.Now().Add(150 * time.Millisecond)}
	e.markStateDirtyLocked()
	started := make(chan int64, 8)
	release := make(chan struct{})
	var active, peak atomic.Int32
	e.Evaluate = func(ctx context.Context, a Account, _ Target) (*fingerprint.Analysis, string, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		started <- a.ID
		if a.ID == 1 {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, "", ctx.Err()
			}
		}
		return &fingerprint.Analysis{Decision: "match", Used: 3}, "pass", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); joinBatch(t, done) }()
	seen := map[int64]bool{}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for len(seen) < 3 {
		select {
		case id := <-started:
			seen[id] = true
		case <-deadline.C:
			t.Fatal("slow account blocked a newly due account")
		}
	}
	if active.Load() != 1 || peak.Load() > 2 {
		t.Fatal("slot accounting is not bounded")
	}
	close(release)
	waitEngine(t, e, func(s State, b *BatchState) bool { return b != nil && b.Completed == 3 && len(b.ActiveIDs) == 0 })
}
func TestManualUsesAvailableSlotAndJoinsExistingRound(t *testing.T) {
	e := parallelFixture(t, 2, 2)
	e.readyAt = time.Now().Add(-time.Second)
	targets := e.CurrentConfig().Targets
	e.Store.State.Records[2] = &Record{AccountID: 2, Model: targets[1].Model, Expected: targets[1].Expected, NextAt: time.Now().Add(time.Hour)}
	started := make(chan int64, 8)
	release := make(chan struct{})
	var firstCalls atomic.Int32
	e.Evaluate = func(ctx context.Context, a Account, _ Target) (*fingerprint.Analysis, string, error) {
		started <- a.ID
		if a.ID == 1 {
			firstCalls.Add(1)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, "", ctx.Err()
			}
		}
		return &fingerprint.Analysis{Decision: "match", Used: 3}, "pass", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); joinBatch(t, done) }()
	select {
	case id := <-started:
		if id != 1 {
			t.Fatal(id)
		}
	case <-time.After(time.Second):
		t.Fatal("first round did not start")
	}
	if err := e.RequestManual("join-current-round"); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-started:
		if id != 2 {
			t.Fatal("manual duplicated the active account")
		}
	case <-time.After(time.Second):
		t.Fatal("manual waited for the slow account")
	}
	close(release)
	waitEngine(t, e, func(s State, _ *BatchState) bool {
		return s.Manual != nil && s.Manual.Status == "completed" && s.Manual.Completed == 2 && s.Manual.Passed == 2
	})
	if firstCalls.Load() != 1 {
		t.Fatal("manual replayed active account")
	}
}
func TestSoftConfigDisablesPauseBeforeDecisionWithoutInterruptingSample(t *testing.T) {
	e, admin, _, _ := fixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	e.Evaluate = func(ctx context.Context, _ Account, _ Target) (*fingerprint.Analysis, string, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
		return &fingerprint.Analysis{Decision: "mismatch", Used: 3}, "mismatch", nil
	}
	done := make(chan struct{})
	go func() { _ = e.Tick(context.Background(), time.Now()); close(done) }()
	<-started
	ready := e.readyAt
	cfg := e.CurrentConfig()
	cfg.AutoPause = false
	cfg.Concurrency = 1
	if err := e.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if e.readyAt != ready {
		t.Fatal("soft config restarted startup grace")
	}
	close(release)
	joinBatch(t, done)
	if admin.pauses != 0 || e.Store.State.Records[1].Status != StatusPauseRecommended {
		t.Fatal("stale policy paused account")
	}
}
func TestStopCurrentCancelsAndRetainsExistingPause(t *testing.T) {
	e := parallelFixture(t, 2, 2)
	base := e.Admin.(*parallelAdmin)
	a := base.accounts[1]
	a.Schedulable = false
	base.accounts[1] = a
	started := make(chan int64, 2)
	e.Evaluate = func(ctx context.Context, a Account, _ Target) (*fingerprint.Analysis, string, error) {
		started <- a.ID
		<-ctx.Done()
		return nil, "", ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); joinBatch(t, done) }()
	if err := e.RequestManual("stop-test"); err != nil {
		t.Fatal(err)
	}
	receiveStarted(t, started, 2)
	if !e.StopCurrent() {
		t.Fatal("active cancellation not acknowledged")
	}
	waitEngine(t, e, func(s State, b *BatchState) bool {
		return s.Manual != nil && s.Manual.Status == "interrupted" && b != nil && len(b.ActiveIDs) == 0
	})
	after, _ := e.Admin.Account(context.Background(), 1)
	if after.Schedulable {
		t.Fatal("stop released existing pause")
	}
	if e.reservedRequests != 0 {
		t.Fatal("stop leaked reserved rounds")
	}
}
func TestGroupUpdateDoesNotWaitForControlReadAndPreventsStalePause(t *testing.T) {
	e := parallelFixture(t, 1, 1)
	admin := &slowControlAdmin{Admin: e.Admin, operation: "account", entered: make(chan struct{}), release: make(chan struct{})}
	e.Admin = admin
	e.Evaluate = func(context.Context, Account, Target) (*fingerprint.Analysis, string, error) {
		return &fingerprint.Analysis{Decision: "mismatch", Used: 3}, "mismatch", nil
	}
	done := make(chan struct{})
	go func() { _ = e.Tick(context.Background(), time.Now()); close(done) }()
	select {
	case <-admin.entered:
	case <-time.After(time.Second):
		t.Fatal("control read not entered")
	}
	updated := make(chan error, 1)
	go func() { updated <- e.UpdateManagedTargets(nil) }()
	select {
	case err := <-updated:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("membership waited behind network I/O")
	}
	close(admin.release)
	joinBatch(t, done)
	account, _ := e.Admin.Account(context.Background(), 1)
	if !account.Schedulable || e.Store.State.Records[1].Status != StatusArchived {
		t.Fatal("stale control request changed removed account")
	}
}
func TestTransientControlReadDoesNotEraseRecoveryEvidence(t *testing.T) {
	e, admin, verdict, _ := fixture(t)
	tick(t, e)
	*verdict = "match"
	tick(t, e)
	admin.err = &AdminHTTPError{StatusCode: 503}
	if err := e.Tick(context.Background(), time.Now().Add(24*time.Hour)); err == nil {
		t.Fatal("expected read error")
	}
	if e.Store.State.Records[1].RecoveryPasses != 1 || admin.resumes != 0 {
		t.Fatal("transient control error advanced or erased recovery")
	}
	admin.err = &AdminHTTPError{StatusCode: 404}
	_ = e.Tick(context.Background(), time.Now().Add(48*time.Hour))
	if e.Store.State.Records[1].RecoveryPasses != 0 {
		t.Fatal("confirmed missing account retained passes")
	}
}
func TestRecoverySpacingAndInconclusiveCounters(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RecoverySpacingSeconds = 1800
	now := time.Now().UTC()
	r := &Record{Hold: true}
	Transition(r, "match", "pass", now, cfg)
	Transition(r, "match", "pass", now.Add(time.Minute), cfg)
	if r.RecoveryPasses != 1 || r.NextAt.Before(now.Add(1800*time.Second)) {
		t.Fatal("manual checks bypassed recovery spacing")
	}
	Transition(r, "indeterminate", "close candidates", now.Add(2*time.Minute), cfg)
	if r.Status != StatusInconclusive || r.Inconclusive != 1 || r.Unavailable != 0 || r.RecoveryPasses != 0 {
		t.Fatal("inconclusive mixed with transport failure")
	}
	Transition(r, "match", "pass", now.Add(3*time.Minute), cfg)
	Transition(r, "match", "pass", now.Add(33*time.Minute), cfg)
	if r.RecoveryPasses != 2 || r.Status != StatusRecoveryReady {
		t.Fatal("spaced passes did not recover")
	}
}
func TestSoftConfigRejectsHardChanges(t *testing.T) {
	e, _, _, _ := fixture(t)
	cfg := e.CurrentConfig()
	cfg.AdminKey = "different"
	if e.UpdateConfig(cfg) == nil {
		t.Fatal("hard config applied as soft")
	}
	if e.CurrentConfig().AdminKey == cfg.AdminKey {
		t.Fatal("rejected config leaked into engine")
	}
}
func TestConcurrentConfigSnapshotsAndStopAreRaceSafe(t *testing.T) {
	e := parallelFixture(t, 4, 3)
	e.Evaluate = func(ctx context.Context, _ Account, _ Target) (*fingerprint.Analysis, string, error) {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(time.Millisecond):
		}
		return &fingerprint.Analysis{Decision: "match", Used: 3}, "pass", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	if err := e.RequestManual("concurrent"); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 1)
	go func() {
		for i := 0; i < 15; i++ {
			cfg := e.CurrentConfig()
			cfg.Concurrency = 1 + i%3
			cfg.AutoPause = i%2 == 0
			if err := e.UpdateConfig(cfg); err != nil {
				errs <- err
				return
			}
			_ = e.Snapshot()
		}
		errs <- nil
	}()
	for i := 0; i < 25; i++ {
		_ = e.Snapshot()
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	e.StopCurrent()
	cancel()
	joinBatch(t, done)
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("test context did not cancel")
	}
}

func TestPartialSamplesAreUnavailableAndNeverConfirmPauseOrRecovery(t *testing.T) {
	for _, verdict := range []string{"match", "mismatch", "indeterminate"} {
		t.Run(verdict, func(t *testing.T) {
			e, admin, _, _ := fixture(t)
			e.Evaluate = func(context.Context, Account, Target) (*fingerprint.Analysis, string, error) {
				return &fingerprint.Analysis{Decision: verdict, Used: 1}, "only one complete sample", nil
			}
			e.runManual(context.Background(), "partial-"+verdict)
			record := e.Store.State.Records[1]
			if record.Status != StatusUnavailable || record.Unavailable != 1 || record.Inconclusive != 0 || admin.pauses != 0 || admin.resumes != 0 {
				t.Fatal("partial samples became a quality decision", record)
			}
			if run := e.Store.State.Manual; run.Unavailable != 1 || run.Passed != 0 || run.Mismatches != 0 || run.Inconclusive != 0 {
				t.Fatal("manual summary confused incomplete samples with scoring")
			}
		})
	}
}
func TestDisablingFamilyMatchDiscardsInFlightFamilyVerdict(t *testing.T) {
	e, admin, _, _ := fixture(t)
	e.Config.FamilyMatch = true
	admin.account.Schedulable = false
	target := e.Config.Targets[0]
	e.Store.State.Records[1] = &Record{AccountID: 1, Model: target.Model, Expected: target.Expected, Hold: true, Status: StatusRecovering, RecoveryPasses: 1}
	started := make(chan struct{})
	release := make(chan struct{})
	e.Evaluate = func(context.Context, Account, Target) (*fingerprint.Analysis, string, error) {
		close(started)
		<-release
		return &fingerprint.Analysis{Decision: "match", MatchScope: "family", Used: 3}, "family", nil
	}
	done := make(chan struct{})
	go func() { _ = e.Tick(context.Background(), time.Now()); close(done) }()
	<-started
	cfg := e.CurrentConfig()
	cfg.FamilyMatch = false
	if err := e.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	close(release)
	joinBatch(t, done)
	record := e.Store.State.Records[1]
	if admin.resumes != 0 || record.RecoveryPasses != 0 || record.Status != StatusInconclusive {
		t.Fatal("disabled family policy still recovered an account")
	}
}

func TestContinuousSchedulerRetriesDurabilityAfterDiskRecovery(t *testing.T) {
	e, _, _, _ := fixture(t)
	original := e.Store.dir
	e.Store.dir = original + "/missing-parent"
	if err := e.RequestManual("disk-retry"); err == nil {
		t.Fatal("accepted an undurable request")
	}
	e.Store.dir = original
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); joinBatch(t, done) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		e.stateMu.Lock()
		ready := e.storageError == ""
		e.stateMu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("disk recovery required a restart or configuration edit")
		}
		time.Sleep(time.Millisecond)
	}
	if err := e.RequestManual("disk-retry"); err != nil {
		t.Fatal(err)
	}
	waitEngine(t, e, func(s State, _ *BatchState) bool { return s.Manual != nil && s.Manual.Status == "completed" })
}
