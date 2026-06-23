package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"strings"
)

// Provider-agnostic harness neutralizer.
//
// Every upstream that is NOT first-party Anthropic needs the inbound Claude Code
// harness system prompt de-fingerprinted before it is forwarded: the full
// behavioral contract (tone, tool-use discipline, output format, agent loop) is
// KEPT so the model stays capable, but the brand/identity tokens that mark it as
// "Claude Code talking to Anthropic" are rewritten to the upstream's own brand,
// and the tokens that hard-fail validation (the Bedrock-reserved billing header)
// are removed. This replaces the two divergent ad-hoc paths that used to live in
// translator.go (Kiro-only) and codebuddy_sanitize.go (CodeBuddy-only) with one
// registry-driven path so a NEW provider is onboarded by adding a single
// brandProfile entry — not by writing another bespoke sanitizer.

// brandProfile is one provider's neutralization ruleset. replacer rewrites ONLY
// the Claude Code harness identity phrases — the self-identification tagline and
// the "Claude Code" product name. It must never match bare "Claude"/"Anthropic"
// words: those occur in real context the model depends on (working directories
// like "claude_core", paths like ".claude/", filenames like "CLAUDE.md", package
// names like "@anthropic-ai/...", model ids like "claude-sonnet-4.6"), and
// rewriting them silently corrupts the request so the model can no longer find
// the user's actual files. Matching exact product phrases is collision-free with
// such identifiers. moderateContent additionally applies the same phrase rewrite
// to non-system message and tool content for providers (CodeBuddy) that scan the
// whole body.
type brandProfile struct {
	replacer        *strings.Replacer
	moderateContent bool
	// stripIdentityLine removes the "You are Claude Code…" self-identification
	// sentence entirely instead of rebranding it. Set for slot-less backends
	// (Kiro) where the neutralized prompt is prepended to the user turn: a
	// rebranded "You are Kiro…" sentence there reads as user-typed text and
	// derails the model into injection-detection. Backends with a real
	// role:system slot leave this false (rebrand is invisible as identity).
	stripIdentityLine bool
}

func tencentReplacer() *strings.Replacer {
	return strings.NewReplacer(
		"Anthropic's official CLI for Claude", "Tencent's official CLI for CodeBuddy",
		"Claude Code", "CodeBuddy Code",
	)
}

// brandProfiles maps a resolved backend id to its identity-rewrite profile. The
// keys are the same lowercase backend ids used everywhere else
// (config.GetAccountBackend / ParseModelBackend). To onboard a provider, add one
// row here — no other code changes are required.
//
// Providers absent from this map fall back to defaultBrandProfile (generic
// "the assistant" wording).
var brandProfiles = map[string]*brandProfile{
	"kiro": {replacer: strings.NewReplacer(
		"Anthropic's official CLI for Claude", "Amazon's official CLI for Kiro",
		"Claude Code", "Kiro",
	), stripIdentityLine: true},
	"codebuddy":    {replacer: tencentReplacer(), moderateContent: true},
	"codebuddy-ai": {replacer: tencentReplacer(), moderateContent: true},
	"codebuddy-cn": {replacer: tencentReplacer(), moderateContent: true},
	"rcodebuddycn": {replacer: tencentReplacer(), moderateContent: true},
	"qoder": {replacer: strings.NewReplacer(
		"Anthropic's official CLI for Claude", "Qoder's official CLI",
		"Claude Code", "Qoder",
	)},
}

// defaultBrandProfile rewrites the harness identity phrases to neutral wording for
// any backend without a bespoke profile. Like the others it touches only the
// product-identity phrases, never bare brand words in context.
var defaultBrandProfile = &brandProfile{replacer: strings.NewReplacer(
	"Anthropic's official CLI for Claude", "the assistant's official CLI",
	"Claude Code", "the assistant",
)}

// brandProfileFor returns the rewrite profile for a backend id, falling back to
// defaultBrandProfile. Backend matching is case-insensitive and trimmed.
func brandProfileFor(backend string) *brandProfile {
	if p, ok := brandProfiles[strings.ToLower(strings.TrimSpace(backend))]; ok {
		return p
	}
	return defaultBrandProfile
}

