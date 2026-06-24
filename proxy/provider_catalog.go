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
	// Models is a STATIC fallback catalog for providers that do NOT expose a
	// GET /models listing endpoint (e.g. Tencent CodeBuddy, iFlow, the Alibaba
	// "alicode" coding hosts, GitLab Duo, Perplexity). When the live fetch 404s
	// or errors, ListModels falls back to this list so the dashboard shows a real
	// model count and the pool's per-account model filter has ids to match —
	// instead of "0 models" / "fetch failed". 9router ships per-provider static
	// catalogs for exactly these no-/models providers. Leave nil for providers
	// whose /models works (the live fetch wins and stays current).
	Models []string
	// OAuth marks a backend that authenticates via an OAuth flow (a device login),
	// NOT a pasted API key. The catalog surfaces it with authType "oauth" so the
	// dashboard routes it to its connect flow, and the api-key add paths reject it
	// with a redirect message. Inference is still the generic dialect above once a
	// token exists. Currently: qwen (qwen-code device flow).
	OAuth bool
}

// sharedClaudeHeaders mirrors 9router's CLAUDE_API_HEADERS — the version/beta
// headers Anthropic-compatible endpoints expect.
var sharedClaudeHeaders = map[string]string{
	"Anthropic-Version": "2023-06-01",
	"Anthropic-Beta":    "claude-code-20250219,interleaved-thinking-2025-05-14",
}

// aliCoderModels is the advisory (display-only) Qwen-Coder catalog for the Alibaba
// "alicode" coding-assistant hosts, which (unlike the DashScope compatible-mode
// bases) do NOT serve a GET /models list (404). Shared by alicode + alicode-intl,
// which expose the same coder lineup. Source: Alibaba Cloud Model Studio
// Qwen-Coder docs (alibabacloud.com/help/en/model-studio/qwen-coder). Advisory
// only — a missing id is never shed; the upstream validates at call time.
var aliCoderModels = []string{
	"qwen3-coder-next", "qwen3-coder-plus", "qwen3-coder-flash",
	"qwen2.5-coder-7b-instruct", "qwen2.5-coder-14b-instruct", "qwen2.5-coder-32b-instruct",
	"qwen-coder-turbo", "qwen-coder-turbo-latest", "qwen-coder-turbo-0919",
}

// codeBuddyModels is the advisory (display-only) model catalog for CodeBuddy.
// CodeBuddy exposes NO GET /models endpoint (every /v1, /v2, and /v2/plugin
// models path returns 404), so the live fetch can never populate the list — this
// is why the dashboard showed 0 models. We ship the real model ids the gateway
// accepts as an advisory list (SetAdvisoryModelList), so the count is real and
// clients can route to a named model; a missing id is never shed because the
// upstream validates at call time. Shared by both hosts (codebuddy CN +
// codebuddy-ai international), which expose the same lineup.
//
// Source: the CodeBuddy CLI's own model definitions (ported from 9router_wyx0's
// open-sse/config/providerModels.js `cb` set, smoke-verified against
// www.codebuddy.ai). "default-model" is the gateway's auto-routed default.
var codeBuddyModels = []string{
	"default-model", "default-model-lite",
	"claude-sonnet-4.6", "claude-opus-4.7-1m", "claude-opus-4.6", "claude-haiku-4.5",
	"gpt-5.5", "gpt-5.4", "gpt-5.3-codex", "gpt-5.1-codex",
	"gemini-3.1-pro", "gemini-3.0-flash", "gemini-3.5-flash", "gemini-2.5-flash",
	"gemini-3.1-flash-lite", "gemini-2.5-pro",
	"deepseek-v3-0324", "glm-5.0", "glm-5v-turbo", "glm-4.6",
	"kimi-k2.6", "kimi-k2.5",
}

