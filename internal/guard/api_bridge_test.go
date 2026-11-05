package guard

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPIProbeUsesSpecificUpstreamKeyModelAndResponsesPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer private-test-key" || r.Header.Get("Chatgpt-Account-Id") != "" {
			t.Error("wrong endpoint or identity")
		}
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		if b["model"] != "upstream-sol" {
			t.Error("model mapping ignored")
		}
		w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"content\":[{\"type\":\"output_text\",\"text\":\"42,51,62\"}]}]}}\n\n"))
	}))
	defer server.Close()
	p := Prober{}
	a := Account{Type: "apikey", Credentials: map[string]any{"base_url": server.URL, "api_key": "private-test-key", "model_mapping": map[string]any{"sol": "upstream-sol"}}}
	got, err := p.Sample(context.Background(), a, Target{Model: "sol"}, "probe")
	if err != nil || got != "42,51,62" {
		t.Fatal(got, err)
	}
	for _, u := range []string{"http://remote.example", "https://user:pass@example.com", "https://example.com?x=y"} {
		if _, err := apiResponsesURL(u); err == nil {
			t.Fatal("unsafe URL allowed", u)
		}
	}
}
func TestAPIBridgeReadsOnlyMatchingAccountAndNeverExportsSecrets(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ag-bridge-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	os.WriteFile(filepath.Join(dir, "api-bridge.token"), []byte("local-token"), 0600)
	listener, err := net.Listen("unix", filepath.Join(dir, "api-bridge.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-admin-key") != "scope" || r.Header.Get("x-bridge-token") != "local-token" {
			w.WriteHeader(403)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 8, "type": "apikey", "platform": "openai", "status": "active", "schedulable": true, "updated_at": "2026-10-30T10:00:00Z", "credentials": map[string]any{"api_key": "PRIVATE", "base_url": "https://example.com"}})
	})}
	go server.Serve(listener)
	defer server.Close()
	c := DefaultConfig()
	c.DataDir = dir
	c.AdminKey = "scope"
	a, err := ReadAPIAccount(context.Background(), c, 8)
	if err != nil || credential(a, "api_key") != "PRIVATE" {
		t.Fatal(err)
	}
	if _, err := ReadAPIAccount(context.Background(), c, 9); err == nil {
		t.Fatal("wrong account accepted")
	}
	c.AdminKey = "wrong"
	if _, err := ReadAPIAccount(context.Background(), c, 8); err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("scope bypass or secret leak")
	}
}
