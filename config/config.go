// Package config provides configuration management for Kiro API Proxy.
//
// This package handles persistent storage and retrieval of:
//   - Account credentials and authentication tokens
//   - Server settings (port, host, API keys)
//   - Usage statistics and metrics
//   - Thinking mode configuration for AI responses
//
// All configuration is stored in a JSON file with thread-safe access
// via read-write mutex protection.
package config

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"kiro-go/logger"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// GenerateMachineId generates a UUID v4 format machine identifier.
// This ID is used to uniquely identify the proxy instance in Kiro API requests,
// helping with request tracking and rate limiting on the server side.
func GenerateMachineId() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	bytes[6] = (bytes[6] & 0x0f) | 0x40 // 版本 4
	bytes[8] = (bytes[8] & 0x3f) | 0x80 // 变体
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// Account represents a Kiro API account with authentication credentials and usage statistics.
type Account struct {
	// Basic identification
	ID       string `json:"id"`                 // Unique account identifier (UUID)
	Email    string `json:"email,omitempty"`    // User email address
	UserId   string `json:"userId,omitempty"`   // Kiro user ID
	Nickname string `json:"nickname,omitempty"` // Display name for admin panel

	// Authentication credentials
	AccessToken  string `json:"accessToken"`            // OAuth access token for API calls
	RefreshToken string `json:"refreshToken"`           // OAuth refresh token for token renewal
	ClientID     string `json:"clientId,omitempty"`     // OIDC client ID (for IdC auth)
	ClientSecret string `json:"clientSecret,omitempty"` // OIDC client secret (for IdC auth)
	AuthMethod   string `json:"authMethod"`             // Authentication method: "idc" (AWS IdC) or "social" (GitHub/Google)
	Provider     string `json:"provider,omitempty"`     // Identity provider name (e.g., "BuilderId", "GitHub")
	Region       string `json:"region"`                 // AWS region for OIDC endpoints
	StartUrl     string `json:"startUrl,omitempty"`     // AWS SSO start URL
	ExpiresAt    int64  `json:"expiresAt,omitempty"`    // Token expiration timestamp (Unix seconds)
	MachineId    string `json:"machineId,omitempty"`    // UUID machine identifier for request tracking
	ProfileArn   string `json:"profileArn,omitempty"`   // CodeWhisperer/Kiro profile ARN for generation requests

	// Per-account outbound proxy (falls back to global ProxyURL if empty)
	ProxyURL string `json:"proxyURL,omitempty"`

	// Priority weight for load balancing (higher = more requests)
	Weight int `json:"weight,omitempty"` // 0 or 1 = normal, 2+ = higher priority

	// Overage behavior after the main usage limit is reached.
	AllowOverage  bool `json:"allowOverage,omitempty"`  // Whether to keep using the account after UsageLimit is reached
	OverageWeight int  `json:"overageWeight,omitempty"` // 1-10, lower values reduce overage request frequency

	// Cached snapshot of the REAL AWS user-level Overages switch + billing
	// figures, synced from GET /getUsageLimits and flipped via POST
	// /setUserPreference (see proxy/kiro_overage.go). These are ADDITIVE and
	// informational: AllowOverage/OverageWeight above still drive pool routing.
	// They let the dashboard show accumulated overage $ and toggle the actual
	// AWS billing switch. Empty/zero until the first successful upstream sync.
	OverageStatus     string  `json:"overageStatus,omitempty"`     // "ENABLED" | "DISABLED" | "UNKNOWN" (AWS switch)
	OverageCapability string  `json:"overageCapability,omitempty"` // "OVERAGE_CAPABLE" / "NOT_OVERAGE_CAPABLE"
	OverageCap        float64 `json:"overageCap,omitempty"`        // Hard upper bound (USD)
	OverageRate       float64 `json:"overageRate,omitempty"`       // Per-invocation rate (USD)
	CurrentOverages   float64 `json:"currentOverages,omitempty"`   // Cumulative overage charges (USD)
	OverageCheckedAt  int64   `json:"overageCheckedAt,omitempty"`  // Last successful upstream sync (Unix seconds)

	// Account status
	Enabled   bool   `json:"enabled"`             // Whether account is active in the pool
	BanStatus string `json:"banStatus,omitempty"` // Ban status: "ACTIVE", "BANNED", "SUSPENDED"
	BanReason string `json:"banReason,omitempty"` // Reason for ban/suspension
	BanTime   int64  `json:"banTime,omitempty"`   // Timestamp when ban was detected

	// AutoDisabledAtFull marks an account that was disabled by the auto-disable
	// path (UsageCurrent ≥ UsageLimit on paid OR trial). Used to distinguish
	// "system disabled because quota was exhausted" from "operator manually
	// disabled this" so the next refresh can auto-re-enable the former without
	// trampling the latter. Cleared when the operator manually toggles Enabled.
	AutoDisabledAtFull bool `json:"autoDisabledAtFull,omitempty"`

	// Subscription information
	SubscriptionType  string `json:"subscriptionType,omitempty"`  // Tier: FREE, PRO, PRO_PLUS, or POWER
	SubscriptionTitle string `json:"subscriptionTitle,omitempty"` // Human-readable subscription name
	DaysRemaining     int    `json:"daysRemaining,omitempty"`     // Days until subscription expires

	// Usage tracking
	UsageCurrent  float64 `json:"usageCurrent,omitempty"`  // Current period usage (credits)
	UsageLimit    float64 `json:"usageLimit,omitempty"`    // Maximum allowed usage per period
	UsagePercent  float64 `json:"usagePercent,omitempty"`  // Usage percentage (0.0-1.0)
	NextResetDate string  `json:"nextResetDate,omitempty"` // Date when usage resets (YYYY-MM-DD)
	LastRefresh   int64   `json:"lastRefresh,omitempty"`   // Last info refresh timestamp

	// Trial usage tracking
	TrialUsageCurrent float64 `json:"trialUsageCurrent,omitempty"` // Trial quota current usage
	TrialUsageLimit   float64 `json:"trialUsageLimit,omitempty"`   // Trial quota total limit
	TrialUsagePercent float64 `json:"trialUsagePercent,omitempty"` // Trial quota usage percentage (0.0-1.0)
	TrialStatus       string  `json:"trialStatus,omitempty"`       // Trial status: ACTIVE, EXPIRED, NONE
	TrialExpiresAt    int64   `json:"trialExpiresAt,omitempty"`    // Trial expiration timestamp (Unix seconds)

	// Runtime statistics (updated during operation)
	RequestCount int     `json:"requestCount,omitempty"` // Total requests processed
	ErrorCount   int     `json:"errorCount,omitempty"`   // Total errors encountered
	LastUsed     int64   `json:"lastUsed,omitempty"`     // Last request timestamp
	TotalTokens  int     `json:"totalTokens,omitempty"`  // Cumulative tokens processed
	TotalCredits float64 `json:"totalCredits,omitempty"` // Cumulative credits consumed
}