// codeBuddyCNModels is the advisory (display-only) model catalog for the CodeBuddy
// CN reseller provider (codebuddy-cn), which uses ck_ API keys against the Tencent
// copilot.tencent.com gateway. This mirrors the reseller tool's own SUPPORTED_MODELS
// list verbatim (codebuddychina/_internal/src/modules/proxy_server.py), which the
// tool author marks 已实测验证可用 ("smoke-verified usable") against the gateway — it
// is the ground truth for what copilot.tencent.com/v2/chat/completions accepts for
// reseller ck_ keys. CodeBuddy serves no GET /models route, so this ships as an
// advisory list (a missing id is never shed; the upstream validates at call time).
var codeBuddyCNModels = []string{
	"auto",
	"deepseek-v4-pro", "deepseek-v4-flash",
	"deepseek-v3-2-volc", "deepseek-v3-1", "deepseek-v3-0324",
	"deepseek-r1",
	"glm-5.2", "glm-5.1", "glm-5.0", "glm-5.0-turbo", "glm-5v-turbo",
	"glm-4.7", "glm-4.6",
	"minimax-m3", "minimax-m2.7", "minimax-m2.5",
	"kimi-k2.7", "kimi-k2.6", "kimi-k2.5",
	"hy3-preview", "hunyuan-chat", "hunyuan-2.0-thinking",
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
	{ID: "perplexity", Alias: "pplx", Name: "Perplexity", Dialect: DialectOpenAI, BaseURL: "https://api.perplexity.ai/chat/completions",
		// Perplexity has no GET /models endpoint (404), so we ship its Sonar catalog
		// as an advisory (display-only) list. Source: docs.perplexity.ai model cards.
		Models: []string{"sonar", "sonar-pro", "sonar-reasoning-pro", "sonar-deep-research"}},
	{ID: "xai", Alias: "xai", Name: "xAI (Grok)", Dialect: DialectOpenAI, BaseURL: "https://api.x.ai/v1/chat/completions"},
	{ID: "zai", Alias: "zai", Name: "Z.AI (Zhipu GLM)", Dialect: DialectOpenAI, BaseURL: "https://api.z.ai/api/paas/v4/chat/completions",
		// z.ai exposes GET /api/paas/v4/models, but it is plan-gated and omits
		// models the key can actually call, so the live fetch alone undercounts.
		// Ship the current GLM lineup as an advisory fallback. Source: z.ai pricing
		// docs (docs.z.ai/guides/overview/pricing). A missing id is never shed.
		Models: []string{"glm-5.1", "glm-5", "glm-5-turbo", "glm-4.7", "glm-4.7-flashx", "glm-4.7-flash", "glm-4.6", "glm-4.5", "glm-4.5-x", "glm-4.5-air", "glm-4.5-airx", "glm-4.5-flash"}},
	{ID: "nvidia", Alias: "nvidia", Name: "NVIDIA NIM", Dialect: DialectOpenAI, BaseURL: "https://integrate.api.nvidia.com/v1/chat/completions"},
	{ID: "chutes", Alias: "ch", Name: "Chutes AI", Dialect: DialectOpenAI, BaseURL: "https://llm.chutes.ai/v1/chat/completions"},
	{ID: "deepinfra", Alias: "deepinfra", Name: "DeepInfra", Dialect: DialectOpenAI, BaseURL: "https://api.deepinfra.com/v1/openai/chat/completions"},
	{ID: "sambanova", Alias: "sambanova", Name: "SambaNova", Dialect: DialectOpenAI, BaseURL: "https://api.sambanova.ai/v1/chat/completions"},
	{ID: "vercel-ai-gateway", Alias: "vercel", Name: "Vercel AI Gateway", Dialect: DialectOpenAI, BaseURL: "https://ai-gateway.vercel.sh/v1/chat/completions"},

	// ---- Coding-assistant OpenAI-compatible providers (ported from 9router) ----
	// CodeBuddy ships two interchangeable official hosts (per the CLI's product.json):
	// the China gateway copilot.tencent.com and the international site www.codebuddy.ai.
	// Both share the same /v2/plugin/auth OAuth flow (auth/codebuddy_oauth.go); the
	// backend id selects the host so token refresh hits the gateway the account logged
	// in against.
	//
	// The inference endpoint is /v2/chat/completions (OpenAI dialect) — NOT /v1:
	// probing shows /v1/chat/completions 404s ("Route Not Found") while
	// /v2/chat/completions returns 401 (exists, needs auth) and /v2/messages 404s
	// (so it's OpenAI-shaped, not Anthropic). The old /v1 base was the real cause of
	// both broken inference and the empty model list. CodeBuddy serves no /models
	// route, so codeBuddyModels is shipped as an advisory list (see above).
	{ID: "codebuddy", Alias: "cb", Name: "CodeBuddy (Tencent CN)", Dialect: DialectOpenAI, BaseURL: "https://copilot.tencent.com/v2/chat/completions", OAuth: true, Models: codeBuddyModels},
	{ID: "codebuddy-ai", Alias: "cbai", Name: "CodeBuddy (International)", Dialect: DialectOpenAI, BaseURL: "https://www.codebuddy.ai/v2/chat/completions", OAuth: true, Models: codeBuddyModels},
	{ID: "codebuddy-cn", Alias: "cbcn", Name: "CodeBuddy (Tencent CN, reseller key)", Dialect: DialectOpenAI, BaseURL: "https://copilot.tencent.com/v2/chat/completions", Models: codeBuddyCNModels},
	{ID: "qwen", Alias: "qwen", Name: "Qwen (Alibaba)", Dialect: DialectOpenAI, BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions", OAuth: true},
	{ID: "iflow", Alias: "iflow", Name: "iFlow", Dialect: DialectOpenAI, BaseURL: "https://apis.iflow.cn/v1/chat/completions", OAuth: true, Headers: map[string]string{"User-Agent": "iFlow-Cli"},
		// iFlow's /models endpoint 404s; advisory catalog. Source: iFlow model
		// router (mastra.ai/models/providers/iflowcn) — bare ids (the iflowcn/ prefix
		// is the router's, not iFlow's API id).
		Models: []string{
			"deepseek-r1", "deepseek-v3", "deepseek-v3.2", "glm-4.6",
			"kimi-k2", "kimi-k2-0905", "qwen3-235b", "qwen3-235b-a22b-instruct",
			"qwen3-235b-a22b-thinking-2507", "qwen3-32b", "qwen3-coder-plus",
			"qwen3-max", "qwen3-max-preview", "qwen3-vl-plus",
		}},
	{ID: "glm-cn", Alias: "glmcn", Name: "GLM (bigmodel.cn)", Dialect: DialectOpenAI, BaseURL: "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions"},
	{ID: "kilocode", Alias: "kilo", Name: "Kilo Code", Dialect: DialectOpenAI, BaseURL: "https://api.kilo.ai/api/openrouter/chat/completions", OAuth: true},
	{ID: "cline", Alias: "cline", Name: "Cline", Dialect: DialectOpenAI, BaseURL: "https://api.cline.bot/api/v1/chat/completions", OAuth: true, Headers: map[string]string{"HTTP-Referer": "https://cline.bot", "X-Title": "Cline"}},
	{ID: "longcat", Alias: "longcat", Name: "LongCat (Meituan)", Dialect: DialectOpenAI, BaseURL: "https://api.longcat.chat/openai/v1/chat/completions"},
	{ID: "alicode", Alias: "alicode", Name: "Alibaba Code", Dialect: DialectOpenAI, BaseURL: "https://coding.dashscope.aliyuncs.com/v1/chat/completions", Models: aliCoderModels},
	{ID: "alicode-intl", Alias: "alicodeintl", Name: "Alibaba Code (Intl)", Dialect: DialectOpenAI, BaseURL: "https://coding-intl.dashscope.aliyuncs.com/v1/chat/completions", Models: aliCoderModels},
	// Alibaba DashScope / Model Studio — the GENERAL-PURPOSE OpenAI-compatible
	// endpoints (distinct from the coding-assistant "alicode" hosts above). Unlike
	// the coding hosts, the compatible-mode base serves a working GET /models list
	// (OpenAI convention) and the standard chat/completions, so "fetch models on
	// add" and the dashboard model count work here. Auth is Bearer $DASHSCOPE_API_KEY.
	// Three regional bases (China / International-Singapore / US); pick the one your
	// key was issued in. Source: Alibaba Cloud Model Studio OpenAI-compat docs.
	{ID: "dashscope", Alias: "dashscope", Name: "Alibaba Model Studio (China)", Dialect: DialectOpenAI, BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions"},
	{ID: "dashscope-intl", Alias: "dashscopeintl", Name: "Alibaba Model Studio (Intl)", Dialect: DialectOpenAI, BaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1/chat/completions"},
	{ID: "dashscope-us", Alias: "dashscopeus", Name: "Alibaba Model Studio (US)", Dialect: DialectOpenAI, BaseURL: "https://dashscope-us.aliyuncs.com/compatible-mode/v1/chat/completions"},
	{ID: "gitlab", Alias: "gitlab", Name: "GitLab Duo", Dialect: DialectOpenAI, BaseURL: "https://gitlab.com/api/v4/chat/completions", OAuth: true},

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

	// GitHub Copilot: OpenAI-compatible inference at api.githubcopilot.com, auth via
	// the OAuth device flow (auth/github_oauth.go) that mints a short-lived Copilot
	// token. The editor/copilot headers are REQUIRED by the Copilot gateway.
	{ID: "github", Alias: "ghcp", Name: "GitHub Copilot", Dialect: DialectOpenAI, BaseURL: "https://api.githubcopilot.com/chat/completions", OAuth: true,
		Headers: map[string]string{
			"copilot-integration-id": "vscode-chat",
			"editor-version":         "vscode/1.110.0",
			"editor-plugin-version":  "copilot-chat/0.38.0",
			"User-Agent":             "GitHubCopilotChat/0.38.0",
			"openai-intent":          "conversation-panel",
			"x-github-api-version":   "2025-04-01",
		}},

	// ---- Additional OpenAI-compatible providers ported from 9router's APIKEY_PROVIDERS
	// set (open-sse/config/providers.js). Bearer auth + DialectOpenAI unless noted.
	// These had backend config in 9router (some with the UI entry commented out) but
	// no Kiro-Go catalog row yet. All bring-your-own-key. ----
	{ID: "byteplus", Alias: "bpm", Name: "BytePlus ModelArk", Dialect: DialectOpenAI, BaseURL: "https://ark.ap-southeast.bytepluses.com/api/coding/v3/chat/completions"},
	{ID: "volcengine-ark", Alias: "ark", Name: "Volcengine Ark", Dialect: DialectOpenAI, BaseURL: "https://ark.cn-beijing.volces.com/api/coding/v3/chat/completions"},
	{ID: "blackbox", Alias: "blackbox", Name: "Blackbox AI", Dialect: DialectOpenAI, BaseURL: "https://api.blackbox.ai/chat/completions"},
	{ID: "opencode-go", Alias: "ocgo", Name: "OpenCode Go", Dialect: DialectOpenAI, BaseURL: "https://opencode.ai/zen/go/v1/chat/completions"},
	{ID: "reka", Alias: "reka", Name: "Reka AI", Dialect: DialectOpenAI, BaseURL: "https://api.reka.ai/v1/chat/completions"},
	{ID: "nlpcloud", Alias: "nlpcloud", Name: "NLP Cloud", Dialect: DialectOpenAI, BaseURL: "https://api.nlpcloud.io/v1/gpu/chatbot"},
	{ID: "bazaarlink", Alias: "bazaarlink", Name: "BazaarLink", Dialect: DialectOpenAI, BaseURL: "https://bazaarlink.ai/api/v1/chat/completions"},
	{ID: "completions", Alias: "completions", Name: "Completions.me", Dialect: DialectOpenAI, BaseURL: "https://completions.me/api/v1/chat/completions"},
	{ID: "freetheai", Alias: "freetheai", Name: "FreeTheAI", Dialect: DialectOpenAI, BaseURL: "https://api.freetheai.xyz/v1/chat/completions"},
	{ID: "llm7", Alias: "llm7", Name: "LLM7", Dialect: DialectOpenAI, BaseURL: "https://api.llm7.io/v1/chat/completions"},
	{ID: "lepton", Alias: "lepton", Name: "Lepton AI", Dialect: DialectOpenAI, BaseURL: "https://api.lepton.ai/api/v1/chat/completions"},
	{ID: "predibase", Alias: "predibase", Name: "Predibase", Dialect: DialectOpenAI, BaseURL: "https://serving.app.predibase.com/v1/chat/completions"},
	{ID: "nous-research", Alias: "nous", Name: "Nous Research", Dialect: DialectOpenAI, BaseURL: "https://inference-api.nousresearch.com/v1/chat/completions"},
	{ID: "publicai", Alias: "publicai", Name: "Public AI", Dialect: DialectOpenAI, BaseURL: "https://api.publicai.co/v1/chat/completions"},
	{ID: "glhf", Alias: "glhf", Name: "glhf.chat", Dialect: DialectOpenAI, BaseURL: "https://glhf.chat/api/openai/v1/chat/completions"},
	{ID: "puter", Alias: "puter", Name: "Puter AI", Dialect: DialectOpenAI, BaseURL: "https://api.puter.com/puterai/openai/v1/chat/completions"},
	{ID: "lambda", Alias: "lambda", Name: "Lambda", Dialect: DialectOpenAI, BaseURL: "https://api.lambda.ai/v1/chat/completions"},
	// enally uses an x-api-key header instead of Bearer (per 9router config).
	{ID: "enally", Alias: "enally", Name: "Enally", Dialect: DialectOpenAI, AuthHeader: "x-api-key", BaseURL: "https://ai.enally.in/v1/chat/completions"},

	// ---- Anthropic Messages dialect (x-api-key auth) ----
	{ID: "anthropic", Alias: "anthropic", Name: "Anthropic", Dialect: DialectAnthropic, BaseURL: "https://api.anthropic.com/v1/messages", Headers: sharedClaudeHeaders},
	// Claude Code: same Anthropic endpoint as the api-key "anthropic" row, but auth
	// is an OAuth Bearer token (auth/claude_oauth.go), NOT x-api-key — so AuthHeader
	// is "bearer" and the oauth beta flag is added to the version/beta headers.
	{ID: "claude-code", Alias: "cc", Name: "Claude Code (OAuth)", Dialect: DialectAnthropic, BaseURL: "https://api.anthropic.com/v1/messages", AuthHeader: "bearer", OAuth: true,
		Headers: map[string]string{
			"Anthropic-Version": "2023-06-01",
			"Anthropic-Beta":    "oauth-2025-04-20,claude-code-20250219,interleaved-thinking-2025-05-14",
		}},
	{ID: "glm", Alias: "glm", Name: "GLM Coding", Dialect: DialectAnthropic, BaseURL: "https://api.z.ai/api/anthropic/v1/messages", Headers: sharedClaudeHeaders},
	{ID: "kimi", Alias: "kimi", Name: "Kimi", Dialect: DialectAnthropic, BaseURL: "https://api.kimi.com/coding/v1/messages", Headers: sharedClaudeHeaders},
	{ID: "minimax", Alias: "minimax", Name: "MiniMax", Dialect: DialectAnthropic, BaseURL: "https://api.minimax.io/anthropic/v1/messages", Headers: sharedClaudeHeaders},
	{ID: "minimax-cn", Alias: "minimaxcn", Name: "MiniMax (CN)", Dialect: DialectAnthropic, BaseURL: "https://api.minimaxi.com/anthropic/v1/messages", Headers: sharedClaudeHeaders},
	{ID: "agentrouter", Alias: "ar", Name: "AgentRouter", Dialect: DialectAnthropic, BaseURL: "https://agentrouter.org/v1/messages", Headers: sharedClaudeHeaders},
	// Kimi Coding (Moonshot): Anthropic-compatible coding endpoint, but auth is the
	// OAuth device flow (auth/kimi_coding_oauth.go), not a pasted api key. /models 404s.
	{ID: "kimi-coding", Alias: "kc", Name: "Kimi Coding (Moonshot)", Dialect: DialectAnthropic, BaseURL: "https://api.kimi.com/coding/v1/messages", Headers: sharedClaudeHeaders, OAuth: true},

	// ---- Google Gemini dialect (x-goog-api-key auth) ----
	{ID: "gemini", Alias: "gemini", Name: "Gemini", Dialect: DialectGemini, BaseURL: "https://generativelanguage.googleapis.com/v1beta/models"},
	// Gemini CLI (Cloud Code Assist): bespoke provider (proxy/provider_gemini_cli.go)
	// with Google OAuth login. BaseURL is informational — the provider hardcodes the
	// cloudcode-pa endpoint.
	{ID: "gemini-cli", Alias: "gcli", Name: "Gemini CLI (Cloud Code Assist)", Dialect: DialectGeminiCLI, BaseURL: "https://cloudcode-pa.googleapis.com/v1internal", OAuth: true},
	// Antigravity: second Cloud Code Assist provider (bespoke, proxy/provider_antigravity.go),
	// Google OAuth login with its own client + the daily-cloudcode endpoint.
	{ID: "antigravity", Alias: "ag", Name: "Antigravity (Cloud Code Assist)", Dialect: DialectGeminiCLI, BaseURL: "https://daily-cloudcode-pa.googleapis.com/v1internal", OAuth: true},
	// Vertex AI: bespoke provider (proxy/provider_vertex.go) with Service-Account JSON
	// auth. Dialect is informational — the provider builds the regional URL + Gemini
	// body itself. OAuth:true routes it to the SA-import connect flow, not api-key paste.
	{ID: "vertex", Alias: "vertex", Name: "Vertex AI (Service Account)", Dialect: DialectGemini, BaseURL: "https://aiplatform.googleapis.com", OAuth: true},

	// ---- Ollama dialect (/api/chat NDJSON) ----
	// ollama is the hosted Ollama Cloud (Bearer api key); ollama-local targets a
	// local daemon (no auth). Both speak /api/chat and list models at /api/tags.
	{ID: "ollama", Alias: "ollama", Name: "Ollama Cloud", Dialect: DialectOllama, BaseURL: "https://ollama.com/api/chat"},
	{ID: "ollama-local", Alias: "ollamalocal", Name: "Ollama (Local)", Dialect: DialectOllama, BaseURL: "http://localhost:11434/api/chat"},

	// ---- Web-subscription providers (cookie auth, bespoke providers) ----
	// grok-web (grok.com sso cookie) and perplexity-web (perplexity.ai session
	// cookie). Dialect is informational — both have bespoke providers that build the
	// request and parse the custom stream. OAuth:true routes to the cookie-import flow.
	{ID: "grok-web", Alias: "gw", Name: "Grok (Web Subscription)", Dialect: DialectOpenAI, BaseURL: "https://grok.com/rest/app-chat/conversations/new", OAuth: true},
	{ID: "perplexity-web", Alias: "pw", Name: "Perplexity (Web Subscription)", Dialect: DialectOpenAI, BaseURL: "https://www.perplexity.ai/rest/sse/perplexity_ask", OAuth: true},
	// Cursor IDE: bespoke provider (proxy/provider_cursor.go) speaking Connect-RPC
	// protobuf. Token + machine id are imported from the Cursor IDE. Dialect is
	// informational. OAuth:true routes to the token-import flow.
	{ID: "cursor", Alias: "cur", Name: "Cursor IDE", Dialect: DialectOpenAI, BaseURL: "https://api2.cursor.sh", OAuth: true},

	// ---- Embedding providers (OpenAI-compatible /embeddings; served via the
	// /v1/embeddings passthrough, see embeddings.go) ----
	{ID: "voyage-ai", Alias: "voyage", Name: "Voyage AI (Embeddings)", Dialect: DialectOpenAI, BaseURL: "https://api.voyageai.com/v1/embeddings"},
	{ID: "jina-ai", Alias: "jina", Name: "Jina AI (Embeddings)", Dialect: DialectOpenAI, BaseURL: "https://api.jina.ai/v1/embeddings"},
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

// backendShipsStaticCatalog reports whether a backend id resolves to a provider
// that has NO working GET /models endpoint and therefore ships a hardcoded
// advisory catalog (builtinProvider.Models). For these backends a live /models
// fetch will always 404/error, so the background model refresh must NOT keep
// hitting the upstream every tick — it just wastes a network round-trip and
// re-logs the same advisory fallback. The static list is seeded once (on add /
// cold start); thereafter only quota is refreshed.
//
// For a built-in catalog entry, a non-empty Models field IS the signal: it is
// populated only for no-/models providers (see provider_catalog.go comments).
// For a user-defined config.ProviderConfig we require Models AND NOT FetchModels
// — a config that pins Models as extra ids but also sets FetchModels has a
// working /models endpoint whose live list is unioned with the pinned ids, so we
// must keep refreshing it.
func backendShipsStaticCatalog(backend string) bool {
	key := strings.ToLower(strings.TrimSpace(backend))
	if bp, ok := builtinByID[key]; ok && len(bp.Models) > 0 {
		return true
	}
	if pc, ok := config.GetProviderConfig(key); ok && len(pc.Models) > 0 && !pc.FetchModels {
		return true
	}
	// Self-contained custom account (backend id is its own routing prefix):
	// the static catalog lives on the account as CustomModels. Custom accounts
	// have no FetchModels toggle, so a non-empty CustomModels IS the static-only
	// signal — skip the pointless live /models fetch that always 404s. The
	// sibling lookup (GetCustomAccountByBackend) covers bulk-added keys on the
	// same backend whose inline fields live on the first-added sibling.
	if acct, ok := config.GetCustomAccountByBackend(key); ok && len(acct.CustomModels) > 0 {
		return true
	}
	return false
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
