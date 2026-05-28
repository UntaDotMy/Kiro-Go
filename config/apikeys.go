package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// APIKey represents a single client API key with optional per-key limits.
//
// A key can carry up to four orthogonal kinds of limits:
//
//  1. Per-minute rate limit  (MinuteReqLimit)         — burst protection
//  2. Per-hour rate limit    (HourReqLimit)           — coarse burst control
//  3. Periodic budget        (DailyReqLimit, etc.)    — main quota; window
//     controlled by
//     ResetPeriod + ResetTZ
//  4. Lifetime budget        (LifetimeReqLimit, etc.) — never resets;
//     key auto-disables
//     when any dimension is
//     exhausted
//
// All limits are independent. A zero on any dimension means "no limit on that
// dimension". Zero on every dimension means an unlimited key.
//
// ResetPeriod controls when the periodic counters (DailyRequests/Tokens/
// Credits) reset:
//
//	"daily"   — every day at midnight in ResetTZ (default "UTC")
//	"weekly"  — every Monday at midnight in ResetTZ
//	"monthly" — first day of every month at midnight in ResetTZ
//	""        — same as "daily" for backward compat
//
// Expiry has three orthogonal modes; first one to fire wins:
//
//	ExpiresAt        — absolute Unix-seconds timestamp; 0 = ignored
//	LazyExpirySeconds — counts down from FirstUsedAt; 0 = ignored. Useful for
//	                   "this key is valid for 30 days from first use".
//
// Models is an optional whitelist; empty = any model.
type APIKey struct {
	ID                string   `json:"id"`
	Name              string   `json:"name,omitempty"`
	Key               string   `json:"key"`
	Enabled           bool     `json:"enabled"`
	CreatedAt         int64    `json:"createdAt"`
	LastUsedAt        int64    `json:"lastUsedAt,omitempty"`
	FirstUsedAt       int64    `json:"firstUsedAt,omitempty"`
	ExpiresAt         int64    `json:"expiresAt,omitempty"`         // absolute Unix seconds
	LazyExpirySeconds int64    `json:"lazyExpirySeconds,omitempty"` // countdown from FirstUsedAt
	Models            []string `json:"models,omitempty"`

	// Group restricts this API key to Kiro accounts whose Groups list
	// includes the named group. Empty = no restriction (any account is
	// eligible). This is the per-API-key flavor of pool routing — the
	// operator can set up a "premium" key that only routes to premium
	// Kiro accounts, and a "free" key that only routes to a smaller pool,
	// without the requesting client needing to know which model picks
	// which pool.
	Group string `json:"group,omitempty"`

	// Periodic limits. Period defaults to daily (UTC midnight) for backward
	// compatibility with pre-A13 keys.
	ResetPeriod    string  `json:"resetPeriod,omitempty"` // "daily" | "weekly" | "monthly"
	ResetTZ        string  `json:"resetTZ,omitempty"`     // IANA tz, e.g. "Asia/Singapore"; default "UTC"
	DailyReqLimit  int     `json:"dailyReqLimit,omitempty"`
	DailyTokLimit  int     `json:"dailyTokLimit,omitempty"`
	DailyCredLimit float64 `json:"dailyCredLimit,omitempty"`

	// Periodic counters; reset when the period rolls over.
	CountersDate  string  `json:"countersDate,omitempty"` // identifies the current period bucket
	DailyRequests int     `json:"dailyRequests,omitempty"`
	DailyTokens   int     `json:"dailyTokens,omitempty"`
	DailyCredits  float64 `json:"dailyCredits,omitempty"`

	// Per-minute / per-hour rate limits (request count only).
	MinuteReqLimit  int    `json:"minuteReqLimit,omitempty"`
	HourReqLimit    int    `json:"hourReqLimit,omitempty"`
	minuteBucketKey string // not persisted
	hourBucketKey   string // not persisted
	MinuteRequests  int    `json:"minuteRequests,omitempty"`
	HourRequests    int    `json:"hourRequests,omitempty"`

	// Lifetime caps. When any non-zero limit is reached, the key auto-disables.
	LifetimeReqLimit  int     `json:"lifetimeReqLimit,omitempty"`
	LifetimeTokLimit  int     `json:"lifetimeTokLimit,omitempty"`
	LifetimeCredLimit float64 `json:"lifetimeCredLimit,omitempty"`

	// Lifetime totals (persisted, used for both display and lifetime-limit
	// enforcement).
	TotalRequests int     `json:"totalRequests,omitempty"`
	TotalTokens   int     `json:"totalTokens,omitempty"`
	TotalCredits  float64 `json:"totalCredits,omitempty"`
}

