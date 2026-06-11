package proxy

import (
	"path/filepath"
	"testing"

	"kiro-go/config"
	"kiro-go/pool"
)

// TestWebSearchProviderIsolation locks in the rule that a request explicitly
// routed to a NON-Kiro provider never borrows a Kiro account for the web-search
// MCP side-call. A provider-prefixed request (e.g. "fable/claude-fable-5") must
// stay entirely on that provider — the prior behavior re-routed the search
// side-call through any usable Kiro account, mixing providers against the user's
// explicit selection.
//
// Regression for: "if im using provider ... it should only use this provider
// instead of other like kiro".
func TestWebSearchProviderIsolation(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	// A usable Kiro account exists in the pool — the OLD code would have borrowed
	// it for a non-Kiro request's search side-call.
	kiroAcct := config.Account{
		ID:          "kiro-1",
		Backend:     "kiro",
		AccessToken: "valid-token",
		ExpiresAt:   0, // never expires -> ensureValidToken is a no-op
		Enabled:     true,
	}
	if err := config.AddAccount(kiroAcct); err != nil {
		t.Fatalf("AddAccount kiro: %v", err)
	}

	// A self-contained custom non-Kiro provider, anthropic-compatible (NOT native
	// web search), routed via the "fable" backend.
	fableAcct := config.Account{
		ID:              "fable-1",
		Backend:         "fable",
		CustomDialect:   "anthropic",
		BaseURLOverride: "https://api.example.com/v1",
		CustomModels:    []string{"claude-fable-5"},
		APIKey:          "sk-fable",
		Enabled:         true,
	}
	if err := config.AddAccount(fableAcct); err != nil {
		t.Fatalf("AddAccount fable: %v", err)
	}

	h := &Handler{pool: pool.NewForTesting()}

	// The custom anthropic-compatible provider is NOT native (only real
	// anthropic.com is), so the only way the OLD code engaged emulation was by
	// borrowing the Kiro account. The fix makes a non-Kiro backend never emulate.
	if h.shouldEmulateWebSearch("fable") {
		t.Error("non-Kiro backend 'fable' must NOT emulate web search via a Kiro account — provider isolation violated")
	}

	// Sanity: a Kiro-backed request with a usable Kiro account still emulates.
	if !h.shouldEmulateWebSearch("kiro") {
		t.Error("kiro backend with a usable Kiro account should still emulate web search")
	}
}

// TestWebSearchNonKiroNoKiroAccount confirms a non-Kiro backend still does not
// emulate even when there is NO Kiro account (it simply drops the tool — the
// generic provider strips the hosted web_search tool before the upstream call).
func TestWebSearchNonKiroNoKiroAccount(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	fableAcct := config.Account{
		ID:              "fable-1",
		Backend:         "fable",
		CustomDialect:   "anthropic",
		BaseURLOverride: "https://api.example.com/v1",
		CustomModels:    []string{"claude-fable-5"},
		APIKey:          "sk-fable",
		Enabled:         true,
	}
	if err := config.AddAccount(fableAcct); err != nil {
		t.Fatalf("AddAccount fable: %v", err)
	}
	h := &Handler{pool: pool.NewForTesting()}
	if h.shouldEmulateWebSearch("fable") {
		t.Error("non-Kiro backend must not emulate web search")
	}
}
