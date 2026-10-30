package guard

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cyeinfpro/ModelGuard/internal/fingerprint"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeAdmin struct {
	mu                     sync.Mutex
	account                Account
	pauses, resumes, reads int
	err                    error
	beforeResume           func()
}

func (a *fakeAdmin) Account(context.Context, int64) (Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reads++
	return a.account, a.err
}
func (a *fakeAdmin) Pause(context.Context, int64) (Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pauses++
	a.account.Schedulable = false
	a.account.UpdatedAt = fmt.Sprintf("paused-%d", a.pauses)
	return a.account, a.err
}
func (a *fakeAdmin) Resume(context.Context, int64) (Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.beforeResume != nil {
		a.beforeResume()
	}
	a.resumes++
	a.account.Schedulable = true
	a.account.UpdatedAt = "resumed"
	return a.account, a.err
}
func (a *fakeAdmin) Proxy(context.Context, int64) (string, error) { return "", nil }
func fixture(t *testing.T) (*Engine, *fakeAdmin, *string, *int) {
	t.Helper()
	d, err := fingerprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig()
	c.Enabled = true
	c.AutoPause = true
	c.DataDir = t.TempDir()
	c.Targets = []Target{{1, "gpt-6.1-terra", "gpt-6.1-terra", true}}
	a := &fakeAdmin{account: Account{ID: 1, Platform: "openai", Type: "oauth", Status: "active", Schedulable: true, UpdatedAt: "original"}}
	e, err := NewEngine(c, d, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Store.Close)
	verdict := "mismatch"
	calls := 0
	e.Evaluate = func(context.Context, Account, Target) (*fingerprint.Analysis, string, error) {
		calls++
		return &fingerprint.Analysis{Decision: verdict, Used: 3}, verdict, nil
	}
	return e, a, &verdict, &calls
}
func tick(t *testing.T, e *Engine) {
	t.Helper()
	if r := e.Store.State.Records[1]; r != nil {
		r.NextAt = time.Time{}
	}
	if err := e.Tick(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
}
func TestPauseAndAutomaticRecoveryEndToEnd(t *testing.T) {
	e, a, v, calls := fixture(t)
	tick(t, e)
	if a.pauses != 1 || a.account.Schedulable || !e.Store.State.Records[1].Hold {
		t.Fatal("first complete mismatch round did not pause")
	}
	*v = "match"
	tick(t, e)
	if a.resumes != 0 {
		t.Fatal("recovered after only one pass")
	}
	tick(t, e)
	if a.resumes != 1 || !a.account.Schedulable || e.Store.State.Records[1].Hold || *calls != 3 {
		t.Fatal("two-pass automatic recovery failed")
	}
}
func TestExternalPauseIsNeverAdoptedOrRecovered(t *testing.T) {
	e, a, v, calls := fixture(t)
	*v = "match"
	a.account.Schedulable = false
	for i := 0; i < 5; i++ {
		tick(t, e)
	}
	if a.pauses != 0 || a.resumes != 0 || *calls != 0 {
		t.Fatal("external pause touched")
	}
}
func TestExternalEditBlocksAutomaticRecovery(t *testing.T) {
	e, a, v, _ := fixture(t)
	for i := 0; i < 3; i++ {
		tick(t, e)
	}
	*v = "match"
	tick(t, e)
	a.account.UpdatedAt = "human-edit"
	tick(t, e)
	if a.resumes != 0 || e.Store.State.Records[1].Status != "external_change" {
		t.Fatal("external edit overwritten")
	}
}
func TestUnavailableNeverIsolatesEvenWithLegacyOptIn(t *testing.T) {
	e, a, v, _ := fixture(t)
	*v = "indeterminate"
	for i := 0; i < 3; i++ {
		tick(t, e)
	}
	r := e.Store.State.Records[1]
	if a.pauses != 0 || r.Failures != 0 || r.Unavailable != 3 {
		t.Fatal("unavailable treated as mismatch")
	}
	e.Config.PauseUnavailable = true
	tick(t, e)
	if a.pauses != 0 {
		t.Fatal("legacy unavailable policy created an isolation")
	}
}
func TestUnavailableRetainsMismatchEvidenceWithoutActingOnIt(t *testing.T) {
	e, a, v, _ := fixture(t)
	e.Config.AutoPause = false
	tick(t, e)
	*v = "indeterminate"
	e.Config.AutoPause = true
	tick(t, e)
	r := e.Store.State.Records[1]
	if a.pauses != 0 || r.Failures != 1 || r.Unavailable != 1 {
		t.Fatal("unknown outcome erased evidence or triggered a mismatch pause")
	}
	*v = "match"
	tick(t, e)
	if r.Failures != 0 || r.Unavailable != 0 {
		t.Fatal("valid pass did not clear evidence")
	}
	*v = "mismatch"
	tick(t, e)
	if a.pauses != 1 || !r.Hold {
		t.Fatal("new complete mismatch did not pause immediately")
	}
}
func TestUnknownOutcomeInterruptsRecoveryConfirmation(t *testing.T) {
	e, a, v, _ := fixture(t)
	tick(t, e)
	*v = "match"
	tick(t, e)
	*v = "indeterminate"
	tick(t, e)
	*v = "match"
	tick(t, e)
	if a.resumes != 0 || e.Store.State.Records[1].RecoveryPasses != 1 {
		t.Fatal("recovery accepted nonconsecutive passes")
	}
	tick(t, e)
	if a.resumes != 1 {
		t.Fatal("second consecutive valid pass did not recover")
	}
}
func TestObservationModeNeverWrites(t *testing.T) {
	e, a, _, _ := fixture(t)
	e.Config.AutoPause = false
	for i := 0; i < 4; i++ {
		tick(t, e)
	}
	if a.pauses+a.resumes != 0 {
		t.Fatal("observation mode mutated scheduling")
	}
}
func TestInterruptedPauseHasNoRecoveryOwnership(t *testing.T) {
	e, a, v, _ := fixture(t)
	e.Store.State.Records[1] = &Record{AccountID: 1, Model: "gpt-6.1-terra", Expected: "gpt-6.1-terra", Pending: true}
	a.account.Schedulable = false
	*v = "match"
	for i := 0; i < 4; i++ {
		tick(t, e)
	}
	if a.resumes != 0 || !e.Store.State.Records[1].Hold {
		t.Fatal("ambiguous pause gained recovery ownership")
	}
}
func TestControlPlaneErrorDoesNotBecomeQualityFailure(t *testing.T) {
	e, a, _, calls := fixture(t)
	a.err = errors.New("outage")
	if e.Tick(context.Background(), time.Now()) == nil {
		t.Fatal("expected failure")
	}
	if *calls != 0 || e.Store.State.Records[1].Unavailable != 0 || a.pauses != 0 {
		t.Fatal("admin outage counted as model failure")
	}
}
func TestOwnershipSurvivesRestart(t *testing.T) {
	e, a, v, _ := fixture(t)
	for i := 0; i < 3; i++ {
		tick(t, e)
	}
	e.Store.Close()
	s, err := OpenStore(e.Config.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e.Store = s
	*v = "match"
	tick(t, e)
	tick(t, e)
	if a.resumes != 1 {
		t.Fatal("lost persisted ownership")
	}
}
func TestStoreLocksCorruptionAndBudget(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := OpenStore(dir); err == nil {
		other.Close()
		t.Fatal("duplicate writer")
	}
	for i := 0; i < 3; i++ {
		if err := s.ReserveRequest(time.Now(), 3); err != nil {
			t.Fatal(err)
		}
	}
	if s.ReserveRequest(time.Now(), 3) == nil {
		t.Fatal("budget exceeded")
	}
	s.Close()
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.State.Requests != 3 {
		t.Fatal("budget not persisted")
	}
	s.Close()
	os.WriteFile(filepath.Join(dir, "state.json"), []byte("broken"), 0600)
	if s, err := OpenStore(dir); err == nil {
		s.Close()
		t.Fatal("corruption silently reset")
	}
}
func TestStreamRequiresCompletedAndIgnoresReasoning(t *testing.T) {
	for _, s := range []string{"data: {\"type\":\"response.output_text.delta\",\"delta\":\"1 2 3\"}\n\n", "data: {\"type\":\"response.incomplete\"}\n\n", "data: [DONE]\n\n", "data: {\"type\":\"response.completed\",\"response\":{}}\n\n"} {
		if _, err := ReadResponses(strings.NewReader(s)); err == nil {
			t.Fatal("accepted incomplete stream")
		}
	}
	s := "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"SECRET\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"1 2 3\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	out, err := ReadResponses(strings.NewReader(s))
	if err != nil || out != "1 2 3" {
		t.Fatalf("%q %v", out, err)
	}
}
func TestAdminRedirectNeverLeaksKey(t *testing.T) {
	received := false
	to := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received = true }))
	defer to.Close()
	from := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, to.URL, 302) }))
	defer from.Close()
	c := DefaultConfig()
	c.AdminURL = from.URL
	c.AdminKey = "secret"
	_, err := NewAdmin(c).Account(context.Background(), 1)
	if err == nil || received || strings.Contains(err.Error(), "secret") {
		t.Fatal("redirect or credential leak")
	}
}
func TestConfigRejectsUnknownAndUnknownReferences(t *testing.T) {
	d, _ := fingerprint.Load()
	for _, raw := range []string{`{"surprise":true}`, `{"admin_url":"http://public.example"}`, `{"enabled":true}`, `{"data_dir":"/"}`, `{"targets":[{"account_id":1,"model":"x","expected":"imaginary","enabled":true}]}`, `{} {}`} {
		if _, err := ParseConfig([]byte(raw), d); err == nil {
			t.Fatal(raw)
		}
	}
	c, err := ParseConfig([]byte(`{}`), d)
	if err != nil || c.Enabled || c.AutoPause || !c.AutoRecover {
		t.Fatal("unsafe defaults")
	}
}
func TestStateContainsNoCredentialsOrAnswers(t *testing.T) {
	e, a, _, _ := fixture(t)
	a.account.Credentials = map[string]any{"access_token": "DO_NOT_PERSIST"}
	tick(t, e)
	raw, _ := json.Marshal(e.Store.State)
	if strings.Contains(string(raw), "DO_NOT_PERSIST") || strings.Contains(string(raw), "access_token") {
		t.Fatal("credential persisted")
	}
}

