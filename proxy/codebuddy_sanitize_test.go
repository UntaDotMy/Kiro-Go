package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSanitizeCodeBuddyBody_RewritesBrandTokens verifies that the system prompt is
// fully replaced with the neutral one (the cause of CodeBuddy's sensitive-content
// rejection) and residual brand tokens in other messages are rewritten, while the
// top-level model id is preserved verbatim.
func TestSanitizeCodeBuddyBody_RewritesBrandTokens(t *testing.T) {
	in := map[string]interface{}{
		"model": "claude-sonnet-4.6", // must NOT be rewritten
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "system",
				"content": "You are Claude Code, Anthropic's official CLI for Claude. Long brand-dense prompt with many triggers.",
			},
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "Ask Claude about Anthropic and claude code."},
				},
			},
		},
	}
	raw, _ := json.Marshal(in)
	out := sanitizeCodeBuddyBody(raw)

	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	// Model id preserved.
	if got["model"] != "claude-sonnet-4.6" {
		t.Errorf("model id was altered: got %q, want %q", got["model"], "claude-sonnet-4.6")
	}

	// System prompt fully replaced with the neutral one — no remnants of the
	// original brand-dense prompt should survive.
	msgs := got["messages"].([]interface{})
	sys := msgs[0].(map[string]interface{})
	if sys["content"] != codeBuddyNeutralSystemPrompt {
		t.Errorf("system content not replaced: got %q, want %q", sys["content"], codeBuddyNeutralSystemPrompt)
	}

	// No brand tokens survive in the messages (the moderated content). The model
	// id is intentionally preserved even though it contains "claude", so scan the
	// messages array rather than the whole body.
	msgBytes, _ := json.Marshal(got["messages"])
	s := string(msgBytes)
	for _, banned := range []string{"Claude", "Anthropic", "claude", "anthropic"} {
		if strings.Contains(s, banned) {
			t.Errorf("brand token %q still present in sanitized messages: %s", banned, s)
		}
	}
}

// TestSanitizeCodeBuddyBody_InvalidJSONFailsOpen verifies the sanitizer returns
// the body unchanged when it cannot parse it, rather than corrupting the request.
func TestSanitizeCodeBuddyBody_InvalidJSONFailsOpen(t *testing.T) {
	raw := []byte("not json {{{")
	if got := sanitizeCodeBuddyBody(raw); string(got) != string(raw) {
		t.Errorf("invalid JSON should pass through unchanged: got %q", string(got))
	}
}

func TestIsCodeBuddyBackend(t *testing.T) {
	cases := map[string]bool{
		"codebuddy":    true,
		"codebuddy-ai": true,
		"CodeBuddy":    true,
		"openai":       false,
		"kiro":         false,
		"":             false,
	}
	for id, want := range cases {
		if got := isCodeBuddyBackend(id); got != want {
			t.Errorf("isCodeBuddyBackend(%q) = %v, want %v", id, got, want)
		}
	}
}
