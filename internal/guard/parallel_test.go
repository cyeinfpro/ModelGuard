package guard

import (
	"context"
	"encoding/json"
	"github.com/cyeinfpro/ModelGuard/internal/fingerprint"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type parallelAdmin struct {
	mu       sync.Mutex
	accounts map[int64]Account
}

func (a *parallelAdmin) Account(_ context.Context, id int64) (Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accounts[id], nil
}
func (a *parallelAdmin) Pause(_ context.Context, id int64) (Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := a.accounts[id]
	v.Schedulable = false
	v.UpdatedAt = "paused"
	a.accounts[id] = v
	return v, nil
}
func (a *parallelAdmin) Resume(_ context.Context, id int64) (Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := a.accounts[id]
	v.Schedulable = true
	v.UpdatedAt = "resumed"
	a.accounts[id] = v
	return v, nil
}
func (a *parallelAdmin) Proxy(context.Context, int64) (string, error) { return "", nil }
func parallelFixture(t *testing.T, n, limit int) *Engine {
	e, _, _, _ := fixture(t)
	e.Config.Concurrency = limit
	e.Config.Targets = nil
	a := &parallelAdmin{accounts: map[int64]Account{}}
	for i := 1; i <= n; i++ {
		id := int64(i)
		e.Config.Targets = append(e.Config.Targets, Target{id, "gpt-6.1-terra", "gpt-6.1-terra", true})
		a.accounts[id] = Account{ID: id, Platform: "openai", Type: "oauth", Status: "active", Schedulable: true, UpdatedAt: "original"}
	}
	e.Admin = a
	return e
}
func receiveStarted(t *testing.T, c <-chan int64, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-c:
		case <-time.After(3 * time.Second):
			t.Fatal("accounts did not start concurrently")
		}
	}
}
func joinBatch(t *testing.T, c <-chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(5 * time.Second):
		t.Fatal("workers failed to finish")
	}
}
func TestManualAccountsParallelBoundedAndResultsComplete(t *testing.T) {
	e := parallelFixture(t, 6, 3)
	started := make(chan int64, 6)
	release := make(chan struct{})
	var active, peak atomic.Int32
	var mu sync.Mutex
	seen := map[int64]int{}
	e.Evaluate = func(ctx context.Context, a Account, target Target) (*fingerprint.Analysis, string, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		mu.Lock()
		seen[a.ID]++
		mu.Unlock()
		started <- a.ID
		select {
		case <-release:
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
		return &fingerprint.Analysis{Decision: "match", Used: 3}, "pass", nil
	}
	done := make(chan struct{})
	go func() { e.runManual(context.Background(), "parallel"); close(done) }()
	receiveStarted(t, started, 3)
	var snap struct {
		State State `json:"state"`
	}
	if json.Unmarshal([]byte(e.Snapshot()), &snap) != nil || len(snap.State.Manual.CurrentIDs) != 3 {
		t.Fatal("missing multi-account progress")
	}
	close(release)
	joinBatch(t, done)
	if peak.Load() != 3 || e.Store.State.Manual.Completed != 6 || e.Store.State.Manual.Passed != 6 || e.Store.State.Manual.Errors != 0 {
		t.Fatal("parallel batch results incomplete")
	}
	for _, n := range seen {
		if n != 1 {
			t.Fatal("duplicate account test")
		}
	}
}
func TestScheduledAccountsParallelAndOverlappingTicksDoNotDuplicate(t *testing.T) {
	e := parallelFixture(t, 4, 2)
	started := make(chan int64, 4)
	release := make(chan struct{})
	var calls atomic.Int32
	e.Evaluate = func(ctx context.Context, a Account, _ Target) (*fingerprint.Analysis, string, error) {
		calls.Add(1)
		started <- a.ID
		<-release
		return &fingerprint.Analysis{Decision: "match", Used: 3}, "pass", nil
	}
	now := time.Now()
	done := make(chan struct{})
	go func() { e.Tick(context.Background(), now); close(done) }()
	receiveStarted(t, started, 2)
	second := make(chan struct{})
	go func() { e.Tick(context.Background(), now); close(second) }()
	close(release)
	joinBatch(t, done)
	joinBatch(t, second)
	if calls.Load() != 4 {
		t.Fatal("overlapping ticks duplicated account work", calls.Load())
	}
}
func TestParallelCancellationJoinsWorkersAndReturnsUnusedReservations(t *testing.T) {
	e := parallelFixture(t, 6, 3)
	e.Evaluate = nil
	started := make(chan int64, 6)
	e.Probe = probeFunc(func(ctx context.Context, a Account, _ Target, _ string) (string, error) {
		started <- a.ID
		<-ctx.Done()
		return "", ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.runManual(ctx, "cancel"); close(done) }()
	receiveStarted(t, started, 3)
	cancel()
	joinBatch(t, done)
	if e.reservedRequests != 0 || len(e.activeSamples) != 0 || len(e.inFlight) != 0 || e.Store.State.Requests != 3 || e.Store.State.Manual.Status != "interrupted" {
		t.Fatal("cancel leaked work or budget")
	}
	for _, r := range e.Store.State.Records {
		if r.Hold || r.Failures != 0 || r.Unavailable != 0 {
			t.Fatal("cancel counted as account failure")
		}
	}
}
func TestParallelBudgetReservesCompleteRoundsAndCannotOverspend(t *testing.T) {
	e := parallelFixture(t, 5, 3)
	e.Config.DailyRequests = 3
	e.Evaluate = nil
	started := make(chan int64, 3)
	release := make(chan struct{})
	var mu sync.Mutex
	calls := map[int64]int{}
	e.Probe = probeFunc(func(ctx context.Context, a Account, _ Target, _ string) (string, error) {
		mu.Lock()
		calls[a.ID]++
		first := calls[a.ID] == 1
		mu.Unlock()
		if first {
			started <- a.ID
			<-release
		}
		return strings.Repeat("42,", 310), nil
	})
	done := make(chan struct{})
	go func() { e.runManual(context.Background(), "budget"); close(done) }()
	receiveStarted(t, started, 1)
	close(release)
	joinBatch(t, done)
	if e.Store.State.Requests != 3 || e.reservedRequests != 0 || len(calls) != 1 {
		t.Fatal("budget exceeded or split across incomplete rounds", calls)
	}
	for _, n := range calls {
		if n != 3 {
			t.Fatal("partial round budget allocation")
		}
	}
}
func TestParallelFirstMismatchPausesOnlyMatchingAccount(t *testing.T) {
	e := parallelFixture(t, 3, 3)
	e.Evaluate = func(_ context.Context, a Account, _ Target) (*fingerprint.Analysis, string, error) {
		v := "match"
		if a.ID == 2 {
			v = "mismatch"
		}
		return &fingerprint.Analysis{Decision: v, Used: 3}, v, nil
	}
	e.runManual(context.Background(), "isolation")
	for _, id := range []int64{1, 2, 3} {
		a, _ := e.Admin.Account(context.Background(), id)
		if a.Schedulable == (id == 2) {
			t.Fatalf("wrong account schedule %d", id)
		}
	}
	if !e.Store.State.Records[2].Hold || e.Store.State.Manual.Passed != 2 || e.Store.State.Manual.Mismatches != 1 {
		t.Fatal("parallel policy results incorrect")
	}
}
func TestConcurrencyConfigBounds(t *testing.T) {
	d, _ := fingerprint.Load()
	for _, raw := range []string{`{"concurrency":0}`, `{"concurrency":9}`} {
		if _, err := ParseConfig([]byte(raw), d); err == nil {
			t.Fatal("unbounded concurrency accepted")
		}
	}
	c, err := ParseConfig([]byte(`{}`), d)
	if err != nil || c.Concurrency != 3 {
		t.Fatal("legacy config lost safe concurrency default")
	}
}
