package guard

import (
	"context"
	"github.com/cyeinfpro/ModelGuard/internal/fingerprint"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestGroupTargetsRequireMetadataAndExplicitModel(t *testing.T) {
	c := DefaultConfig()
	c.GroupIDs = []int64{3}
	c.Targets = []Target{{AccountID: 1, Model: "explicit", Expected: "gpt-6.1-terra", Enabled: true}}
	accounts := []AccountOption{{ID: 1, GroupIDs: []int64{3}, Supported: true}, {ID: 2, GroupIDs: []int64{3, 4}, Supported: true}, {ID: 9, GroupIDs: []int64{}, Supported: true}}
	targets, pending, err := TargetsForGroups(c, accounts)
	if err != nil || len(targets) != 1 || targets[0].Model != "explicit" || len(pending) != 1 || pending[0] != 2 {
		t.Fatalf("%v %v %v", targets, pending, err)
	}
	c.GroupModel = "chosen"
	c.GroupExpected = "gpt-6.1-terra"
	targets, pending, err = TargetsForGroups(c, accounts)
	if err != nil || len(targets) != 2 || len(pending) != 0 || targets[1].Model != "chosen" {
		t.Fatal(targets, pending, err)
	}
	accounts[2].GroupIDs = nil
	if _, _, err = TargetsForGroups(c, accounts); err == nil {
		t.Fatal("missing metadata accepted")
	}
	targets, _, err = TargetsForGroups(c, []AccountOption{})
	if err != nil || targets == nil || len(targets) != 0 {
		t.Fatal("successful empty list rejected")
	}
}
func TestManagedTargetsArchiveHistoryAndInvalidateOldWork(t *testing.T) {
	e, _, _, _ := fixture(t)
	original := e.Config.Targets[0]
	e.Store.State.Records[1] = &Record{AccountID: 1, Model: original.Model, Expected: original.Expected, Hold: true, Pending: true, Status: "recovering", RecoveryPasses: 1, PauseUpdatedAt: "evidence", CheckedAt: time.Now()}
	if err := e.UpdateManagedTargets(nil); err != nil {
		t.Fatal(err)
	}
	r := e.Store.State.Records[1]
	if len(e.ManagedTargets()) != 0 || r.Status != "archived" || !r.Hold || !r.Pending || r.PauseUpdatedAt != "evidence" {
		t.Fatal("history or membership lost", r)
	}
	generation := e.targetGenerationLocked(1)
	events := len(e.Store.State.Events)
	if err := e.UpdateManagedTargets(nil); err != nil {
		t.Fatal(err)
	}
	if generation != e.targetGenerationLocked(1) || events != len(e.Store.State.Events) {
		t.Fatal("non-idempotent removal")
	}
	if err := e.UpdateManagedTargets([]Target{original}); err != nil {
		t.Fatal(err)
	}
	if e.targetGenerationLocked(1) <= generation || r.RecoveryPasses != 0 || r.Status != "pending" {
		t.Fatal("old round survived rejoin")
	}
	snapshot := e.ManagedTargets()
	snapshot[0].Enabled = false
	if !e.ManagedTargets()[0].Enabled {
		t.Fatal("snapshot aliases engine")
	}
}
func TestManagedTargetConcurrentSnapshots(t *testing.T) {
	e, _, _, _ := fixture(t)
	targets := e.ManagedTargets()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				if j%2 == 0 {
					_ = e.UpdateManagedTargets(nil)
				} else {
					_ = e.UpdateManagedTargets(targets)
				}
				_ = e.ManagedTargets()
				_ = e.Snapshot()
			}
		}()
	}
	wg.Wait()
}
func TestGroupDiscoveryRejectsChangingOrMissingTotals(t *testing.T) {
	for _, mode := range []string{"changed", "missing", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "missing" {
					w.Write([]byte(`{"code":0,"data":{"items":[],"page":1}}`))
					return
				}
				if mode == "overflow" {
					w.Write([]byte(`{"code":0,"data":{"items":[{"id":1}],"total":0,"page":1}}`))
					return
				}
				if r.URL.Query().Get("page") == "1" {
					w.Write([]byte(`{"code":0,"data":{"items":[{"id":1}],"total":2,"pages":2,"page":1}}`))
				} else {
					w.Write([]byte(`{"code":0,"data":{"items":[{"id":2}],"total":1,"pages":2,"page":2}}`))
				}
			}))
			defer s.Close()
			c := DefaultConfig()
			c.AdminURL = s.URL
			c.AdminKey = "synthetic"
			if a, err := NewAdmin(c).ListAccounts(context.Background()); err == nil || a != nil {
				t.Fatal("unconfirmed list accepted", a, err)
			}
		})
	}
}

