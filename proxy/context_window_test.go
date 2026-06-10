package proxy

import "testing"

// TestGetContextWindowSize verifies the FALLBACK context window resolution used
// when Kiro reports no authoritative tokenLimits for a model.
//
// This drives both the advertised /v1/models window and the pct×window input-
// token back-conversion clients use to decide when to compact. The fallback
// version-PARSES the id: Claude Opus/Sonnet/Haiku >= 4.6 (and any major >= 5)
// have a 1M window; earlier versions are 200K. Advertising the TRUE window keeps
// a context-aware client's (Claude Code) usage numerator and window denominator
// consistent so its ~95%-of-window auto-compaction threshold fires at the right
// point. The authoritative non-default window still comes from Kiro's
// tokenLimits.maxInputTokens first (see contextWindowFromTokenLimits); this
// helper is only reached when the upstream reports nothing.
func TestGetContextWindowSize(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		// >= 4.6 (and major >= 5): 1M window, dotted and dashed forms.
		{"claude-opus-4.6", 1_000_000},
		{"claude-opus-4-6", 1_000_000},
		{"claude-opus-4.8", 1_000_000},
		{"claude-opus-4-8", 1_000_000},
		{"claude-sonnet-4.6", 1_000_000},
		{"claude-sonnet-4-6", 1_000_000},
		{"claude-opus-5.0", 1_000_000},
		// < 4.6: 200K window.
		{"claude-opus-4.5", defaultContextWindow},
		{"claude-sonnet-4.5", defaultContextWindow},
		{"claude-haiku-4-5", defaultContextWindow},
		{"claude-3-5-sonnet", defaultContextWindow},
		// Unparseable / unknown ids fall back to the safe default.
		{"unknown-model", defaultContextWindow},
		{"", defaultContextWindow},
	}
	for _, c := range cases {
		if got := getContextWindowSize(c.model); got != c.want {
			t.Errorf("getContextWindowSize(%q) = %d, want %d", c.model, got, c.want)
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
