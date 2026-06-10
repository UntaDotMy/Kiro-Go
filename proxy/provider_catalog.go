package proxy

import (
	"kiro-go/config"
	"strings"
)

// builtinProvider is a compiled-in provider definition, ported from 9router's
// PROVIDERS map (open-sse/config/providers.js). Adding an OpenAI-compatible
// provider is data-only: append a row here (or define a config.ProviderConfig at
// runtime). Only auth-bespoke providers (kiro, codex, qoder) need their own
// Provider implementation; everything in this table is served by the shared
// generic provider keyed on Dialect.
type builtinProvider struct {
	ID         string
	Alias      string
	Name       string
	Dialect    Dialect
	BaseURL    string
	AuthHeader string            // "bearer" | "x-api-key" | "x-goog-api-key"; "" -> default by dialect
	Headers    map[string]string // static headers merged into every request
}

// sharedClaudeHeaders mirrors 9router's CLAUDE_API_HEADERS — the version/beta
// headers Anthropic-compatible endpoints expect.
var sharedClaudeHeaders = map[string]string{
	"Anthropic-Version": "2023-06-01",
	"Anthropic-Beta":    "claude-code-20250219,interleaved-thinking-2025-05-14",
}

// builtinProviders is the data-only catalog. The OpenAI-compatible rows are all
// served by genericProvider with no per-provider code. Ported from 9router's
// open-sse/config/providers.js (the LLM subset; TTS/STT/image/embedding-only
// providers are omitted).
var builtinProviders = []builtinProvider{
	// ---- OpenAI Chat Completions dialect (Bearer auth) ----
	{ID: "openai", Alias: "openai", Name: "OpenAI", Dialect: DialectOpenAI, BaseURL: "https://api.openai.com/v1/chat/completions"},
	{ID: "openrouter", Alias: "or", Name: "OpenRouter", Dialect: DialectOpenAI, BaseURL: "https://openrouter.ai/api/v1/chat/completions", Headers: map[string]string{"HTTP-Referer": "https://kiro-go.local", "X-Title": "Kiro-Go"}},
	{ID: "groq", Alias: "groq", Name: "Groq", Dialect: DialectOpenAI, BaseURL: "https://api.groq.com/openai/v1/chat/completions"},
	{ID: "cerebras", Alias: "cerebras", Name: "Cerebras", Dialect: DialectOpenAI, BaseURL: "https://api.cerebras.ai/v1/chat/completions"},
	{ID: "deepseek", Alias: "ds", Name: "DeepSeek", Dialect: DialectOpenAI, BaseURL: "https://api.deepseek.com/chat/completions"},
	{ID: "mistral", Alias: "mistral", Name: "Mistral", Dialect: DialectOpenAI, BaseURL: "https://api.mistral.ai/v1/chat/completions"},
	{ID: "together", Alias: "together", Name: "Together AI", Dialect: DialectOpenAI, BaseURL: "https://api.together.xyz/v1/chat/completions"},
	{ID: "fireworks", Alias: "fireworks", Name: "Fireworks AI", Dialect: DialectOpenAI, BaseURL: "https://api.fireworks.ai/inference/v1/chat/completions"},
	{ID: "cohere", Alias: "cohere", Name: "Cohere", Dialect: DialectOpenAI, BaseURL: "https://api.cohere.ai/v1/chat/completions"},
	{ID: "nebius", Alias: "nebius", Name: "Nebius AI", Dialect: DialectOpenAI, BaseURL: "https://api.studio.nebius.ai/v1/chat/completions"},
	{ID: "siliconflow", Alias: "siliconflow", Name: "SiliconFlow", Dialect: DialectOpenAI, BaseURL: "https://api.siliconflow.cn/v1/chat/completions"},
	{ID: "hyperbolic", Alias: "hyp", Name: "Hyperbolic", Dialect: DialectOpenAI, BaseURL: "https://api.hyperbolic.xyz/v1/chat/completions"},
	{ID: "perplexity", Alias: "pplx", Name: "Perplexity", Dialect: DialectOpenAI, BaseURL: "https://api.perplexity.ai/chat/completions"},
	{ID: "xai", Alias: "xai", Name: "xAI (Grok)", Dialect: DialectOpenAI, BaseURL: "https://api.x.ai/v1/chat/completions"},
	{ID: "nvidia", Alias: "nvidia", Name: "NVIDIA NIM", Dialect: DialectOpenAI, BaseURL: "https://integrate.api.nvidia.com/v1/chat/completions"},
	{ID: "chutes", Alias: "ch", Name: "Chutes AI", Dialect: DialectOpenAI, BaseURL: "https://llm.chutes.ai/v1/chat/completions"},
	{ID: "deepinfra", Alias: "deepinfra", Name: "DeepInfra", Dialect: DialectOpenAI, BaseURL: "https://api.deepinfra.com/v1/openai/chat/completions"},
	{ID: "sambanova", Alias: "sambanova", Name: "SambaNova", Dialect: DialectOpenAI, BaseURL: "https://api.sambanova.ai/v1/chat/completions"},
	{ID: "vercel-ai-gateway", Alias: "vercel", Name: "Vercel AI Gateway", Dialect: DialectOpenAI, BaseURL: "https://ai-gateway.vercel.sh/v1/chat/completions"},

	// ---- Coding-assistant OpenAI-compatible providers (ported from 9router) ----
	{ID: "codebuddy", Alias: "cb", Name: "CodeBuddy (Tencent)", Dialect: DialectOpenAI, BaseURL: "https://copilot.tencent.com/v1/chat/completions"},
	{ID: "qwen", Alias: "qwen", Name: "Qwen (Alibaba)", Dialect: DialectOpenAI, BaseURL: "https://portal.qwen.ai/v1/chat/completions"},
	{ID: "iflow", Alias: "iflow", Name: "iFlow", Dialect: DialectOpenAI, BaseURL: "https://apis.iflow.cn/v1/chat/completions"},
	{ID: "glm-cn", Alias: "glmcn", Name: "GLM (bigmodel.cn)", Dialect: DialectOpenAI, BaseURL: "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions"},
	{ID: "kilocode", Alias: "kilo", Name: "Kilo Code", Dialect: DialectOpenAI, BaseURL: "https://api.kilo.ai/api/openrouter/chat/completions"},
	{ID: "cline", Alias: "cline", Name: "Cline", Dialect: DialectOpenAI, BaseURL: "https://api.cline.bot/api/v1/chat/completions"},
	{ID: "longcat", Alias: "longcat", Name: "LongCat (Meituan)", Dialect: DialectOpenAI, BaseURL: "https://api.longcat.chat/openai/v1/chat/completions"},
	{ID: "alicode", Alias: "alicode", Name: "Alibaba Code", Dialect: DialectOpenAI, BaseURL: "https://coding.dashscope.aliyuncs.com/v1/chat/completions"},
	{ID: "alicode-intl", Alias: "alicodeintl", Name: "Alibaba Code (Intl)", Dialect: DialectOpenAI, BaseURL: "https://coding-intl.dashscope.aliyuncs.com/v1/chat/completions"},
	{ID: "gitlab", Alias: "gitlab", Name: "GitLab Duo", Dialect: DialectOpenAI, BaseURL: "https://gitlab.com/api/v4/chat/completions"},

	// ---- Additional OpenAI-compatible inference providers (ported from 9router) ----
	{ID: "novita", Alias: "novita", Name: "Novita AI", Dialect: DialectOpenAI, BaseURL: "https://api.novita.ai/v3/openai/chat/completions"},
	{ID: "aimlapi", Alias: "aiml", Name: "AI/ML API", Dialect: DialectOpenAI, BaseURL: "https://api.aimlapi.com/v1/chat/completions"},
	{ID: "baseten", Alias: "baseten", Name: "Baseten", Dialect: DialectOpenAI, BaseURL: "https://inference.baseten.co/v1/chat/completions"},
	{ID: "scaleway", Alias: "scaleway", Name: "Scaleway", Dialect: DialectOpenAI, BaseURL: "https://api.scaleway.ai/v1/chat/completions"},
	{ID: "nscale", Alias: "nscale", Name: "nscale", Dialect: DialectOpenAI, BaseURL: "https://inference.api.nscale.com/v1/chat/completions"},
	{ID: "modal", Alias: "modal", Name: "Modal", Dialect: DialectOpenAI, BaseURL: "https://api.modal.com/v1/chat/completions"},
	{ID: "ai21", Alias: "ai21", Name: "AI21 Labs", Dialect: DialectOpenAI, BaseURL: "https://api.ai21.com/studio/v1/chat/completions"},
	{ID: "inference-net", Alias: "inferencenet", Name: "Inference.net", Dialect: DialectOpenAI, BaseURL: "https://api.inference.net/v1/chat/completions"},
	{ID: "kluster", Alias: "kluster", Name: "Kluster AI", Dialect: DialectOpenAI, BaseURL: "https://api.kluster.ai/v1/chat/completions"},
	{ID: "morph", Alias: "morph", Name: "Morph", Dialect: DialectOpenAI, BaseURL: "https://api.morphllm.com/v1/chat/completions"},
	{ID: "xiaomi-mimo", Alias: "mimo", Name: "Xiaomi MiMo", Dialect: DialectOpenAI, BaseURL: "https://api.xiaomimimo.com/v1/chat/completions"},

	// ---- Anthropic Messages dialect (x-api-key auth) ----
	{ID: "anthropic", Alias: "anthropic", Name: "Anthropic", Dialect: DialectAnthropic, BaseURL: "https://api.anthropic.com/v1/messages", Headers: sharedClaudeHeaders},
	{ID: "glm", Alias: "glm", Name: "GLM Coding", Dialect: DialectAnthropic, BaseURL: "https://api.z.ai/api/anthropic/v1/messages", Headers: sharedClaudeHeaders},
	{ID: "kimi", Alias: "kimi", Name: "Kimi", Dialect: DialectAnthropic, BaseURL: "https://api.kimi.com/coding/v1/messages", Headers: sharedClaudeHeaders},
	{ID: "minimax", Alias: "minimax", Name: "MiniMax", Dialect: DialectAnthropic, BaseURL: "https://api.minimax.io/anthropic/v1/messages", Headers: sharedClaudeHeaders},
	{ID: "minimax-cn", Alias: "minimaxcn", Name: "MiniMax (CN)", Dialect: DialectAnthropic, BaseURL: "https://api.minimaxi.com/anthropic/v1/messages", Headers: sharedClaudeHeaders},
	{ID: "agentrouter", Alias: "ar", Name: "AgentRouter", Dialect: DialectAnthropic, BaseURL: "https://agentrouter.org/v1/messages", Headers: sharedClaudeHeaders},

	// ---- Google Gemini dialect (x-goog-api-key auth) ----
	{ID: "gemini", Alias: "gemini", Name: "Gemini", Dialect: DialectGemini, BaseURL: "https://generativelanguage.googleapis.com/v1beta/models"},
}

