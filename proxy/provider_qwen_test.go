package proxy

import (
	"kiro-go/config"
	"path/filepath"
	"testing"
)

// TestQwenProviderResolvesAndDelegates verifies the qwen backend: it resolves to
// the dedicated qwenProvider (not the generic OpenAI registration), reports the
// OAuth flag in the catalog, and — because a qwen account carries a BaseURLOverride
// from its resource_url — resolveProviderSettings derives the right OpenAI chat /
// models URLs through the shared generic settings path.
func TestQwenProviderResolvesAndDelegates(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	// The backend resolves to the qwen provider.
	p := ProviderForBackend("qwen")
	if p == nil || p.Name() != "qwen" {
		t.Fatalf("ProviderForBackend(qwen) = %v, want qwen provider", p)
	}

	// Catalog marks qwen as an OAuth backend (so the UI routes it to device login).
	bp, ok := resolveBuiltinProvider("qwen")
	if !ok {
		t.Fatal("qwen missing from builtin catalog")
	}
	if !bp.OAuth {
		t.Error("qwen catalog entry should have OAuth=true")
	}

	// A qwen account whose resource_url resolved to an intl base derives the right
	// OpenAI-compatible chat + models URLs via the generic settings path.
	acct := &config.Account{
		Backend:         "qwen",
		AccessToken:     "qwen-access-token",
		BaseURLOverride: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1",
	}
	ps, ok := resolveProviderSettings(acct)
	if !ok {
		t.Fatal("resolveProviderSettings(qwen acct) ok=false")
	}
	if ps.dialect != DialectOpenAI {
		t.Errorf("qwen dialect = %q, want openai", ps.dialect)
	}
	if got := ps.chatURL(); got != "https://dashscope-intl.aliyuncs.com/compatible-mode/v1/chat/completions" {
		t.Errorf("qwen chatURL = %q", got)
	}
	if got := ps.modelsURL(); got != "https://dashscope-intl.aliyuncs.com/compatible-mode/v1/models" {
		t.Errorf("qwen modelsURL = %q", got)
	}

	// Routing prefix + model split work: "qwen/qwen-max" -> backend qwen.
	if id, ok := resolveProviderPrefix("qwen"); !ok || id != "qwen" {
		t.Errorf("resolveProviderPrefix(qwen) = (%q,%v)", id, ok)
	}
	if b, m := ParseModelBackend("qwen/qwen-max"); b != "qwen" || m != "qwen-max" {
		t.Errorf("ParseModelBackend(qwen/qwen-max) = (%q,%q)", b, m)
	}
}
