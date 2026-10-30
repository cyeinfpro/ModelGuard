package guard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiscoveryPaginatesAndNeverExportsSecrets(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("x-api-key") != "scoped-key" || r.URL.Query().Get("platform") != "openai" {
			t.Error("wrong scope or auth")
		}
		page := r.URL.Query().Get("page")
		if page == "1" {
			w.Write([]byte(`{"code":0,"data":{"items":[{"id":1,"name":"First","platform":"openai","type":"oauth","status":"active","schedulable":true,"credentials":{"access_token":"NEVER_EXPORT"}}],"total":2,"pages":2,"page":1}}`))
		} else {
			w.Write([]byte(`{"code":0,"data":{"items":[{"id":2,"name":"Second","platform":"openai","type":"apikey","status":"active","schedulable":false}],"total":2,"pages":2,"page":2}}`))
		}
	}))
	defer server.Close()
	a := NewAdmin(DefaultConfig())
	a.Base = server.URL
	a.Key = "scoped-key"
	accounts, err := a.ListAccounts(context.Background())
	if err != nil || len(accounts) != 2 || calls != 2 {
		t.Fatalf("%v %v %d", accounts, err, calls)
	}
	b, _ := json.Marshal(accounts)
	if strings.Contains(string(b), "NEVER_EXPORT") || strings.Contains(string(b), "credentials") {
		t.Fatal("credential leaked")
	}
	if !accounts[1].Supported || !accounts[0].Supported {
		t.Fatal("API or OAuth account incorrectly disabled")
	}
}
func TestDiscoveryFailsClosedOnPageFailureAndDuplicate(t *testing.T) {
	for _, mode := range []string{"forbidden", "duplicate", "incomplete"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") == "2" && mode == "forbidden" {
					w.WriteHeader(403)
					return
				}
				if r.URL.Query().Get("page") == "2" && mode == "incomplete" {
					w.Write([]byte(`{"code":0,"data":{"items":[],"total":2,"pages":2,"page":2}}`))
					return
				}
				page := r.URL.Query().Get("page")
				w.Write([]byte(`{"code":0,"data":{"items":[{"id":1,"name":"A","platform":"openai","type":"oauth"}],"total":2,"pages":2,"page":` + page + `}}`))
			}))
			defer server.Close()
			a := NewAdmin(DefaultConfig())
			a.Base = server.URL
			a.Key = "key"
			items, err := a.ListAccounts(context.Background())
			if err == nil || items != nil {
				t.Fatal("partial list reported as success")
			}
		})
	}
}
func TestDiscoveryEmptyScopeAndMissingKey(t *testing.T) {
	a := NewAdmin(DefaultConfig())
	if _, err := a.ListAccounts(context.Background()); err == nil {
		t.Fatal("empty key succeeded")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"data":{"items":[],"total":0,"pages":0,"page":1}}`))
	}))
	defer server.Close()
	a.Base = server.URL
	a.Key = "key"
	items, err := a.ListAccounts(context.Background())
	if err != nil || items == nil || len(items) != 0 {
		t.Fatal("empty authorized scope not represented")
	}
}