// PromptFilterRule defines a single custom prompt sanitization rule.
// Type can be: "regex" (regexp find/replace within prompt) or
// "lines-containing" (remove lines containing the match substring).
type PromptFilterRule struct {
	ID      string `json:"id"`                // Unique rule identifier
	Name    string `json:"name"`              // Human-readable rule name
	Type    string `json:"type"`              // "regex" or "lines-containing"
	Match   string `json:"match"`             // Pattern to match (regex pattern or substring)
	Replace string `json:"replace,omitempty"` // Replacement string (only for regex; empty = delete match)
	Enabled bool   `json:"enabled"`           // Whether this rule is active
}

// Config represents the global application configuration.
type Config struct {
	// Server settings
	Password      string    `json:"password"`          // Admin panel password
	Port          int       `json:"port"`              // HTTP server port (default: 8080)
	Host          string    `json:"host"`              // HTTP server bind address (default: 0.0.0.0)
	ApiKey        string    `json:"apiKey,omitempty"`  // Legacy single API key (kept for backward compatibility)
	RequireApiKey bool      `json:"requireApiKey"`     // Whether to enforce API key validation
	APIKeys       []APIKey  `json:"apiKeys,omitempty"` // Multi API keys with per-key limits
	KiroVersion   string    `json:"kiroVersion,omitempty"`
	SystemVersion string    `json:"systemVersion,omitempty"`
	NodeVersion   string    `json:"nodeVersion,omitempty"`
	Accounts      []Account `json:"accounts"` // Registered Kiro accounts

	// Thinking mode configuration for extended reasoning output
	ThinkingSuffix       string `json:"thinkingSuffix,omitempty"`       // Model suffix to trigger thinking mode (default: "-thinking")
	OpenAIThinkingFormat string `json:"openaiThinkingFormat,omitempty"` // OpenAI output format: "reasoning_content", "thinking", or "think"
	ClaudeThinkingFormat string `json:"claudeThinkingFormat,omitempty"` // Claude output format: "reasoning_content", "thinking", or "think"

	// PreferredEndpoint configuration: "auto", "kiro", "codewhisperer", or "amazonq"
	PreferredEndpoint string `json:"preferredEndpoint,omitempty"`

	// EndpointFallback controls whether to try other endpoints when the preferred one fails.
	// Defaults to true. Set to false to only use the preferred endpoint.
	EndpointFallback *bool `json:"endpointFallback,omitempty"`

	// KiroAPIRegion is the AWS region used when constructing the streaming +
	// REST endpoints (e.g. "us-east-1", "eu-west-1"). Defaults to "us-east-1"
	// to preserve historical behavior. The KIRO_API_REGION environment variable
	// overrides this if set, so docker users can rotate regions without
	// editing config.json.
	//
	// AWS rate-limits per (identity, region) — moving traffic to a different
	// region is one of the few mitigations that genuinely reduces 429s when
	// the operator has a shared identity (e.g. one Builder ID across many
	// accounts). It also impacts latency: us-east-1 is fastest from the US
	// East coast and will be slow from APAC; eu-west-1 / ap-northeast-1
	// are reasonable alternates.
	KiroAPIRegion string `json:"kiroAPIRegion,omitempty"`

	// KiroAPIRegions is the optional multi-region failover chain. When
	// set, the endpoint loop will expand to try every region in this list
	// before declaring all-endpoints-throttled. The first region in the
	// list is the preferred / lowest-latency target; later regions are
	// only hit if every endpoint in the prior region 429s. This is the
	// "auto" mode mentioned in admin docs — empty = single-region
	// (KiroAPIRegion), non-empty = cross-region failover.
	//
	// No competitor project documented in the field survey rotates
	// regions on 429 — they only rotate accounts. We do this because AWS
	// rate-limits per (identity, region), so a fresh region is the one
	// remaining mitigation when an entire account pool is throttled in
	// the primary region.
	KiroAPIRegions []string `json:"kiroAPIRegions,omitempty"`

	// PoolStrategy chooses how the account pool picks the next account for a
	// request. Recognized values:
	//
	//   "least-request" / "" — least-outstanding-request (DEFAULT). Picks the
	//                      eligible account with the fewest in-flight requests
	//                      (Envoy's weighted form: score = weight/(inflight+1)),
	//                      and applies an AIMD per-account concurrency limit that
	//                      grows on success and halves on a 429. Best for bursty
	//                      parallel load (agent fan-out) against per-identity
	//                      rate limits — it prevents the whole pool throttling at
	//                      once. This is the only strategy that reserves in-flight
	//                      slots and gates admission.
	//   "swr"            — smooth weighted round-robin (previous default).
	//                      Balances assignment rate with predictable interleaving
	//                      but is blind to concurrency, so a simultaneous burst
	//                      can throttle every account at once. No in-flight gate.
	//   "least-used"     — pick the eligible account with the lowest
	//                      RequestCount (lifetime) so traffic naturally
	//                      tilts toward fresher / less-burned accounts.
	//                      Useful when accounts have heterogeneous quota
	//                      and you want to drain the freshest first.
	//   "random"         — uniform random pick among eligible accounts.
	//                      Cheap, jitter-friendly, useful as a control.
	//
	// All strategies obey cooldowns and model-list filters identically; only the
	// picker among the eligible subset (and whether the AIMD gate applies)
	// differs.
	PoolStrategy string `json:"poolStrategy,omitempty"`

	// AllowOverUsage allows accounts to continue serving requests even when their
	// usage quota has been exhausted. When enabled, the pool will not skip accounts
	// solely because usageCurrent >= usageLimit.
	AllowOverUsage bool `json:"allowOverUsage,omitempty"`

	// WebSearchEnabled turns on proxy-side emulation of Anthropic's hosted
	// web_search tool (and Claude Code's WebSearch). When enabled, an inbound
	// web_search server tool is serviced by calling Kiro's own native MCP
	// web_search endpoint (https://q.<region>.amazonaws.com/mcp) with the
	// account's existing token, and the results are reshaped into native
	// web_search_tool_result blocks. ON by default (matching jwadow/kiro-gateway
	// and aliom-v/KiroGate, where this is the verified default): a nil pointer
	// means "use the default (on)"; set it explicitly to false to opt out. The
	// upstream MCP endpoint is not guaranteed on every account tier/region; a
	// failed call falls back to dropping the tool so a request never breaks.
	WebSearchEnabled *bool `json:"webSearchEnabled,omitempty"`

	// ToolSearchEnabled turns on proxy-side emulation of Anthropic's Tool Search
	// feature (the tool_search_tool_regex / tool_search_tool_bm25 server tools).
	// When a client (e.g. Claude Code with ENABLE_TOOL_SEARCH) sends most of its
	// tools marked defer_loading:true plus a tool_search server tool, the proxy
	// withholds the deferred tool schemas from the upstream model, exposes a
	// single synthetic search tool, runs the regex/BM25 search itself over the
	// deferred tool descriptions, and expands only the matched tools — reshaping
	// the result into native server_tool_use + tool_search_tool_result blocks.
	// This keeps both the client context AND the upstream CodeWhisperer context
	// small WHEN it works.
	//
	// OFF by default. The emulation depends on the upstream CodeWhisperer/Kiro
	// model reliably CALLING the synthetic search tool to discover its deferred
	// tools, and that indirection is unreliable: the model often narrates an
	// action and ends the turn without ever calling the search tool, so its real
	// tools never become visible and it emits no tool_use ("narrate then stop").
	// With this OFF, a tool_search request falls through to the normal path,
	// which drops the unsupported search server tool and forwards every real tool
	// eagerly (defer_loading ignored) so the model calls tools directly — the same
	// fallback Claude Code itself uses on a non-first-party ANTHROPIC_BASE_URL,
	// and what the sibling kiro2api / kiro-gateway relays do. A nil pointer means
	// "use the default (off)"; set it explicitly to true to opt into the emulation
	// (trades reliability for tool-definition token savings).
	ToolSearchEnabled *bool `json:"toolSearchEnabled,omitempty"`

	// GlobalRateLimitPerMinute caps the proxy's TOTAL inbound request rate
	// across all API keys (token-bucket). 0 = disabled (default), which leaves
	// behavior unchanged — only the existing per-key limits and per-account
	// cooldowns apply. A positive value engages a global backstop that returns
	// 429 + Retry-After when the steady rate is exceeded.
	GlobalRateLimitPerMinute int `json:"globalRateLimitPerMinute,omitempty"`

	// Proxy configuration: optional outbound proxy for Kiro API requests
	// Format: "socks5://host:port", "socks5://user:pass@host:port",
	//         "http://host:port",  "http://user:pass@host:port"
	// Leave empty to connect directly.
	ProxyURL string `json:"proxyURL,omitempty"`

	// SanitizeClaudeCodePrompt is kept for backward-compatible JSON loading only.
	// Migrated to FilterClaudeCode on first load. Do not use directly.
	SanitizeClaudeCodePrompt bool `json:"sanitizeClaudeCodePrompt,omitempty"`

	// FilterClaudeCode detects the Claude Code CLI built-in system prompt and replaces it
	// with a compact backend-only prompt, reducing token usage significantly.
	FilterClaudeCode bool `json:"filterClaudeCode,omitempty"`

	// FilterEnvNoise strips environment metadata lines from system prompts:
	// git status, recent commits, environment sections, fast_mode_info tags, etc.
	// It also drops whole <system-reminder>...</system-reminder> blocks. Because
	// Claude Code delivers CLAUDE.md / AGENTS.md content inside <system-reminder>
	// blocks, this defaults OFF (since 1.0.10-A11) so project memory files reach
	// the model; enabling it trades that context for fewer tokens per request.
	FilterEnvNoise bool `json:"filterEnvNoise,omitempty"`

	// FilterStripBoundaries removes --- SYSTEM PROMPT --- / --- END SYSTEM PROMPT --- markers.
	FilterStripBoundaries bool `json:"filterStripBoundaries,omitempty"`

	// PromptFilterRules is a list of user-defined prompt sanitization rules (regex or line-filter).
	PromptFilterRules []PromptFilterRule `json:"promptFilterRules,omitempty"`

	// FilterDefaultsApplied marks that the one-shot migration which sets the
	// three Filter* flags ON for upgrading users has run. Without this flag
	// we'd keep flipping them ON every restart, trampling explicit user
	// "off" choices. New installs go through the bootstrap path in Load()
	// and arrive here pre-migrated. See migrateFilterDefaults.
	FilterDefaultsApplied bool `json:"filterDefaultsApplied,omitempty"`

	// LogLevel controls verbosity of application logs.
	// Accepted values: "debug", "info", "warn", "error". Defaults to "info".
	// Can be overridden by the LOG_LEVEL environment variable.
	LogLevel string `json:"logLevel,omitempty"`

	// Global statistics (persisted across restarts)
	TotalRequests   int     `json:"totalRequests,omitempty"`   // Total API requests received
	SuccessRequests int     `json:"successRequests,omitempty"` // Successful requests count
	FailedRequests  int     `json:"failedRequests,omitempty"`  // Failed requests count
	TotalTokens     int     `json:"totalTokens,omitempty"`     // Total tokens processed
	TotalCredits    float64 `json:"totalCredits,omitempty"`    // Total credits consumed

	// KnownModels is the last-known-good model catalog fetched from the
	// upstream Kiro ListAvailableModels endpoint, persisted so a restart
	// serves real model ids immediately instead of falling back to a
	// hardcoded guess. It is refreshed automatically whenever a live fetch
	// succeeds (see proxy.refreshModelsCache). The ids are the raw upstream
	// (dotted) model ids; the /v1/models layer derives the dashed Claude
	// Code aliases from them. Empty only on a truly fresh install that has
	// never reached upstream.
	KnownModels []string `json:"knownModels,omitempty"`
}