// neutralizeHarness runs the full provider-agnostic neutralization on a system
// prompt for the given backend, keeping the entire harness contract and only
// de-fingerprinting it. It is the single implementation behind both the Kiro
// translator path and the generic-provider body path. Passes, in order:
//
//  1. Remove the x-anthropic-billing-header line (Bedrock rejects it as a
//     reserved keyword with a 400 before the model runs — anthropics/claude-code
//     #24168). Harmless to strip for every backend.
//  2. Drop noise <system-reminder> blocks (deferred-tool catalog, environment,
//     git status — env scaffolding and a structural fingerprint) while PRESERVING
//     memory-carrying reminders (CLAUDE.md / AGENTS.md). Real tools ride in the
//     structured tools field, so dropping noise reminders never breaks tools.
//  3. Apply the shared noise-strip + operator-defined rule chain.
//  4. Rewrite brand/identity tokens for THIS backend so the kept harness is not
//     recognizable as the Claude Code harness. Instructions are unchanged — only
//     the identity labels.
//
// No synthetic identity line is injected: on slot-less backends like Kiro the
// returned text is prepended to the user's turn, where a fabricated "You are X…"
// sentence reads as user-typed text and derails the model into injection
// detection (the prior failure this design avoids).
func neutralizeHarness(prompt, backend string) string {
	cfg := config.GetPromptFilterConfig()

	prompt = billingHeaderLineRe.ReplaceAllString(prompt, "")
	prompt = billingHeaderInlineRe.ReplaceAllString(prompt, "")

	prompt = systemReminderBlockRe.ReplaceAllStringFunc(prompt, func(block string) string {
		if reminderCarriesUserMemory(block) {
			return block
		}
		return ""
	})

	prompt = applySharedFiltersWithConfig(prompt, cfg)
	profile := brandProfileFor(backend)
	if profile.stripIdentityLine {
		prompt = harnessIdentityLineRe.ReplaceAllString(prompt, "")
	}
	if profile.moderateContent {
		prompt = softenModerationVocabulary(prompt)
	}
	prompt = profile.replacer.Replace(prompt)

	return strings.TrimSpace(collapseBlankLines(prompt))
}

// neutralizeProviderBody neutralizes a marshaled OpenAI-dialect request body for
// a non-Kiro provider: each system/developer message is neutralized in place
// (full harness kept, de-branded) via neutralizeHarness, and — for providers
// whose profile sets moderateContent — residual brand tokens are also rewritten
// across every other message and tool string so an active moderation pass can't
// flag them. The top-level "model" id is never touched (provider model ids such
// as "claude-sonnet-4.6" legitimately contain "claude"). On any parse/marshal
// error the body is returned unchanged (fail-open: a moderation rejection beats a
// corrupted request).
func neutralizeProviderBody(body []byte, backend string) []byte {
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return body
	}
	profile := brandProfileFor(backend)

	if sys, ok := root["system"]; ok {
		if s, ok := sys.(string); ok {
			if isClaudeCodeSystemPrompt(s) {
				root["system"] = neutralizeHarness(s, backend)
			} else if profile.moderateContent {
				root["system"] = neutralizeBrandValue(s, profile)
			}
		} else if profile.moderateContent {
			root["system"] = neutralizeBrandValue(sys, profile)
		}
	}

	if msgs, ok := root["messages"].([]interface{}); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			if role == "system" || role == "developer" {
				// Rewrite the non-standard "developer" role to the universally
				// accepted "system" role so that upstream providers (notably
				// Chinese gateways with active content moderation) do not flag
				// or reject the request on role alone.  The semantic meaning is
				// identical — both carry developer/system instructions — and
				// "system" is the role every OpenAI-compatible backend expects.
				if role == "developer" {
					msg["role"] = "system"
				}
				if s, ok := msg["content"].(string); ok {
					if isClaudeCodeSystemPrompt(s) {
						msg["content"] = neutralizeHarness(s, backend)
					} else if profile.moderateContent {
						msg["content"] = neutralizeBrandValue(s, profile)
					}
				} else if profile.moderateContent {
					msg["content"] = neutralizeBrandValue(msg["content"], profile)
				}
				continue
			}
			if profile.moderateContent {
				msg["content"] = neutralizeBrandValue(msg["content"], profile)
			}
		}
	}

	if profile.moderateContent {
		for k, v := range root {
			if k == "model" || k == "messages" {
				continue
			}
			root[k] = neutralizeBrandValue(v, profile)
		}
	}

	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

// neutralizeBrandValue recursively rewrites brand tokens in any string within a
// decoded JSON value using the given profile's replacer.
func neutralizeBrandValue(v interface{}, profile *brandProfile) interface{} {
	switch t := v.(type) {
	case string:
		return profile.replacer.Replace(softenModerationVocabulary(t))
	case []interface{}:
		for i := range t {
			t[i] = neutralizeBrandValue(t[i], profile)
		}
		return t
	case map[string]interface{}:
		for k, val := range t {
			t[k] = neutralizeBrandValue(val, profile)
		}
		return t
	default:
		return v
	}
}
