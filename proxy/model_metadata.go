package proxy

import "strings"

// ============================================================================
// Static model metadata dictionary.
//
// Kiro-Go learns a model's context window and reasoning-effort support
// live-first: Kiro's ListAvailableModels.tokenLimits (context window) and
// AdditionalModelRequestFieldsSchema (effort enum) are the authoritative
// sources, consulted by contextWindowForModel / effortLevelsForModel. But the
// NON-Kiro providers (codebuddy-cn, grok, deepseek, zai/glm, xiaomi-mimo,
// minimax, the OpenAI-compatible hosts) and the CLI providers (codex/gpt-5,
// claude-code, gemini-cli, pi, opencode-go) never appear in the Kiro cache, so
// for them the live lookup always misses and we fall back to:
//
//   - context window: familyContextWindowFor (context_window.go) — already
//     covers gemini/qwen/glm/kimi/minimax/deepseek/hunyuan.
//   - effort levels: NOTHING — every non-Kiro model reported "no effort
//     support" and logged the noisy [Effort] warning, even for models that DO
//     accept a reasoning_effort / thinking knob on their own backend.
//
// This file fills the effort gap and extends the context-window table so the
// /v1/models advert and per-request usage reporting show real numbers (instead
// of "context length: none") and so a graded effort request against a non-Kiro
// reasoning model is satisfied from the dictionary instead of warn-dropped.
//
// CONVENTIONS
//   - Keys are lowercased model-id PREFIXES; longest-prefix wins (same scheme
//     as familyContextWindow). A prefixed client id ("cbcn/glm-5.2",
//     "or/anthropic/claude-...") is reduced to its UPSTREAM id via
//     ParseModelBackend before lookup, so the table keys upstream ids only.
//   - Effort levels are the backend's NATIVE enum, ordered low->high. We only
//     list models that genuinely accept a graded effort/reasoning knob; models
//     with a binary thinking on/off (or none) are omitted (they take the
//     thinking path, no warning).
//   - Context windows are the documented MAX INPUT window. Conservative when a
//     family ships several (see context_window.go rationale).
//
// Sources are noted inline. Numbers reflect public docs / model cards as of
// 2026-06; a live tokenLimits value always overrides these.
// ============================================================================

