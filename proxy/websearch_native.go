package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"strings"
)

// ============================================================================
// Provider-native web search.
//
// Web search is resolved PROVIDER-NATIVE FIRST, with Kiro's MCP endpoint used
// only as a fallback. This is the opposite of the original design (which routed
// every web search through a Kiro account) — a user on DashScope/Qwen, Gemini,
// or a direct Anthropic key should get web search from THEIR provider, and must
// not need a Kiro account at all.
//
// Resolution order, per request that carries a web_search tool (feature on):
//
//  1. NATIVE — the inference provider has its own web search. We inject the
//     provider's native switch into the outbound body and let it run the search
//     server-side; the grounded answer comes back as normal text. No agentic
//     loop, no Kiro account.
//       - DashScope / Qwen (OpenAI-compatible):  enable_search: true
//       - Gemini (generateContent):              tools += {google_search:{}}
//       - Anthropic (api.anthropic.com):         tools += web_search_20250305
//  2. EMULATE — the inference provider has NO native search, but a usable Kiro
//     account exists. We run the agentic loop: generate on the request's own
//     backend, execute each search via the Kiro MCP endpoint, feed results back.
//     (For a Kiro-backed request this is the original Kiro path.)
//  3. NONE — no native capability and no Kiro account. We drop the web_search
//     tool and let the model answer from training. NEVER a 404.
//
// This file owns step 1's capability detection + body injection, and the
// emulate-vs-native decision the handler routes on.
// ============================================================================

// nativeWebSearchKind classifies how a provider performs web search natively, or
// "" when it has no native capability. The value selects the injection shape.
const (
	nativeSearchDashScope = "dashscope" // OpenAI-compatible enable_search
	nativeSearchGemini    = "gemini"    // google_search grounding tool
	nativeSearchAnthropic = "anthropic" // hosted web_search_20250305 tool
)

// nativeWebSearchKindForSettings reports the native web-search mechanism for a
// resolved providerSettings, or "" if none. Used by the generic provider when it
// builds the outbound body.
func nativeWebSearchKindForSettings(ps providerSettings) string {
	return nativeWebSearchKind(ps.dialect, ps.id, ps.baseURL)
}

// nativeWebSearchKindForBackend resolves the native web-search mechanism for a
// backend id WITHOUT a live account, by reading the same sources
// resolveProviderSettings layers (built-in catalog, user ProviderConfig,
// self-contained custom account). Used by the handler to decide native vs
// emulate before account selection. Returns "" for Kiro (which has no native
// hosted tool — it is the emulation engine) and unknown backends.
func nativeWebSearchKindForBackend(backend string) string {
	b := strings.ToLower(strings.TrimSpace(backend))
	if b == "" || b == "kiro" {
		return ""
	}
	if bp, ok := resolveBuiltinProvider(b); ok {
		return nativeWebSearchKind(bp.Dialect, bp.ID, bp.BaseURL)
	}
	if pc, ok := config.GetProviderConfig(b); ok {
		return nativeWebSearchKind(Dialect(strings.ToLower(strings.TrimSpace(pc.Dialect))), pc.ID, pc.BaseURL)
	}
	if acct, ok := config.GetCustomAccountByBackend(b); ok {
		return nativeWebSearchKind(Dialect(strings.ToLower(strings.TrimSpace(acct.CustomDialect))), b, acct.BaseURLOverride)
	}
	return ""
}

// nativeWebSearchKind is the shared classifier. dialect drives the default; id +
// baseURL disambiguate OpenAI-compatible hosts (only DashScope ships a native
// enable_search) and gate the Anthropic hosted tool to real anthropic.com (an
// Anthropic-COMPATIBLE host like GLM/Kimi would 400 on the hosted tool).
func nativeWebSearchKind(dialect Dialect, id, baseURL string) string {
	lid := strings.ToLower(strings.TrimSpace(id))
	lurl := strings.ToLower(strings.TrimSpace(baseURL))
	switch dialect {
	case DialectGemini:
		// Google Search grounding is broadly available across Gemini models.
		return nativeSearchGemini
	case DialectOpenAI:
		// DashScope / Alibaba Model Studio expose a native enable_search switch.
		// Detect by provider id (qwen + the alicode coding hosts) or by the
		// DashScope/aliyuncs base URL (covers self-contained custom accounts that
		// paste the compatible-mode endpoint, e.g. dashscope-intl).
		if lid == "qwen" || lid == "alicode" || lid == "alicode-intl" ||
			strings.Contains(lurl, "dashscope") || strings.Contains(lurl, "aliyuncs") {
			return nativeSearchDashScope
		}
		return ""
	case DialectAnthropic:
		// Only real Anthropic accepts the hosted web_search tool. Anthropic-
		// COMPATIBLE third parties (glm/kimi/minimax) do not, so don't forward it
		// to them — they fall back to emulate/none instead.
		if strings.Contains(lurl, "anthropic.com") {
			return nativeSearchAnthropic
		}
		return ""
	}
	return ""
}