// AccountInfo contains account metadata retrieved from Kiro API.
// Used for updating subscription and usage information.
type AccountInfo struct {
	Email             string
	UserId            string
	SubscriptionType  string
	SubscriptionTitle string
	DaysRemaining     int
	UsageCurrent      float64
	UsageLimit        float64
	UsagePercent      float64
	NextResetDate     string
	LastRefresh       int64
	TrialUsageCurrent float64
	TrialUsageLimit   float64
	TrialUsagePercent float64
	TrialStatus       string
	TrialExpiresAt    int64
}

// Version current version
const Version = "1.0.10-A18"

var (
	cfg     *Config
	cfgLock sync.RWMutex
	cfgPath string
	// firstRunStarterKey is set non-empty exactly once when Load() bootstraps
	// a new config.json. main.go logs it loudly so the operator can copy the
	// generated default API key on first run.
	firstRunStarterKey string

	// statsDirty is set whenever an in-memory request-level mutation happens
	// (currently UpdateAccountStats — bumped on every successful API call).
	// A background flusher debounces these into a single Save() per
	// statsFlushInterval, replacing the previous per-request fsync hot path.
	// The flag is cleared inside the same write that produces the snapshot
	// so a successful flush truly drains the queue.
	statsDirty   atomic.Bool
	statsSaverOn atomic.Bool // ensures we only spin up one saver goroutine

	// statsFlushMu serializes FlushStats() calls so the shutdown drain
	// can't return while the background ticker is mid-write. Without it,
	// FlushStats() from main.go's shutdown path could see Swap(false)
	// because a tick already grabbed the dirty flag, return immediately,
	// and let the process exit before the tick's saveLocked finishes.
	statsFlushMu sync.Mutex
)

// statsFlushInterval is the debounce window for coalescing per-request stats
// writes into a single config.json save. Five seconds keeps the admin UI
// responsive (refreshes show roughly current numbers) while collapsing
// hundreds of requests into one fsync.
const statsFlushInterval = 5 * time.Second

// FirstRunStarterKey returns the API key generated on first-run bootstrap, or
// "" if the config was loaded from an existing file. main.go calls this once
// after config.Init to log the key for the operator. The value is not
// persisted in this variable across restarts — config.json is the durable
// home.
func FirstRunStarterKey() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return firstRunStarterKey
}

// Init initializes the configuration system with the specified file path.
// If the file doesn't exist, a default configuration is created.
func Init(path string) error {
	cfgPath = path
	return Load()
}

