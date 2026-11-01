package runtime

import (
	"context"
	"github.com/cyeinfpro/ModelGuard/internal/guard"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestGroupSyncCommitsOnlyCompleteSuccess(t *testing.T) {
	var mode atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 1:
			w.WriteHeader(403)
		case 2:
			w.Write([]byte(`{"code":0,"data":{"items":[{"id":1,"platform":"openai","type":"oauth"}],"total":1,"page":1}}`))
		case 3:
			w.Write([]byte(`{"code":0,"data":{"items":[],"total":0,"page":1}}`))
		case 4:
			if r.URL.Query().Get("page") == "2" {
				w.WriteHeader(500)
			} else {
				w.Write([]byte(`{"code":0,"data":{"items":[{"id":2,"group_ids":[5],"platform":"openai","type":"oauth"}],"total":2,"pages":2,"page":1}}`))
			}
		default:
			w.Write([]byte(`{"code":0,"data":{"items":[{"id":1,"group_ids":[5],"platform":"openai","type":"oauth"}],"total":1,"page":1}}`))
		}
	}))
	defer s.Close()
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	c := guard.DefaultConfig()
	c.DataDir = t.TempDir()
	c.AdminURL = s.URL
	c.AdminKey = "synthetic"
	c.GroupIDs = []int64{5}
	c.GroupModel = "gpt-6.1-terra"
	c.GroupExpected = "gpt-6.1-terra"
	e, err := guard.NewEngine(c, p.detector, guard.NewAdmin(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Store.Close()
	p.config = c
	p.engine = e
	if len(e.ManagedTargets()) != 0 {
		t.Fatal("unverified startup membership")
	}
	if _, err = p.refreshAccounts(context.Background(), c); err != nil || len(e.ManagedTargets()) != 1 {
		t.Fatal(err)
	}
	for _, m := range []int32{1, 2, 4} {
		mode.Store(m)
		if _, err = p.refreshAccounts(context.Background(), c); err == nil {
			t.Fatal("failed group refresh falsely succeeded", m)
		}
		if targets := e.ManagedTargets(); len(targets) != 1 || targets[0].AccountID != 1 {
			t.Fatal("failure pruned or replaced valid targets", m, targets)
		}
	}
	p.config.ManualRequestID = "new-manual-request"
	mode.Store(3)
	if _, err = p.refreshAccounts(context.Background(), c); err != nil || len(e.ManagedTargets()) != 0 {
		t.Fatal("confirmed empty scope not applied", err)
	}
}
