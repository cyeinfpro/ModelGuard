package runtime

import (
	"context"
	v1 "github.com/cyeinfpro/ModelGuard/internal/pluginv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func grpcFixture(t *testing.T) (v1.TransportPluginClient, *Plugin) {
	t.Helper()
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	v1.RegisterTransportPluginServer(server, p)
	go server.Serve(listener)
	conn, err := grpc.NewClient("passthrough:///guard", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); server.Stop(); listener.Close(); p.Close() })
	return v1.NewTransportPluginClient(conn), p
}
func TestForwardPreservesBodyHeadersStatusAndChunks(t *testing.T) {
	api, p := grpcFixture(t)
	payload := `{"model":"gpt-6.1-terra","input":"untouched"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != payload || r.Header.Get("Authorization") != "Bearer transport-secret" || r.Header.Get("X-Custom") != "kept" {
			t.Error("request mutated")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Upstream", "preserved")
		w.WriteHeader(402)
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		io.WriteString(w, "data: two\n\n")
	}))
	defer upstream.Close()
	stream, err := api.Forward(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stream.Send(&v1.ForwardRequest{Frame: &v1.ForwardRequest_Start{Start: &v1.ForwardRequestStart{Method: "POST", Url: upstream.URL, HasBody: true, ContentLength: int64(len(payload)), Headers: map[string]*v1.HeaderValues{"Authorization": {Values: []string{"Bearer transport-secret"}}, "X-Custom": {Values: []string{"kept"}}}}}})
	stream.Send(&v1.ForwardRequest{Frame: &v1.ForwardRequest_BodyChunk{BodyChunk: []byte(payload)}})
	stream.Send(&v1.ForwardRequest{Frame: &v1.ForwardRequest_BodyEnd{BodyEnd: true}})
	stream.CloseSend()
	var body strings.Builder
	started, ended := false, false
	for {
		frame, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if s := frame.GetStart(); s != nil {
			started = true
			if s.StatusCode != 402 || s.Headers["X-Upstream"].Values[0] != "preserved" {
				t.Fatal("response metadata lost")
			}
		}
		body.Write(frame.GetBodyChunk())
		if e := frame.GetError(); e != nil {
			t.Fatalf("unexpected transport error %+v", e)
		}
		if e := frame.GetEnd(); e != nil {
			ended = true
		}
	}
	if !started || !ended || body.String() != "data: one\n\ndata: two\n\n" {
		t.Fatal("stream altered")
	}
	metrics := p.forwardStats.snapshot()
	if metrics.Requests != 1 || metrics.InFlight != 0 || metrics.Completed != 1 || metrics.HTTPStatusClasses["4xx"] != 1 || metrics.FirstByteSamples != 1 || len(metrics.Errors) != 0 {
		t.Fatalf("forward transport accounting incorrect: %+v", metrics)
	}
}
func TestForwardCancellationReachesUpstream(t *testing.T) {
	api, _ := grpcFixture(t)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(cancelled) }))
	defer upstream.Close()
	ctx, cancel := context.WithCancel(context.Background())
	stream, _ := api.Forward(ctx)
	stream.Send(&v1.ForwardRequest{Frame: &v1.ForwardRequest_Start{Start: &v1.ForwardRequestStart{Method: "GET", Url: upstream.URL}}})
	stream.Send(&v1.ForwardRequest{Frame: &v1.ForwardRequest_BodyEnd{BodyEnd: true}})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request orphaned")
	}
}
func TestMalformedStartIsDefinitelyUnsent(t *testing.T) {
	api, p := grpcFixture(t)
	stream, _ := api.Forward(context.Background())
	stream.Send(&v1.ForwardRequest{Frame: &v1.ForwardRequest_BodyEnd{BodyEnd: true}})
	r, err := stream.Recv()
	if err != nil || r.GetError() == nil || r.GetError().RequestSent {
		t.Fatal("invalid request marked sent")
	}
	if _, err = stream.Recv(); err != io.EOF {
		t.Fatalf("terminal malformed stream: %v", err)
	}
	metrics := p.forwardStats.snapshot()
	if metrics.Requests != 1 || metrics.InFlight != 0 || metrics.Completed != 0 || metrics.Errors["invalid_start"] != 1 || metrics.FirstByteSamples != 0 {
		t.Fatalf("unsent failure accounting incorrect: %+v", metrics)
	}
}
func TestRealGRPCConfigAndHealth(t *testing.T) {
	api, _ := grpcFixture(t)
	info, err := api.GetInfo(context.Background(), &v1.GetInfoRequest{})
	if err != nil || info.PluginId != ID {
		t.Fatal(err)
	}
	v, err := api.ValidateConfig(context.Background(), &v1.ValidateConfigRequest{ConfigJson: []byte(`{}`)})
	if err != nil || !v.Valid {
		t.Fatal("defaults rejected")
	}
	a, err := api.ApplyConfig(context.Background(), &v1.ApplyConfigRequest{ConfigJson: v.NormalizedConfigJson})
	if err != nil || !a.Applied {
		t.Fatal("apply failed")
	}
	h, err := api.Health(context.Background(), &v1.HealthRequest{})
	if err != nil || !h.Healthy || strings.Contains(h.StatusJson, "admin_key") {
		t.Fatal("bad or sensitive health")
	}
}