func Load() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Create default configuration.
			// Binds to 0.0.0.0 by default for Docker/container compatibility.
			// Filter toggles default ON so a fresh install Just Works for
			// Claude Code without the user having to enable them manually.
			// RequireApiKey defaults to true and a starter API key is
			// generated on first run, so the proxy is secure-by-default
			// even when the user runs `docker compose up` and does nothing
			// else. The generated key is logged once at startup so the
			// operator can copy it; the same key is also visible in the
			// admin panel's "API Keys" tab on first login.
			starterKey, _ := generateAPIKeySecret()
			firstRunStarterKey = starterKey
			cfg = &Config{
				Password:              "changeme",
				Port:                  8080,
				Host:                  "0.0.0.0",
				RequireApiKey:         true,
				APIKeys: []APIKey{{
					ID:        generateAPIKeyID(),
					Name:      "default",
					Key:       starterKey,
					Enabled:   true,
					CreatedAt: time.Now().Unix(),
				}},
				Accounts:              []Account{},
				FilterClaudeCode:      true,
				FilterEnvNoise:        false,
				FilterStripBoundaries: true,
				FilterDefaultsApplied: true,
			}
			return Save()
		}
		return err
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	cfg = &c

	// One-shot migration: configs created before the FilterDefaultsApplied
	// flag existed have FilterClaudeCode/FilterEnvNoise/FilterStripBoundaries
	// silently zeroed by Go's json.Unmarshal whenever those fields were
	// absent from the JSON document. Without this migration, every upgrade
	// would surface a "filters off by default" UX regression. We only set
	// this once per install; subsequent runs respect operator toggles.
	migrateFilterDefaults()

	// Auto-migrate the legacy single-key field into the multi-key list so the
	// dashboard's "API Keys" tab is the single source of truth. Triggers only
	// when ApiKey is set AND APIKeys is empty (i.e. existing users coming up
	// from a pre-A7 config). Idempotent on subsequent restarts.
	if cfg.ApiKey != "" && len(cfg.APIKeys) == 0 {
		cfg.APIKeys = []APIKey{{
			ID:        generateAPIKeyID(),
			Name:      "legacy",
			Key:       cfg.ApiKey,
			Enabled:   true,
			CreatedAt: 0,
		}}
		// Keep cfg.ApiKey populated for backward-compat clients that read
		// /admin/api/settings.apiKey, but it now mirrors APIKeys[0] rather
		// than being the only source.
		_ = Save()
	}

	// One-shot migration: hash a legacy PLAINTEXT admin password at rest. Older
	// configs (and the bundled default) stored the password verbatim. bcrypt is
	// idempotent here — looksLikeBcrypt skips an already-hashed value — so this
	// runs at most once per config and is a no-op on every subsequent load.
	// VerifyPassword / IsDefaultPassword handle both forms, so this never breaks
	// the default-password startup guard.
	if cfg.Password != "" && !looksLikeBcrypt(cfg.Password) {
		cfg.Password = hashPasswordForStorage(cfg.Password)
		_ = Save()
	}
	return nil
}

// Save persists the current configuration to the JSON file.
// Uses indented formatting for human readability and writes atomically
// (temp file + rename) so a crash mid-write cannot leave config.json
// truncated or empty.
//
// Save assumes the caller is already holding cfgLock (most callers are
// inside a "Lock + mutate + Save" block) — it does NOT take the lock itself.
// FlushConfig is the public wrapper that acquires cfgLock and then calls Save,
// for callers (e.g. the background refresh sweep) that mutated via the
// *NoSave helpers and want a single batched persist afterwards. Use
// FlushStats / saveLocked when you want a snapshot under your own lock
// discipline.
func Save() error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeConfigBytes(data)
}

// writeConfigBytes does the temp-file + fsync + rename dance with a
// pre-marshaled byte slice. Split out so Save (caller holds cfgLock) and
// saveLocked (acquires RLock for marshaling) can share the disk-side
// implementation.
func writeConfigBytes(data []byte) error {
	dir := filepath.Dir(cfgPath)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, ".config.json.*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		// Best-effort cleanup if rename fails.
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Restrict the file to the owning user before the rename. NOTE: on Windows
	// os.Chmod only maps the read-only bit — NTFS permissions are governed by
	// ACLs, not POSIX mode, so 0600 does NOT restrict who can read the file
	// there. config.json holds AWS tokens, the bcrypt admin hash, and API-key
	// secrets, so on a multi-user Windows host the data directory must be
	// ACL-restricted to the service account out of band (documented in the
	// README). On Linux/macOS this enforces 0600 as intended.
	if err := os.Chmod(tmpPath, 0600); err != nil {
		return err
	}
	return os.Rename(tmpPath, cfgPath)
}

// migrateFilterDefaults applies the default prompt-filter flags for any
// pre-existing config that was loaded before the FilterDefaultsApplied
// migration marker existed: FilterClaudeCode and FilterStripBoundaries ON,
// FilterEnvNoise OFF. FilterEnvNoise defaults OFF because it strips whole
// <system-reminder> blocks, and Claude Code delivers CLAUDE.md / AGENTS.md
// inside those blocks — leaving it on would silently drop the user's project
// memory files before they reach the model. Caller must hold cfgLock for
// write — Load() satisfies that. The migration runs at most once per install:
// once FilterDefaultsApplied is true (either from this migration or from the
// fresh-install bootstrap), we never re-apply, so explicit operator toggles
// are preserved.
func migrateFilterDefaults() {
	if cfg == nil || cfg.FilterDefaultsApplied {
		return
	}
	cfg.FilterClaudeCode = true
	cfg.FilterEnvNoise = false
	cfg.FilterStripBoundaries = true
	cfg.FilterDefaultsApplied = true
	// Persist immediately so a subsequent restart sees the marker even if
	// no other state changes between now and the next save.
	_ = Save()
}

// SetPassword updates the admin password.
// Primarily used for environment variable override in containerized deployments.
// DefaultAdminPassword is the bundled first-run password. The startup guard
// refuses to bind a public port while the admin password still verifies against
// this value (see IsDefaultPassword), forcing the operator to change it first.
const DefaultAdminPassword = "changeme"

// bcryptCost is the work factor for hashing the admin password at rest. Admin
// auth is human-paced (dashboard login + low-volume admin API), so the ~60ms
// of a cost-10 hash per verify is imperceptible while still forcing an
// expensive offline brute-force if config.json is ever exfiltrated.
const bcryptCost = bcrypt.DefaultCost

// looksLikeBcrypt reports whether a stored password value is already a bcrypt
// hash (vs legacy plaintext). bcrypt hashes start with $2a$ / $2b$ / $2y$.
func looksLikeBcrypt(s string) bool {
	return strings.HasPrefix(s, "$2a$") || strings.HasPrefix(s, "$2b$") || strings.HasPrefix(s, "$2y$")
}

// hashPasswordForStorage returns the value to persist for a password. Non-empty
// plaintext is bcrypt-hashed; an empty string is stored as-is (treated as "no
// password set"); a value that is already a bcrypt hash is returned unchanged
// so re-saving config doesn't double-hash.
func hashPasswordForStorage(password string) string {
	if password == "" || looksLikeBcrypt(password) {
		return password
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		// Hashing only fails on absurd inputs (>72 bytes is truncated, not an
		// error). Fall back to storing plaintext rather than losing the
		// password — VerifyPassword still handles the plaintext branch.
		logger.Errorf("[Config] bcrypt hashing failed, storing password unhashed: %v", err)
		return password
	}
	return string(h)
}

