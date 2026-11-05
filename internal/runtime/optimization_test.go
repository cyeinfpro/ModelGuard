package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyeinfpro/ModelGuard/internal/guard"
	"github.com/cyeinfpro/ModelGuard/internal/metadata"
	v1 "github.com/cyeinfpro/ModelGuard/internal/pluginv1"
	"google.golang.org/grpc"
)

func uniqueTopLevelKeys(t *testing.T, raw string) map[string]json.RawMessage {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(raw))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		t.Fatalf("invalid JSON object: %v", err)
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			t.Fatal(err)
		}
		name := key.(string)
		if _, exists := fields[name]; exists {
			t.Fatalf("duplicate top-level key %q", name)
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			t.Fatal(err)
		}
		fields[name] = value
	}
	if _, err := d.Token(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Token(); err != io.EOF {
		t.Fatalf("trailing JSON: %v", err)
	}
	return fields
}

func TestHealthCachedSectionsHaveUniqueKeysAndExactIDs(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	c := guard.DefaultConfig()
	c.DataDir = t.TempDir()
	id := int64(9007199254740993)
	options := []guard.AccountOption{{ID: id, Name: "precise account", Supported: true, Type: "oauth"}}
	p.config = c
	p.cacheAccountsLocked(options)
	revision := p.accountsRevision
	rawCopy := string(p.accountsEncoded)
	for _, withEngine := range []bool{false, true} {
		if withEngine {
			c.Targets = []guard.Target{{AccountID: id, Model: metadata.Defaults.OAuthModel, Expected: metadata.Defaults.OAuthExpected, Enabled: true}}
			raw, _ := json.Marshal(c)
			r, err := p.ApplyConfig(context.Background(), &v1.ApplyConfigRequest{ConfigJson: raw})
			if err != nil || !r.Applied {
				t.Fatalf("apply: %v %v", r, err)
			}
		}
		h, err := p.Health(context.Background(), &v1.HealthRequest{})
		if err != nil {
			t.Fatal(err)
		}
		fields := uniqueTopLevelKeys(t, h.StatusJson)
		var got []guard.AccountOption
		if err := json.Unmarshal(fields["accounts"], &got); err != nil || len(got) != 1 || got[0].ID != id {
			t.Fatalf("integer ID changed: %s %v", fields["accounts"], err)
		}
		if len(fields["status_catalog"]) < 3 || len(fields["models"]) < 3 || len(fields["defaults"]) < 3 {
			t.Fatal("missing shared metadata")
		}
		var defaults struct {
			OAuthModel string `json:"oauth_model"`
			APIModel   string `json:"api_model"`
		}
		if err := json.Unmarshal(fields["defaults"], &defaults); err != nil || defaults.OAuthModel != metadata.Defaults.OAuthModel || defaults.APIModel != metadata.Defaults.APIModel {
			t.Fatal("defaults differ from metadata source")
		}
	}
	if revision != p.accountsRevision || rawCopy != string(p.accountsEncoded) {
		t.Fatal("Health changed discovery cache")
	}
	p.mu.Lock()
	p.cacheAccountsLocked(options)
	same := p.accountsRevision
	options = append([]guard.AccountOption{}, options...)
	options[0].Name = "renamed"
	p.cacheAccountsLocked(options)
	changed := p.accountsRevision
	p.mu.Unlock()
	if revision != same || revision == changed {
		t.Fatal("account revision does not track content")
	}
}

func TestAdaptiveDiscoveryPolicy(t *testing.T) {
	c := guard.DefaultConfig()
	tests := []struct {
		name      string
		groups    []int64
		failures  int
		err       error
		delay     time.Duration
		suspended bool
	}{
		{"manual", nil, 0, nil, 120 * time.Second, false},
		{"groups", []int64{2}, 0, nil, 15 * time.Second, false},
		{"first failure", nil, 1, errors.New("network"), 10 * time.Second, false},
		{"backoff", nil, 4, errors.New("network"), 80 * time.Second, false},
		{"cap", nil, 100, errors.New("network"), 300 * time.Second, false},
		{"retry after", nil, 2, &guard.AdminHTTPError{StatusCode: 429, RetryAfter: 450 * time.Second}, 450 * time.Second, false},
		{"unauthorized", nil, 1, &guard.AdminHTTPError{StatusCode: 401}, 120 * time.Second, true},
		{"forbidden", []int64{2}, 2, &guard.AdminHTTPError{StatusCode: 403}, 15 * time.Second, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.GroupIDs = tt.groups
			delay, suspended := discoveryDelay(c, tt.failures, tt.err)
			if delay != tt.delay || suspended != tt.suspended {
				t.Fatalf("got %v/%v", delay, suspended)
			}
		})
	}
	c.AdminKey = "test"
	now := time.Now()
	for _, interval := range []time.Duration{15 * time.Second, 120 * time.Second, 300 * time.Second} {
		recent := connectionStatusForInterval(c, 1, "", false, now, now.Add(-3*interval), now, interval)
		stale := connectionStatusForInterval(c, 1, "", false, now, now.Add(-3*interval-time.Second), now, interval)
		if recent.State != "ready" || stale.State != "stale" {
			t.Fatalf("stale threshold differs from discovery interval %v", interval)
		}
	}
}

