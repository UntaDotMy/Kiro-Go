package config

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"kiro-go/logger"
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

// generateAPIKeyID returns a 16-byte hex identifier. crypto/rand.Read
// effectively never fails, but on a broken entropy source it would otherwise
// silently yield an all-zero buffer (and a duplicate id across keys). On the
// rare error we log it and fall back to a nanosecond-timestamp seed so the id
// stays unique and non-zero rather than colliding.
func generateAPIKeyID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		logger.Warnf("[apikeys] crypto/rand unavailable for key id, using time fallback: %v", err)
		binary.BigEndian.PutUint64(buf, uint64(time.Now().UnixNano()))
	}
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

// modelWhitelistCandidates returns the set of ids the per-key model
// allowlist should compare against. Mirrors pool.modelLookupKeys: the
// first entry is the input as-is (lowercased + trimmed), the second is
// the dotted-vs-dashed twin for Claude family ids — Claude Code uses
// dashed (claude-opus-4-7), Kiro upstream uses dotted (claude-opus-4.7),
// they refer to the same model. The transform is purely mechanical so
// future Claude minor versions (4-8 / 4.8 / 5-0 / etc.) work without a
// code change.
func modelWhitelistCandidates(model string) []string {
	normalizedModel := strings.ToLower(strings.TrimSpace(model))
	if normalizedModel == "" {
		return nil
	}
	candidates := []string{normalizedModel}
	if twin := claudeAliasTwin(normalizedModel); twin != "" && twin != normalizedModel {
		candidates = append(candidates, twin)
	}
	return candidates
}

// claudeAliasTwin returns the dotted-or-dashed twin of a Claude family
// id, or "" if the input doesn't match the family-version shape we know
// is interchangeable. Mirrors pool.claudeAliasTwin so allowlist and
// router stay in lockstep — keep these two implementations identical
// when adding families or relaxing the version pattern.
//
// Pattern: "claude-<family>-<digits><sep><digits>" where family ∈ {opus,
// sonnet, haiku}, sep is "." or "-", each side is 1-2 digits. Anything
// else (bare family, dated suffix, non-claude id) returns "".
func claudeAliasTwin(id string) string {
	const prefix = "claude-"
	if !strings.HasPrefix(id, prefix) {
		return ""
	}
	rest := id[len(prefix):]
	for _, fam := range []string{"opus", "sonnet", "haiku"} {
		famPrefix := fam + "-"
		if !strings.HasPrefix(rest, famPrefix) {
			continue
		}
		ver := rest[len(famPrefix):]
		sepIdx := -1
		for i := 0; i < len(ver); i++ {
			if ver[i] == '.' || ver[i] == '-' {
				sepIdx = i
				break
			}
		}
		if sepIdx < 1 || sepIdx > 2 {
			return ""
		}
		major := ver[:sepIdx]
		minor := ver[sepIdx+1:]
		if len(minor) < 1 || len(minor) > 2 {
			return ""
		}
		if !apikeyAllDigits(major) || !apikeyAllDigits(minor) {
			return ""
		}
		if sepIdx+1+len(minor) != len(ver) {
			return ""
		}
		var altSep byte
		if ver[sepIdx] == '.' {
			altSep = '-'
		} else {
			altSep = '.'
		}
		return prefix + famPrefix + major + string(altSep) + minor
	}
	return ""
}

func apikeyAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
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

// AddRawAPIKey appends a fully-formed APIKey record verbatim (id + secret value
// supplied by the caller), used by the 9router import path where the inbound key
// value is preserved as-is rather than minted. Fills in a sane CreatedAt /
// ResetPeriod / CountersDate when the caller left them zero, so the periodic
// limiter behaves like a natively-created key. Deduplicates on the literal key
// value: a record whose Key already exists is a no-op (returns nil).
func AddRawAPIKey(k APIKey) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if strings.TrimSpace(k.Key) == "" {
		return fmt.Errorf("key value is required")
	}
	for i := range cfg.APIKeys {
		if cfg.APIKeys[i].Key == k.Key {
			return nil // already present; idempotent import
		}
	}
	if strings.TrimSpace(k.ID) == "" {
		k.ID = generateAPIKeyID()
	}
	if k.CreatedAt == 0 {
		k.CreatedAt = time.Now().Unix()
	}
	if k.ResetPeriod == "" {
		k.ResetPeriod = "daily"
	}
	if k.ResetTZ == "" {
		k.ResetTZ = "UTC"
	}
	if k.CountersDate == "" {
		k.CountersDate = periodBucketKey(k.ResetPeriod, k.ResetTZ)
	}
	cfg.APIKeys = append(cfg.APIKeys, k)
	return Save()
}

// AddAPIKey appends a new key with a freshly generated id+secret.
func AddAPIKey(name string, models []string, reqLimit, tokLimit int, credLimit float64, expiresAt int64) (*APIKey, error) {
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
	return checkAPIKeyLimit(id, model, true)
}

// CheckAPIKeyRateLimit is the model-AGNOSTIC gate for metadata routes
// (/v1/models, /v1/stats) that authenticate a key but don't invoke a model. It
// enforces enable / absolute-expiry / lazy-expiry / per-minute / per-hour /
// periodic / lifetime limits — closing the hole where a disabled, expired, or
// rate-exhausted key could still hit those routes — but skips the model-
// whitelist check so a restricted key can still list its (filtered) catalog.
func CheckAPIKeyRateLimit(id string) (rejected bool, reason string) {
	return checkAPIKeyLimit(id, "", false)
}

