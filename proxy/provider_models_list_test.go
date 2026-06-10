package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"kiro-go/config"
	"kiro-go/pool"
)

// TestProviderPrefixedModelEntries verifies that an added provider account's
// cached models surface in /v1/models WITH their routing prefix, so they are
// both discoverable in the picker and route back to the provider — while their
// bare ids never leak into the shared (Kiro) catalog.
func TestProviderPrefixedModelEntries(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// A custom provider with a short alias; the prefix should prefer the alias.
	if err := config.AddProvider(config.ProviderConfig{
		ID: "mygw", Alias: "mg", Dialect: "openai", BaseURL: "https://gw.example.com/v1",
	}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	// Accounts: a built-in (groq, alias "groq"), the custom provider, qoder, and
	// a Kiro account that must NOT be double-listed here.
	for _, a := range []config.Account{
		{ID: "a-groq", Backend: "groq", APIKey: "k", Enabled: true},
		{ID: "a-mygw", Backend: "mygw", APIKey: "k", Enabled: true},
		{ID: "a-qoder", Backend: "qoder", AccessToken: "t", QoderUserID: "u", Enabled: true},
		{ID: "a-kiro", Backend: "kiro", RefreshToken: "r", Enabled: true},
	} {
		if err := config.AddAccount(a); err != nil {
			t.Fatalf("AddAccount %s: %v", a.ID, err)
		}
	}

	h := &Handler{pool: pool.NewForTesting()}
	h.pool.SetModelList("a-groq", []string{"llama-3.3-70b", "mixtral-8x7b"})
	h.pool.SetModelList("a-mygw", []string{"gpt-4o"})
	h.pool.SetModelList("a-qoder", []string{"auto", "ultimate"})
	h.pool.SetModelList("a-kiro", []string{"claude-opus-4-7"})

	entries := h.providerPrefixedModelEntries()
	got := map[string]bool{}
	for _, e := range entries {
		if id, ok := e["id"].(string); ok {
			got[id] = true
		}
	}

	// Built-in alias prefix.
	for _, want := range []string{"groq/llama-3.3-70b", "groq/mixtral-8x7b"} {
		if !got[want] {
			t.Errorf("missing expected prefixed model %q; got %v", want, keysOf(got))
		}
	}
	// Custom provider uses its alias "mg".
	if !got["mg/gpt-4o"] {
		t.Errorf("custom provider model should be listed as mg/gpt-4o; got %v", keysOf(got))
	}
	// Qoder addressed by its canonical id.
	if !got["qoder/auto"] || !got["qoder/ultimate"] {
		t.Errorf("qoder models should be prefixed qoder/*; got %v", keysOf(got))
	}
	// The Kiro account must NOT be emitted here (it's in the shared catalog).
	for id := range got {
		if id == "claude-opus-4-7" || id == "kiro/claude-opus-4-7" {
			t.Errorf("Kiro model %q must not appear in provider-prefixed entries", id)
		}
	}
}

// TestRoutingPrefixForBackend pins the prefix chosen per backend.
func TestRoutingPrefixForBackend(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	_ = config.AddProvider(config.ProviderConfig{ID: "mygw", Alias: "mg", Dialect: "openai", BaseURL: "https://x/v1"})
	_ = config.AddProvider(config.ProviderConfig{ID: "noalias", Dialect: "openai", BaseURL: "https://y/v1"})

	cases := []struct {
		backend string
		want    string
	}{
		{"kiro", ""},
		{"", ""},
		{"groq", "groq"},       // built-in alias == id
		{"openrouter", "or"},   // built-in alias differs from id
		{"codebuddy", "cb"},    // newly added built-in alias
		{"qoder", "qoder"},     // bespoke backend, no catalog entry -> id
		{"codex", "codex"},     // bespoke backend -> id
		{"mygw", "mg"},         // custom provider alias
		{"noalias", "noalias"}, // custom provider without alias -> id
	}
	for _, c := range cases {
		if got := routingPrefixForBackend(c.backend); got != c.want {
			t.Errorf("routingPrefixForBackend(%q) = %q, want %q", c.backend, got, c.want)
		}
	}
}

// TestApiGetAvailableModelsIncludesProviderModels verifies the API-key editor's
// model checklist (GET /admin/api/models/available) lists NON-Kiro provider
// models WITH their routing prefix, so an operator can allowlist a key to e.g.
// "groq/llama-3.3-70b". Before the fix this endpoint only emitted the Kiro
// catalog + auto/gpt aliases, so provider models could never be allowlisted.
func TestApiGetAvailableModelsIncludesProviderModels(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	config.SetPassword("pw")
	if err := config.AddAccount(config.Account{ID: "a-groq", Backend: "groq", APIKey: "k", Enabled: true}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	h := &Handler{pool: pool.NewForTesting()}
	h.pool.SetModelList("a-groq", []string{"llama-3.3-70b"})

	req := httptest.NewRequest(http.MethodGet, "/admin/api/models/available", nil)
	req.Header.Set("X-Admin-Password", "pw")
	w := httptest.NewRecorder()
	h.apiGetAvailableModels(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	found := false
	for _, id := range resp.Models {
		if id == "groq/llama-3.3-70b" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected prefixed provider model groq/llama-3.3-70b in the editor list; got %v", resp.Models)
	}
}
