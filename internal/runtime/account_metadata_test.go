package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/cyeinfpro/ModelGuard/internal/guard"
	v1 "github.com/cyeinfpro/ModelGuard/internal/pluginv1"
	"google.golang.org/grpc"
	"testing"
)

type metadataHost struct {
	v1.HostServiceClient
	info *v1.AccountInfo
	err  error
}

func (h metadataHost) ListAccounts(context.Context, *v1.ListAccountsRequest, ...grpc.CallOption) (*v1.ListAccountsResponse, error) {
	return &v1.ListAccountsResponse{Accounts: []*v1.AccountInfo{h.info}}, h.err
}
func TestEnrichScopedAccountMetadata(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*v1.AccountInfo)
		wantErr bool
	}{
		{"paused", func(*v1.AccountInfo) {}, false},
		{"different account", func(i *v1.AccountInfo) { i.Id = 2 }, true},
		{"missing revision", func(i *v1.AccountInfo) { i.MetadataJson = []byte(`{"ID":1,"Schedulable":false}`) }, true},
		{"changed scheduling", func(i *v1.AccountInfo) {
			i.MetadataJson = []byte(`{"ID":1,"Schedulable":true,"UpdatedAt":"2026-10-30T18:57:33.769372+08:00"}`)
		}, true},
		{"changed status", func(i *v1.AccountInfo) { i.Status = "disabled" }, true},
		{"shadow", func(i *v1.AccountInfo) { i.IsShadow = true }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta, _ := json.Marshal(map[string]any{"ID": 1, "Schedulable": false, "UpdatedAt": "2026-10-30T18:57:33.769372+08:00", "ExpiresAt": "2027-01-01T00:00:00Z"})
			info := &v1.AccountInfo{Id: 1, Platform: "openai", AccountType: "oauth", Status: "active", MetadataJson: meta}
			tc.mutate(info)
			p := &Plugin{host: metadataHost{info: info}}
			a := guard.Account{ID: 1, Platform: "openai", Type: "oauth", Status: "active"}
			err := p.enrichAccount(context.Background(), &a)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error %v", err)
			}
			if err == nil && (a.UpdatedAt != "2026-10-30T10:57:33.769372Z" || a.ExpiresAt == nil || a.ExpiresAt.Year() != 2027) {
				t.Fatalf("metadata not supplemented: %+v", a)
			}
		})
	}
	p := &Plugin{host: metadataHost{err: errors.New("unavailable")}}
	if p.enrichAccount(context.Background(), &guard.Account{ID: 1}) == nil {
		t.Fatal("unavailable host accepted")
	}
}
