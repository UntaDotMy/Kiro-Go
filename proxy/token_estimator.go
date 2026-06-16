package proxy

import (
	"encoding/json"
	"math"
)

// resolveInputTokens applies the token-count precedence used across every
// response finalization path (most → least accurate):
//
//  1. upstream: the exact input_tokens reported in the Kiro event stream.
//  2. contextDerived: contextUsagePercentage × context window — the model's
//     own accounting, but coarse (rounded to a percentage).
//  3. estimated: our local char-heuristic estimate, a last resort.
//
// CLIs (Claude Code, opencode, Cline, Codex) trust the usage we return
// verbatim, so an exact upstream count must never be overwritten by the
// coarser fallbacks.
func resolveInputTokens(upstream, contextDerived, estimated int) int {
	if upstream > 0 {
		return upstream
	}
	if contextDerived > 0 {
		return contextDerived
	}
	if estimated > 0 {
		return estimated
	}
	return 0
}

// ResolvedUsage is the final, format-neutral token usage every client emitter
// renders from. It is produced by resolveUsage — the SINGLE place that decides
// real-vs-estimated — and consumed verbatim by all three client formats
// (Claude, OpenAI chat, Responses) on both the stream and non-stream paths.
//
// OutputTokens includes ReasoningTokens (the UpstreamUsage contract is carried
// through). TotalTokens is never InputTokens+OutputTokens+ReasoningTokens — that
// would double-count reasoning, which already lives inside OutputTokens.
type ResolvedUsage struct {
	InputTokens         int
	OutputTokens        int
	TotalTokens         int
	CacheReadTokens     int
	CacheCreationTokens int
	ReasoningTokens     int
	CachePresent        bool
}

// resolveUsage is the one source of truth for turning captured upstream usage
// plus local fallbacks into the ResolvedUsage every emitter renders. It mirrors
// resolveResponseCache (format-neutral resolver + dumb emitters):
//
//   - InputTokens via resolveInputTokens (upstream → contextDerived → estimated).
//   - OutputTokens: the REAL upstream output when present (which already includes
//     reasoning), else the local estimate.
//   - ReasoningTokens passed through from upstream (0 when not reported).
//   - TotalTokens: the real upstream total when present, else InputTokens +
//     OutputTokens — NEVER + ReasoningTokens (reasoning is inside output).
//   - cache via resolveResponseCache: Kiro uses the local estimate, non-Kiro
//     passes through the provider's real read/creation or emits none.
//
// estimatedProfilePresent + estimated carry the Kiro local prompt-cache estimate;
// non-Kiro ignores them and uses up.CacheReadTokens / up.CacheCreationTokens.
func resolveUsage(isKiro bool, up UpstreamUsage, estimated promptCacheUsage, estimatedProfilePresent bool, contextDerivedInput, estimatedInput, estimatedOutput int) ResolvedUsage {
	input := resolveInputTokens(up.InputTokens, contextDerivedInput, estimatedInput)

	output := estimatedOutput
	if up.HasRealCounts && up.OutputTokens > 0 {
		output = up.OutputTokens
	}

	total := input + output
	if up.TotalTokens > 0 {
		total = up.TotalTokens
	}

	cache, cachePresent := resolveResponseCache(isKiro, estimated, estimatedProfilePresent, up.CacheReadTokens, up.CacheCreationTokens)

	return ResolvedUsage{
		InputTokens:         input,
		OutputTokens:        output,
		TotalTokens:         total,
		CacheReadTokens:     cache.CacheReadInputTokens,
		CacheCreationTokens: cache.CacheCreationInputTokens,
		ReasoningTokens:     up.ReasoningTokens,
		CachePresent:        cachePresent,
	}
}

// claudeCountTokensCorrection inflates our local char-heuristic estimate to
// better match Anthropic's real tokenizer for the /v1/messages/count_tokens
// endpoint that Claude Code calls to PRE-measure context before sending.
//
// estimateApproxTokens is a char-class heuristic tuned to be close on average,
// but it runs slightly LOW on real code/prose vs Anthropic's BPE tokenizer.
// count_tokens is the number Claude Code uses to decide when to auto-compact;
// if it reads low, compaction is delayed (or, combined with a mis-sized window,
// skipped). A modest 1.15x correction — the same factor kiro-gateway applies
// for the same reason — biases the pre-send measurement slightly HIGH so
// compaction triggers on time rather than late. It is applied ONLY to the
// count_tokens response, never to the streamed usage block (which prefers the
// exact upstream count).
const claudeCountTokensCorrection = 1.15

// countTokensWithClaudeCorrection applies claudeCountTokensCorrection to a raw
// local estimate and rounds up.
func countTokensWithClaudeCorrection(raw int) int {
	if raw <= 0 {
		return raw
	}
	return int(math.Ceil(float64(raw) * claudeCountTokensCorrection))
}

