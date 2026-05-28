package stats

import "testing"

// TestCanonicalModelID pins the normalization rules: case, dotted vs
// dashed version, thinking-suffix collapse, and idempotence.
func TestCanonicalModelID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"  ", ""},
		{"claude-opus-4-7", "claude-opus-4-7"},
		{"claude-opus-4.7", "claude-opus-4-7"},
		{"Claude-Opus-4.7", "claude-opus-4-7"},
		{"claude-opus-4.7-thinking", "claude-opus-4-7"},
		{"claude-sonnet-4-5-think", "claude-sonnet-4-5"},
		{"claude-haiku-4-5-reasoning", "claude-haiku-4-5"},
		{"  claude-sonnet-4.5-thinking  ", "claude-sonnet-4-5"},
	}
	for _, c := range cases {
		if got := CanonicalModelID(c.in); got != c.want {
			t.Errorf("CanonicalModelID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCanonicalModelIDIdempotence proves f(f(x)) == f(x) for diverse inputs
// without hardcoding `want` on both sides. A bug like "always strip last
// char" would not survive this test (the two calls would produce different
// strings on the second pass), unlike a tautological f(x) == f(f(x)) where
// `want` is itself f(...).
func TestCanonicalModelIDIdempotence(t *testing.T) {
	inputs := []string{
		"claude-opus-4.7",
		"Claude-Opus-4-7-thinking",
		"  CLAUDE-SONNET-4.5-THINK  ",
		"claude-haiku-4-5-reasoning",
		"gpt-4o", // non-Claude, no thinking suffix — should pass through stable
		"random-model-id",
	}
	for _, in := range inputs {
		once := CanonicalModelID(in)
		twice := CanonicalModelID(once)
		if once != twice {
			t.Errorf("CanonicalModelID not idempotent for %q: f(x)=%q, f(f(x))=%q", in, once, twice)
		}
	}
}
