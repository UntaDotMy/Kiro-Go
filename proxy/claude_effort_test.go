package proxy

import (
	"encoding/json"
	"testing"
)

// TestClaudeRequestEffortParsing verifies output_config.effort is decoded from a
// raw Anthropic Messages body and read back by claudeRequestEffort. This is the
// field Claude Code's CLAUDE_CODE_EFFORT_LEVEL maps onto.
func TestClaudeRequestEffortParsing(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"high", `{"model":"claude-opus-4-6","messages":[],"output_config":{"effort":"high"}}`, "high"},
		{"xhigh", `{"model":"claude-opus-4-8","messages":[],"output_config":{"effort":"xhigh"}}`, "xhigh"},
		{"max", `{"model":"claude-opus-4-6","messages":[],"output_config":{"effort":"max"}}`, "max"},
		{"trimmed", `{"model":"m","messages":[],"output_config":{"effort":"  low  "}}`, "low"},
		{"absent output_config", `{"model":"m","messages":[]}`, ""},
		{"empty effort", `{"model":"m","messages":[],"output_config":{"effort":""}}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var req ClaudeRequest
			if err := json.Unmarshal([]byte(c.body), &req); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := claudeRequestEffort(&req); got != c.want {
				t.Fatalf("claudeRequestEffort = %q, want %q", got, c.want)
			}
		})
	}

	// nil receiver / nil config must be safe.
	if got := claudeRequestEffort(nil); got != "" {
		t.Fatalf("claudeRequestEffort(nil) = %q, want empty", got)
	}
}

// TestClaudeEffortNativeForwardingEndToEnd exercises the actual Claude path
// wiring: ClaudeToKiro builds the payload, then applyReasoningEffort attaches
// output_config.effort, clamped to the model's advertised levels — exactly as
// handleClaudeMessages now does. Guards against the previous gap where the
// Claude path dropped effort entirely.
func TestClaudeEffortNativeForwardingEndToEnd(t *testing.T) {
	h := newHandlerWithModelCache([]ModelInfo{
		{ModelId: "claude-opus-4.6", AdditionalModelRequestFieldsSchema: effortSchema("low", "medium", "high", "xhigh", "max")},
		{ModelId: "claude-sonnet-4.6", AdditionalModelRequestFieldsSchema: effortSchema("low", "medium", "high", "max")},
		{ModelId: "claude-haiku-4.5"}, // no effort schema
	})

	cases := []struct {
		name  string
		model string
		body  string
		want  string // expected output_config.effort, "" = omitted
	}{
		{"opus high", "claude-opus-4.6", `{"model":"claude-opus-4.6","messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"high"}}`, "high"},
		{"sonnet xhigh clamps to high", "claude-sonnet-4.6", `{"model":"claude-sonnet-4.6","messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"xhigh"}}`, "high"},
		{"haiku unsupported omitted", "claude-haiku-4.5", `{"model":"claude-haiku-4.5","messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"high"}}`, ""},
		{"no effort omitted", "claude-opus-4.6", `{"model":"claude-opus-4.6","messages":[{"role":"user","content":"hi"}]}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var req ClaudeRequest
			if err := json.Unmarshal([]byte(c.body), &req); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			payload := ClaudeToKiro(&req, false)
			h.applyReasoningEffort(payload, claudeRequestEffort(&req))
			if got := effortField(payload); got != c.want {
				t.Fatalf("output_config.effort = %q, want %q", got, c.want)
			}
			// ResolvedEffort must match the attached level so per-effort analytics
			// on the Claude path are populated (previously always empty).
			if payload.ResolvedEffort != c.want {
				t.Fatalf("ResolvedEffort = %q, want %q", payload.ResolvedEffort, c.want)
			}
		})
	}
}

// TestClaudeEffortFoldsIntoThinking verifies the effort→thinking fold used by
// handleClaudeMessages: "minimal" forces thinking off, a graded level forces it
// on, and unset leaves the base decision untouched.
func TestClaudeEffortFoldsIntoThinking(t *testing.T) {
	cases := []struct {
		name   string
		base   bool
		effort string
		want   bool
	}{
		{"minimal forces off", true, "minimal", false},
		{"high forces on", false, "high", true},
		{"max forces on", false, "max", true},
		{"unset keeps base true", true, "", true},
		{"unset keeps base false", false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveThinkingWithEffort(c.base, c.effort); got != c.want {
				t.Fatalf("resolveThinkingWithEffort(%v,%q) = %v, want %v", c.base, c.effort, got, c.want)
			}
		})
	}
}