// VerifyPassword reports whether candidate matches the stored admin password.
// It transparently handles BOTH a bcrypt hash (current format) and a legacy
// plaintext value (pre-hash configs, or the bundled default), so upgrades and
// the default-password guard keep working. Both branches are constant-time.
func VerifyPassword(candidate string) bool {
	stored := GetPassword()
	if looksLikeBcrypt(stored) {
		return bcrypt.CompareHashAndPassword([]byte(stored), []byte(candidate)) == nil
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(stored)) == 1
}

// IsDefaultPassword reports whether the admin password still verifies against
// the bundled default. Used by the startup guard so the check works regardless
// of whether the stored value is hashed or plaintext.
func IsDefaultPassword() bool {
	return VerifyPassword(DefaultAdminPassword)
}

// SetPassword sets the admin password, hashing it at rest. Primarily used for
// the ADMIN_PASSWORD environment-variable override in containerized
// deployments (runtime-only; not persisted by this call).
func SetPassword(password string) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Password = hashPasswordForStorage(password)
}

func GetPassword() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.Password
}

func GetPort() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Port == 0 {
		return 8080
	}
	return cfg.Port
}

func GetHost() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Host == "" {
		return "127.0.0.1"
	}
	return cfg.Host
}

func GetAccounts() []Account {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	accounts := make([]Account, len(cfg.Accounts))
	copy(accounts, cfg.Accounts)
	return accounts
}

func GetEnabledAccounts() []Account {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	var accounts []Account
	for _, a := range cfg.Accounts {
		if a.Enabled {
			accounts = append(accounts, a)
		}
	}
	return accounts
}

func AddAccount(account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Accounts = append(cfg.Accounts, account)
	return Save()
}

func UpdateAccount(id string, account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i] = account
			return Save()
		}
	}
	return nil
}

// DisableAccountOverage turns off AllowOverage for a specific account.
func DisableAccountOverage(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].AllowOverage = false
			return Save()
		}
	}
	return nil
}

// UpdateAccountOverageStatus persists the cached snapshot of the REAL AWS
// user-level Overages switch + billing figures (synced via
// proxy.FetchOverageStatus / flipped via proxy.SetOverageStatus). Additive
// cache only — it does NOT change AllowOverage/OverageWeight routing. Empty
// status/capability and non-positive checkedAt are left untouched so a partial
// snapshot can't wipe good cached values.
func UpdateAccountOverageStatus(id, status, capability string, cap, rate, current float64, checkedAt int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if status != "" {
				cfg.Accounts[i].OverageStatus = status
			}
			if capability != "" {
				cfg.Accounts[i].OverageCapability = capability
			}
			cfg.Accounts[i].OverageCap = cap
			cfg.Accounts[i].OverageRate = rate
			cfg.Accounts[i].CurrentOverages = current
			if checkedAt > 0 {
				cfg.Accounts[i].OverageCheckedAt = checkedAt
			}
			return Save()
		}
	}
	return nil
}

func UpdateAccountProfileArn(id, profileArn string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].ProfileArn = profileArn
			return Save()
		}
	}
	return nil
}

func DeleteAccount(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts = append(cfg.Accounts[:i], cfg.Accounts[i+1:]...)
			return Save()
		}
	}
	return nil
}

func UpdateAccountToken(id, accessToken, refreshToken string, expiresAt int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				cfg.Accounts[i].RefreshToken = refreshToken
			}
			cfg.Accounts[i].ExpiresAt = expiresAt
			return Save()
		}
	}
	return nil
}

func GetApiKey() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ApiKey
}

func IsApiKeyRequired() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.RequireApiKey
}

func UpdateSettingsPartial(apiKey *string, requireApiKey *bool, password *string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if apiKey != nil {
		cfg.ApiKey = *apiKey
	}
	if requireApiKey != nil {
		cfg.RequireApiKey = *requireApiKey
	}
	if password != nil && *password != "" {
		cfg.Password = hashPasswordForStorage(*password)
	}
	return Save()
}

func UpdateStats(totalReq, successReq, failedReq, totalTokens int, totalCredits float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.TotalRequests = totalReq
	cfg.SuccessRequests = successReq
	cfg.FailedRequests = failedReq
	cfg.TotalTokens = totalTokens
	cfg.TotalCredits = totalCredits
	return Save()
}

func GetStats() (int, int, int, int, float64) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.TotalRequests, cfg.SuccessRequests, cfg.FailedRequests, cfg.TotalTokens, cfg.TotalCredits
}

// UpdateAccountStats updates per-account counters in memory and marks the
// config as dirty. The actual disk write is coalesced by the background
// stats saver started via StartStatsSaver, so a burst of N successful
// requests collapses into a single config.json fsync rather than N. Call
// FlushStats from the shutdown path to drain any pending mutation before
// exit.
func UpdateAccountStats(id string, requestCount, errorCount, totalTokens int, totalCredits float64, lastUsed int64) error {
	cfgLock.Lock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].RequestCount = requestCount
			cfg.Accounts[i].ErrorCount = errorCount
			cfg.Accounts[i].TotalTokens = totalTokens
			cfg.Accounts[i].TotalCredits = totalCredits
			cfg.Accounts[i].LastUsed = lastUsed
			cfgLock.Unlock()
			statsDirty.Store(true)
			return nil
		}
	}
	cfgLock.Unlock()
	return nil
}

// StartStatsSaver launches the background coalescing flusher. Idempotent:
// only the first call wins, so re-invoking from tests or hot-reload paths
// is safe. The flusher checks statsDirty every statsFlushInterval and
// runs Save() only when there is unflushed mutation.
func StartStatsSaver() {
	if !statsSaverOn.CompareAndSwap(false, true) {
		return
	}
	go func() {
		// Recover so a panic in the long-lived stats-flush goroutine (e.g.
		// during json.MarshalIndent of cfg) can't crash the whole process and
		// silently stop all stats persistence. Log and keep ticking.
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[StatsSaver] flush goroutine panic recovered: %v", r)
			}
		}()
		ticker := time.NewTicker(statsFlushInterval)
		defer ticker.Stop()
		for range ticker.C {
			FlushStats()
		}
	}()
}

// FlushStats writes the current in-memory config to disk if anything has
// been marked dirty since the last flush. Safe to call concurrently with
// the background saver; both paths atomically swap the dirty flag before
// taking the disk lock so a flush in progress can't mask a fresh mutation
// that arrives between the swap and the rename. Exported so the shutdown
// path can drain pending stats before exit.
//
// statsFlushMu serializes flushers so the shutdown call doesn't return
// while a ticker tick is mid-write — otherwise the process could exit
// before the on-disk file is fully replaced.
func FlushStats() {
	statsFlushMu.Lock()
	defer statsFlushMu.Unlock()
	if !statsDirty.Swap(false) {
		return
	}
	if err := saveLocked(); err != nil {
		// Restore the dirty flag so the next tick retries; otherwise a
		// transient disk error would silently lose the most recent batch
		// of mutations.
		statsDirty.Store(true)
	}
}