// generateAPIKeySecret returns a random sk-kg-style key.
func generateAPIKeySecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "sk-kg-" + hex.EncodeToString(buf), nil
}

// generateAPIKeyID returns a 16-byte hex identifier.
func generateAPIKeyID() string {
	buf := make([]byte, 8)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}

// resolveResetTZ returns the time.Location for the key's ResetTZ field, or
// time.UTC if invalid / unset. Cached at the call site is fine; loading a tz
// is cheap.
func resolveResetTZ(tz string) *time.Location {
	if tz == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return loc
}

// periodBucketKey returns the canonical date string identifying the current
// period for a given reset period and timezone. Used as the CountersDate so
// counter resets are deterministic across daily/weekly/monthly modes.
func periodBucketKey(period, tz string) string {
	loc := resolveResetTZ(tz)
	now := time.Now().In(loc)
	switch period {
	case "weekly":
		// ISO week: year-Wweekno.
		year, week := now.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", year, week)
	case "monthly":
		return now.Format("2006-01")
	default: // "daily" or ""
		return now.Format("2006-01-02")
	}
}

// minuteBucket / hourBucket return integer keys identifying the current
// per-minute / per-hour window in the key's timezone.
func minuteBucket(tz string) string {
	return time.Now().In(resolveResetTZ(tz)).Format("200601021504")
}
func hourBucket(tz string) string {
	return time.Now().In(resolveResetTZ(tz)).Format("2006010215")
}

var apiKeyModelAliases = map[string][]string{
	"claude-opus-4-7":   {"claude-opus-4.7"},
	"claude-opus-4.7":   {"claude-opus-4-7"},
	"claude-opus-4-6":   {"claude-opus-4.6"},
	"claude-opus-4.6":   {"claude-opus-4-6"},
	"claude-opus-4-5":   {"claude-opus-4.5"},
	"claude-opus-4.5":   {"claude-opus-4-5"},
	"claude-sonnet-4-6": {"claude-sonnet-4.6"},
	"claude-sonnet-4.6": {"claude-sonnet-4-6"},
	"claude-sonnet-4-5": {"claude-sonnet-4.5"},
	"claude-sonnet-4.5": {"claude-sonnet-4-5"},
	"claude-haiku-4-5":  {"claude-haiku-4.5"},
	"claude-haiku-4.5":  {"claude-haiku-4-5"},
}

func modelWhitelistCandidates(model string) []string {
	normalizedModel := strings.ToLower(strings.TrimSpace(model))
	if normalizedModel == "" {
		return nil
	}

	candidates := []string{normalizedModel}
	for _, alias := range apiKeyModelAliases[normalizedModel] {
		candidates = append(candidates, alias)
	}
	return candidates
}

func modelIsAllowedByWhitelist(allowedModels []string, model string) bool {
	if len(allowedModels) == 0 {
		return true
	}
	candidates := modelWhitelistCandidates(model)
	if len(candidates) == 0 {
		return false
	}

	for _, allowedModel := range allowedModels {
		normalizedAllowed := strings.ToLower(strings.TrimSpace(allowedModel))
		if normalizedAllowed == "" {
			continue
		}
		for _, candidate := range candidates {
			if normalizedAllowed == candidate {
				return true
			}
		}
	}
	return false
}

// IsModelAllowedForAPIKey reports whether the given model id is permitted
// for the supplied API key. Empty key.Models = "no restriction" (returns
// true for any model). Comparison uses the same dotted/dashed alias
// resolver that the request-time pre-flight gate (CheckAPIKeyLimit) uses,
// so a key configured with "claude-opus-4.7" still matches an inbound
// "claude-opus-4-7" and vice versa.
//
// Used by /v1/models to filter the listed models down to what the calling
// key is allowed to invoke.
func IsModelAllowedForAPIKey(k APIKey, model string) bool {
	return modelIsAllowedByWhitelist(k.Models, model)
}

