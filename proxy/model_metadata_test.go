package proxy

import "testing"

// TestStaticEffortLevelsForModel pins the dictionary fallback that stops the
// noisy "[Effort] ... advertises no effort support" warning for non-Kiro
// reasoning models (codebuddy-cn/glm-5.2, grok-4, gpt-5-codex, ...). A graded
// effort request against these models must resolve to a clamp-able level set
// instead of warn-dropping. Prefixed client ids are de-prefixed before lookup.
func TestStaticEffortLevelsForModel(t *testing.T) {
	cases := []struct {
		model    string
		wantAny  bool // expect a non-nil, non-empty level set
		wantHas  string
	}{
		// Prefixed non-Kiro ids (the form clients send) resolve on the upstream id.
		{"rcodebuddycn/glm-5.2", false, ""}, // GLM uses binary thinking, NOT graded effort
		{"cb/gpt-5.5", true, "high"},        // OpenAI gpt-5 reasoning_effort
		{"grok/grok-4", true, "high"},        // xAI reasoning_effort (none/low/medium/high)
		{"ds/deepseek-r1", false, ""},        // DeepSeek: always-on CoT, no effort knob
		// Bare upstream ids with a VERIFIED graded effort enum.
		{"gpt-5-codex", true, "high"},        // OpenAI
		{"grok-4", true, "high"},             // xAI
		{"claude-opus-4-7", true, "xhigh"},   // Anthropic output_config.effort
		{"claude-sonnet-4.6", true, "max"},   // Anthropic (no xhigh on sonnet)
		{"gemini-2.5-pro", true, "high"},     // Gemini thinkingLevel (proxy rank)
		// Models with NO graded effort knob -> nil (take the thinking path silently).
		{"gpt-4o", false, ""},
		{"deepseek-chat", false, ""},
		{"deepseek-reasoner", false, ""},
		{"qwen-plus", false, ""},
		{"minimax-m3", false, ""},   // binary thinking, unverified enum
		{"mimo", false, ""},         // binary thinking, unverified enum
		{"kimi-k2.7", false, ""},    // binary thinking, unverified enum
		{"rcodebuddycn/glm-5.2", false, ""}, // GLM = binary thinking
		{"claude-mystery-9", false, ""},
		{"", false, ""},
	}
	for _, c := range cases {
		got := staticEffortLevelsForModel(c.model)
		if c.wantAny && len(got) == 0 {
			t.Errorf("staticEffortLevelsForModel(%q) = nil, want non-empty effort levels", c.model)
			continue
		}
		if !c.wantAny && len(got) > 0 {
			t.Errorf("staticEffortLevelsForModel(%q) = %v, want nil (no graded effort knob)", c.model, got)
			continue
		}
		if c.wantAny && c.wantHas != "" {
			found := false
			for _, l := range got {
				if l == c.wantHas {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("staticEffortLevelsForModel(%q) = %v, want it to contain %q", c.model, got, c.wantHas)
			}
		}
	}
}

// TestStaticContextWindowForModel pins that non-Kiro provider models and the
// CLI provider model ids advertise a real context window (not the flat 200K
// default, and never 0/none). Prefixed client ids are de-prefixed first.
func TestStaticContextWindowForModel(t *testing.T) {
	cases := []struct {
		model   string
		wantMin int // the resolved window must be >= this (family-level floor ok)
		wantEq  int // when >0, the window must equal this exactly
	}{
		// Prefixed non-Kiro ids resolve on the upstream id.
		{"rcodebuddycn/glm-5.2", 1_000_000, 1_000_000}, // GLM-5.2 = 1M (verified docs.z.ai)
		{"cbcn/deepseek-v4-pro", 1_000_000, 1_000_000},
		{"ghcp/gpt-5-codex", 400_000, 400_000},
		// Bare CLI / provider model ids.
		{"gpt-5", 400_000, 400_000},
		{"gpt-4o", 128_000, 128_000},
		{"claude-opus-4-7", 1_000_000, 1_000_000},
		{"claude-sonnet-4.5", 200_000, 200_000},
		{"grok-4", 256_000, 256_000},
		{"deepseek-r1", 131_072, 131_072},
		{"glm-4.6", 200_000, 200_000},
		{"glm-5.1", 200_000, 200_000},
		{"glm-5.2", 1_000_000, 1_000_000},
		{"minimax-m3", 204_800, 204_800},
		{"mimo", 262_144, 262_144},
		{"gemini-2.5-pro", 1_000_000, 0}, // family table floor (1M) — just assert >= 1M
		// Unknown model: dictionary returns 0 (caller falls back to the Claude
		// version parse / 200K default), never a false window.
		{"totally-unknown-model-xyz", 0, 0},
	}
	for _, c := range cases {
		got := staticContextWindowForModel(c.model)
		if c.wantEq > 0 {
			if got != c.wantEq {
				t.Errorf("staticContextWindowForModel(%q) = %d, want exactly %d", c.model, got, c.wantEq)
			}
			continue
		}
		if c.wantMin > 0 {
			if got < c.wantMin {
				t.Errorf("staticContextWindowForModel(%q) = %d, want >= %d", c.model, got, c.wantMin)
			}
		} else {
			// wantMin == 0 means the model is unknown to the dictionary; a 0
			// return is correct (the caller applies its own default).
			if got != 0 {
				t.Errorf("staticContextWindowForModel(%q) = %d, want 0 (unknown to dictionary)", c.model, got)
			}
		}
	}
}

// TestEffortLevelsForModelFallsBackToStaticDictWhenNotCached verifies the live
// cache MISS path consults the static dictionary, so a non-Kiro reasoning model
// (absent from the Kiro cache) still resolves effort levels instead of warn-
// dropping. The live cache HIT path with an explicit empty schema must still
// return nil (the upstream authoritatively advertised no effort).
func TestEffortLevelsForModelFallsBackToStaticDictWhenNotCached(t *testing.T) {
	h := &Handler{}
	h.cachedModels = []ModelInfo{
		{ModelId: "claude-haiku-4.5"}, // in cache, no schema -> authoritative "no effort"
	}
	// Cached model with empty schema -> nil (NOT the static dict's haiku levels).
	if got := h.effortLevelsForModel("claude-haiku-4.5"); got != nil {
		t.Errorf("cached model with empty schema must return nil, got %v", got)
	}
	// Non-cached non-Kiro reasoning model -> static dictionary fallback.
	if got := h.effortLevelsForModel("grok-4"); len(got) == 0 {
		t.Errorf("non-cached grok-4 must fall back to static dict effort levels, got nil")
	}
	// Non-cached GLM model -> nil (binary thinking, no graded effort).
	if got := h.effortLevelsForModel("rcodebuddycn/glm-5.2"); got != nil {
		t.Errorf("non-cached glm-5.2 has binary thinking, want nil effort levels, got %v", got)
	}
	// Non-cached model with no graded knob -> nil (no false effort).
	if got := h.effortLevelsForModel("gpt-4o"); got != nil {
		t.Errorf("non-cached gpt-4o has no effort knob, want nil, got %v", got)
	}
}
