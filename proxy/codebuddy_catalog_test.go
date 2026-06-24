package proxy

import (
	"kiro-go/config"
	"testing"
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