// saveLocked is Save with caller-holds-no-lock semantics; it acquires the
// read lock around marshaling so concurrent mutators can't observe a
// partial snapshot. Save() itself does not lock because most callers are
// already holding cfgLock.Lock at the time of the call.
func saveLocked() error {
	cfgLock.RLock()
	data, err := json.MarshalIndent(cfg, "", "  ")
	cfgLock.RUnlock()
	if err != nil {
		return err
	}
	return writeConfigBytes(data)
}

// IsQuotaExhausted reports whether either the paid or the trial quota has
// been fully consumed. A zero limit means "no limit declared by upstream"
// and is treated as not-exhausted (we never auto-disable accounts whose
// limit we can't see).
func IsQuotaExhausted(a Account) bool {
	if a.UsageLimit > 0 && a.UsageCurrent >= a.UsageLimit {
		return true
	}
	if a.TrialUsageLimit > 0 && a.TrialUsageCurrent >= a.TrialUsageLimit {
		return true
	}
	return false
}

// applyAutoDisableTransition encodes the state machine for the auto-disable
// feature. Mutates `a` in place. Returns true when the call flipped Enabled —
// callers use that to know they need to Reload the pool.
//
// `globalOverage` is the value of cfg.AllowOverUsage at the call site. We
// receive it as an arg rather than reading it ourselves because every
// production caller already holds cfgLock for write, and GetAllowOverUsage
// would deadlock on the RLock from inside that critical section.
//
// Auto-disable is suppressed when overage is allowed for the account (either
// per-account `AllowOverage` or the global `AllowOverUsage` flag). Without
// this guard the auto-disable would defeat AllowOverage: the account would
// disappear from the pool entirely instead of staying in rotation at its
// reduced overage weight (1..10), as the operator intended.
//
// State table (E = Enabled, F = AutoDisabledAtFull, Q = quota exhausted,
// O = overage allowed for this account):
//
//	(E=t, Q=t, O=f)         →  (E=f, F=t)   auto-disable
//	(E=t, Q=t, O=t)         →  unchanged    overage takes over (pool weight 1..10)
//	(E=f, F=t, Q=f)         →  (E=t, F=f)   auto-recover (quota dropped)
//	(E=f, F=t, O=t)         →  (E=t, F=f)   auto-recover (overage now allowed)
//	(E=f, F=f, *)           →  unchanged    operator manually disabled — respect it
//	any other               →  unchanged
func applyAutoDisableTransition(a *Account, globalOverage bool) bool {
	full := IsQuotaExhausted(*a)
	overageAllowed := a.AllowOverage || globalOverage

	if a.Enabled && full && !overageAllowed {
		a.Enabled = false
		a.AutoDisabledAtFull = true
		return true
	}
	// Recovery: an account we previously auto-disabled becomes routable again
	// when EITHER quota dropped below the limit OR overage was turned on.
	if !a.Enabled && a.AutoDisabledAtFull && (!full || overageAllowed) {
		a.Enabled = true
		a.AutoDisabledAtFull = false
		return true
	}
	return false
}

// UpdateAccountInfo updates an account's subscription and usage information.
// Called after refreshing account data from Kiro API.
//
// Returns true when the call flipped Enabled (auto-disable or auto-recover);
// callers use that to decide whether to Reload the pool.
func UpdateAccountInfo(id string, info AccountInfo) (bool, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	flipped, found := applyAccountInfoLocked(id, info)
	if !found {
		return false, nil
	}
	return flipped, Save()
}

// UpdateAccountInfoNoSave applies the same mutation as UpdateAccountInfo but
// does NOT persist to disk — the caller is responsible for one batched
// FlushConfig() afterward. This exists for the background refresh sweep, which
// updates every account on each tick: the prior per-account Save() rewrote the
// ENTIRE config.json (marshal + fsync + rename) under the write lock once per
// account, so a 30-account pool did 30 full-file rewrites per 5-minute tick.
// Batching collapses that to a single write. Returns (flipped, found).
func UpdateAccountInfoNoSave(id string, info AccountInfo) (bool, bool) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	return applyAccountInfoLocked(id, info)
}

// applyAccountInfoLocked mutates the matching account's subscription/usage
// fields and applies the auto-disable transition. Caller MUST hold cfgLock.
// Returns (flipped, found): flipped is true when Enabled changed (caller should
// Reload the pool), found is true when an account with the id existed.
func applyAccountInfoLocked(id string, info AccountInfo) (flipped bool, found bool) {
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if info.Email != "" {
				cfg.Accounts[i].Email = info.Email
			}
			if info.UserId != "" {
				cfg.Accounts[i].UserId = info.UserId
			}
			cfg.Accounts[i].SubscriptionType = info.SubscriptionType
			cfg.Accounts[i].SubscriptionTitle = info.SubscriptionTitle
			cfg.Accounts[i].DaysRemaining = info.DaysRemaining
			cfg.Accounts[i].UsageCurrent = info.UsageCurrent
			cfg.Accounts[i].UsageLimit = info.UsageLimit
			cfg.Accounts[i].UsagePercent = info.UsagePercent
			cfg.Accounts[i].NextResetDate = info.NextResetDate
			cfg.Accounts[i].LastRefresh = info.LastRefresh
			cfg.Accounts[i].TrialUsageCurrent = info.TrialUsageCurrent
			cfg.Accounts[i].TrialUsageLimit = info.TrialUsageLimit
			cfg.Accounts[i].TrialUsagePercent = info.TrialUsagePercent
			cfg.Accounts[i].TrialStatus = info.TrialStatus
			cfg.Accounts[i].TrialExpiresAt = info.TrialExpiresAt
			flipped = applyAutoDisableTransition(&cfg.Accounts[i], cfg.AllowOverUsage)
			return flipped, true
		}
	}
	return false, false
}

// FlushConfig persists the current config to disk. Used after a batch of
// UpdateAccountInfoNoSave calls to write once instead of per-account.
func FlushConfig() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	return Save()
}

// GetFilterClaudeCode returns whether Claude Code system prompt detection is enabled.
// Also checks the legacy SanitizeClaudeCodePrompt flag for backward compatibility.
func GetFilterClaudeCode() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt
}

// GetFilterEnvNoise returns whether environment noise line stripping is enabled.
func GetFilterEnvNoise() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterEnvNoise
}

// GetFilterStripBoundaries returns whether boundary marker stripping is enabled.
func GetFilterStripBoundaries() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterStripBoundaries
}

// PromptFilterConfig holds all prompt filter settings for API responses.
type PromptFilterConfig struct {
	FilterClaudeCode      bool               `json:"filterClaudeCode"`
	FilterEnvNoise        bool               `json:"filterEnvNoise"`
	FilterStripBoundaries bool               `json:"filterStripBoundaries"`
	Rules                 []PromptFilterRule `json:"rules"`
}

// GetPromptFilterConfig returns all prompt filter settings.
func GetPromptFilterConfig() PromptFilterConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return PromptFilterConfig{Rules: []PromptFilterRule{}}
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return PromptFilterConfig{
		FilterClaudeCode:      cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt,
		FilterEnvNoise:        cfg.FilterEnvNoise,
		FilterStripBoundaries: cfg.FilterStripBoundaries,
		Rules:                 rules,
	}
}

