package proxy

import (
	"kiro-go/config"
	"strings"
)

// ParseModelBackend resolves an inbound model string to (backend, upstreamModel).
//
// 9router's routing convention is "provider/model" (e.g. "groq/llama-3.3-70b",
// "or/anthropic/claude-sonnet-4.5", "cx/gpt-5-codex"). We split on the FIRST
// "/", look the prefix up as a provider id or alias, and if it matches, the
// remainder is the upstream model id. If the prefix is not a known provider, the
// whole string is treated as a Kiro model (backend "kiro", upstreamModel = the
// original) — so every existing Claude/OpenAI model id ("claude-opus-4-7",
// "gpt-4o", "auto", ...) continues to route to Kiro exactly as before.
//
// Returns:
//   - backend: the resolved provider id ("kiro" for unprefixed / unknown-prefix).
//   - upstreamModel: the model id to send upstream (prefix stripped for a matched
//     provider; the original string for Kiro).
func ParseModelBackend(model string) (backend, upstreamModel string) {
	m := strings.TrimSpace(model)
	if m == "" {
		return "kiro", m
	}
	slash := strings.Index(m, "/")
	if slash <= 0 || slash == len(m)-1 {
		return "kiro", m
	}
	prefix := strings.ToLower(m[:slash])
	rest := m[slash+1:]

	if id, ok := resolveProviderPrefix(prefix); ok {
		return id, rest
	}
	// Unknown prefix — not a provider route. Treat the whole string as a Kiro
	// model (it may legitimately contain a slash, e.g. a vendor-qualified id).
	return "kiro", m
}

// resolveProviderPrefix maps a routing prefix (provider id or alias) to a backend
// id. Checks built-in ids, built-in aliases, then user-defined ProviderConfig
// ids/aliases. Also recognizes the bespoke backends (kiro/codex/qoder).
func resolveProviderPrefix(prefix string) (string, bool) {
	p := strings.ToLower(strings.TrimSpace(prefix))
	switch p {
	case "kiro", "kr":
		return "kiro", true
	case "codex", "cx":
		return "codex", true
	case "qoder", "qd":
		return "qoder", true
	}
	if bp, ok := builtinByID[p]; ok {
		return bp.ID, true
	}
	if id, ok := builtinByAlias[p]; ok {
		return id, true
	}
	// User-defined providers.
	if pc, ok := config.GetProviderConfig(p); ok {
		return pc.ID, true
	}
	for _, pc := range config.GetProviders() {
		if strings.EqualFold(pc.Alias, p) {
			return pc.ID, true
		}
	}
	return "", false
}