// modelEffortDict maps a lowercased upstream model-id prefix to the ordered
// reasoning-effort levels that model's backend accepts natively. Used as a
// fallback when the live Kiro model cache has no schema for the model (i.e. for
// every non-Kiro provider). nil/absent => the model has no graded effort knob
// (binary thinking on/off, or none) and the request takes the thinking path
// silently.
//
// IMPORTANT: this dictionary describes the EFFORT KNOB THE UPSTREAM HONORS, not
// the Kiro output_config.effort field. For non-Kiro providers the resolved
// effort is currently mapped to the thinking on/off lever (see
// resolveThinkingWithEffort); listing a model here lets the proxy (a) stop
// emitting the false "no effort support" warning, and (b) know the model is a
// reasoning model so a client's graded request is at least engaged as thinking
// rather than warn-dropped. The levels are kept for forward-compatibility: a
// future per-provider native-effort forwarder will clamp against them exactly
// like the Kiro path does.
var modelEffortDict = map[string][]string{
	// ---- OpenAI (gpt-5 / o-series reasoning models) ----
	// Source: platform.openai.com/docs/guides/reasoning — reasoning_effort
	// supported values are model-dependent: none, minimal, low, medium, high,
	// xhigh. none/minimal map to our thinking-off path (handled separately), so
	// the graded set listed here is {low, medium, high}; xhigh is omitted as a
	// conservative common floor (a request for xhigh clamps down to high rather
	// than over-claiming a tier a specific model may not expose).
	"gpt-5":         {"low", "medium", "high"},
	"gpt-5-codex":   {"low", "medium", "high"},
	"gpt-5.1-codex": {"low", "medium", "high"},
	"gpt-5.3-codex": {"low", "medium", "high"},
	"gpt-5.4":       {"low", "medium", "high"},
	"gpt-5.5":       {"low", "medium", "high"},
	"o3":            {"low", "medium", "high"},
	"o3-mini":       {"low", "medium", "high"},
	"o4-mini":       {"low", "medium", "high"},
	"codex-mini-latest": {"low", "medium", "high"},

	// ---- DeepSeek (reasoning models) ----
	// NOT listed: DeepSeek-R1 / deepseek-reasoner use an always-on CoT exposed
	// via `reasoning_content` — there is NO reasoning_effort / graded knob on the
	// public API (verified api-docs.deepseek.com/guides/reasoning_model). A graded
	// request takes the thinking on/off path silently; no invented levels.

	// ---- xAI Grok (reasoning models) ----
	// Source: docs.x.ai/docs/guides/reasoning — reasoning_effort enum is
	// {none, low, medium, high} (none = thinking off, handled separately). The
	// graded set is {low, medium, high}. grok-4 / grok-4-fast / grok-4-heavy.
	"grok-4":         {"low", "medium", "high"},
	"grok-4-fast":    {"low", "medium", "high"},
	"grok-4-heavy":   {"low", "medium", "high"},

	// ---- GLM / Zhipu (zai) ----
	// NOT listed: GLM uses a BINARY thinking toggle (thinking:{type:enabled|
	// disabled}), not a graded effort enum (verified docs.z.ai/guides/capabilities/
	// thinking-mode). A graded request therefore takes the thinking on/off path
	// silently — no false "no effort support" warning, no invented levels.

	// ---- MiniMax / Kimi / Xiaomi MiMo ----
	// NOT listed (unverified): these ship binary thinking modes (MiniMax M-series
	// thinking, Kimi k2-thinking, MiMo reasoning) rather than a confirmed graded
	// reasoning_effort enum on their public OpenAI-compatible APIs. A graded
	// request takes the thinking on/off path silently — no false warning, no
	// invented levels. Add a verified entry here only when the upstream's effort
	// enum is confirmed from its docs.

	// ---- Gemini (thinking models) ----
	// Gemini 2.5/3 expose thinkingConfig.thinkingLevel (a Gemini-specific enum:
	// minimal/dynamic/...), NOT OpenAI's reasoning_effort. We do not currently
	// forward thinkingLevel, but listing these as reasoning models means a graded
	// request engages the thinking path WITHOUT the false "no effort support"
	// warning. The {low,medium,high} levels are a proxy rank so resolveModelEffort
	// can clamp; they are not sent to Gemini as-is.
	"gemini-2.5-pro":   {"low", "medium", "high"},
	"gemini-2.5-flash": {"low", "medium", "high"},
	"gemini-2.0-flash-thinking": {"low", "medium", "high"},
	"gemini-exp":       {"low", "medium", "high"},

	// ---- Anthropic Claude (claude-code OAuth backend) ----
	// The claude-code backend hits api.anthropic.com directly; Claude 4.x
	// supports the native output_config.effort enum. Mirrors the Kiro path's
	// observed schemas (see reasoning_effort.go). Sonnet 4/4.5/4.6 lack xhigh.
	"claude-opus-4-7": {"low", "medium", "high", "xhigh", "max"},
	"claude-opus-4.7": {"low", "medium", "high", "xhigh", "max"},
	"claude-opus-4-8": {"low", "medium", "high", "xhigh", "max"},
	"claude-opus-4.8": {"low", "medium", "high", "xhigh", "max"},
	"claude-opus-4-6": {"low", "medium", "high", "xhigh", "max"},
	"claude-opus-4.6": {"low", "medium", "high", "xhigh", "max"},
	"claude-sonnet-4-6": {"low", "medium", "high", "max"},
	"claude-sonnet-4.6": {"low", "medium", "high", "max"},
	"claude-sonnet-4-5": {"low", "medium", "high", "max"},
	"claude-sonnet-4.5": {"low", "medium", "high", "max"},
	"claude-haiku-4-5": {"low", "medium", "high", "max"},
	"claude-haiku-4.5": {"low", "medium", "high", "max"},
}