func estimateApproxTokens(text string) int {
	if text == "" {
		return 0
	}

	runes := []rune(text)
	length := len(runes)
	if length == 0 {
		return 0
	}
	if length < 5 {
		return max(1, int(math.Ceil(float64(length)/3.0)))
	}

	var regularAscii, digits, symbols, nonASCII int
	for _, r := range runes {
		switch {
		case r >= 0x80:
			nonASCII++
		case r >= '0' && r <= '9':
			digits++
		case (r >= '!' && r <= '/') || (r >= ':' && r <= '@') || (r >= '[' && r <= '`') || (r >= '{' && r <= '~'):
			symbols++
		default:
			regularAscii++
		}
	}

	estimated := int(math.Ceil(
		float64(regularAscii)/4.5 +
			float64(digits)/2.0 +
			float64(symbols)/1.5 +
			float64(nonASCII)/1.5,
	))

	if estimated < 1 {
		return 1
	}
	return estimated
}

func estimateClaudeRequestInputTokens(req *ClaudeRequest) int {
	if req == nil {
		return 0
	}

	total := estimateClaudeValueTokens(req.System)

	for _, msg := range req.Messages {
		total += estimateClaudeValueTokens(msg.Content)
	}

	for _, tool := range req.Tools {
		total += estimateApproxTokens(tool.Name)
		total += estimateApproxTokens(tool.Description)
		total += estimateJSONTokens(tool.InputSchema)
	}

	return total
}

func estimateClaudeOutputTokens(content, thinkingContent string, toolUses []KiroToolUse) int {
	total := estimateApproxTokens(content)
	total += estimateApproxTokens(thinkingContent)

	for _, tu := range toolUses {
		total += estimateApproxTokens(tu.Name)
		total += estimateJSONTokens(tu.Input)
	}

	return total
}

func estimateClaudeValueTokens(v interface{}) int {
	switch value := v.(type) {
	case nil:
		return 0
	case string:
		return estimateApproxTokens(value)
	case []interface{}:
		total := 0
		for _, part := range value {
			total += estimateClaudeValueTokens(part)
		}
		return total
	case map[string]interface{}:
		typeName, _ := value["type"].(string)
		switch typeName {
		case "text":
			if text, ok := value["text"].(string); ok {
				return estimateApproxTokens(text)
			}
		case "thinking":
			if thinking, ok := value["thinking"].(string); ok {
				return estimateApproxTokens(thinking)
			}
		case "tool_use":
			total := 0
			if name, ok := value["name"].(string); ok {
				total += estimateApproxTokens(name)
			}
			if input, ok := value["input"]; ok {
				total += estimateJSONTokens(input)
			}
			if total > 0 {
				return total
			}
		case "tool_result":
			if content, ok := value["content"]; ok {
				return estimateClaudeValueTokens(content)
			}
		}

		total := 0
		if text, ok := value["text"].(string); ok {
			total += estimateApproxTokens(text)
		}
		if thinking, ok := value["thinking"].(string); ok {
			total += estimateApproxTokens(thinking)
		}
		if content, ok := value["content"]; ok {
			total += estimateClaudeValueTokens(content)
		}
		if total > 0 {
			return total
		}

		return estimateJSONTokens(value)
	default:
		return estimateJSONTokens(value)
	}
}

func estimateJSONTokens(v interface{}) int {
	if v == nil {
		return 0
	}

	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}

	return estimateApproxTokens(string(b))
}

func estimateOpenAIRequestInputTokens(req *OpenAIRequest) int {
	if req == nil {
		return 0
	}

	total := 0

	for _, msg := range req.Messages {
		total += estimateOpenAIContentTokens(msg.Content)
		total += estimateApproxTokens(msg.ToolCallID)
		for _, tc := range msg.ToolCalls {
			total += estimateApproxTokens(tc.Function.Name)
			total += estimateApproxTokens(tc.Function.Arguments)
		}
	}

	for _, tool := range req.Tools {
		total += estimateApproxTokens(tool.Function.Name)
		total += estimateApproxTokens(tool.Function.Description)
		total += estimateJSONTokens(tool.Function.Parameters)
	}

	return total
}

func estimateOpenAIContentTokens(content interface{}) int {
	switch value := content.(type) {
	case nil:
		return 0
	case string:
		return estimateApproxTokens(value)
	default:
		text := extractOpenAIMessageText(value)
		if text != "" {
			return estimateApproxTokens(text)
		}
		return estimateJSONTokens(value)
	}
}

func estimateOpenAIOutputTokens(content, reasoningContent string, toolUses []KiroToolUse) int {
	return estimateClaudeOutputTokens(content, reasoningContent, toolUses)
}