func TestOAuthProbePinsIdentityAndCustomChallenge(t *testing.T) {
	const challenge = "独立指纹挑战，不能被替换成 hi"
	var connections []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections = append(connections, r.RemoteAddr)
		if r.Header.Get("Authorization") != "Bearer host-token" || r.Header.Get("Chatgpt-Account-Id") != "upstream-123" {
			t.Error("identity headers lost")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("bad JSON")
		}
		raw, _ := json.Marshal(body)
		if !strings.Contains(string(raw), challenge) || body["model"] != "terra" {
			t.Error("probe model or prompt not preserved")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Sub2API-Guard-Account-ID", "123")
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"3, 8, 42\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer upstream.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Test-only loopback TLS fixture.
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "https://"))
	}
	pool := &ClientPool{entries: map[string]*http.Transport{"": transport}}
	defer pool.Close()
	p := &Prober{Pool: pool, Resolve: func(ctx context.Context, id int64) (Identity, error) {
		if id != 123 {
			t.Fatal("wrong account identity")
		}
		return Identity{Token: "host-token", Headers: http.Header{"Chatgpt-Account-Id": []string{"upstream-123"}}}, nil
	}}
	a := Account{ID: 123, Type: "oauth", Credentials: map[string]any{"model_mapping": map[string]any{"terra": "mapped-terra"}}}
	out, err := p.Sample(context.Background(), a, Target{Model: "terra"}, challenge)
	if err != nil || out != "3, 8, 42" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err = p.Sample(context.Background(), a, Target{Model: "terra"}, challenge); err != nil {
		t.Fatal(err)
	}
	if len(connections) != 2 || connections[0] == connections[1] {
		t.Fatal("fingerprint samples reused upstream connection")
	}
}