// stripRoutingPrefix reduces a possibly-prefixed client model id to its
// UPSTREAM model id for dictionary lookup. It first consults
// ParseModelBackend (which knows every provider id/alias, including
// user-defined ones); when that recognizes the prefix it returns the stripped
// remainder. For an UNRECOGNIZED prefix (a user-defined provider alias not yet
// registered, or a vendor-qualified id like "or/anthropic/claude-..." whose
// first segment is itself a provider), it falls back to taking the substring
// after the LAST "/" so the upstream model id is what the dictionary matches.
// A bare id with no "/" is returned unchanged.
func stripRoutingPrefix(model string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		return ""
	}
	if _, upstream := ParseModelBackend(m); upstream != "" && upstream != m {
		// Prefix was recognized. Nested qualifiers ("anthropic/claude-...") may
		// still carry a slash; take the tail for dictionary matching.
		if idx := strings.LastIndex(upstream, "/"); idx >= 0 {
			return strings.TrimSpace(upstream[idx+1:])
		}
		return upstream
	}
	// Unrecognized prefix or a Kiro id: take the tail after the last slash so a
	// vendor-qualified id still resolves on its model segment.
	if idx := strings.LastIndex(m, "/"); idx >= 0 {
		return strings.TrimSpace(m[idx+1:])
	}
	return m
}

// modelEffortDictFor returns the ordered effort levels for an upstream model id
// by longest-prefix match against modelEffortDict, or nil when the model has no
// graded effort knob. The caller must pass the UPSTREAM (de-prefixed) id.
func modelEffortDictFor(upstreamModel string) []string {
	m := strings.ToLower(strings.TrimSpace(upstreamModel))
	if m == "" {
		return nil
	}
	bestLen := 0
	var best []string
	for prefix, levels := range modelEffortDict {
		if strings.HasPrefix(m, prefix) && len(prefix) > bestLen {
			bestLen = len(prefix)
			best = levels
		}
	}
	return best
}

// staticEffortLevelsForModel resolves the static (dictionary) effort levels for
// a possibly-prefixed client model id ("cbcn/glm-5.2", "claude-opus-4.7", or a
// bare Kiro id). It strips any provider routing prefix first so the dictionary
// keys upstream ids only. Returns nil when the model has no graded effort knob.
//
// This is the FALLBACK consulted by effortLevelsForModel when the live Kiro
// cache has no schema for the model (i.e. every non-Kiro provider model and the
// post-restart window before the first models refresh).
func staticEffortLevelsForModel(modelID string) []string {
	return modelEffortDictFor(stripRoutingPrefix(modelID))
}

// staticContextWindowForModel resolves the static (dictionary + family-table)
// context window for a possibly-prefixed client model id. It strips the routing
// prefix, checks the model metadata table (modelContextWindow, below) for a
// precise per-model window, then falls back to familyContextWindowFor, then the
// Claude version parse via getContextWindowSize. Returns 0 when truly unknown.
//
// This is the FALLBACK consulted by contextWindowForModel when the live Kiro
// cache has no tokenLimits for the model.
func staticContextWindowForModel(model string) int {
	m := stripRoutingPrefix(model)
	if w := modelContextWindowFor(m); w > 0 {
		return w
	}
	if w := familyContextWindowFor(m); w > 0 {
		return w
	}
	return 0
}

