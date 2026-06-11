package proxy

import (
	"kiro-go/config"
	"testing"
)

// TestDashScopeModelStudioEndpoints verifies the Alibaba Model Studio
// (compatible-mode) catalog entries resolve to the generic OpenAI provider and
// derive the working chat + /models URLs from their compatible-mode base. These
// are distinct from the coding-assistant "alicode" hosts (which have no /models
// route) — this is the fix for "Alibaba Intl can't fetch models".
func TestDashScopeModelStudioEndpoints(t *testing.T) {
	cases := []struct {
		backend   string
		wantBase  string
		wantChat  string
		wantModel string
	}{
		{
			"dashscope-intl",
			"https://dashscope-intl.aliyuncs.com/compatible-mode/v1",
			"https://dashscope-intl.aliyuncs.com/compatible-mode/v1/chat/completions",
			"https://dashscope-intl.aliyuncs.com/compatible-mode/v1/models",
		},
		{
			"dashscope",
			"https://dashscope.aliyuncs.com/compatible-mode/v1",
			"https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions",
			"https://dashscope.aliyuncs.com/compatible-mode/v1/models",
		},
		{
			"dashscope-us",
			"https://dashscope-us.aliyuncs.com/compatible-mode/v1",
			"https://dashscope-us.aliyuncs.com/compatible-mode/v1/chat/completions",
			"https://dashscope-us.aliyuncs.com/compatible-mode/v1/models",
		},
	}
	for _, c := range cases {
		t.Run(c.backend, func(t *testing.T) {
			// Resolves to the OpenAI generic provider (the resolver-fix path).
			if p := ProviderForBackend(c.backend); p == nil || p.Name() != "generic:openai" {
				t.Fatalf("%s resolved to %v, want generic:openai", c.backend, p)
			}
			acct := &config.Account{Backend: c.backend, APIKey: "sk-test"}
			ps, ok := resolveProviderSettings(acct)
			if !ok {
				t.Fatalf("resolveProviderSettings(%s) ok=false", c.backend)
			}
			if got := ps.apiBase(); got != c.wantBase {
				t.Errorf("%s apiBase = %q, want %q", c.backend, got, c.wantBase)
			}
			if got := ps.chatURL(); got != c.wantChat {
				t.Errorf("%s chatURL = %q, want %q", c.backend, got, c.wantChat)
			}
			if got := ps.modelsURL(); got != c.wantModel {
				t.Errorf("%s modelsURL = %q, want %q", c.backend, got, c.wantModel)
			}
		})
	}

	// The routing prefix resolves too, so "dashscope-intl/qwen-max" routes here.
	if id, ok := resolveProviderPrefix("dashscope-intl"); !ok || id != "dashscope-intl" {
		t.Errorf("resolveProviderPrefix(dashscope-intl) = (%q,%v), want (dashscope-intl,true)", id, ok)
	}
	if b, m := ParseModelBackend("dashscope-intl/qwen-max"); b != "dashscope-intl" || m != "qwen-max" {
		t.Errorf("ParseModelBackend = (%q,%q), want (dashscope-intl,qwen-max)", b, m)
	}
}