func TestGroupRemovalAndRejoinDuringSampleDiscardsOldRound(t *testing.T) {
	e, a, _, _ := fixture(t)
	targets := e.ManagedTargets()
	a.account.Schedulable = false
	e.Store.State.Records[1] = &Record{AccountID: 1, Model: targets[0].Model, Expected: targets[0].Expected, Hold: true, Status: "recovering", RecoveryPasses: 1}
	e.Evaluate = func(context.Context, Account, Target) (*fingerprint.Analysis, string, error) {
		if err := e.UpdateManagedTargets(nil); err != nil {
			t.Fatal(err)
		}
		if err := e.UpdateManagedTargets(targets); err != nil {
			t.Fatal(err)
		}
		return &fingerprint.Analysis{Decision: "match", Used: 3}, "match", nil
	}
	if err := e.Tick(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if a.resumes != 0 || e.Store.State.Records[1].RecoveryPasses != 0 {
		t.Fatal("stale round restored removed/rejoined account")
	}
}
func TestGroupRemovalPersistenceFailureRemainsInactive(t *testing.T) {
	e, _, _, _ := fixture(t)
	e.Store.State.Records[1] = &Record{AccountID: 1, Status: "recovering", Hold: true}
	dir := e.Store.dir
	e.Store.dir = dir + "/missing"
	if err := e.UpdateManagedTargets(nil); err == nil {
		t.Fatal("save failure hidden")
	}
	if len(e.ManagedTargets()) != 0 {
		t.Fatal("save failure reopened removed target")
	}
	if err := e.UpdateManagedTargets(nil); err == nil {
		t.Fatal("same failed snapshot falsely succeeded")
	}
	e.Store.dir = dir
	if err := e.UpdateManagedTargets(nil); err != nil {
		t.Fatal(err)
	}
	if e.storageError != "" {
		t.Fatal("persistence did not recover")
	}
}

func TestGroupSyncFailureFreezesWithoutRemovingAndInvalidatesRound(t *testing.T) {
	e, a, _, calls := fixture(t)
	targets := e.ManagedTargets()
	e.Config.GroupIDs = []int64{5}
	if err := e.UpdateManagedTargets(targets); err != nil {
		t.Fatal(err)
	}
	a.account.Schedulable = false
	r := &Record{AccountID: 1, Model: targets[0].Model, Expected: targets[0].Expected, Hold: true, Status: "recovering", RecoveryPasses: 1}
	e.Store.State.Records[1] = r
	e.SetGroupSyncBlocked(true)
	if len(e.ManagedTargets()) != 1 || !r.Hold || r.RecoveryPasses != 0 || e.targetEnabledLocked(1) {
		t.Fatal("failure removed history or left control open")
	}
	if err := e.Tick(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if *calls != 0 || a.resumes != 0 {
		t.Fatal("blocked group still sampled/restored")
	}
	generation := e.targetGenerationLocked(1)
	e.SetGroupSyncBlocked(true)
	if e.targetGenerationLocked(1) != generation {
		t.Fatal("non-idempotent repeated failure")
	}
	if err := e.UpdateManagedTargets(targets); err != nil {
		t.Fatal(err)
	}
	if !e.targetEnabledLocked(1) {
		t.Fatal("identical complete snapshot did not reopen")
	}
	e.Evaluate = func(context.Context, Account, Target) (*fingerprint.Analysis, string, error) {
		e.SetGroupSyncBlocked(true)
		if err := e.UpdateManagedTargets(targets); err != nil {
			t.Fatal(err)
		}
		return &fingerprint.Analysis{Decision: "match", Used: 3}, "match", nil
	}
	if err := e.Tick(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if a.resumes != 0 || r.RecoveryPasses != 0 {
		t.Fatal("failed then restored sync accepted stale round")
	}
}
