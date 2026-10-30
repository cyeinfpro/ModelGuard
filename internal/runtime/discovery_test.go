package runtime

import (
	"context"
	"encoding/json"
	v1 "github.com/cyeinfpro/ModelGuard/internal/pluginv1"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDiscoveryWithMonitoringDisabledAndPassiveHealth(t *testing.T) {
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write([]byte(`{"code":0,"data":{"items":[{"id":7,"name":"Real account","platform":"openai","type":"oauth","status":"active","schedulable":true}],"total":1,"page":1,"pages":1}}`))
	}))
	defer s.Close()
	api, _ := grpcFixture(t)
	cfg, _ := json.Marshal(map[string]any{"admin_url": s.URL, "admin_key": "key", "enabled": false})
	applied, err := api.ApplyConfig(context.Background(), &v1.ApplyConfigRequest{ConfigJson: cfg})
	if err != nil || !applied.Applied {
		t.Fatal(err)
	}
	var snapshot map[string]any
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h, _ := api.Health(context.Background(), &v1.HealthRequest{})
		json.Unmarshal([]byte(h.StatusJson), &snapshot)
		if len(snapshot["accounts"].([]any)) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(snapshot["accounts"].([]any)) != 1 || snapshot["target_count"] != float64(0) || snapshot["monitoring_state"] != "disabled" {
		t.Fatal(snapshot)
	}
	before := requests.Load()
	for i := 0; i < 4; i++ {
		api.Health(context.Background(), &v1.HealthRequest{})
	}
	if requests.Load() != before {
		t.Fatal("Health performed network I/O")
	}
	result, err := api.TestConfig(context.Background(), &v1.TestConfigRequest{ConfigJson: cfg})
	if err != nil || !result.Success || requests.Load() <= before {
		t.Fatal("empty targets bypassed real credential check")
	}
}
func TestDiagnosticRejectsInvalidKeyWithoutTargets(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer s.Close()
	api, _ := grpcFixture(t)
	cfg, _ := json.Marshal(map[string]any{"admin_url": s.URL, "admin_key": "invalid"})
	r, err := api.TestConfig(context.Background(), &v1.TestConfigRequest{ConfigJson: cfg})
	if err != nil || r.Success {
		t.Fatal("invalid key falsely passed")
	}
}