// GetAPIKeys returns a snapshot of all configured API keys.
func GetAPIKeys() []APIKey {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]APIKey, len(cfg.APIKeys))
	copy(out, cfg.APIKeys)
	return out
}

// AddAPIKey appends a new key with a freshly generated id+secret. The
// optional group string restricts which Kiro accounts the key can route
// to (passed via per-key Group field). An empty group means no
// restriction.
func AddAPIKey(name string, models []string, reqLimit, tokLimit int, credLimit float64, expiresAt int64, group string) (*APIKey, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	secret, err := generateAPIKeySecret()
	if err != nil {
		return nil, err
	}
	key := APIKey{
		ID:             generateAPIKeyID(),
		Name:           strings.TrimSpace(name),
		Key:            secret,
		Enabled:        true,
		CreatedAt:      time.Now().Unix(),
		ExpiresAt:      expiresAt,
		Models:         models,
		Group:          strings.ToLower(strings.TrimSpace(group)),
		DailyReqLimit:  reqLimit,
		DailyTokLimit:  tokLimit,
		DailyCredLimit: credLimit,
		ResetPeriod:    "daily",
		ResetTZ:        "UTC",
		CountersDate:   periodBucketKey("daily", "UTC"),
	}
	cfg.APIKeys = append(cfg.APIKeys, key)
	if err := Save(); err != nil {
		return nil, err
	}
	return &key, nil
}

// UpdateAPIKeyOptions captures every patchable field on an APIKey. All
// fields are pointers so omission means "leave as-is".
type UpdateAPIKeyOptions struct {
	Name              *string
	Enabled           *bool
	Models            *[]string
	Group             *string
	ExpiresAt         *int64
	LazyExpirySeconds *int64
	ResetPeriod       *string
	ResetTZ           *string
	DailyReqLimit     *int
	DailyTokLimit     *int
	DailyCredLimit    *float64
	MinuteReqLimit    *int
	HourReqLimit      *int
	LifetimeReqLimit  *int
	LifetimeTokLimit  *int
	LifetimeCredLimit *float64
}

// UpdateAPIKey applies a patch to an existing key.
func UpdateAPIKey(id string, opts UpdateAPIKeyOptions) bool {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i := range cfg.APIKeys {
		k := &cfg.APIKeys[i]
		if k.ID != id {
			continue
		}
		if opts.Name != nil {
			k.Name = strings.TrimSpace(*opts.Name)
		}
		if opts.Enabled != nil {
			k.Enabled = *opts.Enabled
		}
		if opts.Models != nil {
			k.Models = *opts.Models
		}
		if opts.Group != nil {
			// Lowercase + trim so admin-typed casing matches account.Groups
			// lookups (which use EqualFold via accountInGroup).
			k.Group = strings.ToLower(strings.TrimSpace(*opts.Group))
		}
		if opts.ExpiresAt != nil {
			k.ExpiresAt = *opts.ExpiresAt
		}
		if opts.LazyExpirySeconds != nil {
			k.LazyExpirySeconds = *opts.LazyExpirySeconds
		}
		if opts.ResetPeriod != nil {
			k.ResetPeriod = *opts.ResetPeriod
		}
		if opts.ResetTZ != nil {
			k.ResetTZ = *opts.ResetTZ
		}
		if opts.DailyReqLimit != nil {
			k.DailyReqLimit = *opts.DailyReqLimit
		}
		if opts.DailyTokLimit != nil {
			k.DailyTokLimit = *opts.DailyTokLimit
		}
		if opts.DailyCredLimit != nil {
			k.DailyCredLimit = *opts.DailyCredLimit
		}
		if opts.MinuteReqLimit != nil {
			k.MinuteReqLimit = *opts.MinuteReqLimit
		}
		if opts.HourReqLimit != nil {
			k.HourReqLimit = *opts.HourReqLimit
		}
		if opts.LifetimeReqLimit != nil {
			k.LifetimeReqLimit = *opts.LifetimeReqLimit
		}
		if opts.LifetimeTokLimit != nil {
			k.LifetimeTokLimit = *opts.LifetimeTokLimit
		}
		if opts.LifetimeCredLimit != nil {
			k.LifetimeCredLimit = *opts.LifetimeCredLimit
		}
		_ = Save()
		return true
	}
	return false
}

