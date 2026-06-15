package proxy

import (
	"context"
	"fmt"
	"kiro-go/config"
	"strings"
)

// Dialect identifies the wire format a provider speaks upstream. It selects the
// request builder, the streaming parser, and the URL/header/auth conventions.
type Dialect string

const (
	DialectKiro      Dialect = "kiro"      // AWS CodeWhisperer binary eventstream
	DialectCodex     Dialect = "codex"     // ChatGPT backend Responses API (SSE)
	DialectOpenAI    Dialect = "openai"    // OpenAI /v1/chat/completions (SSE)
	DialectAnthropic Dialect = "anthropic" // Anthropic /v1/messages (SSE)
	DialectGemini    Dialect = "gemini"    // Google generateContent (SSE)
	DialectOllama    Dialect = "ollama"    // Ollama /api/chat (NDJSON stream)
	DialectGeminiCLI Dialect = "gemini-cli" // Google Cloud Code Assist (wraps generateContent)

	// Client-side dialects (what the inbound request spoke). These tag
	// NormalizedRequest.ClientDialect so a provider can tell whether to expect
	// nr.Claude or nr.OpenAI. The Responses/Codex client funnels into Claude.
	DialectClaude    Dialect = "claude"    // inbound /v1/messages
	DialectResponses Dialect = "responses" // inbound /v1/responses (funnels to Claude)
)

// NormalizedRequest is the canonical internal request handed to a Provider.Call.
// Exactly one of Claude / OpenAI is set, matching the CLIENT dialect that hit the
// proxy (the Responses/Codex client funnels into Claude, preserving today's
// path). A provider that needs a different upstream shape converts from whichever
// side is set via the translate_* helpers.
//
// The Kiro fast-path field carries the prebuilt CodeWhisperer payload so the kiro
// provider reuses the existing, byte-identical build (ClaudeToKiro/OpenAIToKiro
// already ran in the handler). Other providers ignore it.
type NormalizedRequest struct {
	Model         string         // public model id as requested (pre-MapModel)
	ClientDialect Dialect        // claude | openai (responses -> claude)
	Claude        *ClaudeRequest // set for Claude or Responses clients
	OpenAI        *OpenAIRequest // set for OpenAI clients
	Thinking      bool
	Stream        bool
	Effort        string // resolved reasoning effort

	// Allow1MContext is true when the inbound Claude request opted into the 1M
	// context window — signaled by the context-1m-2025-08-07 anthropic-beta
	// header, which Claude Code sends only when the [1M] model variant is active
	// (the plain model id meters against the default 200K window). It gates the
	// window used to back-convert Kiro's contextUsagePercentage into an
	// input-token count: the proxy MUST report tokens against the same window the
	// client meters against, or the client's "% context used" gauge desyncs — a
	// 1M-scaled count read against a 200K assumption pegs at 100% and suppresses
	// auto-compaction (observed in the field). Set only on the Claude path.
	Allow1MContext bool

	// Kiro is the prebuilt CodeWhisperer payload for the kiro provider. Non-nil
	// only on the Kiro path; kiroProvider.Call uses it verbatim so Phase 2 is a
	// pure pass-through with zero behavior change.
	Kiro *KiroPayload
}

// TokenSet generalizes the 5-tuple auth.RefreshToken returns today, so every
// provider can report renewed credentials through one shape. ExpiresAt is Unix
// seconds; 0 means "never expires" (api-key providers). ProfileArn is Kiro-only.
// Extra carries provider-specific fields (e.g. id_token, chatgpt-account-id) the
// caller persists onto the account.
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	ProfileArn   string
	Extra        map[string]string
}

