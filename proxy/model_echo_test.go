package proxy

import "testing"

// Story s2: an OpenAI-dialect client must not be told it got gpt-4o when the
// Kiro path actually served Claude. openAIResponseEchoModel reflects the model
// actually served on the Kiro path, and echoes the honest requested id for
// real (non-Kiro) backends.
func TestOpenAIResponseEchoModel(t *testing.T) {
	cases := []struct {
		name      string
		backend   string
		requested string
		served    string
		want      string
	}{
		// Kiro path: gpt-* is remapped to a Claude model upstream; echo the
		// served Claude id, not the requested GPT id.
		{"kiro remaps gpt-4o to claude", "kiro", "gpt-4o", "claude-sonnet-4.5", "claude-sonnet-4-5"},
		{"kiro remaps gpt-4 to claude", "kiro", "gpt-4", "claude-sonnet-4.5", "claude-sonnet-4-5"},
		{"kiro remaps gpt-3.5-turbo to claude", "kiro", "gpt-3.5-turbo", "claude-sonnet-4.5", "claude-sonnet-4-5"},
		// Kiro path with a genuine Claude request: served == requested family,
		// echoed in canonical dash form.
		{"kiro claude passthrough dotted->dashed", "kiro", "claude-opus-4.7", "claude-opus-4.7", "claude-opus-4-7"},
		// Non-Kiro backend: the served model IS the requested upstream id, so the
		// requested id is honest and echoed unchanged.
		{"openai backend keeps gpt-4o", "openai", "gpt-4o", "gpt-4o", "gpt-4o"},
		{"groq backend keeps llama", "groq", "llama-3.3-70b", "llama-3.3-70b", "llama-3-3-70b"},
		// Defensive: empty served on Kiro falls back to the requested id.
		{"kiro empty served falls back", "kiro", "gpt-4o", "", "gpt-4o"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := openAIResponseEchoModel(c.backend, c.requested, c.served)
			if got != c.want {
				t.Errorf("openAIResponseEchoModel(%q,%q,%q) = %q, want %q",
					c.backend, c.requested, c.served, got, c.want)
			}
		})
	}
}

// Guard: a gpt-4o request that the Kiro path served as Claude must NEVER echo a
// gpt id back — that was the trust failure.
func TestOpenAIResponseEchoModelNeverLiesGPT(t *testing.T) {
	got := openAIResponseEchoModel("kiro", "gpt-4o", "claude-sonnet-4.5")
	if got == "gpt-4o" || got == "gpt-4" {
		t.Fatalf("Kiro path served Claude but echoed a GPT id %q — silent model swap", got)
	}
}
