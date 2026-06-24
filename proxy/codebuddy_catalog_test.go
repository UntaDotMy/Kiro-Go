package proxy

import (
	"path/filepath"
	"testing"

	"kiro-go/config"
)

// TestCodeBuddyCatalogEndpoints locks in the CodeBuddy routing fix.
//
// The original catalog pointed CodeBuddy at /v1/chat/completions, which 404s on
// the real gateway ("Route Not Found"); the actual inference endpoint is
// /v2/chat/completions (probing returns 401 = exists+needs-auth, while
// /v2/messages 404s, confirming OpenAI dialect). That wrong base URL was the root
// cause of BOTH broken inference and the empty model list. This test guards
// against a regression to /v1 and confirms the derived chat URL.
func TestCodeBuddyCatalogEndpoints(t *testing.T) {
	cases := []struct {
		backend  string
		wantChat string
	}{
		{"codebuddy", "https://copilot.tencent.com/v2/chat/completions"},
		{"codebuddy-ai", "https://www.codebuddy.ai/v2/chat/completions"},
	}
	for _, c := range cases {
		t.Run(c.backend, func(t *testing.T) {
			// CodeBuddy is served by its own provider (token refresh) but delegates
			// inference to the generic OpenAI provider; resolveProviderSettings must
			// yield the OpenAI dialect and the /v2 chat URL.
			acct := &config.Account{Backend: c.backend, AccessToken: "tok"}
			ps, ok := resolveProviderSettings(acct)
			if !ok {
				t.Fatalf("resolveProviderSettings(%s) ok=false", c.backend)
			}
			if ps.dialect != DialectOpenAI {
				t.Errorf("%s dialect = %q, want openai", c.backend, ps.dialect)
			}
			if got := ps.chatURL(); got != c.wantChat {
				t.Errorf("%s chatURL = %q, want %q", c.backend, got, c.wantChat)
			}
			// The advisory model list must be present (CodeBuddy has no /models
			// route, so this is what makes the dashboard show a real model count).
			if len(ps.models) == 0 {
				t.Errorf("%s has no advisory models; dashboard would show 0", c.backend)
			}
		})
	}
}

// TestCodeBuddyModelsAdvisory verifies the shipped advisory list contains the
// real gateway ids (smoke-verified upstream) including the auto-routed default,
// so a client can address e.g. "codebuddy/claude-sonnet-4.6".
func TestCodeBuddyModelsAdvisory(t *testing.T) {
	want := map[string]bool{
		"default-model":     false,
		"claude-sonnet-4.6": false,
		"gpt-5.5":           false,
		"gemini-2.5-pro":    false,
	}
	for _, id := range codeBuddyModels {
		if _, ok := want[id]; ok {
			want[id] = true
		}
	}
	for id, found := range want {
		if !found {
			t.Errorf("codeBuddyModels missing expected id %q", id)
		}
	}
}

// TestBackendShipsStaticCatalog pins that no-/models backends are flagged as
// static-only so the background model refresh skips re-fetching their catalog
// (a live GET /models would always 404 and just re-log the advisory fallback).
// Only quota is refreshed for these backends.
func TestBackendShipsStaticCatalog(t *testing.T) {
	for _, b := range []string{"codebuddy", "codebuddy-ai", "codebuddy-cn", "perplexity", "iflow", "alicode", "alicode-intl"} {
		if !backendShipsStaticCatalog(b) {
			t.Errorf("backend %q should be flagged static-only (it has no working /models endpoint)", b)
		}
	}
	// Backends WITH a working /models endpoint must NOT be flagged.
	for _, b := range []string{"openai", "openrouter", "groq", "deepseek", "anthropic", "gemini", "dashscope"} {
		if backendShipsStaticCatalog(b) {
			t.Errorf("backend %q should NOT be flagged static-only (it has a working /models endpoint)", b)
		}
	}
}

// TestBackendShipsStaticCatalogCustomAccount pins the self-contained custom
// account case: a user-added account whose backend id is its own routing
// prefix and whose pinned model list lives INLINE on the account as CustomModels
// (no shared ProviderConfig, no builtin catalog row). This is the shape of a
// reseller endpoint like rcodebuddycn -> dpc-tcb.chicross.cn that has no working
// /models endpoint; the background refresh must skip the live fetch that would
// always 404 and instead seed directly from CustomModels.
func TestBackendShipsStaticCatalogCustomAccount(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	const backend = "rcodebuddycn"
	acct := config.Account{
		ID:              "acc-rcb",
		Backend:         backend,
		Nickname:        backend,
		CustomDialect:   "openai",
		BaseURLOverride: "https://dpc-tcb.chicross.cn/api/v2",
		CustomModels:    []string{"glm-5.2", "deepseek-v4-pro", "deepseek-v4-flash", "minimax-m3", "kimi-k2.7"},
		APIKey:          "ck-test",
		Enabled:         true,
	}
	if err := config.AddAccount(acct); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if !backendShipsStaticCatalog(backend) {
		t.Errorf("custom account backend %q with non-empty CustomModels should be flagged static-only "+
			"(it has no working /models endpoint; the live fetch 404s every refresh tick)", backend)
	}
	// Sanity: the helper sees the 5 pinned models.
	sib, ok := config.GetCustomAccountByBackend(backend)
	if !ok || len(sib.CustomModels) != 5 {
		t.Fatalf("GetCustomAccountByBackend(%q) = (%+v, %v), want 5 CustomModels", backend, sib, ok)
	}
}

// TestBackendShipsStaticCatalogCustomAccountNoModels pins the negative: a
// custom account with NO CustomModels is NOT flagged static-only, because a
// live /models fetch is the only way to populate its catalog.
func TestBackendShipsStaticCatalogCustomAccountNoModels(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	const backend = "livegw"
	acct := config.Account{
		ID:              "acc-live",
		Backend:         backend,
		CustomDialect:   "openai",
		BaseURLOverride: "https://api.example.com/v1",
		CustomModels:    nil, // no pinned list -> rely on live /models
		APIKey:          "sk-test",
		Enabled:         true,
	}
	if err := config.AddAccount(acct); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if backendShipsStaticCatalog(backend) {
		t.Errorf("custom account backend %q with no CustomModels should NOT be flagged static-only "+
			"(it needs the live /models fetch to populate its catalog)", backend)
	}
}

// TestCodeBuddyRoutingPrefix confirms both the id and alias route to the backend
// so "cb/..." and "codebuddy/..." both reach CodeBuddy.
func TestCodeBuddyRoutingPrefix(t *testing.T) {
	for _, p := range []struct{ prefix, want string }{
		{"codebuddy", "codebuddy"},
		{"cb", "codebuddy"},
		{"codebuddy-ai", "codebuddy-ai"},
		{"cbai", "codebuddy-ai"},
	} {
		if id, ok := resolveProviderPrefix(p.prefix); !ok || id != p.want {
			t.Errorf("resolveProviderPrefix(%q) = (%q,%v), want (%q,true)", p.prefix, id, ok, p.want)
		}
	}
}