func TestManualRefreshReleasesDiscoveryAuthSuspension(t *testing.T) {
	var authorized atomic.Bool
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !authorized.Load() {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, `{"code":0,"data":{"items":[],"total":0,"page":1}}`)
	}))
	defer server.Close()
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	c := guard.DefaultConfig()
	c.AdminURL = server.URL
	c.AdminKey = "test-only"
	c.DataDir = t.TempDir()
	raw, _ := json.Marshal(c)
	r, err := p.ApplyConfig(context.Background(), &v1.ApplyConfigRequest{ConfigJson: raw})
	if err != nil || !r.Applied {
		t.Fatalf("apply: %v %v", r, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.RLock()
		suspended := p.discoverySuspended
		p.mu.RUnlock()
		if suspended {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("auth error did not suspend discovery")
		}
		time.Sleep(time.Millisecond)
	}
	before := calls.Load()
	p.mu.Lock()
	p.discoveryInterval = time.Millisecond
	p.wakeDiscoveryLocked(false)
	p.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != before {
		t.Fatal("suspended discovery kept polling")
	}
	authorized.Store(true)
	response, err := p.TestConfig(context.Background(), &v1.TestConfigRequest{ConfigJson: raw})
	if err != nil || !response.Success {
		t.Fatalf("refresh: %v %v", response, err)
	}
	p.mu.RLock()
	suspended, interval := p.discoverySuspended, p.discoveryInterval
	p.mu.RUnlock()
	if suspended || interval != manualDiscoveryInterval {
		t.Fatal("manual refresh did not restore discovery")
	}
	if calls.Load() != before+1 {
		t.Fatal("refresh duplicated administrative request")
	}
}

type cachingMetadataHost struct {
	v1.HostServiceClient
	calls    atomic.Int32
	revision atomic.Int32
	entered  chan struct{}
	release  chan struct{}
}

func (h *cachingMetadataHost) ListAccounts(ctx context.Context, r *v1.ListAccountsRequest, _ ...grpc.CallOption) (*v1.ListAccountsResponse, error) {
	n := h.calls.Add(1)
	revision := h.revision.Load()
	if n == 1 && h.entered != nil {
		close(h.entered)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-h.release:
		}
	}
	return &v1.ListAccountsResponse{Accounts: []*v1.AccountInfo{{Id: 1, Platform: r.Platform, AccountType: r.AccountType, Name: fmt.Sprint(revision)}}}, nil
}
func TestMetadataTTLCoalescesGroupAndForcesPostWriteRead(t *testing.T) {
	host := &cachingMetadataHost{entered: make(chan struct{}), release: make(chan struct{})}
	p := &Plugin{host: host}
	var wg sync.WaitGroup
	failures := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := p.accountMetadata(context.Background(), "openai", "oauth", false)
			failures <- err
		}()
	}
	<-host.entered
	close(host.release)
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if host.calls.Load() != 1 {
		t.Fatalf("group read was not coalesced: %d", host.calls.Load())
	}
	host.revision.Store(2)
	cached, hit, err := p.accountMetadata(context.Background(), "openai", "oauth", false)
	if err != nil || !hit || cached[1].Name != "0" {
		t.Fatal("unexpired metadata was not cached")
	}
	fresh, hit, err := p.accountMetadata(context.Background(), "openai", "oauth", true)
	if err != nil || hit || fresh[1].Name != "2" || host.calls.Load() != 2 {
		t.Fatal("post-write read reused stale metadata")
	}
	p.metadataMu.Lock()
	p.metadataCache["openai\x00oauth"].expires = time.Now().Add(-time.Second)
	p.metadataMu.Unlock()
	_, _, err = p.accountMetadata(context.Background(), "openai", "oauth", false)
	if err != nil || host.calls.Load() != 3 {
		t.Fatal("expired metadata was retained")
	}
	_, _, err = p.accountMetadata(context.Background(), "openai", "apikey", false)
	if err != nil || host.calls.Load() != 4 {
		t.Fatal("different account types shared cache")
	}
}
func TestMetadataRejectsInFlightOldHost(t *testing.T) {
	old := &cachingMetadataHost{entered: make(chan struct{}), release: make(chan struct{})}
	p := &Plugin{host: old}
	result := make(chan error, 1)
	go func() { _, _, err := p.accountMetadata(context.Background(), "openai", "oauth", false); result <- err }()
	<-old.entered
	replacement := &cachingMetadataHost{}
	replacement.revision.Store(3)
	p.mu.Lock()
	p.host = replacement
	p.hostEpoch++
	p.invalidateMetadata()
	p.mu.Unlock()
	close(old.release)
	if err := <-result; err == nil {
		t.Fatal("old host metadata accepted after reconnect")
	}
	got, hit, err := p.accountMetadata(context.Background(), "openai", "oauth", false)
	if err != nil || hit || got[1].Name != "3" || replacement.calls.Load() != 1 {
		t.Fatal("new host did not get an independent read")
	}
}

