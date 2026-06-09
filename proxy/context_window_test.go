package proxy

import "testing"

// TestGetContextWindowSize verifies the FALLBACK context window is always the
// safe 200K default, regardless of the model id's version number.
//
// This drives the input-token count clients use to decide when to compact. The
// previous implementation version-parsed the id and returned 1_000_000 for any
// Claude >= 4.6 — a FABRICATED window not backed by Kiro. That made /v1/models
// advertise a 1M window for sonnet-4.6 / opus-4.8, which pushed a context-aware
// client's (Claude Code) auto-compaction threshold to ~920K so it never
// compacted in a normal session while the usage gauge sailed past 100%. The
// authoritative non-default window now comes ONLY from Kiro's
// tokenLimits.maxInputTokens (see contextWindowFromTokenLimits); this helper is
// the safe fallback and never guesses 1M from an id.
func TestGetContextWindowSize(t *testing.T) {
	cases := []string{
		"claude-opus-4.6",
		"claude-opus-4-6",
		"claude-opus-4.8",
		"claude-opus-4-8",
		"claude-sonnet-4.6",
		"claude-sonnet-4-6",
		"claude-opus-5.0",
		"claude-opus-4.5",
		"claude-sonnet-4.5",
		"claude-haiku-4-5",
		"claude-3-5-sonnet",
		"unknown-model",
		"",
	}
	for _, model := range cases {
		if got := getContextWindowSize(model); got != defaultContextWindow {
			t.Errorf("getContextWindowSize(%q) = %d, want %d (fallback must never guess a larger window from the id)", model, got, defaultContextWindow)
		}
	}
}

// tokenLimits builds the anonymous struct shape ModelInfo.TokenLimits uses, so
// the tests can exercise contextWindowFromTokenLimits with real values.
func tokenLimits(maxIn, maxOut int) *struct {
	MaxInputTokens  int `json:"maxInputTokens"`
	MaxOutputTokens int `json:"maxOutputTokens"`
} {
	return &struct {
		MaxInputTokens  int `json:"maxInputTokens"`
		MaxOutputTokens int `json:"maxOutputTokens"`
	}{MaxInputTokens: maxIn, MaxOutputTokens: maxOut}
}

// TestContextWindowFromTokenLimits verifies the ONLY trusted source of a
// non-default window: Kiro's ListAvailableModels tokenLimits. A nil or
// non-positive maxInputTokens returns 0 so the caller uses the safe default; a
// real value (200K or the beta 1M) is passed through verbatim.
func TestContextWindowFromTokenLimits(t *testing.T) {
	cases := []struct {
		name string
		tl   *struct {
			MaxInputTokens  int `json:"maxInputTokens"`
			MaxOutputTokens int `json:"maxOutputTokens"`
		}
		want int
	}{
		{"nil → 0 (use default)", nil, 0},
		{"zero maxInput → 0 (use default)", tokenLimits(0, 8192), 0},
		{"negative maxInput → 0 (use default)", tokenLimits(-1, 8192), 0},
		{"200K window passthrough", tokenLimits(200_000, 8192), 200_000},
		{"1M beta window passthrough", tokenLimits(1_000_000, 32000), 1_000_000},
	}
	for _, c := range cases {
		if got := contextWindowFromTokenLimits(c.tl); got != c.want {
			t.Errorf("%s: contextWindowFromTokenLimits = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestClampPercent pins the [0,100] clamp applied to Kiro's contextUsage
// percentage before it is multiplied by the window. An out-of-range value must
// never synthesize an input-token count outside [0, window] (which would push a
// client's context gauge past 100%).
func TestClampPercent(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{-5, 0},
		{0, 0},
		{42.5, 42.5},
		{100, 100},
		{100.0001, 100},
		{150, 100},
	}
	for _, c := range cases {
		if got := clampPercent(c.in); got != c.want {
			t.Errorf("clampPercent(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
