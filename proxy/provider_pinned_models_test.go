package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"kiro-go/config"
)

// TestListModelsUnionsPinnedModels is the regression for the "503 No available
// accounts for a bound model the upstream actually serves" bug.
//
// When a custom provider's live GET /models returns an INCOMPLETE list (it omits
// a model id the operator explicitly pinned via CustomModels, even though the
// endpoint would serve it), the live list previously REPLACED the pinned list
// wholesale. Seeded as a strict routing filter, the pinned-but-unlisted model
// then failed accountHasModel and the pool shed the only account -> 503.
//
// The fix unions the pinned ids into the live list so an explicit binding is
// always routable.
func TestListModelsUnionsPinnedModels(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	// Upstream /models returns three ids — NOT including the operator's bound
	// "claude-fable-5".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"},{"id":"model-c"}]}`))
	}))
	defer srv.Close()

	acct := config.Account{
		ID:              "fable-1",
		Backend:         "fable",
		CustomDialect:   "openai",
		BaseURLOverride: srv.URL + "/v1",
		CustomModels:    []string{"claude-fable-5"}, // pinned, but absent from /models
		APIKey:          "sk-test",
		Enabled:         true,
	}
	if err := config.AddAccount(acct); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	gp := &genericProvider{dialect: DialectOpenAI}
	models, err := gp.ListModels(&acct)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	got := make(map[string]bool, len(models))
	for _, m := range models {
		got[m.ModelId] = true
	}
	// Live ids preserved...
	for _, id := range []string{"model-a", "model-b", "model-c"} {
		if !got[id] {
			t.Errorf("live model %q missing from unioned list: %v", id, models)
		}
	}
	// ...and the pinned-but-unlisted binding is included (the fix).
	if !got["claude-fable-5"] {
		t.Errorf("pinned model claude-fable-5 was shed by an incomplete /models listing: %v", models)
	}
}

// TestFetchModelsForAccountUnionsPinned verifies the admin add/refresh path
// (FetchModelsForAccount) applies the same union, returning advisory=false (the
// live fetch succeeded) with the pinned id folded in.
func TestFetchModelsForAccountUnionsPinned(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"live-1"},{"id":"live-2"}]}`))
	}))
	defer srv.Close()

	acct := config.Account{
		ID:              "fable-2",
		Backend:         "fable2",
		CustomDialect:   "openai",
		BaseURLOverride: srv.URL + "/v1",
		CustomModels:    []string{"claude-fable-5", "live-1"}, // one unlisted, one already present
		APIKey:          "sk-test",
		Enabled:         true,
	}
	if err := config.AddAccount(acct); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	gp := &genericProvider{dialect: DialectOpenAI}
	ids, advisory, err := gp.FetchModelsForAccount(context.Background(), &acct)
	if err != nil {
		t.Fatalf("FetchModelsForAccount: %v", err)
	}
	if advisory {
		t.Error("a successful live fetch must be authoritative (advisory=false)")
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	if !set["claude-fable-5"] {
		t.Errorf("pinned unlisted model dropped: %v", ids)
	}
	// No duplicate of the already-present id.
	count := 0
	for _, id := range ids {
		if id == "live-1" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("pinned id already in live list was duplicated (count=%d): %v", count, ids)
	}
}
