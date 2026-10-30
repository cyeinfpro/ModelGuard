package runtime

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyeinfpro/ModelGuard/internal/guard"
	v1 "github.com/cyeinfpro/ModelGuard/internal/pluginv1"
)

func decodeFlowStatus(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestConnectionStatusIndependentOfMonitoringAndNeverLeaksKey(t *testing.T) {
	_, p := grpcFixture(t)
	p.config.AdminKey = "private-management-key"
	p.accounts = []guard.AccountOption{{ID: 1}}
	p.accountsUpdated = time.Now().UTC()
	p.accountsChecked = p.accountsUpdated
	h, _ := p.Health(context.Background(), &v1.HealthRequest{})
	s := decodeFlowStatus(t, h.StatusJson)
	if s["connection"].(map[string]any)["state"] != "ready" || s["monitoring_state"] != "disabled" {
		t.Fatal("connection and schedule state were conflated", s)
	}
	if strings.Contains(h.StatusJson, p.config.AdminKey) || strings.Contains(h.StatusJson, "admin_key") {
		t.Fatal("status leaked a credential")
	}
	p.accountsError = "管理员访问密钥缺少该操作或账号范围的权限（HTTP 403）"
	h, _ = p.Health(context.Background(), &v1.HealthRequest{})
	s = decodeFlowStatus(t, h.StatusJson)
	c := s["connection"].(map[string]any)
	if c["state"] != "error" || c["code"] != "permission_denied" || c["next_action"] == "" || len(s["accounts"].([]any)) != 1 {
		t.Fatal("error lacks actionable context or erased cached accounts", s)
	}
	p.runtimeError = "配置失败且旧监测无法重启；请检查状态目录"
	h, _ = p.Health(context.Background(), &v1.HealthRequest{})
	if h.Healthy || h.Message != p.runtimeError {
		t.Fatal("health message falsely claims the failed runtime is ready", h)
	}
}

func TestBridgeReadinessRequiresSocketAndPrivateTokenButDoesNotClaimConnectivity(t *testing.T) {
	// AF_UNIX paths are limited on macOS; the runtime's default temp root may already exceed that limit.
	dir, err := os.MkdirTemp(".", ".mg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if got := inspectBridge(dir); got.State != "missing" {
		t.Fatal(got)
	}
	socket := filepath.Join(dir, "api-bridge.sock")
	if err := os.WriteFile(socket, []byte("not a socket"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := inspectBridge(dir); got.State != "incomplete" {
		t.Fatal(got)
	}
	os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if got := inspectBridge(dir); got.State != "incomplete" {
		t.Fatal("missing token passed", got)
	}
	token := filepath.Join(dir, "api-bridge.token")
	if err := os.WriteFile(token, []byte("test-only-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := inspectBridge(dir); got.State != "installed" || !strings.Contains(got.Message, "将在检测时验证") {
		t.Fatal(got)
	}
	if err := os.Chmod(token, 0644); err != nil {
		t.Fatal(err)
	}
	if got := inspectBridge(dir); got.State != "incomplete" {
		t.Fatal("public token was accepted", got)
	}
}

func TestReadinessNamesMissingRequirementsWithoutTreatingStaleListAsDeletion(t *testing.T) {
	c := guard.DefaultConfig()
	c.AdminKey = "key"
	c.Targets = []guard.Target{{AccountID: 1, Enabled: true}, {AccountID: 2, Enabled: true}}
	a := []guard.AccountOption{{ID: 1, Type: "oauth", Status: "active", Supported: true}}
	ready := ConnectionStatus{State: "ready", Code: "connection_ready"}
	r := readinessStatus(c, a, ready, false, bridgeInstallation{State: "missing"}, "")
	codes := map[string]bool{}
	for _, check := range r.Checks {
		codes[check.Code] = true
	}
	if r.State != "needs_attention" || !codes["targets_not_visible"] || !codes["host_identity_unavailable"] || !codes["status_permission_on_use"] {
		t.Fatal(r)
	}
	r = readinessStatus(c, a, ConnectionStatus{State: "stale", Code: "accounts_stale"}, true, bridgeInstallation{State: "missing"}, "")
	for _, check := range r.Checks {
		if check.Code == "targets_not_visible" {
			t.Fatal("stale data was treated as current authorization", r)
		}
	}
	if r.State != "needs_connection" {
		t.Fatal(r)
	}
}

func TestManualCommandSurvivesMismatchedDiagnosticAndTemporaryQueueRejection(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/admin/accounts/1" {
			w.Write([]byte(`{"code":0,"data":{"id":1,"platform":"openai","type":"oauth","status":"inactive","schedulable":false,"updated_at":"2026-10-01T00:00:00Z"}}`))
			return
		}
		w.Write([]byte(`{"code":0,"data":{"items":[],"total":0,"page":1,"pages":0}}`))
	}))
	defer s.Close()
	_, p := grpcFixture(t)
	c := guard.DefaultConfig()
	c.AdminURL, c.AdminKey, c.DataDir = s.URL, "test-key", t.TempDir()
	c.Targets = []guard.Target{{AccountID: 1, Model: "gpt-6.1-terra", Expected: "gpt-6.1-terra", Enabled: true}}
	apply := func() {
		raw, _ := json.Marshal(c)
		r, _ := p.ApplyConfig(context.Background(), &v1.ApplyConfigRequest{ConfigJson: raw})
		if !r.Applied {
			t.Fatal(r.Message)
		}
	}
	apply()
	c.ManualRequestID = "retained-manual-command"
	apply()
	wrong := c
	wrong.NormalSeconds += 300
	raw, _ := json.Marshal(wrong)
	r, _ := p.TestConfig(context.Background(), &v1.TestConfigRequest{ConfigJson: raw})
	if r.Success || p.pendingManual != c.ManualRequestID {
		t.Fatal("mismatched diagnostic consumed pending command", r)
	}
	r, _ = p.TestConfig(context.Background(), &v1.TestConfigRequest{ConfigJson: []byte("{")})
	if r.Success || r.StatusJson == "" || p.pendingManual != c.ManualRequestID {
		t.Fatal("invalid diagnostic consumed pending command")
	}
	if !p.engine.TrySuspend() {
		t.Fatal("idle engine could not suspend")
	}
	raw, _ = json.Marshal(c)
	r, _ = p.TestConfig(context.Background(), &v1.TestConfigRequest{ConfigJson: raw})
	if r.Success || p.pendingManual != c.ManualRequestID {
		t.Fatal("queue rejection consumed pending command")
	}
	p.engine.Resume()
	r, _ = p.TestConfig(context.Background(), &v1.TestConfigRequest{ConfigJson: raw})
	status := decodeFlowStatus(t, r.StatusJson)
	ack, ok := status["manual_request"].(map[string]any)
	if !r.Success || !ok || ack["id"] != c.ManualRequestID || ack["accepted"] != true || p.pendingManual != "" {
		t.Fatal("manual delivery was not acknowledged truthfully", r, status)
	}
	deadline := time.Now().Add(3 * time.Second)
	for p.engine.ManualBusy.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if p.engine.ManualBusy.Load() {
		t.Fatal("manual queue did not complete")
	}
	r, _ = p.TestConfig(context.Background(), &v1.TestConfigRequest{ConfigJson: raw})
	status = decodeFlowStatus(t, r.StatusJson)
	if !r.Success || status["diagnostic"].(map[string]any)["kind"] != "connection" || status["manual_request"] != nil {
		t.Fatal("ordinary diagnostic falsely acknowledged another run", r)
	}
}

