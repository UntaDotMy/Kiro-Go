package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// TestProviderModelsDoNotPolluteGlobalCatalog verifies the provider-first
// separation fix: when a NON-Kiro provider account's models are fetched (via the
// per-account refresh path), the bare model ids populate ONLY that account's
// per-account routing list (pool model list) — they must NOT be merged into the
// shared /v1/models catalog (h.cachedModels), because a Kiro client would then
// request them unprefixed and mis-route to Kiro.
func TestProviderModelsDoNotPolluteGlobalCatalog(t *testing.T) {
	// A custom OpenAI-compatible provider whose /models returns bare ids.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{
					{"id": "llama-3.3-70b"},
					{"id": "mixtral-8x7b"},
				},
			})
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	// Register a custom provider pointing at the test server, with a real config
	// so GetProviderConfig resolves it.
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddProvider(config.ProviderConfig{
		ID: "mygw", Dialect: "openai", BaseURL: srv.URL + "/v1", FetchModels: true,
	}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	h := &Handler{pool: pool.NewForTesting(), startTime: 0}
	// Seed the shared catalog with a Kiro model so we can prove it's untouched.
	h.cachedModels = []ModelInfo{{ModelId: "claude-sonnet-4.5"}}

	acct := &config.Account{ID: "acc-mygw", Backend: "mygw", APIKey: "sk-test"}

	if err := h.fetchAndCacheAccountModels(acct); err != nil {
		t.Fatalf("fetchAndCacheAccountModels: %v", err)
	}

	// 1) The account's per-account routing list MUST contain the fetched ids.
	perAccount := h.pool.GetModelList(acct.ID)
	if len(perAccount) != 2 {
		t.Fatalf("expected 2 models in the account's routing list, got %d: %v", len(perAccount), perAccount)
	}

	// 2) The shared /v1/models catalog MUST NOT contain the provider's bare ids.
	h.modelsCacheMu.RLock()
	cached := append([]ModelInfo(nil), h.cachedModels...)
	h.modelsCacheMu.RUnlock()
	for _, m := range cached {
		if m.ModelId == "llama-3.3-70b" || m.ModelId == "mixtral-8x7b" {
			t.Fatalf("non-Kiro provider model %q leaked into the shared /v1/models catalog: %v", m.ModelId, cached)
		}
	}
	// And the pre-existing Kiro model is still there (catalog not clobbered).
	foundKiro := false
	for _, m := range cached {
		if m.ModelId == "claude-sonnet-4.5" {
			foundKiro = true
		}
	}
	if !foundKiro {
		t.Fatalf("the Kiro catalog entry was lost; cached=%v", cached)
	}
}