// UpdatePromptFilterConfig saves all prompt filter settings atomically.
func UpdatePromptFilterConfig(filterClaudeCode, filterEnvNoise, filterStripBoundaries bool, rules []PromptFilterRule) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.FilterClaudeCode = filterClaudeCode
	cfg.FilterEnvNoise = filterEnvNoise
	cfg.FilterStripBoundaries = filterStripBoundaries
	// Clear legacy flag to avoid double-applying after first save
	cfg.SanitizeClaudeCodePrompt = false
	if rules != nil {
		cfg.PromptFilterRules = rules
	}
	return Save()
}

// GetPromptFilterRules returns the current prompt filter rules.
func GetPromptFilterRules() []PromptFilterRule {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return rules
}

// ThinkingConfig holds settings for AI thinking/reasoning mode.
// When enabled, models output their reasoning process alongside the response.
type ThinkingConfig struct {
	Suffix       string `json:"suffix"`       // Model name suffix that triggers thinking mode
	OpenAIFormat string `json:"openaiFormat"` // Output format for OpenAI-compatible responses
	ClaudeFormat string `json:"claudeFormat"` // Output format for Claude-compatible responses
}

// GetThinkingConfig 获取 thinking 配置
func GetThinkingConfig() ThinkingConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	suffix := cfg.ThinkingSuffix
	if suffix == "" {
		suffix = "-thinking"
	}
	openaiFormat := cfg.OpenAIThinkingFormat
	if openaiFormat == "" {
		openaiFormat = "reasoning_content"
	}
	claudeFormat := cfg.ClaudeThinkingFormat
	if claudeFormat == "" {
		claudeFormat = "thinking"
	}

	return ThinkingConfig{
		Suffix:       suffix,
		OpenAIFormat: openaiFormat,
		ClaudeFormat: claudeFormat,
	}
}

// GetThinkingConfigOrEmpty is a nil-safe variant of GetThinkingConfig usable
// from helpers that may run before config.Init (e.g. unit tests covering the
// model translator). Returns nil when cfg has not been initialised so the
// caller can fall back to defaults rather than panic on a nil dereference.
func GetThinkingConfigOrEmpty() *ThinkingConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	suffix := cfg.ThinkingSuffix
	if suffix == "" {
		suffix = "-thinking"
	}
	return &ThinkingConfig{
		Suffix:       suffix,
		OpenAIFormat: cfg.OpenAIThinkingFormat,
		ClaudeFormat: cfg.ClaudeThinkingFormat,
	}
}

// UpdateThinkingConfig 更新 thinking 配置
func UpdateThinkingConfig(suffix, openaiFormat, claudeFormat string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ThinkingSuffix = suffix
	cfg.OpenAIThinkingFormat = openaiFormat
	cfg.ClaudeThinkingFormat = claudeFormat
	return Save()
}

// GetPreferredEndpoint 获取首选端点配置
func GetPreferredEndpoint() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.PreferredEndpoint == "" {
		return "auto"
	}
	return cfg.PreferredEndpoint
}

// UpdatePreferredEndpoint 更新首选端点配置
func UpdatePreferredEndpoint(endpoint string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.PreferredEndpoint = endpoint
	return Save()
}

// GetEndpointFallback returns whether endpoint fallback is enabled. Defaults to true.
func GetEndpointFallback() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.EndpointFallback == nil {
		return true
	}
	return *cfg.EndpointFallback
}

// UpdateEndpointFallback sets the endpoint fallback switch and persists the change.
func UpdateEndpointFallback(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.EndpointFallback = &enabled
	return Save()
}