func TestStopCommandRequiresLiveCommitAndIsNotReplayed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":0,"data":{"items":[],"total":0,"page":1}}`)
	}))
	defer server.Close()
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	c := guard.DefaultConfig()
	c.AdminURL = server.URL
	c.AdminKey = "test-only"
	c.DataDir = t.TempDir()
	c.StopRequestID = "persisted-stop"
	c.Targets = []guard.Target{{AccountID: 1, Model: metadata.Defaults.OAuthModel, Expected: metadata.Defaults.OAuthExpected, Enabled: true}}
	apply := func() {
		t.Helper()
		raw, _ := json.Marshal(c)
		r, err := p.ApplyConfig(context.Background(), &v1.ApplyConfigRequest{ConfigJson: raw})
		if err != nil || !r.Applied {
			t.Fatalf("apply: %v %v", r, err)
		}
	}
	diagnostic := func() *v1.TestConfigResponse {
		t.Helper()
		raw, _ := json.Marshal(c)
		r, err := p.TestConfig(context.Background(), &v1.TestConfigRequest{ConfigJson: raw})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	apply()
	e := p.engine
	first := diagnostic()
	if strings.Contains(first.StatusJson, `"checks_stopped"`) {
		t.Fatal("persisted stop replayed at startup")
	}
	c.StopRequestID = "fresh-stop"
	apply()
	if strings.Contains(e.Snapshot(), `"checks_stopped"`) {
		t.Fatal("save executed an uncommitted stop")
	}
	response := diagnostic()
	fields := uniqueTopLevelKeys(t, response.StatusJson)
	var d struct {
		Kind string `json:"kind"`
	}
	json.Unmarshal(fields["diagnostic"], &d)
	var ack struct {
		ID       string `json:"id"`
		Accepted bool   `json:"accepted"`
		State    string `json:"state"`
	}
	json.Unmarshal(fields["stop_request"], &ack)
	if !response.Success || d.Kind != "stop" || ack.ID != "fresh-stop" || !ack.Accepted || ack.State != "stopped" {
		t.Fatalf("incorrect stop acknowledgement: %s", response.StatusJson)
	}
	before := strings.Count(e.Snapshot(), `"checks_stopped"`)
	if before == 0 {
		t.Fatal("stop event missing")
	}
	apply()
	again := diagnostic()
	if !again.Success || strings.Count(e.Snapshot(), `"checks_stopped"`) != before || strings.Contains(again.StatusJson, `"stop_request":`) {
		t.Fatal("completed stop was replayed")
	}
	if p.engine != e {
		t.Fatal("stop command replaced engine")
	}
}

func BenchmarkHealthCachedAccounts(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			p, err := New()
			if err != nil {
				b.Fatal(err)
			}
			p.config.DataDir = b.TempDir()
			accounts := make([]guard.AccountOption, n)
			for i := range accounts {
				accounts[i] = guard.AccountOption{ID: int64(i + 1), Name: fmt.Sprintf("Account %d", i), Type: "oauth", Supported: true}
			}
			p.cacheAccountsLocked(accounts)
			warm, _ := p.Health(context.Background(), &v1.HealthRequest{})
			b.SetBytes(int64(len(warm.StatusJson)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := p.Health(context.Background(), &v1.HealthRequest{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