// builtinByID / builtinByAlias index the catalog for O(1) lookup. Built once at
// init; read-only thereafter.
var (
	builtinByID    = map[string]builtinProvider{}
	builtinByAlias = map[string]string{} // alias -> id
)

func init() {
	for _, p := range builtinProviders {
		builtinByID[p.ID] = p
		if p.Alias != "" {
			builtinByAlias[p.Alias] = p.ID
		}
	}
}

// resolveBuiltinProvider returns the catalog entry for an id, or false.
func resolveBuiltinProvider(id string) (builtinProvider, bool) {
	bp, ok := builtinByID[strings.ToLower(strings.TrimSpace(id))]
	return bp, ok
}

// dialectFor resolves the wire dialect for a backend id: a built-in catalog
// entry's Dialect, or a user-defined config.ProviderConfig's dialect, or "" if
// unknown.
func dialectFor(backend string) Dialect {
	key := strings.ToLower(strings.TrimSpace(backend))
	if bp, ok := builtinByID[key]; ok {
		return bp.Dialect
	}
	if pc, ok := config.GetProviderConfig(key); ok {
		return Dialect(strings.ToLower(strings.TrimSpace(pc.Dialect)))
	}
	switch key {
	case "kiro", "":
		return DialectKiro
	case "codex":
		return DialectCodex
	}
	// Self-contained custom account carrying an inline dialect.
	if acct, ok := config.GetCustomAccountByBackend(key); ok {
		return Dialect(strings.ToLower(strings.TrimSpace(acct.CustomDialect)))
	}
	return ""
}

// defaultAuthHeaderForDialect returns the HTTP auth header convention for a
// dialect when a provider doesn't override it.
func defaultAuthHeaderForDialect(d Dialect) string {
	switch d {
	case DialectAnthropic:
		return "x-api-key"
	case DialectGemini:
		return "x-goog-api-key"
	default: // openai and everything else
		return "bearer"
	}
}
