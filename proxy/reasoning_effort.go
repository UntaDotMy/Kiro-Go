package proxy

import "strings"

// ============================================================================
// Reasoning-effort control.
//
// Kiro CLI 2.4+ exposes a per-model reasoning-effort knob. Clients surface it
// three different ways:
//   - OpenAI Chat Completions: `reasoning_effort` ("minimal"|"low"|"medium"|"high")
//   - OpenAI Responses / Codex: `reasoning.effort` (same scale, also "max")
//   - Anthropic Messages:       `output_config.effort` — a native, GA top-level
//                               field (low|medium|high|xhigh|max) that Claude
//                               Code's CLAUDE_CODE_EFFORT_LEVEL maps onto 1:1
//                               ("auto" omits it). We read it via
//                               claudeRequestEffort and forward it the same way
//                               as the OpenAI knobs. (Thinking on/off is still
//                               derived from thinking.type and the -thinking
//                               suffix, and an explicit "minimal" effort folds
//                               into that decision — see resolveThinkingWithEffort.)
//
// HOW KIRO ACTUALLY ACCEPTS EFFORT (verified against kiro-cli 2.5.0):
// The Kiro generateAssistantResponse backend DOES accept a graded effort value,
// carried as a Bedrock-style passthrough field at the TOP LEVEL of the request
// (sibling of conversationState):
//
//   "additionalModelRequestFields": {"output_config": {"effort": "high"}}
//
// This was confirmed two ways: (1) the kiro-cli 2.5.0 binary's own
// GenerateAssistantResponseInput serializer carries an
// `additional_model_request_fields` field and the runtime string
// "output_config.effort"; (2) a live ListAvailableModels response advertises a
// per-model `additionalModelRequestFieldsSchema` whose
// properties.output_config.properties.effort.enum lists the accepted levels.
//
// Support is PER MODEL and the backend validates it — sending effort to a model
// that doesn't declare support yields HTTP 400 ("model does not support
// additional fields"). Observed enums:
//   - Opus 4.6 / 4.7 / 4.8 / auto:  low, medium, high, xhigh, max
//   - Sonnet 4 / 4.5 / 4.6, Opus 4.5: low, medium, high, max  (no xhigh)
//   - Haiku 4.5, DeepSeek, MiniMax, GLM, Qwen: no effort support
//
// So we forward effort NATIVELY when the resolved model's cached schema
// declares it (see Handler.applyReasoningEffort), clamping a requested level
// down to the nearest supported one. For models that DON'T support native
// effort, we keep the legacy fallback: map effort to the one lever those models
// honor — whether thinking is engaged at all (minimal -> off, else on). The
// thinking flag also drives response-side reasoning parsing in both cases.
// ============================================================================

// reasoning effort levels, normalized. These mirror the upstream effort enum,
// plus effortMinimal which is a proxy-only sentinel meaning "reasoning off"
// (the upstream enum has no "off"/"minimal"; its floor is "low").
const (
	effortUnset   = ""
	effortMinimal = "minimal"
	effortLow     = "low"
	effortMedium  = "medium"
	effortHigh    = "high"
	effortXHigh   = "xhigh"
	effortMax     = "max"
)

// effortRank orders effort levels for clamping. effortMinimal ranks below the
// real enum floor (low) because it means "reasoning off", not a graded level.
var effortRank = map[string]int{
	effortMinimal: 0,
	effortLow:     1,
	effortMedium:  2,
	effortHigh:    3,
	effortXHigh:   4,
	effortMax:     5,
}

// normalizeReasoningEffort lower-cases/trims an effort string and maps it to one
// of the canonical levels. Unrecognized values normalize to "" (unset) so they
// fall through to the caller's default rather than silently disabling thinking.
//
// "xhigh"/"x-high" map to the distinct effortXHigh level (NOT max): the upstream
// enum treats xhigh as its own tier between high and max on the models that
// support it.
func normalizeReasoningEffort(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "minimal", "none", "off":
		return effortMinimal
	case "low":
		return effortLow
	case "medium", "med":
		return effortMedium
	case "high":
		return effortHigh
	case "xhigh", "x-high", "extrahigh", "extra-high":
		return effortXHigh
	case "max", "maximum":
		return effortMax
	default:
		return effortUnset
	}
}

// effortEngagesThinking reports whether a normalized effort level should turn
// reasoning ON. Returns (engage, specified):
//   - specified=false means the client gave no usable effort, so the caller
//     should keep its existing thinking decision (suffix / thinking config).
//   - specified=true, engage=false means an explicit "minimal" -> thinking off.
//   - specified=true, engage=true means low/medium/high/xhigh/max -> thinking on.
func effortEngagesThinking(normalizedEffort string) (engage bool, specified bool) {
	switch normalizedEffort {
	case effortUnset:
		return false, false
	case effortMinimal:
		return false, true
	default: // low / medium / high / xhigh / max
		return true, true
	}
}