// Provider is the upstream abstraction. Each backend (kiro, codex, openai,
// anthropic, gemini, qoder, ...) implements it. The handler resolves a provider
// per account via ProviderFor and drives it through these three methods; the
// pool, failover, and response rendering are provider-agnostic because Call
// drives the shared KiroStreamCallback.
type Provider interface {
	// Name matches config.Account.Backend (and the registry key). "kiro" for the
	// AWS path.
	Name() string

	// RefreshToken renews credentials. For api-key providers this is a no-op that
	// returns the current (static) credentials with ExpiresAt 0. The contract
	// mirrors auth.RefreshToken: on success the caller persists the returned set.
	RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error)

	// ListModels returns the model catalog this account can serve, in the same
	// ModelInfo shape the Kiro path already produces, so the existing models
	// cache / routing filter work unchanged.
	ListModels(acct *config.Account) ([]ModelInfo, error)

	// Call performs ONE upstream attempt: it translates nr -> the provider wire,
	// streams the response, and drives cb with normalized events. The ctx aborts
	// the in-flight call on cancellation (client disconnect / idle timeout). The
	// error contract matches CallKiroAPIContext: a pre-commit error is retryable
	// by the failover dispatcher; a QuotaError signals 429/cooldown.
	Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error
}

// providerRegistry maps a backend id to its Provider implementation. Populated
// by RegisterProvider in package init() functions (provider_kiro.go registers
// "kiro"; later phases register codex / generic / qoder).
var providerRegistry = map[string]Provider{}

// RegisterProvider adds a provider to the registry under its Name(). Called from
// init(); not safe for concurrent use after startup (the map is read-only once
// the server is serving).
func RegisterProvider(p Provider) {
	providerRegistry[p.Name()] = p
}

// ProviderForBackend resolves a backend id (empty -> "kiro") to its Provider, or
// nil if no implementation is registered. A generic ProviderConfig id resolves
// to the shared generic provider (registered per dialect) in a later phase.
func ProviderForBackend(backend string) Provider {
	if backend == "" {
		backend = "kiro"
	}
	if p, ok := providerRegistry[backend]; ok {
		return p
	}
	// A built-in catalog provider (groq, qwen, alicode, alicode-intl, anthropic,
	// gemini, ...) is served by the shared generic provider for its dialect. These
	// are NOT keyed in providerRegistry by their own id (only "generic:<dialect>"
	// is), and they are not config.ProviderConfig entries, so without this branch
	// every built-in api-key account resolved to nil — surfacing as "no provider
	// registered for backend" on live calls AND silently skipping the on-add /
	// refresh model fetch (the *genericProvider type assertion failed). This is the
	// branch that makes the built-in api-key providers actually work end to end.
	if bp, ok := resolveBuiltinProvider(backend); ok {
		if p, ok := providerRegistry["generic:"+string(bp.Dialect)]; ok {
			return p
		}
	}
	// A user-defined ProviderConfig id resolves to the generic provider for its
	// dialect, if that provider has been registered (Phase 3).
	if pc, ok := config.GetProviderConfig(backend); ok {
		if p, ok := providerRegistry["generic:"+pc.Dialect]; ok {
			return p
		}
	}
	// A self-contained custom account (inline dialect + base URL, no shared
	// ProviderConfig) resolves to the generic provider for its inline dialect.
	if acct, ok := config.GetCustomAccountByBackend(backend); ok {
		if p, ok := providerRegistry["generic:"+strings.ToLower(strings.TrimSpace(acct.CustomDialect))]; ok {
			return p
		}
	}
	return nil
}

// ProviderFor resolves the Provider for an account, defaulting a Backend-less
// (pre-existing) account to the kiro provider.
func ProviderFor(acct *config.Account) Provider {
	return ProviderForBackend(config.GetAccountBackend(acct))
}

// ProviderForOrErr is the nil-safe lookup for call sites that must not panic on
// an account whose backend has no registered provider (e.g. a config left over
// from a removed plugin). Returns a descriptive error instead.
func ProviderForOrErr(acct *config.Account) (Provider, error) {
	p := ProviderFor(acct)
	if p == nil {
		return nil, fmt.Errorf("no provider registered for backend %q", config.GetAccountBackend(acct))
	}
	return p, nil
}