func TestOlderDiscoveryCompletionCannotOverwriteNewerResult(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requests.Add(1)
		if id == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		if id == 1 {
			w.Write([]byte(`{"code":0,"data":{"items":[{"id":1,"platform":"openai","type":"oauth"}],"total":1,"page":1,"pages":1}}`))
		} else {
			w.Write([]byte(`{"code":0,"data":{"items":[{"id":2,"platform":"openai","type":"oauth"}],"total":1,"page":1,"pages":1}}`))
		}
	}))
	defer s.Close()
	_, p := grpcFixture(t)
	c := guard.DefaultConfig()
	c.AdminURL, c.AdminKey = s.URL, "key"
	p.config = c
	done := make(chan error, 1)
	go func() { _, err := p.refreshAccounts(context.Background(), c); done <- err }()
	select {
	case <-firstStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("first discovery did not start")
	}
	if _, err := p.refreshAccounts(context.Background(), c); err != nil {
		close(releaseFirst)
		t.Fatal(err)
	}
	close(releaseFirst)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(p.accounts) != 1 || p.accounts[0].ID != 2 {
		t.Fatal("old completion replaced new list", p.accounts)
	}
}

func TestCancelledDiscoveryDoesNotReplaceLastKnownConnectionWithShutdownError(t *testing.T) {
	started := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
	defer s.Close()
	_, p := grpcFixture(t)
	c := guard.DefaultConfig()
	c.AdminURL, c.AdminKey = s.URL, "key"
	p.config = c
	p.accounts = []guard.AccountOption{{ID: 8}}
	p.accountsUpdated = time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := p.refreshAccounts(ctx, c); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("discovery did not start")
	}
	cancel()
	<-done
	if p.accountsError != "" || p.accountsLoading || len(p.accounts) != 1 || p.accounts[0].ID != 8 {
		t.Fatal("shutdown corrupted connection status")
	}
}