// checkAPIKeyLimit is the shared pre-flight implementation. checkModel gates the
// model-whitelist / required-model checks so metadata routes can reuse the
// rate/quota/expiry logic without a model dimension.
func checkAPIKeyLimit(id, model string, checkModel bool) (rejected bool, reason string) {
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
		if checkModel {
			if model != "" && !modelIsAllowedByWhitelist(k.Models, model) {
				return true, "model '" + model + "' not allowed for this key"
			}
			// Defense-in-depth: a key that declares a model whitelist must never
			// be served an EMPTY model. Callers should reject empty model upstream
			// (the request-shape validators do), but if one slips through, an
			// empty model would otherwise short-circuit the `model != ""` guard
			// above and bypass the whitelist entirely. A key with no whitelist
			// (len==0) is unrestricted and unaffected. Metadata routes pass
			// checkModel=false so this never blocks /v1/models or /v1/stats.
			if model == "" && len(k.Models) > 0 {
				return true, "model is required for this key"
			}
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
		// Token / credit limits: token & credit amounts per request are only
		// known after the response completes, so the pre-flight gate can't
		// predict the next request's size. Instead we reject once the recorded
		// totals have crossed the line (ConsumeAPIKey records actual usage,
		// including the request that crossed it). This is the gate that enforces
		// periodic token/credit caps — ConsumeAPIKey only records, never blocks.
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
	// Key ID not found. Callers only pass an id that was matched at auth time,
	// so a miss here means the key was DELETED between auth and this gate —
	// reject the in-flight request rather than treating the absent key as
	// unrestricted (which would let a just-revoked key through).
	return true, "key not found or revoked"
}

// ConsumeAPIKey records the usage of a request that has ALREADY completed and
// been delivered to the client. Because the response is already on the wire
// (streaming can't be un-sent), this function cannot gate the current request —
// blocking is the pre-flight gate's job (CheckAPIKeyLimit). Its responsibilities
// are therefore:
//
//  1. ALWAYS record actual usage (requests, tokens, credits) against the minute/
//     hour/periodic/lifetime counters, after rolling over any expired bucket.
//     This is critical: token & credit amounts are unknown until the response
//     finishes, so if we skipped recording whenever a request crossed the limit
//     (the old behavior), the counter would freeze just below the threshold and
//     the periodic token/credit cap would NEVER trip on the next pre-flight —
//     i.e. the key would serve forever. We must record the crossing so the next
//     CheckAPIKeyLimit sees DailyTokens >= limit and rejects.
//  2. Auto-disable the key when a HARD cap is reached: absolute/lazy expiry and
//     lifetime request/token/credit limits. (Periodic caps are NOT permanent —
//     they reset on the period boundary and are enforced purely by the
//     pre-flight gate, so we do not disable on them.)
//
// The (rejected, reason) return is advisory (used for logging) since callers
// already committed the response; recording + auto-disable are the real effects.
func ConsumeAPIKey(id string, tokens int, credits float64, model string) (rejected bool, reason string) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i := range cfg.APIKeys {
		if cfg.APIKeys[i].ID != id {
			continue
		}
		k := &cfg.APIKeys[i]
		now := time.Now().Unix()

		// Roll over minute / hour / periodic buckets BEFORE recording so usage
		// lands in the current window.
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
		bucket := periodBucketKey(k.ResetPeriod, k.ResetTZ)
		if k.CountersDate != bucket {
			k.CountersDate = bucket
			k.DailyRequests = 0
			k.DailyTokens = 0
			k.DailyCredits = 0
		}

		// Record actual usage unconditionally (the request happened).
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

		// Evaluate caps for reporting + hard-cap auto-disable.
		rejected, reason = false, ""

		// Expiry → disable.
		if k.ExpiresAt > 0 && now > k.ExpiresAt {
			k.Enabled = false
			rejected, reason = true, "key expired"
		} else if k.LazyExpirySeconds > 0 && k.FirstUsedAt > 0 && now > k.FirstUsedAt+k.LazyExpirySeconds {
			k.Enabled = false
			rejected, reason = true, "key expired (lazy)"
		}

		// Lifetime caps → disable (permanent until an operator intervenes).
		if k.LifetimeReqLimit > 0 && k.TotalRequests >= k.LifetimeReqLimit {
			k.Enabled = false
			rejected, reason = true, "lifetime request limit reached (key disabled)"
		}
		if k.LifetimeTokLimit > 0 && k.TotalTokens >= k.LifetimeTokLimit {
			k.Enabled = false
			rejected, reason = true, "lifetime token limit reached (key disabled)"
		}
		if k.LifetimeCredLimit > 0 && k.TotalCredits >= k.LifetimeCredLimit {
			k.Enabled = false
			rejected, reason = true, "lifetime credit limit reached (key disabled)"
		}

		// Periodic caps reached → report only. The NEXT request is blocked by
		// CheckAPIKeyLimit (DailyX >= limit); the period rollover re-enables
		// organically. Never disable the key for a periodic cap.
		if !rejected {
			switch {
			case k.DailyReqLimit > 0 && k.DailyRequests >= k.DailyReqLimit:
				rejected, reason = true, "periodic request limit reached"
			case k.DailyTokLimit > 0 && k.DailyTokens >= k.DailyTokLimit:
				rejected, reason = true, "periodic token limit reached"
			case k.DailyCredLimit > 0 && k.DailyCredits >= k.DailyCredLimit:
				rejected, reason = true, "periodic credit limit reached"
			}
		}

		_ = Save()
		return rejected, reason
	}
	// Key ID not found — deleted between auth and this post-response accounting
	// call. The response already streamed (this function never gates), so there
	// is nothing to record; surface a non-empty reason for the caller's log.
	return true, "key not found or revoked"
}
