package proxy

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
)

func enableFiltersForNeutralize(t *testing.T) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config init: %v", err)
	}
	if err := config.UpdatePromptFilterConfig(true, false, false, nil); err != nil {
		t.Fatalf("enable filter: %v", err)
	}
}

const neutralizeHarnessSample = `You are Claude Code, Anthropic's official CLI for Claude.

# Tone and style
Be concise.

# Doing tasks
Do software engineering tasks.

x-anthropic-billing-header: cc_version=2.1.38; cch=00000;`

func TestNeutralizeHarnessKeepsContractPerBackend(t *testing.T) {
	enableFiltersForNeutralize(t)

	cases := []struct {
		backend   string
		wantBrand string
	}{
		{"kiro", "Kiro"},
		{"codebuddy", "CodeBuddy"},
		{"qoder", "Qoder"},
		{"groq", "the assistant"},
	}
	for _, c := range cases {
		out := neutralizeHarness(neutralizeHarnessSample, c.backend)
		if out == "" {
			t.Fatalf("%s: harness dropped entirely", c.backend)
		}
		if !strings.Contains(out, "Do software engineering tasks.") {
			t.Errorf("%s: harness instructions lost:\n%s", c.backend, out)
		}
		if strings.Contains(strings.ToLower(out), "claude") || strings.Contains(strings.ToLower(out), "anthropic") {
			t.Errorf("%s: brand token survived:\n%s", c.backend, out)
		}
		if strings.Contains(out, "x-anthropic-billing-header") {
			t.Errorf("%s: billing header survived (Bedrock 400 risk):\n%s", c.backend, out)
		}
		if !strings.Contains(out, c.wantBrand) {
			t.Errorf("%s: expected rebrand to %q:\n%s", c.backend, c.wantBrand, out)
		}
	}
}

func TestNeutralizeProviderBodyNeutralizesHarnessButKeepsContext(t *testing.T) {
	enableFiltersForNeutralize(t)

	in := map[string]interface{}{
		"model": "claude-sonnet-4.6", // must NOT be rewritten
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "system",
				"content": neutralizeHarnessSample,
			},
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "Read CLAUDE.md in /home/me/claude_core then ask Claude about Anthropic."},
				},
			},
		},
	}
	raw, _ := json.Marshal(in)
	out := neutralizeProviderBody(raw, "codebuddy")

	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if got["model"] != "claude-sonnet-4.6" {
		t.Errorf("model id altered: got %q", got["model"])
	}

	msgs := got["messages"].([]interface{})
	sys := msgs[0].(map[string]interface{})["content"].(string)
	if !strings.Contains(sys, "Do software engineering tasks.") {
		t.Errorf("CodeBuddy harness instructions lost:\n%s", sys)
	}
	// The harness IDENTITY phrases are gone from the system prompt.
	if strings.Contains(sys, "Claude Code") || strings.Contains(sys, "Anthropic's official CLI") {
		t.Errorf("harness identity phrase survived in system prompt:\n%s", sys)
	}

	// CONTEXT must be preserved verbatim: a working dir like "claude_core", a
	// filename like "CLAUDE.md", and bare brand words in the user's own message
	// are NOT fingerprints — rewriting them is the reported bug (the model could
	// no longer find the real directory). Blanket brand substitution is forbidden.
	userText := msgs[1].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})["text"].(string)
	want := "Read CLAUDE.md in /home/me/claude_core then ask Claude about Anthropic."
	if userText != want {
		t.Errorf("user context was corrupted by brand rewrite:\n got:  %q\n want: %q", userText, want)
	}
}

func TestNeutralizeProviderBodyNonModeratedKeepsUserContent(t *testing.T) {
	enableFiltersForNeutralize(t)

	in := map[string]interface{}{
		"model": "llama-3.3-70b",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": neutralizeHarnessSample},
			map[string]interface{}{"role": "user", "content": "Tell me about Claude models."},
		},
	}
	raw, _ := json.Marshal(in)
	out := neutralizeProviderBody(raw, "groq")

	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	msgs := got["messages"].([]interface{})
	user := msgs[1].(map[string]interface{})["content"].(string)
	// Non-moderated providers must not rewrite the user's own content.
	if user != "Tell me about Claude models." {
		t.Errorf("user content was altered on a non-moderated provider: %q", user)
	}
	// But the harness system prompt is still neutralized.
	sys := msgs[0].(map[string]interface{})["content"].(string)
	if strings.Contains(strings.ToLower(sys), "anthropic") {
		t.Errorf("harness brand token survived: %s", sys)
	}
}

func TestNeutralizeProviderBodyInvalidJSONFailsOpen(t *testing.T) {
	raw := []byte("not json {{{")
	if got := neutralizeProviderBody(raw, "codebuddy"); string(got) != string(raw) {
		t.Errorf("invalid JSON should pass through unchanged: got %q", string(got))
	}
}