// nrHasWebSearch reports whether a NormalizedRequest carries a web_search tool,
// across both client dialects.
func nrHasWebSearch(nr *NormalizedRequest) bool {
	if nr == nil {
		return false
	}
	if nr.Claude != nil {
		if _, ok := findClaudeWebSearchTool(nr.Claude.Tools); ok {
			return true
		}
	}
	if nr.OpenAI != nil {
		for _, t := range nr.OpenAI.Tools {
			name := strings.ToLower(strings.TrimSpace(t.Function.Name))
			typ := strings.ToLower(strings.TrimSpace(t.Type))
			if name == webSearchToolName || strings.HasPrefix(typ, "web_search") {
				return true
			}
		}
	}
	return false
}

// injectNativeWebSearch rewrites an already-built request body to turn on the
// provider's native web search per kind. It decodes the body to a generic map,
// adds the native switch, and re-encodes. On any decode/encode error it returns
// the original body unchanged so a malformed injection can never break the call.
func injectNativeWebSearch(kind string, body []byte) []byte {
	if kind == "" || len(body) == 0 {
		return body
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}

	switch kind {
	case nativeSearchDashScope:
		// DashScope OpenAI-compatible: a top-level boolean enables Qwen's built-in
		// web search. search_options is left to the provider's defaults (verified
		// minimal shape) so we don't depend on an unconfirmed sub-schema.
		m["enable_search"] = true

	case nativeSearchGemini:
		// Append the google_search grounding tool alongside any functionDeclarations.
		m["tools"] = appendToolEntry(m["tools"], map[string]interface{}{"google_search": map[string]interface{}{}})

	case nativeSearchAnthropic:
		// Re-add the hosted web_search tool the dialect translator dropped.
		m["tools"] = appendToolEntry(m["tools"], map[string]interface{}{
			"type":     "web_search_20250305",
			"name":     webSearchToolName,
			"max_uses": maxWebSearchResults,
		})
	default:
		return body
	}

	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// appendToolEntry appends entry to a tools value that may be nil, a []interface{},
// or absent, returning the new slice. Used to add a native-search tool to either
// dialect's tools array without clobbering existing function tools.
func appendToolEntry(existing interface{}, entry map[string]interface{}) []interface{} {
	switch v := existing.(type) {
	case []interface{}:
		return append(v, entry)
	case nil:
		return []interface{}{entry}
	default:
		// Unexpected shape (shouldn't happen) — start a fresh array so we never panic.
		return []interface{}{entry}
	}
}

// shouldEmulateWebSearch reports whether the proxy should run the Kiro-backed
// agentic web-search loop for a request on `backend`.
//
// PROVIDER ISOLATION: a request explicitly routed to a provider (e.g.
// "fable/claude-fable-5") must stay entirely on that provider — it must never
// borrow another provider's account. So Kiro-backed web-search emulation is
// engaged ONLY for a Kiro request. A non-Kiro backend either does search
// NATIVELY (the generic provider injects the switch into its own outbound body)
// or the web_search tool is dropped and the model answers without it. A non-Kiro
// request is never silently re-routed through a Kiro account for the MCP search
// side-call — that cross-provider mixing is what this gate exists to prevent.
//
// Returns false when the provider is native (it injects its own search), for any
// non-Kiro backend (stay isolated), or when no usable Kiro account exists for a
// Kiro request (the tool is dropped instead).
func (h *Handler) shouldEmulateWebSearch(backend string) bool {
	if nativeWebSearchKindForBackend(backend) != "" {
		return false // provider does it natively
	}
	b := strings.ToLower(strings.TrimSpace(backend))
	if b == "" || b == "kiro" {
		// Kiro path: emulate via its own MCP (the inference account IS a Kiro
		// account, so this is self-contained). Still require a usable Kiro account
		// so a tokenless pool degrades to "drop tool".
		return h.firstUsableKiroAccount() != nil
	}
	// Non-Kiro, non-native (e.g. a custom anthropic/openai-compatible provider):
	// do NOT borrow a Kiro account. The request is routed to THIS provider and
	// must stay on it; the web_search tool is dropped downstream and the model
	// answers from its own knowledge. Native-search providers are handled above.
	return false
}

// firstUsableKiroAccount returns a Kiro account with a valid token for the MCP
// web-search side-call, or nil if the pool has no usable Kiro account. The search
// MCP endpoint is Kiro-specific, so the side-call MUST run on a Kiro account even
// when inference runs on a different provider. Prefers an enabled account; tops up
// its token before returning.
func (h *Handler) firstUsableKiroAccount() *config.Account {
	for _, a := range config.GetAccounts() {
		if !a.Enabled {
			continue
		}
		if config.GetAccountBackend(&a) != "kiro" {
			continue
		}
		if strings.TrimSpace(a.AccessToken) == "" && strings.TrimSpace(a.RefreshToken) == "" {
			continue
		}
		acct := a
		_ = h.ensureValidToken(&acct)
		if strings.TrimSpace(acct.AccessToken) == "" {
			continue
		}
		return &acct
	}
	return nil
}
