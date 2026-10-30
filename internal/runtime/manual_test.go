package runtime

import (
	"context"
	"encoding/json"
	"github.com/cyeinfpro/ModelGuard/internal/guard"
	v1 "github.com/cyeinfpro/ModelGuard/internal/pluginv1"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestManualCommandNeedsLiveSaveThenCommittedDiagnostic(t *testing.T) {
	var reads atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/admin/accounts/1" {
			reads.Add(1)
			w.Write([]byte(`{"code":0,"data":{"id":1,"platform":"openai","type":"oauth","status":"active","schedulable":false}}`))
			return
		}
		w.Write([]byte(`{"code":0,"data":{"items":[],"total":0,"page":1,"pages":0}}`))
	}))
	defer s.Close()
	_, p := grpcFixture(t)
	// No host identity is provided: this test only verifies command delivery.
	c := guard.DefaultConfig()
	c.AdminURL = s.URL
	c.AdminKey = "test"
	c.DataDir = t.TempDir()
	c.Targets = []guard.Target{{AccountID: 1, Model: "gpt-6.1-terra", Expected: "gpt-6.1-terra", Enabled: true}}
	c.ManualRequestID = "persisted-old"
	apply := func() {
		raw, _ := json.Marshal(c)
		r, err := p.ApplyConfig(context.Background(), &v1.ApplyConfigRequest{ConfigJson: raw})
		if err != nil || !r.Applied {
			t.Fatalf("apply: %v %v", r, err)
		}
	}
	diagnostic := func() *v1.TestConfigResponse {
		raw, _ := json.Marshal(c)
		r, err := p.TestConfig(context.Background(), &v1.TestConfigRequest{ConfigJson: raw})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	apply()
	before := p.engine
	if !diagnostic().Success || reads.Load() != 0 {
		t.Fatal("startup replayed a persisted command")
	}
	apply()
	if before != p.engine {
		t.Fatal("no-op apply restarted worker")
	}
	c.ManualRequestID = "new-click"
	apply()
	if reads.Load() != 0 {
		t.Fatal("save initiated uncommitted probes")
	}
	apply() // host Test always reapplies the committed config
	if !diagnostic().Success {
		t.Fatal("manual command rejected")
	}
	deadline := time.Now().Add(time.Second)
	for p.engine.ManualBusy.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if reads.Load() != 1 {
		t.Fatal("selected account was not checked")
	}
	if !diagnostic().Success || reads.Load() != 1 {
		t.Fatal("diagnostics replayed completed run")
	}

	c.NormalSeconds += 300
	c.ManualRequestID = "save-and-run-with-edits"
	apply()
	if reads.Load() != 1 {
		t.Fatal("config save issued probes before diagnostic commit")
	}
	if !diagnostic().Success {
		t.Fatal("changed-config command failed")
	}
	deadline = time.Now().Add(time.Second)
	for p.engine.ManualBusy.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if reads.Load() != 2 {
		t.Fatal("save-and-run with changed settings silently lost command")
	}
}

func TestDiscoveryFailureRetainsLastKnownList(t *testing.T) {
	var failed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failed.Load() {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(`{"code":0,"data":{"items":[{"id":8,"name":"API","type":"apikey","platform":"openai","status":"active","schedulable":true}],"total":1,"page":1}}`))
	}))
	defer server.Close()
	p, _ := New()
	c := guard.DefaultConfig()
	c.AdminURL = server.URL
	c.AdminKey = "key"
	p.config = c
	if _, err := p.refreshAccounts(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	failed.Store(true)
	if _, err := p.refreshAccounts(context.Background(), c); err == nil {
		t.Fatal("missing failure")
	}
	if len(p.accounts) != 1 || p.accounts[0].ID != 8 || p.accountsError == "" {
		t.Fatal("temporary error erased account list")
	}
}
func TestScheduledBatchPreventsDestructiveConfigRestart(t *testing.T) {
	_, p := grpcFixture(t)
	c := guard.DefaultConfig()
	c.DataDir = t.TempDir()
	c.Targets = []guard.Target{{AccountID: 1, Model: "gpt-6.1-terra", Expected: "gpt-6.1-terra", Enabled: true}}
	raw, _ := json.Marshal(c)
	if r, _ := p.ApplyConfig(context.Background(), &v1.ApplyConfigRequest{ConfigJson: raw}); !r.Applied {
		t.Fatal(r)
	}
	before := p.engine
	p.engine.BatchBusy.Store(true)
	defer p.engine.BatchBusy.Store(false)
	c.NormalSeconds += 300
	raw, _ = json.Marshal(c)
	r, _ := p.ApplyConfig(context.Background(), &v1.ApplyConfigRequest{ConfigJson: raw})
	if r.Applied || p.engine != before {
		t.Fatal("running batch was silently restarted")
	}
}
