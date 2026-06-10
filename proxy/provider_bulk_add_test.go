package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
	"kiro-go/pool"
)

// callBulkAdd posts a bulk-add payload to the handler and decodes the JSON reply.
func callBulkAdd(t *testing.T, h *Handler, payload map[string]interface{}) (int, map[string]interface{}) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/admin/api/providers/account/bulk", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiAddProviderAccountsBulk(rec, req)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestBulkAddProviderAccounts is the core happy path: many keys for one built-in
// backend become one account each, the model catalog is fetched ONCE for the
// batch and seeded onto every new account, and a re-paste of overlapping keys is
// deduped rather than duplicated.
func TestBulkAddProviderAccounts(t *testing.T) {
	// A test server that serves a /models list so the single batch fetch succeeds
	// without real network.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{{"id": "model-a"}, {"id": "model-b"}},
			})
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// Register a custom provider id pointed at the test server so the batch model
	// fetch resolves locally (and the SSRF/https guard on baseURL is not in play —
	// we pass the backend id, not a "custom" inline base URL).
	if err := config.AddProvider(config.ProviderConfig{
		ID: "testgw", Dialect: "openai", BaseURL: srv.URL + "/v1", FetchModels: true,
	}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	h := &Handler{pool: pool.NewForTesting()}

	// First paste: three distinct keys.
	code, out := callBulkAdd(t, h, map[string]interface{}{
		"backend": "testgw",
		"apiKeys": []string{"sk-1", "sk-2", "sk-3", "sk-2"}, // one in-paste dup
		"name":    "GW",
	})
	if code != http.StatusOK {
		t.Fatalf("bulk add HTTP %d, body=%v", code, out)
	}
	if out["success"] != true {
		t.Fatalf("expected success, got %v", out)
	}
	if got := int(out["added"].(float64)); got != 3 {
		t.Fatalf("added = %d, want 3 (one in-paste duplicate dropped)", got)
	}
	if got := int(out["modelCount"].(float64)); got != 2 {
		t.Errorf("modelCount = %d, want 2 (fetched once for the batch)", got)
	}

	// Exactly three accounts exist, all on the testgw backend with the pasted keys.
	accts := config.GetAccounts()
	if len(accts) != 3 {
		t.Fatalf("expected 3 accounts persisted, got %d", len(accts))
	}
	keys := map[string]bool{}
	for _, a := range accts {
		if config.GetAccountBackend(&a) != "testgw" {
			t.Errorf("account %s on wrong backend %q", a.ID, a.Backend)
		}
		keys[a.APIKey] = true
		// Each new account should carry the batch-fetched model list.
		if ml := h.pool.GetModelList(a.ID); len(ml) != 2 {
			t.Errorf("account %s model list = %v, want 2 seeded models", a.ID, ml)
		}
	}
	for _, want := range []string{"sk-1", "sk-2", "sk-3"} {
		if !keys[want] {
			t.Errorf("missing account for key %q", want)
		}
	}

	// Second paste overlapping the first: only the genuinely new key is added.
	code, out = callBulkAdd(t, h, map[string]interface{}{
		"backend": "testgw",
		"apiKeys": []string{"sk-2", "sk-3", "sk-4"},
		"name":    "GW",
	})
	if code != http.StatusOK {
		t.Fatalf("second bulk add HTTP %d, body=%v", code, out)
	}
	if got := int(out["added"].(float64)); got != 1 {
		t.Errorf("added = %d, want 1 (sk-4 only)", got)
	}
	if got := int(out["skipped"].(float64)); got != 2 {
		t.Errorf("skipped = %d, want 2 (sk-2, sk-3 already present)", got)
	}
	if total := len(config.GetAccounts()); total != 4 {
		t.Fatalf("expected 4 accounts after second paste, got %d", total)
	}
}

// TestBulkAddProviderRejectsOAuthBackends confirms the bulk path refuses the
// backends that need a different add flow (kiro refresh tokens / codex+qoder
// OAuth), so an operator gets a clear redirect instead of broken api-key rows.
func TestBulkAddProviderRejectsOAuthBackends(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: pool.NewForTesting()}
	for _, backend := range []string{"kiro", "codex", "qoder"} {
		code, out := callBulkAdd(t, h, map[string]interface{}{
			"backend": backend,
			"apiKeys": []string{"sk-1", "sk-2"},
		})
		if code != http.StatusBadRequest {
			t.Errorf("backend %q: HTTP %d, want 400 (body=%v)", backend, code, out)
		}
		if _, ok := out["error"]; !ok {
			t.Errorf("backend %q: expected an error message, got %v", backend, out)
		}
	}
	if n := len(config.GetAccounts()); n != 0 {
		t.Fatalf("no accounts should have been created for rejected backends, got %d", n)
	}
}

// TestBulkAddProviderEmptyAndUnknown covers the two remaining guard rails: an
// empty key list and an unknown backend are both rejected with no accounts saved.
func TestBulkAddProviderEmptyAndUnknown(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: pool.NewForTesting()}

	// Empty / whitespace-only keys.
	code, _ := callBulkAdd(t, h, map[string]interface{}{
		"backend": "groq",
		"apiKeys": []string{"", "   ", "\t"},
	})
	if code != http.StatusBadRequest {
		t.Errorf("empty keys: HTTP %d, want 400", code)
	}

	// Unknown backend.
	code, _ = callBulkAdd(t, h, map[string]interface{}{
		"backend": "not-a-real-provider",
		"apiKeys": []string{"sk-1"},
	})
	if code != http.StatusBadRequest {
		t.Errorf("unknown backend: HTTP %d, want 400", code)
	}

	if n := len(config.GetAccounts()); n != 0 {
		t.Fatalf("no accounts should have been created, got %d", n)
	}
}