// GetKiroAPIRegion returns the AWS region used to construct Kiro endpoints.
// Resolution precedence:
//
//  1. KIRO_API_REGION environment variable (if set + non-empty).
//  2. cfg.KiroAPIRegion from config.json (if set + non-empty).
//  3. "us-east-1" hard default.
//
// We sniff the env var live (not at startup) so the operator can rotate the
// region by editing the env in their compose file and restarting; we don't
// require the proxy to be aware of init-time vs runtime overrides.
//
// Region strings are lowercased + trimmed to be tolerant of operator typos
// like "  US-East-1  " from a copy-pasted dashboard URL.
func GetKiroAPIRegion() string {
	if envRegion := strings.ToLower(strings.TrimSpace(os.Getenv("KIRO_API_REGION"))); envRegion != "" {
		return envRegion
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg != nil {
		region := strings.ToLower(strings.TrimSpace(cfg.KiroAPIRegion))
		if region != "" {
			return region
		}
	}
	return "us-east-1"
}

// UpdateKiroAPIRegion persists the region setting. Empty string clears the
// override and falls back to env / default. Caller is responsible for
// reinitializing any region-dependent state (handler endpoint cache, etc.).
func UpdateKiroAPIRegion(region string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.KiroAPIRegion = strings.TrimSpace(region)
	return Save()
}

// GetKiroAPIRegions returns the multi-region failover list. Resolution:
//
//  1. KIRO_API_REGIONS environment variable (comma-separated).
//  2. cfg.KiroAPIRegions array.
//  3. Empty (caller falls back to single-region GetKiroAPIRegion).
//
// Empty values, duplicates, and malformed regions are filtered out so
// the env-var path matches the admin-handler contract — passing
// "us-east-1, junk, eu-west-1" to KIRO_API_REGIONS yields
// ["us-east-1","eu-west-1"] rather than carrying the invalid entry into
// endpoint construction. First entry is preferred / lowest-latency;
// later regions only get traffic on 429 from earlier regions.
func GetKiroAPIRegions() []string {
	var raw []string
	if env := strings.TrimSpace(os.Getenv("KIRO_API_REGIONS")); env != "" {
		raw = strings.Split(env, ",")
	} else {
		cfgLock.RLock()
		if cfg != nil && len(cfg.KiroAPIRegions) > 0 {
			raw = make([]string, len(cfg.KiroAPIRegions))
			copy(raw, cfg.KiroAPIRegions)
		}
		cfgLock.RUnlock()
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		s := strings.ToLower(strings.TrimSpace(r))
		if s == "" {
			continue
		}
		if seen[s] {
			continue
		}
		if !isValidAWSRegionShape(s) {
			// Operator-visible warn so a typo in KIRO_API_REGIONS / config
			// isn't silently dropped on every accessor call.
			logger.Warnf("[Config] KiroAPIRegions: dropping malformed region %q", s)
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// UpdateKiroAPIRegions persists the multi-region failover list. An empty
// slice clears it (single-region fallback to GetKiroAPIRegion). Invalid
// entries (empty, duplicates, malformed) are dropped silently so the
// stored list is always usable.
func UpdateKiroAPIRegions(regions []string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	clean := make([]string, 0, len(regions))
	seen := map[string]bool{}
	for _, r := range regions {
		s := strings.ToLower(strings.TrimSpace(r))
		if s == "" {
			continue
		}
		if seen[s] {
			continue
		}
		if !isValidAWSRegionShape(s) {
			logger.Warnf("[Config] UpdateKiroAPIRegions: dropping malformed region %q", s)
			continue
		}
		seen[s] = true
		clean = append(clean, s)
	}
	cfg.KiroAPIRegions = clean
	return Save()
}

// isValidAWSRegionShape mirrors the proxy-layer isValidAWSRegion check
// but kept here so config doesn't depend on proxy. Cheap shape check —
// "us-east-1", "eu-west-1", "us-gov-east-1" all pass; obvious typos and
// unicode garbage don't.
func isValidAWSRegionShape(s string) bool {
	if len(s) < 9 || len(s) > 32 {
		return false
	}
	parts := strings.Split(s, "-")
	if len(parts) < 3 {
		return false
	}
	if len(parts[0]) != 2 {
		return false
	}
	for _, c := range parts[0] {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	last := parts[len(parts)-1]
	if len(last) == 0 {
		return false
	}
	for _, c := range last {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// GetPoolStrategy returns the configured pool selection strategy. An empty or
// unrecognized PoolStrategy maps to "least-request" (the production default).
//
// The cfg==nil case returns "swr" instead: cfg is only nil before Init() (i.e.
// in unit tests that construct a pool directly without loading config), and the
// pool's SWRR/eligibility tests rely on that non-reserving fallback. In
// production Init() always runs first, so a real install with an empty
// PoolStrategy gets least-request.
func GetPoolStrategy() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return "swr"
	}
	switch strings.ToLower(strings.TrimSpace(cfg.PoolStrategy)) {
	case "swr", "swrr":
		return "swr"
	case "least-used", "leastused", "least_used":
		return "least-used"
	case "random":
		return "random"
	case "least-request", "least-conn", "leastrequest", "least_request", "lor":
		return "least-request"
	default:
		return "least-request"
	}
}

// UpdatePoolStrategy persists the pool strategy setting. Caller should call
// pool.Reload() if they want the change to take effect mid-run; otherwise
// the next pool selection picks up the new value via GetPoolStrategy.
func UpdatePoolStrategy(strategy string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.PoolStrategy = strings.TrimSpace(strategy)
	return Save()
}

// GetWebSearchEnabled reports whether proxy-side web_search emulation is on.
// Default TRUE: a nil pointer (fresh/unconfigured install, or an upgrade whose
// config predates this field) means "use the default", which is on — matching
// the verified default in jwadow/kiro-gateway and aliom-v/KiroGate. Only an
// explicit false opts out.
func GetWebSearchEnabled() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.WebSearchEnabled == nil {
		return true
	}
	return *cfg.WebSearchEnabled
}

// UpdateWebSearchEnabled persists the web-search emulation toggle.
func UpdateWebSearchEnabled(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.WebSearchEnabled = &enabled
	return Save()
}

// GetToolSearchEnabled reports whether proxy-side tool-search emulation is on.
// Default FALSE: a nil pointer (fresh install or a config predating an explicit
// toggle) means "use the default", which is OFF. The emulation depends on the
// upstream CodeWhisperer/Kiro model reliably calling a synthetic search tool to
// discover its deferred tools; that indirection is unreliable (the model often
// narrates then ends the turn without calling it, so it never sees its real
// tools and emits no tool_use). With this OFF, a tool_search request falls
// through to the normal path, which drops the unsupported search server tool
// and forwards every real tool eagerly so the model calls tools directly. Only
// an explicit true opts into the emulation.
func GetToolSearchEnabled() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.ToolSearchEnabled == nil {
		return false
	}
	return *cfg.ToolSearchEnabled
}

// UpdateToolSearchEnabled persists the tool-search emulation toggle.
func UpdateToolSearchEnabled(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ToolSearchEnabled = &enabled
	return Save()
}

// GetGlobalRateLimitPerMinute returns the proxy-wide request cap per minute.
// 0 means disabled (default).
func GetGlobalRateLimitPerMinute() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return 0
	}
	if cfg.GlobalRateLimitPerMinute < 0 {
		return 0
	}
	return cfg.GlobalRateLimitPerMinute
}

// UpdateGlobalRateLimitPerMinute persists the global rate-limit setting.
// A value <= 0 disables the limiter.
func UpdateGlobalRateLimitPerMinute(perMinute int) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if perMinute < 0 {
		perMinute = 0
	}
	cfg.GlobalRateLimitPerMinute = perMinute
	return Save()
}

// GetProxyURL 获取出站代理地址
func GetProxyURL() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return ""
	}
	return cfg.ProxyURL
}

// UpdateProxySettings 更新出站代理配置
func UpdateProxySettings(proxyURL string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ProxyURL = proxyURL
	return Save()
}

// GetAllowOverUsage returns whether over-usage is allowed when account quota is exhausted.
func GetAllowOverUsage() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.AllowOverUsage
}

// UpdateAllowOverUsage sets the over-usage setting and persists the change.
func UpdateAllowOverUsage(allow bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.AllowOverUsage = allow
	return Save()
}

// GetKnownModels returns the persisted last-known-good model catalog (raw
// upstream model ids). Returns a copy so callers can't mutate config state.
// Empty only on a fresh install that has never reached upstream.
func GetKnownModels() []string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || len(cfg.KnownModels) == 0 {
		return nil
	}
	out := make([]string, len(cfg.KnownModels))
	copy(out, cfg.KnownModels)
	return out
}

// SetKnownModels persists the model catalog fetched from upstream so a
// restart can serve real model ids before the first live fetch completes.
// The write is skipped (no-op, no disk churn) when the incoming list is
// empty or identical to what's already stored — refreshModelsCache calls
// this on every successful fetch, and most fetches return the same set.
func SetKnownModels(models []string) error {
	if len(models) == 0 {
		return nil
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return nil
	}
	if sameStringSet(cfg.KnownModels, models) {
		return nil
	}
	dedup := make([]string, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		key := strings.ToLower(strings.TrimSpace(m))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		dedup = append(dedup, strings.TrimSpace(m))
	}
	cfg.KnownModels = dedup
	return Save()
}

// sameStringSet reports whether a and b contain the same set of values
// (order-insensitive, case-insensitive on trimmed values).
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, s := range a {
		seen[strings.ToLower(strings.TrimSpace(s))] = true
	}
	for _, s := range b {
		if !seen[strings.ToLower(strings.TrimSpace(s))] {
			return false
		}
	}
	return true
}

// GetLogLevel returns the configured log level (debug/info/warn/error). Defaults to "info".
func GetLogLevel() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.LogLevel == "" {
		return "info"
	}
	return cfg.LogLevel
}

// UpdateLogLevel updates the log level setting and persists the change.
type KiroClientConfig struct {
	KiroVersion   string
	SystemVersion string
	NodeVersion   string
}

func GetKiroClientConfig() KiroClientConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	kiroVersion := "0.11.107"
	if cfg != nil && cfg.KiroVersion != "" {
		kiroVersion = cfg.KiroVersion
	}

	systemVersion := ""
	if cfg != nil {
		systemVersion = cfg.SystemVersion
	}
	if systemVersion == "" {
		systemVersion = defaultSystemVersion()
	}

	nodeVersion := "22.22.0"
	if cfg != nil && cfg.NodeVersion != "" {
		nodeVersion = cfg.NodeVersion
	}

	return KiroClientConfig{
		KiroVersion:   kiroVersion,
		SystemVersion: systemVersion,
		NodeVersion:   nodeVersion,
	}
}

func defaultSystemVersion() string {
	switch runtime.GOOS {
	case "windows":
		return "win32#10.0.22631"
	case "darwin":
		return "darwin#24.6.0"
	default:
		return "linux#6.6.87"
	}
}
