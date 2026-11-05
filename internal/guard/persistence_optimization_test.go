package guard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cyeinfpro/ModelGuard/internal/fingerprint"
)

func TestAuditSurvivesSampleFloodRotationAndRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := 0; i < auditRotateEvents+10; i++ {
		store.Event(int64(i), "paused", "durable evidence", now.Add(time.Duration(i)*time.Millisecond))
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	store.Event(99, "recovered", "latest recovery", now.Add(time.Minute))
	for i := 0; i < 5000; i++ {
		store.Event(1, "sample_started", "volatile sample timing", now)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if len(store.State.AuditEvents) != auditRetention || len(store.State.ActivityEvents) != activityRetention {
		t.Fatal("event retention is not independent")
	}
	if store.State.AuditEvents[auditRetention-1].Action != "recovered" {
		t.Fatal("sample traffic evicted recovery evidence")
	}
	stateBytes, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateBytes), "sample_started") || strings.Contains(string(stateBytes), "durable evidence") {
		t.Fatal("journals still rewritten with state")
	}
	if _, err := os.Stat(filepath.Join(dir, "audit.jsonl.1")); err != nil {
		t.Fatal("audit rotation missing", err)
	}
	store.Close()
	store, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if len(store.State.AuditEvents) != auditRetention || store.State.AuditEvents[auditRetention-1].Action != "recovered" || len(store.State.ActivityEvents) != 0 {
		t.Fatal("audit recovery lost evidence")
	}
}
func TestLegacyEventsMigrateExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	state := State{Version: 1, Records: map[int64]*Record{1: {AccountID: 1, Status: "suspect"}}, Events: []Event{
		{At: time.Now(), AccountID: 1, Action: "paused", Reason: "legacy"},
		{At: time.Now(), AccountID: 1, Action: "sample_started", Reason: "legacy activity"},
	}}
	raw, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if store.State.Records[1].Status != StatusPauseRecommended || len(store.State.AuditEvents) != 1 || len(store.State.ActivityEvents) != 1 {
		t.Fatal("legacy migration incorrect")
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if len(store.State.AuditEvents) != 1 {
		t.Fatal("legacy audit duplicated on reopen")
	}
}
func TestStateCorruptionOffersExplicitPreviousVersion(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Save(); err != nil {
		t.Fatal(err)
	}
	store.State.Records[1] = &Record{AccountID: 1, Status: StatusQuarantined, Hold: true}
	if err = store.Save(); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if err = os.WriteFile(filepath.Join(dir, "state.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenStore(dir)
	if err == nil {
		opened.Close()
		t.Fatal("corrupt state silently recovered")
	}
	if !strings.Contains(err.Error(), "state.json.prev") {
		t.Fatal("valid previous snapshot is not surfaced")
	}
	current, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if string(current) != "broken" {
		t.Fatal("corrupt evidence overwritten")
	}
}
func TestBudgetLeaseConservativelySurvivesCrashAndCleanClose(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveRequest(time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	if store.State.Requests != 1 || store.metrics.StateWrites != 0 {
		t.Fatal("request rewrote full state or charged lease as actual usage")
	}
	store.unlock() // abrupt process exit: do not narrow the write-ahead lease.
	store, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if store.State.Requests != budgetBlock {
		t.Fatal("crash undercounted pre-reserved request ceiling", store.State.Requests)
	}
	if err := store.ReserveRequest(time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	expected := store.State.Requests
	store.Close()
	store, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.State.Requests != expected {
		t.Fatal("clean shutdown left unused quota charged")
	}
}
func TestBudgetPersistenceFailureDoesNotChargeRequest(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	original := store.dir
	store.dir = filepath.Join(original, "missing")
	if store.ReserveRequest(time.Now(), 100) == nil {
		t.Fatal("undurable quota accepted")
	}
	if store.State.Requests != 0 {
		t.Fatal("failed lease charged budget")
	}
	store.dir = original
}
func TestSnapshotCachesColdStateAndDoesNotAliasRecords(t *testing.T) {
	e, _, _, _ := fixture(t)
	e.Store.State.Records[1] = &Record{AccountID: 1, Status: StatusPending}
	e.markStateDirtyLocked()
	first := e.Snapshot()
	stateRevision := e.stateSnapshotRevision
	e.stateMu.Lock()
	e.activeSamples[1] = &ActiveSample{AccountID: 1, SampleProgress: SampleProgress{Phase: "streaming"}}
	e.publishLocked("")
	e.stateMu.Unlock()
	second := e.Snapshot()
	if first == second || e.stateSnapshotRevision != stateRevision {
		t.Fatal("progress rebuilt cold state or did not update")
	}
	e.stateMu.Lock()
	e.Store.State.Records[1].Status = StatusPassed
	e.markStateDirtyLocked()
	e.stateMu.Unlock()
	third := e.Snapshot()
	if e.stateSnapshotRevision == stateRevision || !strings.Contains(third, "\"status\":\"passed\"") {
		t.Fatal("cold changes remained stale")
	}
	if strings.Contains(first, "\"status\":\"passed\"") {
		t.Fatal("snapshot mutated after publication")
	}
	if e.Snapshot() != third {
		t.Fatal("unchanged snapshot is not cached")
	}
}
func TestHundredTargetsKeepAuditAndBoundWrites(t *testing.T) {
	e := parallelFixture(t, 100, 8)
	e.Config.AutoPause = false
	e.Config.AutoRecover = false
	e.Evaluate = nil
	e.Probe = probeFunc(func(context.Context, Account, Target, string) (string, error) {
		return strings.Repeat("7,13,29,43,61,89,", 60), nil
	})
	e.Store.Event(999, "paused", "must survive one hundred accounts", time.Now().UTC())
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	before := e.Store.metrics
	e.runManual(context.Background(), "scale-100")
	run := e.Store.State.Manual
	if run.Completed != 100 || run.Total != 100 {
		t.Fatal("scale run incomplete", run.Completed)
	}
	if e.Store.State.Requests != 300 || e.reservedRequests != 0 {
		t.Fatal("scale budget incorrect")
	}
	// Two record commits plus one manual-progress commit per account, rather
	// than three full state writes per individual sample.
	if writes := e.Store.metrics.StateWrites - before.StateWrites; writes > 305 {
		t.Fatal("sample logs still amplified full-state writes", writes)
	}
	if writes := e.Store.metrics.BudgetWrites - before.BudgetWrites; writes > 20 {
		t.Fatal("budget was not coalesced", writes)
	}
	if len(e.Store.State.AuditEvents) != 1 || e.Store.State.AuditEvents[0].AccountID != 999 {
		t.Fatal("scale sampling evicted audit")
	}
}

func benchEngine(b *testing.B) *Engine {
	b.Helper()
	d, err := fingerprint.Load()
	if err != nil {
		b.Fatal(err)
	}
	c := DefaultConfig()
	c.DataDir = b.TempDir()
	e, err := NewEngine(c, d, nil, nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(e.Store.Close)
	for i := int64(1); i <= 100; i++ {
		e.Store.State.Records[i] = &Record{AccountID: i, Model: "sample", Expected: "sample", Status: StatusPassed}
	}
	for i := 0; i < 300; i++ {
		e.Store.Event(1, "sample_finished", "sample complete", time.Now())
	}
	e.markStateDirtyLocked()
	return e
}
func BenchmarkPublishProgress(b *testing.B) {
	e := benchEngine(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.stateMu.Lock()
		e.publishLocked("")
		e.stateMu.Unlock()
	}
}
func BenchmarkSnapshotCached(b *testing.B) {
	e := benchEngine(b)
	_ = e.Snapshot()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = e.Snapshot()
	}
}
func BenchmarkSnapshotCold100(b *testing.B) {
	e := benchEngine(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.stateMu.Lock()
		e.markStateDirtyLocked()
		e.stateMu.Unlock()
		_ = e.Snapshot()
	}
}
func BenchmarkStoreSave100(b *testing.B) {
	e := benchEngine(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := e.Store.Save(); err != nil {
			b.Fatal(err)
		}
	}
}
func FuzzParseConfig(f *testing.F) {
	d, err := fingerprint.Load()
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range []string{"{}", "{\"concurrency\":3}", "{\"targets\":[]}", "{\"group_ids\":[1]}"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 1<<20 {
			t.Skip()
		}
		c, err := ParseConfig([]byte(raw), d)
		if err == nil && (c.Concurrency < 1 || c.Concurrency > 8 || c.DailyRequests < 3) {
			t.Fatal("accepted invalid config bounds")
		}
	})
}
func FuzzTargetsForGroups(f *testing.F) {
	f.Add(int64(1), int64(2), true)
	f.Add(int64(-1), int64(0), false)
	f.Fuzz(func(t *testing.T, group, id int64, supported bool) {
		c := DefaultConfig()
		c.GroupIDs = []int64{group}
		c.GroupModel = "model"
		c.GroupExpected = "expected"
		_, _, _ = TargetsForGroups(c, []AccountOption{{ID: id, GroupIDs: []int64{group}, Supported: supported}})
	})
}

func TestNullRecordStateFailsWithoutPanicOrReset(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"version":1,"records":{"1":null},"requests":0}`)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(dir)
	if err == nil {
		store.Close()
		t.Fatal("null record accepted")
	}
	kept, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if string(kept) != string(raw) {
		t.Fatal("malformed evidence overwritten")
	}
}