// modelContextWindow holds precise per-model (not just per-family) input
// context windows for providers whose families span multiple windows and that
// never appear in the Kiro cache (so the live tokenLimits lookup always misses).
// Longest-prefix wins. Conservative when a model ships several tiers.
//
// This EXTENDS familyContextWindow (context_window.go) with the providers the
// user called out: OpenAI, Gemini, Anthropic, xAI/Grok, DeepSeek, Zhipu/Z.AI,
// Xiaomi/MiMo, MiniMax, plus the CLI providers' model ids (codex/gpt-5,
// claude-code, gemini-cli, pi, opencode-go).
var modelContextWindow = map[string]int{
	// ---- OpenAI ----
	// Source: platform.openai.com docs / model cards.
	"gpt-5":           400_000,  // 400K input window (gpt-5 / gpt-5-codex)
	"gpt-5-codex":     400_000,
	"gpt-5.1-codex":   400_000,
	"gpt-5.3-codex":   400_000,
	"gpt-5.4":         400_000,
	"gpt-5.5":         400_000,
	"gpt-5-mini":      400_000,
	"gpt-4o":          128_000,
	"gpt-4o-mini":     128_000,
	"gpt-4.1":         1_000_000,
	"gpt-4.1-mini":    1_000_000,
	"gpt-4.1-nano":    1_000_000,
	"gpt-4-turbo":     128_000,
	"gpt-4":           8_192,
	"o3":              200_000,
	"o3-mini":         200_000,
	"o4-mini":         200_000,
	"o1":              200_000,
	"o1-mini":         128_000,
	"codex-mini-latest": 400_000,

	// ---- Anthropic Claude (claude-code OAuth backend hits api.anthropic.com) ----
	// Source: docs.claude.com. Opus/Sonnet/Haiku 4.5 = 200K; 4.6+ = 1M.
	"claude-opus-4-8": 1_000_000,
	"claude-opus-4.8": 1_000_000,
	"claude-opus-4-7": 1_000_000,
	"claude-opus-4.7": 1_000_000,
	"claude-opus-4-6": 1_000_000,
	"claude-opus-4.6": 1_000_000,
	"claude-sonnet-4-6": 1_000_000,
	"claude-sonnet-4.6": 1_000_000,
	"claude-sonnet-4-5": 200_000,
	"claude-sonnet-4.5": 200_000,
	"claude-haiku-4-5": 200_000,
	"claude-haiku-4.5": 200_000,
	"claude-3-7":      200_000,
	"claude-3-5":      200_000,

	// ---- xAI Grok ----
	// Source: x.ai docs. grok-4 / grok-4-fast = 256K; grok-3 = 1M; grok-2 = 128K.
	"grok-4":        256_000,
	"grok-4-fast":   256_000,
	"grok-4-heavy":  256_000,
	"grok-3":        1_000_000,
	"grok-3-mini":   1_000_000,
	"grok-2":        131_072,
	"grok-2-vision": 131_072,
	"grok":          131_072,

	// ---- DeepSeek ----
	// Source: api-docs.deepseek.com. V3/R1 = 128K; V3.1 = 128K; V4 = 1M.
	"deepseek-v4-pro":   1_000_000,
	"deepseek-v4-flash": 1_000_000,
	"deepseek-v4":       1_000_000,
	"deepseek-v3":       131_072,
	"deepseek-r1":       131_072,
	"deepseek-reasoner": 131_072,
	"deepseek-chat":     131_072,

	// ---- Zhipu GLM / Z.AI ----
	// Source: docs.z.ai/guides/llm/<model> spec tables (Context Length field),
	// verified 2026-06. GLM-4.5 = 128K; GLM-4.6/4.7/5/5.1/5-Turbo = 200K;
	// GLM-5.2 = 1M ("truly usable 1M-token context"). Max output is 128K (96K on
	// 4.5) — not tracked here (advertised window is input-only).
	"glm-5.2":       1_000_000,
	"glm-5.1":       200_000,
	"glm-5-turbo":   200_000,
	"glm-5":         200_000,
	"glm-4.7":       200_000,
	"glm-4.6":       200_000,
	"glm-4.5":       131_072,
	"glm-4":         131_072,

	// ---- Xiaomi MiMo ----
	// Source: xiaomimimo.com model card. MiMo coding/reasoning = 256K window.
	"mimo":         262_144,
	"mimo-coding":  262_144,
	"mimo-reasoning": 262_144,

	// ---- MiniMax ----
	// Source: platform.minimax.io / OpenRouter. M2.x/M3 = 204,800.
	"minimax-m3":   204_800,
	"minimax-m2.7": 204_800,
	"minimax-m2.5": 204_800,
	"minimax-m2":   204_800,

	// ---- OpenCode Go / pi ----
	// opencode-go (opencode.ai/zen/go) and the pi agent expose the OpenAI / a
	// coding lineup. Documented at 256K-class for the coding models they serve.
	// Conservative: unknown id on these backends = 200K floor (handled by
	// familyContextWindowFor's default path / getContextWindowSize).
}

// modelContextWindowFor returns the precise per-model context window by
// longest-prefix match against modelContextWindow, or 0 when the id matches no
// pinned entry. Callers should then fall back to familyContextWindowFor.
func modelContextWindowFor(upstreamModel string) int {
	m := strings.ToLower(strings.TrimSpace(upstreamModel))
	if m == "" {
		return 0
	}
	bestLen := 0
	best := 0
	for prefix, window := range modelContextWindow {
		if strings.HasPrefix(m, prefix) && len(prefix) > bestLen {
			bestLen = len(prefix)
			best = window
		}
	}
	return best
}