// DeleteAPIKey removes a key by id.
func DeleteAPIKey(id string) bool {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, k := range cfg.APIKeys {
		if k.ID == id {
			cfg.APIKeys = append(cfg.APIKeys[:i], cfg.APIKeys[i+1:]...)
			_ = Save()
			return true
		}
	}
	return false
}

// FindAPIKeyBySecret returns a copy of the key matching the supplied secret.
func FindAPIKeyBySecret(secret string) *APIKey {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for i := range cfg.APIKeys {
		if cfg.APIKeys[i].Key == secret {
			out := cfg.APIKeys[i]
			return &out
		}
	}
	return nil
}

// CheckAPIKeyLimit returns (rejected, reason) without committing any usage.
// Used as a pre-flight gate before the upstream call so an exhausted key is
// refused with HTTP 429 instead of burning a Kiro account quota slot. This
// function may roll over per-minute / per-hour / periodic buckets if the
// window has crossed (which is correct behaviour — the counter starts fresh
// in the new window), but it never increments any request counter.
//
// Token / credit limits are checked against the current totals + 0 because
// the upstream call hasn't happened yet; the actual values are committed by
// ConsumeAPIKey on the success path.
func CheckAPIKeyLimit(id, model string) (rejected bool, reason string) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i := range cfg.APIKeys {
		if cfg.APIKeys[i].ID != id {
			continue
		}
		k := &cfg.APIKeys[i]
		now := time.Now().Unix()
		if !k.Enabled {
			return true, "key disabled"
		}
		if k.ExpiresAt > 0 && now > k.ExpiresAt {
			k.Enabled = false
			_ = Save()
			return true, "key expired"
		}
		if k.LazyExpirySeconds > 0 && k.FirstUsedAt > 0 && now > k.FirstUsedAt+k.LazyExpirySeconds {
			k.Enabled = false
			_ = Save()
			return true, "key expired (lazy)"
		}
		if model != "" && !modelIsAllowedByWhitelist(k.Models, model) {
			return true, "model '" + model + "' not allowed for this key"
		}
		curMin := minuteBucket(k.ResetTZ)
		if k.minuteBucketKey != curMin {
			k.minuteBucketKey = curMin
			k.MinuteRequests = 0
		}
		curHour := hourBucket(k.ResetTZ)
		if k.hourBucketKey != curHour {
			k.hourBucketKey = curHour
			k.HourRequests = 0
		}
		if k.MinuteReqLimit > 0 && k.MinuteRequests+1 > k.MinuteReqLimit {
			return true, "per-minute rate limit reached"
		}
		if k.HourReqLimit > 0 && k.HourRequests+1 > k.HourReqLimit {
			return true, "per-hour rate limit reached"
		}
		bucket := periodBucketKey(k.ResetPeriod, k.ResetTZ)
		if k.CountersDate != bucket {
			k.CountersDate = bucket
			k.DailyRequests = 0
			k.DailyTokens = 0
			k.DailyCredits = 0
		}
		if k.DailyReqLimit > 0 && k.DailyRequests+1 > k.DailyReqLimit {
			return true, "periodic request limit reached"
		}
		if k.LifetimeReqLimit > 0 && k.TotalRequests+1 > k.LifetimeReqLimit {
			return true, "lifetime request limit reached"
		}
		// Token / credit limits checked again on commit with real values; here
		// we only verify the current totals haven't already crossed the line.
		if k.DailyTokLimit > 0 && k.DailyTokens >= k.DailyTokLimit {
			return true, "periodic token limit reached"
		}
		if k.DailyCredLimit > 0 && k.DailyCredits >= k.DailyCredLimit {
			return true, "periodic credit limit reached"
		}
		if k.LifetimeTokLimit > 0 && k.TotalTokens >= k.LifetimeTokLimit {
			return true, "lifetime token limit reached"
		}
		if k.LifetimeCredLimit > 0 && k.TotalCredits >= k.LifetimeCredLimit {
			return true, "lifetime credit limit reached"
		}
		_ = Save() // persist any bucket roll-over zeroing
		return false, ""
	}
	return false, ""
}

