package guard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cyeinfpro/ModelGuard/internal/fingerprint"
)

type preparedRetryProbe struct {
	*Prober
	delays []time.Duration
}

func (p *preparedRetryProbe) Retry(_ context.Context, _ Account, _, _ int, _ string, d time.Duration) error {
	p.delays = append(p.delays, d)
	return nil
}

func TestPrepareErrorsConsumeNoRequestBudget(t *testing.T) {
	d, err := fingerprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig()
	target := Target{Model: "gpt-6.1-terra", Expected: "gpt-6.1-terra"}
	cases := []struct {
		name string
		a    Account
		p    *Prober
	}{
		{"missing key", Account{Type: "apikey", Credentials: map[string]any{"base_url": "https://example.test"}}, &Prober{}},
		{"invalid endpoint", Account{Type: "apikey", Credentials: map[string]any{"base_url": "http://remote.test", "api_key": "key"}}, &Prober{}},
		{"invalid proxy", Account{Type: "apikey", OutboundProxy: "broken", Credentials: map[string]any{"base_url": "https://example.test", "api_key": "key"}}, &Prober{}},
		{"invalid auth header", Account{Type: "apikey", Credentials: map[string]any{"base_url": "https://example.test", "api_key": "key\nsecret"}}, &Prober{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spent := 0
			_, _, err := Evaluate(context.Background(), tc.a, target, c, tc.p, d, func(bool) error { spent++; return nil })
			var config *ProbeConfigurationError
			if !errors.As(err, &config) || spent != 0 {
				t.Fatalf("spent=%d error=%v", spent, err)
			}
		})
	}
	resolved, spent := 0, 0
	p := &Prober{Resolve: func(context.Context, int64) (Identity, error) {
		resolved++
		return Identity{}, errors.New("private error")
	}}
	_, _, err = Evaluate(context.Background(), Account{ID: 1, Type: "oauth"}, target, c, p, d, func(bool) error { spent++; return nil })
	if resolved != 1 || spent != 0 || err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("resolve=%d spent=%d err=%v", resolved, spent, err)
	}
}
func TestHTTPStatusRetryPolicyAndBudget(t *testing.T) {
	d, err := fingerprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []int{400, 401, 403, 404, 405, 422, 429, 500, 502, 503, 504} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if code == 429 {
					w.Header().Set("Retry-After", "30")
				}
				w.WriteHeader(code)
				fmt.Fprint(w, `{"error":{"code":"model_not_found","type":"invalid_request_error","message":"PRIVATE_KEY private prompt"}}`)
			}))
			defer server.Close()
			p := &preparedRetryProbe{Prober: &Prober{}}
			spent := 0
			a, _, err := Evaluate(context.Background(), Account{Type: "apikey", Credentials: map[string]any{"base_url": server.URL, "api_key": "test"}}, Target{Model: "test", Expected: "gpt-6.1-terra"}, DefaultConfig(), p, d, func(bool) error { spent++; return nil })
			want := 3
			if code < 429 {
				want = 1
			}
			if requests != want || spent != want {
				t.Fatalf("requests=%d spent=%d want=%d", requests, spent, want)
			}
			if want == 1 {
				var config *ProbeConfigurationError
				if !errors.As(err, &config) || !strings.Contains(err.Error(), "model_not_found") {
					t.Fatalf("bad permanent error: %v", err)
				}
			} else if err != nil || a == nil || a.Used != 0 || a.Decision != "indeterminate" {
				t.Fatalf("bad transient result: %+v %v", a, err)
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("unsafe upstream body exposed")
			}
			if code == 429 {
				for _, delay := range p.delays {
					if delay < 30*time.Second {
						t.Fatal("Retry-After ignored")
					}
				}
			}
		})
	}
}
func TestLongRetryAfterEndsRoundWithoutEarlyRetry(t *testing.T) {
	d, _ := fingerprint.Load()
	requests := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
	}))
	defer s.Close()
	p := &preparedRetryProbe{Prober: &Prober{}}
	spent := 0
	a, _, err := Evaluate(context.Background(), Account{Type: "apikey", Credentials: map[string]any{"base_url": s.URL, "api_key": "test"}}, Target{Model: "test", Expected: "gpt-6.1-terra"}, DefaultConfig(), p, d, func(bool) error { spent++; return nil })
	if err != nil || a == nil || requests != 1 || spent != 1 || len(p.delays) != 0 {
		t.Fatalf("cooldown broken requests=%d spent=%d %v", requests, spent, err)
	}
}
func TestRetryAfterFormatsAndBackoff(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for raw, want := range map[string]time.Duration{"30": 30 * time.Second, "-1": 0, "garbage": 0, now.Add(45 * time.Second).Format(http.TimeFormat): 45 * time.Second} {
		if got := ParseRetryAfter(raw, now); got != want {
			t.Fatalf("%s: got %v want %v", raw, got, want)
		}
	}
	for attempt, base := range map[int]time.Duration{2: 5 * time.Second, 3: 15 * time.Second} {
		delay, ok := probeRetryDelay(errors.New("transient"), attempt)
		if !ok || delay < base*8/10 || delay > base*12/10 {
			t.Fatal("backoff out of range", attempt, delay)
		}
	}
}
func TestProductionPoolUsesLRUAndHostDeadlines(t *testing.T) {
	p := &ClientPool{}
	defer p.Close()
	first, _ := p.Client("http://127.0.0.1:10000")
	tr := first.Transport.(*http.Transport)
	if tr.ResponseHeaderTimeout != 0 || tr.MaxIdleConnsPerHost < 32 {
		t.Fatal("probe settings leaked into forwarding")
	}
	for i := 1; i < 64; i++ {
		if _, err := p.Client(fmt.Sprintf("http://127.0.0.1:%d", 10000+i)); err != nil {
			t.Fatal(err)
		}
	}
	p.Client("http://127.0.0.1:10000")
	p.Client("http://127.0.0.1:11000")
	if len(p.entries) != 64 || p.entries["http://127.0.0.1:10000"] == nil || p.entries["http://127.0.0.1:10001"] != nil {
		t.Fatal("hot proxy evicted instead of LRU")
	}
}
func TestLoopbackAdminAnyPortAndIPv6(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1:8080", "http://localhost:8090", "http://[::1]:9080", "https://127.0.0.2", "http://localhost"} {
		if !IsLoopbackAdminURL(u) {
			t.Fatal("loopback rejected", u)
		}
	}
	for _, u := range []string{"http://example.test:8080", "http://127.0.0.1.evil.test", "http://user:pass@localhost", "http://localhost/path", "http://localhost?token=x", "http://localhost:65536"} {
		if IsLoopbackAdminURL(u) {
			t.Fatal("unsafe local endpoint accepted", u)
		}
	}
}
func TestAdminMutationBypassesMetadataCache(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			fmt.Fprint(w, `{"code":0,"data":{"id":1,"status":"active"}}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{"id":1,"type":"oauth","platform":"openai","status":"active","schedulable":false}}`)
	}))
	defer s.Close()
	c := DefaultConfig()
	c.AdminURL = s.URL
	a := NewAdmin(c)
	cached, fresh := 0, 0
	a.Enrich = func(context.Context, *Account) error { cached++; return nil }
	a.EnrichFresh = func(context.Context, *Account) error { fresh++; return nil }
	if _, err := a.Account(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Pause(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if cached != 1 || fresh != 1 {
		t.Fatal("post-write read used stale metadata", cached, fresh)
	}
}
func FuzzReadResponsesProgress(f *testing.F) {
	f.Add("data: [DONE]\n\n")
	f.Add("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 1<<20 {
			t.Skip()
		}
		_, _ = readResponsesProgress(strings.NewReader(raw), nil)
	})
}

func TestMalformedEndpointNeverConsumesBudget(t *testing.T) {
	d, _ := fingerprint.Load()
	for _, base := range []string{"https://example.test:65536", "https://:8443", "https://example.test:0", "https://example.test:abc"} {
		spent := 0
		a := Account{Type: "apikey", Credentials: map[string]any{"base_url": base, "api_key": "test"}}
		_, _, err := Evaluate(context.Background(), a, Target{Model: "test", Expected: "gpt-6.1-terra"}, DefaultConfig(), &Prober{}, d, func(bool) error { spent++; return nil })
		if err == nil || spent != 0 {
			t.Fatalf("%s consumed %d: %v", base, spent, err)
		}
	}
}
func TestUnknownErrorIdentifiersCannotReflectCredentials(t *testing.T) {
	for _, value := range []string{"abc123-privateCredentialValue987", "arbitraryAlphanumericCredential123456", "user@example.test", "sk-private", "a.secret.value"} {
		if got := safeErrorToken(value); got != "" {
			t.Fatalf("untrusted error identifier exposed: %q", got)
		}
	}
	if safeErrorToken("invalid_api_key") != "invalid_api_key" {
		t.Fatal("standard diagnostic was lost")
	}
}
