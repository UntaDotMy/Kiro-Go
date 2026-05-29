package proxy

import (
	"context"
	"encoding/json"
	"kiro-go/config"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// TestHandleModelsFiltersByAPIKey pins the contract that /v1/models
// returns only models permitted by the calling API key's allowlist when
// one is configured. Empty allowlist (no key, or key with no Models set)
// returns the full list — the historical behavior.
func TestHandleModelsFiltersByAPIKey(t *testing.T) {
	// /v1/models touches GetThinkingConfig which panics on nil cfg, so
	// boot a throwaway config under t.TempDir for the lifetime of the
	// test. Other proxy tests follow the same pattern.
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config init: %v", err)
	}

	h := &Handler{startTime: 0}

	// Baseline: no key in context — every model is listed (we should at
	// least see the canonical claude-opus-4-7 from the static fallback).
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.handleModels(rec, req)
	full := decodeModelsResponse(t, rec)
	if !containsModelID(full, "claude-opus-4-7") {
		t.Fatalf("expected claude-opus-4-7 in unfiltered list, got %v", modelIDs(full))
	}
	if !containsModelID(full, "claude-haiku-4-5") {
		t.Fatalf("expected claude-haiku-4-5 in unfiltered list, got %v", modelIDs(full))
	}

	// Filtered: key with Models=[claude-opus-4-7] — only that model and
	// its dotted alias should appear; haiku must be gone.
	k := &config.APIKey{ID: "k1", Models: []string{"claude-opus-4-7"}}
	req2 := httptest.NewRequest("GET", "/v1/models", nil)
	req2 = req2.WithContext(context.WithValue(req2.Context(), apiKeyCtxKey{}, k))
	rec2 := httptest.NewRecorder()
	h.handleModels(rec2, req2)
	filtered := decodeModelsResponse(t, rec2)
	if !containsModelID(filtered, "claude-opus-4-7") {
		t.Fatalf("filtered list should still contain claude-opus-4-7, got %v", modelIDs(filtered))
	}
	if containsModelID(filtered, "claude-haiku-4-5") {
		t.Fatalf("claude-haiku-4-5 must not appear when allowlist excludes it, got %v", modelIDs(filtered))
	}
	// The dotted alias should also pass through because IsModelAllowedFor
	// APIKey treats it as the same model.
	if !containsModelID(filtered, "claude-opus-4.7") {
		t.Fatalf("filtered list should also contain dotted alias claude-opus-4.7, got %v", modelIDs(filtered))
	}

	// Empty allowlist (key.Models == nil) must behave identically to
	// "no key at all" — pin this as a back-compat contract so a future
	// refactor doesn't accidentally start filtering on the nil branch.
	kEmpty := &config.APIKey{ID: "k2"}
	req3 := httptest.NewRequest("GET", "/v1/models", nil)
	req3 = req3.WithContext(context.WithValue(req3.Context(), apiKeyCtxKey{}, kEmpty))
	rec3 := httptest.NewRecorder()
	h.handleModels(rec3, req3)
	full2 := decodeModelsResponse(t, rec3)
	if len(full2) != len(full) {
		t.Fatalf("nil Models allowlist should produce the unfiltered list, got %d entries vs %d", len(full2), len(full))
	}
}

// TestApiGetAvailableModelsReturnsCatalog confirms the admin endpoint
// returns the unfiltered model catalog as a flat sorted id list — this
// is what the API key form uses to render checkboxes. The endpoint must
// NOT apply any per-key filter (the form is showing the universe of
// options, not what some specific key can already access).
func TestApiGetAvailableModelsReturnsCatalog(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config init: %v", err)
	}
	h := &Handler{startTime: 0}

	req := httptest.NewRequest("GET", "/admin/api/available-models", nil)
	rec := httptest.NewRecorder()
	h.apiGetAvailableModels(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Models) == 0 {
		t.Fatalf("expected non-empty catalog, got 0 entries")
	}
	// Catalog must include both canonical and dotted alias forms (the
	// form's UI collapses them, but the API contract is the full list).
	want := map[string]bool{"claude-opus-4-7": true, "claude-opus-4.7": true}
	got := map[string]bool{}
	for _, id := range resp.Models {
		got[id] = true
	}
	for w := range want {
		if !got[w] {
			t.Errorf("expected catalog to include %q, got %v", w, resp.Models)
		}
	}
	// Sort guarantee from the handler — checkbox grid relies on it for
	// stable ordering.
	for i := 1; i < len(resp.Models); i++ {
		if resp.Models[i-1] > resp.Models[i] {
			t.Fatalf("catalog not sorted at index %d: %v", i, resp.Models[i-1:i+1])
		}
	}
}

func decodeModelsResponse(t *testing.T, rec *httptest.ResponseRecorder) []map[string]interface{} {
	t.Helper()
	var resp struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	return resp.Data
}

func modelIDs(models []map[string]interface{}) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		if id, ok := m["id"].(string); ok {
			out = append(out, id)
		}
	}
	return out
}

func containsModelID(models []map[string]interface{}, id string) bool {
	for _, m := range models {
		if got, ok := m["id"].(string); ok && got == id {
			return true
		}
	}
	return false
}