func TestSavedConnectionRejectsStaleDiagnosticConfiguration(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"data":{"items":[],"total":0,"page":1,"pages":0}}`))
	}))
	defer s.Close()
	_, p := grpcFixture(t)
	c := guard.DefaultConfig()
	c.AdminURL, c.AdminKey, c.DataDir = s.URL, "saved-key", t.TempDir()
	raw, _ := json.Marshal(c)
	applied, _ := p.ApplyConfig(context.Background(), &v1.ApplyConfigRequest{ConfigJson: raw})
	if !applied.Applied {
		t.Fatal(applied)
	}
	c.AdminKey = "stale-key"
	raw, _ = json.Marshal(c)
	r, _ := p.TestConfig(context.Background(), &v1.TestConfigRequest{ConfigJson: raw})
	if r.Success || decodeFlowStatus(t, r.StatusJson)["diagnostic"].(map[string]any)["code"] != "config_mismatch" {
		t.Fatal(r)
	}
	if strings.Contains(r.StatusJson, "saved-key") || strings.Contains(r.StatusJson, "stale-key") {
		t.Fatal("diagnostic leaked keys")
	}
}

func TestRemovingAllTargetsKeepsExistingOwnedHoldVisible(t *testing.T) {
	c := guard.DefaultConfig()
	c.DataDir = t.TempDir()
	store, err := guard.OpenStore(c.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	store.State.AdminURL = c.AdminURL
	store.State.Records[19] = &guard.Record{AccountID: 19, Hold: true, Status: "quarantined", PauseUpdatedAt: "2026-10-01T00:00:00Z"}
	if err := store.Save(); err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.Close()
	_, p := grpcFixture(t)
	raw, _ := json.Marshal(c)
	applied, _ := p.ApplyConfig(context.Background(), &v1.ApplyConfigRequest{ConfigJson: raw})
	if !applied.Applied || p.engine == nil {
		t.Fatal("existing ownership disappeared when all targets were removed", applied)
	}
	h, _ := p.Health(context.Background(), &v1.HealthRequest{})
	s := decodeFlowStatus(t, h.StatusJson)
	record := s["state"].(map[string]any)["records"].(map[string]any)["19"].(map[string]any)
	if record["hold"] != true || s["target_count"] != float64(0) || s["monitoring_state"] != "disabled" {
		t.Fatal(s)
	}
}

func TestFailedConnectionDiagnosticIncludesStructuredFailure(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer s.Close()
	_, p := grpcFixture(t)
	c := guard.DefaultConfig()
	c.AdminURL, c.AdminKey = s.URL, "invalid-test-key"
	raw, _ := json.Marshal(c)
	r, _ := p.TestConfig(context.Background(), &v1.TestConfigRequest{ConfigJson: raw})
	if r.Success || r.StatusJson == "" {
		t.Fatal(r)
	}
	d := decodeFlowStatus(t, r.StatusJson)["diagnostic"].(map[string]any)
	if d["code"] != "credentials_invalid" || d["kind"] != "connection" || d["config_applied"] != false {
		t.Fatal(d)
	}
}

func TestFailedConfigRestartRestoresPendingManualCommand(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"data":{"items":[],"total":0,"page":1,"pages":0}}`))
	}))
	defer s.Close()
	_, p := grpcFixture(t)
	c := guard.DefaultConfig()
	c.AdminURL, c.AdminKey, c.DataDir = s.URL, "test-key", t.TempDir()
	c.Targets = []guard.Target{{AccountID: 1, Model: "gpt-6.1-terra", Expected: "gpt-6.1-terra", Enabled: true}}
	apply := func(c guard.Config) *v1.ApplyConfigResponse {
		raw, _ := json.Marshal(c)
		r, _ := p.ApplyConfig(context.Background(), &v1.ApplyConfigRequest{ConfigJson: raw})
		return r
	}
	if r := apply(c); !r.Applied {
		t.Fatal(r)
	}
	c.ManualRequestID = "survive-failed-restart"
	if r := apply(c); !r.Applied {
		t.Fatal(r)
	}
	bad := c
	bad.DataDir = filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(bad.DataDir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	r := apply(bad)
	if r.Applied || p.engine == nil || p.config.DataDir != c.DataDir || p.pendingManual != c.ManualRequestID {
		t.Fatal("failed config restart lost old runtime or pending request", r)
	}
}
