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
		// Provider-prefixed ids resolve on the UPSTREAM model, not the literal
		// prefixed string. GLM windows verified from docs.z.ai/guides/llm spec
		// tables: GLM-5.2 = 1M ("truly usable 1M-token context"), GLM-4.6/4.7/
		// 5/5.1/5-Turbo = 200K, GLM-4.5 = 128K. A 128K cbcn/glm-4.5 must read
		// 128K — not the flat Kiro default the un-stripped id would fall through to.
		{"cbcn/glm-5.2", 1_000_000},
		{"cbcn/glm-4.7", 200_000},
		{"cbcn/glm-4.6", 200_000},
		{"cbcn/glm-4.5", 131_072},
		{"glm-5.2", 1_000_000},
		{"glm-5.0", 200_000},
		// Other cbcn families resolve to their documented vendor window, not the
		// flat Kiro default the un-stripped prefixed id would have hit.
		{"cbcn/kimi-k2.7", 262_144},
		{"cbcn/kimi-k2.5", 262_144},
		{"cbcn/minimax-m2.7", 204_800},
		{"cbcn/minimax-m3", 204_800},
		{"cbcn/deepseek-v4-pro", 1_000_000},
		{"cbcn/deepseek-v4-flash", 1_000_000},
		{"cbcn/deepseek-v3-1", 131_072},
		{"cbcn/deepseek-r1", 131_072},
		{"cbcn/hunyuan-chat", 131_072},
		{"cbcn/hy3-preview", 131_072},
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

// TestContextWindowForModelDefaultsTo1M is the regression guard for the reported
// TestContextWindowForClaudeClient is the regression guard for the FIELD-OBSERVED
// bug: with a plain model id (claude-opus-4-8) Claude Code meters against 200K and
// only switches to 1M for the [1M] model variant (which sends the context-1m beta
// header). When the proxy back-converts Kiro's contextUsagePercentage it MUST use
// the same window the client meters against, or the gauge desyncs — a 1M-scaled
// count read against a 200K assumption overshoots ~5x and pegs at 100% with no
// auto-compaction. So contextWindowForClaudeClient caps at 200K unless allow1M.
//
// contextWindowForModel still returns the model's true window (1M for 4.6+); the
// GATE is what keys the reported window to the client's beta opt-in.
func TestContextWindowForClaudeClient(t *testing.T) {
	h := newHandlerWithModelCache(nil)

	// Plain (no beta): a 1M-capable model is reported as 200K to match the client.
	if got := h.contextWindowForClaudeClient("claude-opus-4-8", false); got != defaultContextWindow {
		t.Errorf("opus-4-8 without 1M beta = %d, want %d (capped to client meter)", got, defaultContextWindow)
	}
	// Beta opt-in ([1M] variant): the true 1M window is honored.
	if got := h.contextWindowForClaudeClient("claude-opus-4-8", true); got != 1_000_000 {
		t.Errorf("opus-4-8 with 1M beta = %d, want 1000000", got)
	}
	// Sub-1M model: 200K either way (no inflation when the client opts in).
	if got := h.contextWindowForClaudeClient("claude-sonnet-4.5", true); got != defaultContextWindow {
		t.Errorf("sonnet-4.5 with beta = %d, want %d", got, defaultContextWindow)
	}

	// The underlying resolver is unchanged: it still reports the model's true
	// window; only the client-facing gate caps it.
	if got := h.contextWindowForModel("claude-opus-4-8"); got != 1_000_000 {
		t.Errorf("contextWindowForModel(opus-4-8) = %d, want 1000000 (true window, ungated)", got)
	}

	// Live tokenLimits 1M is also gated to 200K when the client did not opt in.
	hLive := newHandlerWithModelCache([]ModelInfo{
		{ModelId: "claude-opus-4.7", TokenLimits: tokenLimits(1_000_000, 32000)},
	})
	if got := hLive.contextWindowForClaudeClient("claude-opus-4.7", false); got != defaultContextWindow {
		t.Errorf("live-1M without beta = %d, want %d (capped)", got, defaultContextWindow)
	}
	if got := hLive.contextWindowForClaudeClient("claude-opus-4.7", true); got != 1_000_000 {
		t.Errorf("live-1M with beta = %d, want 1000000", got)
	}
}
