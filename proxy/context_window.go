package proxy

import "strings"

// ============================================================================
// Per-family context-window fallback for NON-Kiro provider models.
//
// Handler.contextWindowForModel resolves a model's context window live-first:
// it reads the cached /models tokenLimits.maxInputTokens for the account's
// backend and only falls back to a static figure when the upstream advertises
// none. For a Kiro/Claude model that static fallback is the version parse in
// getContextWindowSize. For a non-Kiro model (gemini/qwen/glm/...) the Claude
// version parse doesn't apply, and a flat 200K default is wrong by up to ~5x:
// a Gemini 2.5 model has a ~1M window, a qwen-turbo up to 1M, so reporting 200K
// makes a client's usage gauge (real_tokens / advertised_window) overshoot 100%
// and mis-time compaction — exactly the numerator/denominator mismatch the
// Claude version parse was restored to fix.
//
// This table is a FALLBACK ONLY. A live tokenLimits value always wins
// (contextWindowForModel consults it first); these numbers are family-level
// documented windows used solely when the provider's /models endpoint reports
// no per-model limit. They are deliberately conservative — when a family ships
// several windows we pick the smaller common one so we never advertise MORE
// context than the model has (over-advertising is the harmful direction: it
// suppresses compaction; under-advertising only triggers it a little early).
//
// Sources (family-level, docs):
//   - Gemini context windows: https://ai.google.dev/gemini-api/docs/models
//     (2.5 Pro/Flash ~1,048,576 input; 1.5 Flash ~1M; 1.5 Pro up to 2M)
//   - Qwen / Model Studio: https://www.alibabacloud.com/help/en/model-studio
//     (qwen-turbo up to 1M; qwen-plus / qwen2.5 128K; qwen-max 32K; qwen3 ~256K)
//   - GLM / ZhipuAI: https://docs.bigmodel.cn (glm-4 family 128K-class)
// ============================================================================

// familyContextWindow maps a lowercased model-id PREFIX to a documented context
// window (max input tokens). Longest-prefix wins so a more specific id (e.g.
// "qwen-turbo") overrides a shorter family key ("qwen"). Keep entries sorted by
// rough specificity in comments; lookup does longest-match regardless of order.
var familyContextWindow = map[string]int{
	// Gemini
	"gemini-1.5-pro":    2_000_000,
	"gemini-1.5-flash":  1_000_000,
	"gemini-1.5":        1_000_000,
	"gemini-2.0-flash":  1_000_000,
	"gemini-2.0":        1_000_000,
	"gemini-2.5-pro":    1_048_576,
	"gemini-2.5-flash":  1_048_576,
	"gemini-2.5":        1_048_576,
	"gemini-exp":        1_048_576,
	"gemini":            1_000_000, // unknown Gemini minor: assume 1M-class

	// Qwen / DashScope (Alibaba Model Studio)
	"qwen-turbo":  1_000_000,
	"qwen-plus":   131_072,
	"qwen-max":    32_768,
	"qwen2.5":     131_072,
	"qwen-2.5":    131_072,
	"qwen3":       262_144,
	"qwen-3":      262_144,
	"qwq":         131_072,
	"qwen":        131_072, // unknown Qwen variant: 128K-class is the common floor

	// GLM / ZhipuAI (Anthropic- or OpenAI-compatible hosts). GLM-4.6 lifted the
	// window from 128K to 200K and GLM-4.7 / GLM-5 / GLM-5.1 / GLM-5-Turbo keep that
	// 200K window (docs.z.ai/guides/llm spec tables). GLM-5.2 is the 1M-window
	// flagship ("truly usable 1M-token context"); it MUST be listed before the
	// bare "glm-5" prefix so longest-prefix match picks 1M, not 200K. GLM-4.5 and
	// earlier are 128K.
	"glm-5.2":     1_000_000,
	"glm-5.1":     200_000,
	"glm-5-turbo": 200_000,
	"glm-5":       200_000,
	"glm-4.7":     200_000,
	"glm-4.6":     200_000,
	"glm-4.5":     131_072,
	"glm-4":       131_072,
	"glm":         131_072,

	// Kimi / Moonshot. K2.5/K2.6/K2.7 ship a 256K window (platform.kimi.ai:
	// "kimi-k2.6, kimi-k2.5, ... all provide a 256K context window"; 256K =
	// 262,144). The original K2 was 128K, so the bare floor stays 128K for an
	// unknown/older variant.
	"kimi-k2.7": 262_144,
	"kimi-k2.6": 262_144,
	"kimi-k2.5": 262_144,
	"kimi":      131_072,
	"moonshot":  131_072,

	// MiniMax. M2/M2.1/M2.5/M2.7 are 204,800 tokens (platform.minimax.io;
	// confirmed against OpenRouter). No public M3 window yet — fall to the M2-era
	// floor, which never over-advertises if M3 turns out larger.
	"minimax": 204_800,

	// DeepSeek. V3.x and R1 are 128K; V4-Pro/V4-Flash jump to 1M (vendor model
	// card: "both supporting a context length of one million tokens"). The bare
	// floor stays 128K for V3/R1 and any unknown variant. NOTE: the codebuddy-cn
	// reseller gateway may enforce a lower cap than the native 1M; a live
	// tokenLimits value would override this, but cbcn serves no /models route.
	"deepseek-v4": 1_000_000,
	"deepseek":    131_072,

	// Tencent Hunyuan. Instruct/chat serving is documented up to 128K (the larger
	// 256K figure is the pretrained-only ceiling), so 128K is the safe
	// advertised floor for the hunyuan-chat / hy3 ids the CN gateway serves.
	"hunyuan": 131_072,
	"hy3":     131_072,
}

// familyContextWindowFor returns the documented fallback context window for a
// non-Claude model id by longest-prefix match against familyContextWindow, or 0
// when the id matches no known family. The caller (getContextWindowSize) only
// consults this for ids the Claude version parse didn't already classify, so a
// Claude id never reaches here.
func familyContextWindowFor(model string) int {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return 0
	}
	bestLen := 0
	best := 0
	for prefix, window := range familyContextWindow {
		if strings.HasPrefix(m, prefix) && len(prefix) > bestLen {
			bestLen = len(prefix)
			best = window
		}
	}
	return best
}
