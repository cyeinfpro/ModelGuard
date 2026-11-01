package guard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestScopedMetadataPauseRecoverAndExternalEdit(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(map[bool]string{false: "recover", true: "external edit"}[external], func(t *testing.T) {
			e, _, verdict, _ := fixture(t)
			var mu sync.Mutex
			a := Account{ID: 1, Platform: "openai", Type: "oauth", Status: "active", Schedulable: true, UpdatedAt: "2026-10-30T18:00:00+08:00"}
			writes := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method == "POST" {
					var body struct {
						Schedulable bool `json:"schedulable"`
					}
					json.NewDecoder(r.Body).Decode(&body)
					a.Schedulable = body.Schedulable
					writes++
					a.UpdatedAt = time.Date(2026, 10, 30, 10, writes, 0, 0, time.UTC).Format(time.RFC3339Nano)
				}
				dto := a
				dto.UpdatedAt = ""
				json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": dto})
			}))
			defer s.Close()
			cfg := e.Config
			cfg.AdminURL = s.URL
			admin := NewAdmin(cfg)
			admin.Enrich = func(_ context.Context, x *Account) error {
				mu.Lock()
				defer mu.Unlock()
				x.UpdatedAt = a.UpdatedAt
				return nil
			}
			e.Admin = admin
			tick(t, e)
			if !e.Store.State.Records[1].Hold || writes != 1 {
				t.Fatal("did not establish pause ownership")
			}
			*verdict = "match"
			tick(t, e)
			if writes != 1 || e.Store.State.Records[1].RecoveryPasses != 1 {
				t.Fatal("restored after only one match")
			}
			if external {
				mu.Lock()
				a.UpdatedAt = "2026-10-30T12:00:00Z"
				mu.Unlock()
			}
			tick(t, e)
			if writes != 2 || e.Store.State.Records[1].Hold {
				t.Fatal("managed account not restored after two matches")
			}
		})
	}
}
func TestDeniedScopedKeyNeverEnrichesFromHost(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer s.Close()
	c := DefaultConfig()
	c.AdminURL = s.URL
	a := NewAdmin(c)
	calls := 0
	a.Enrich = func(context.Context, *Account) error { calls++; return nil }
	if _, err := a.Account(context.Background(), 1); err == nil || calls != 0 {
		t.Fatal("bypassed management scope")
	}
}
func TestRevisionTimezoneAndMissing(t *testing.T) {
	if !sameRevision("2026-10-30T18:57:33.769372+08:00", "2026-10-30T10:57:33.769372Z") {
		t.Fatal("timezone misdetected as edit")
	}
	if sameRevision("", "") || sameRevision("x", "") || sameRevision("2026-10-30T10:57:34Z", "2026-10-30T10:57:33Z") {
		t.Fatal("missing/different revision accepted")
	}
}

func TestSparseSchedulingReceiptUsesScopedAccountType(t *testing.T) {
	for _, denyRead := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "post-success-read-denied"}[denyRead], func(t *testing.T) {
			writes, reads, enrich := 0, 0, 0
			scheduled := true
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					writes++
					var body struct {
						Schedulable bool `json:"schedulable"`
					}
					json.NewDecoder(r.Body).Decode(&body)
					scheduled = body.Schedulable
					json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"id": 1, "status": "active"}})
					return
				}
				reads++
				if denyRead {
					w.WriteHeader(403)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": Account{ID: 1, Type: "apikey", Platform: "openai", Status: "active", Schedulable: scheduled}})
			}))
			defer server.Close()
			c := DefaultConfig()
			c.AdminURL = server.URL
			admin := NewAdmin(c)
			admin.Enrich = func(_ context.Context, a *Account) error {
				enrich++
				if a.Type != "apikey" {
					t.Fatal("inferred OAuth from sparse receipt")
				}
				a.UpdatedAt = "revision"
				return nil
			}
			_, err := admin.Pause(context.Background(), 1)
			if (err != nil) != denyRead || writes != 1 || reads != 1 {
				t.Fatal("unexpected confirmation or repeated POST", err, writes, reads)
			}
			if denyRead && enrich != 0 {
				t.Fatal("bypassed denied scoped read")
			}
			if !denyRead {
				a, err := admin.Resume(context.Background(), 1)
				if err != nil || !a.Schedulable || writes != 2 || reads != 2 {
					t.Fatal("resume not confirmed", err)
				}
			}
		})
	}
}