// resolveThinkingWithEffort folds a reasoning-effort signal into a base thinking
// decision. The base decision comes from the model suffix (-thinking) or the
// Anthropic thinking config. An explicit effort overrides it: "minimal" forces
// thinking off even if the suffix asked for it; low/medium/high/xhigh/max force
// it on. An unset/unrecognized effort leaves the base decision unchanged.
//
// This governs response-side reasoning parsing for ALL models, and is the only
// effort lever for models without native effort support.
func resolveThinkingWithEffort(baseThinking bool, rawEffort string) bool {
	engage, specified := effortEngagesThinking(normalizeReasoningEffort(rawEffort))
	if !specified {
		return baseThinking
	}
	return engage
}

// modelEffortLevels extracts the ordered list of reasoning-effort levels a model
// accepts from its AdditionalModelRequestFieldsSchema (the JSON-Schema fragment
// ListAvailableModels returns). It reads
// properties.output_config.properties.effort.enum and returns those values
// lower-cased. Returns nil when the model declares no effort field — callers
// then fall back to the thinking on/off path.
func modelEffortLevels(schema map[string]interface{}) []string {
	props, _ := schema["properties"].(map[string]interface{})
	oc, _ := props["output_config"].(map[string]interface{})
	ocProps, _ := oc["properties"].(map[string]interface{})
	effort, _ := ocProps["effort"].(map[string]interface{})
	rawEnum, _ := effort["enum"].([]interface{})
	if len(rawEnum) == 0 {
		return nil
	}
	levels := make([]string, 0, len(rawEnum))
	for _, v := range rawEnum {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			levels = append(levels, s)
		}
	}
	if len(levels) == 0 {
		return nil
	}
	return levels
}

// resolveModelEffort picks the effort level to actually forward for a requested
// value against a model's supported levels. Returns (level, ok):
//   - ok=false -> do NOT attach a native effort field. Happens for unset/unknown
//     effort, for "minimal" (means reasoning-off, handled by the thinking path),
//     or when the model supports no effort levels.
//   - ok=true  -> attach output_config.effort = level. The level is the requested
//     one when supported, otherwise clamped DOWN to the highest supported level
//     that does not exceed the request (e.g. "xhigh" on a model lacking xhigh
//     clamps to "high"). Clamping down is deliberate: never silently escalate a
//     user to a higher-credit tier than they asked for.
func resolveModelEffort(rawEffort string, supportedLevels []string) (string, bool) {
	norm := normalizeReasoningEffort(rawEffort)
	if norm == effortUnset || norm == effortMinimal {
		return "", false
	}
	if len(supportedLevels) == 0 {
		return "", false
	}
	reqRank, ok := effortRank[norm]
	if !ok {
		return "", false
	}

	best := ""
	bestRank := -1
	for _, l := range supportedLevels {
		if l == norm {
			return norm, true // exact support
		}
		r, ok := effortRank[l]
		if !ok {
			continue
		}
		if r <= reqRank && r > bestRank {
			best, bestRank = l, r
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// buildEffortSchema reconstructs the minimal AdditionalModelRequestFieldsSchema
// fragment that modelEffortLevels reads, from a flat list of supported levels.
// It is the inverse of modelEffortLevels and is used to rehydrate a persisted
// per-model effort list (config.KnownModelEffort) back into a cached ModelInfo
// on boot, so graded effort works immediately after a restart instead of being
// silently dropped until the first live ListAvailableModels refresh lands.
// Returns nil for an empty list so callers attach no schema (no effort support).
func buildEffortSchema(levels []string) map[string]interface{} {
	if len(levels) == 0 {
		return nil
	}
	enum := make([]interface{}, 0, len(levels))
	for _, l := range levels {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			enum = append(enum, l)
		}
	}
	if len(enum) == 0 {
		return nil
	}
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"output_config": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"effort": map[string]interface{}{
						"type": "string",
						"enum": enum,
					},
				},
			},
		},
	}
}

// buildEffortRequestFields wraps a resolved effort level into the
// additionalModelRequestFields payload shape the Kiro upstream expects:
//
//	{"output_config": {"effort": "<level>"}}
//
// Returns nil when level is empty so the field is omitted entirely.
func buildEffortRequestFields(level string) map[string]interface{} {
	if level == "" {
		return nil
	}
	return map[string]interface{}{
		"output_config": map[string]interface{}{
			"effort": level,
		},
	}
}