// ConsumeAPIKey records request usage against a key's counters and lifetime
// totals. Order of checks (any failure rejects without consuming):
//
//  1. Key disabled               → reject
//  2. Absolute expiry passed     → reject (also auto-disables)
//  3. Lazy expiry triggered      → reject (also auto-disables)
//  4. Model not in whitelist     → reject
//  5. Per-minute rate limit hit  → reject
//  6. Per-hour rate limit hit    → reject
//  7. Periodic limit reached     → reject
//  8. Lifetime limit reached     → reject (also auto-disables)
//
// On success: increments all counters, updates LastUsedAt + FirstUsedAt,
// persists.
func ConsumeAPIKey(id string, tokens int, credits float64, model string) (rejected bool, reason string) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i := range cfg.APIKeys {
		if cfg.APIKeys[i].ID != id {
			continue
		}
		k := &cfg.APIKeys[i]

		now := time.Now().Unix()
		if !k.Enabled {
			return true, "key disabled"
		}
		// Absolute expiry.
		if k.ExpiresAt > 0 && now > k.ExpiresAt {
			k.Enabled = false
			_ = Save()
			return true, "key expired"
		}
		// Lazy expiry: countdown from FirstUsedAt.
		if k.LazyExpirySeconds > 0 && k.FirstUsedAt > 0 && now > k.FirstUsedAt+k.LazyExpirySeconds {
			k.Enabled = false
			_ = Save()
			return true, "key expired (lazy)"
		}
		// Model whitelist.
		if model != "" && !modelIsAllowedByWhitelist(k.Models, model) {
			return true, "model '" + model + "' not allowed for this key"
		}
		// Reset minute / hour buckets if rolled over.
		curMin := minuteBucket(k.ResetTZ)
		if k.minuteBucketKey != curMin {
			k.minuteBucketKey = curMin
			k.MinuteRequests = 0
		}
		curHour := hourBucket(k.ResetTZ)
		if k.hourBucketKey != curHour {
			k.hourBucketKey = curHour
			k.HourRequests = 0
		}
		// Per-minute rate limit (requests only).
		if k.MinuteReqLimit > 0 && k.MinuteRequests+1 > k.MinuteReqLimit {
			return true, "per-minute rate limit reached"
		}
		// Per-hour rate limit.
		if k.HourReqLimit > 0 && k.HourRequests+1 > k.HourReqLimit {
			return true, "per-hour rate limit reached"
		}
		// Periodic counters reset.
		bucket := periodBucketKey(k.ResetPeriod, k.ResetTZ)
		if k.CountersDate != bucket {
			k.CountersDate = bucket
			k.DailyRequests = 0
			k.DailyTokens = 0
			k.DailyCredits = 0
		}
		// Periodic limits.
		if k.DailyReqLimit > 0 && k.DailyRequests+1 > k.DailyReqLimit {
			return true, "periodic request limit reached"
		}
		if k.DailyTokLimit > 0 && k.DailyTokens+tokens > k.DailyTokLimit {
			return true, "periodic token limit reached"
		}
		if k.DailyCredLimit > 0 && k.DailyCredits+credits > k.DailyCredLimit {
			return true, "periodic credit limit reached"
		}
		// Lifetime limits — these auto-disable the key when reached.
		if k.LifetimeReqLimit > 0 && k.TotalRequests+1 > k.LifetimeReqLimit {
			k.Enabled = false
			_ = Save()
			return true, "lifetime request limit reached (key disabled)"
		}
		if k.LifetimeTokLimit > 0 && k.TotalTokens+tokens > k.LifetimeTokLimit {
			k.Enabled = false
			_ = Save()
			return true, "lifetime token limit reached (key disabled)"
		}
		if k.LifetimeCredLimit > 0 && k.TotalCredits+credits > k.LifetimeCredLimit {
			k.Enabled = false
			_ = Save()
			return true, "lifetime credit limit reached (key disabled)"
		}
		// All checks passed; commit.
		k.MinuteRequests++
		k.HourRequests++
		k.DailyRequests++
		k.DailyTokens += tokens
		k.DailyCredits += credits
		k.TotalRequests++
		k.TotalTokens += tokens
		k.TotalCredits += credits
		k.LastUsedAt = now
		if k.FirstUsedAt == 0 {
			k.FirstUsedAt = now
		}
		_ = Save()
		return false, ""
	}
	return false, ""
}
