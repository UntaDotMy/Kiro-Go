package proxy

import (
	"path/filepath"
	"testing"

	"kiro-go/config"
)

// TestSelfContainedCustomProviderResolution verifies the "bring-your-own
// endpoint" path that stores dialect + base URL + models INLINE on the account
// (no shared Config.Providers[] entry). The account's Backend id must resolve as
// a routing prefix, a dialect, a Provider, and effective providerSettings — all
// from the account alone.
func TestSelfContainedCustomProviderResolution(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	acct := config.Account{
		ID:              "acc-1",
		Backend:         "mygw",
		CustomDialect:   "openai",
		BaseURLOverride: "https://api.example.com/v1",
		CustomModels:    []string{"gpt-4o", "llama-3.3-70b"},
		APIKey:          "sk-test",
		Enabled:         true,
	}
	if err := config.AddAccount(acct); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	// No shared ProviderConfig should exist for it.
	if _, ok := config.GetProviderConfig("mygw"); ok {
		t.Fatal("self-contained custom add must NOT register a ProviderConfig")
	}
	// But the config helper finds it.
	if _, ok := config.GetCustomAccountByBackend("mygw"); !ok {
		t.Fatal("GetCustomAccountByBackend should find the inline custom account")
	}

	// Routing prefix resolves to the backend id.
	if got, ok := resolveProviderPrefix("mygw"); !ok || got != "mygw" {
		t.Errorf("resolveProviderPrefix(mygw) = (%q,%v), want (mygw,true)", got, ok)
	}
	// ParseModelBackend splits "mygw/<model>" correctly.
	if b, m := ParseModelBackend("mygw/gpt-4o"); b != "mygw" || m != "gpt-4o" {
		t.Errorf("ParseModelBackend(mygw/gpt-4o) = (%q,%q), want (mygw,gpt-4o)", b, m)
	}
	// Dialect resolves from the inline field.
	if got := dialectFor("mygw"); got != DialectOpenAI {
		t.Errorf("dialectFor(mygw) = %q, want openai", got)
	}
	// A Provider is resolvable (the generic provider for the dialect).
	if p := ProviderForBackend("mygw"); p == nil {
		t.Error("ProviderForBackend(mygw) returned nil; expected the generic openai provider")
	}
	// Effective provider settings come from the account inline.
	ps, ok := resolveProviderSettings(&acct)
	if !ok {
		t.Fatal("resolveProviderSettings returned ok=false for the inline custom account")
	}
	if ps.dialect != DialectOpenAI {
		t.Errorf("ps.dialect = %q, want openai", ps.dialect)
	}
	if ps.baseURL != "https://api.example.com/v1" {
		t.Errorf("ps.baseURL = %q, want the inline base URL", ps.baseURL)
	}
	if len(ps.models) != 2 {
		t.Errorf("ps.models = %v, want the 2 pinned models", ps.models)
	}
	if got := ps.chatURL(); got != "https://api.example.com/v1/chat/completions" {
		t.Errorf("chatURL = %q", got)
	}
}

// TestEnsureUniqueBackendID confirms a custom routing id that collides with a
// built-in provider or a reserved bespoke backend gets a disambiguating suffix,
// while a free id passes through.
func TestEnsureUniqueBackendID(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if got := ensureUniqueBackendID("totally-free-id"); got != "totally-free-id" {
		t.Errorf("free id should pass through, got %q", got)
	}
	// Built-in and reserved bespoke backends must all be disambiguated.
	for _, reserved := range []string{"groq", "kiro", "codex", "qoder", "codebuddy"} {
		if got := ensureUniqueBackendID(reserved); got == reserved {
			t.Errorf("reserved/built-in id %q must be disambiguated, got %q", reserved, got)
		}
	}
}
