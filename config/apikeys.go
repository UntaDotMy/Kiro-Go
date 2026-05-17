package config

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
)

// APIKey represents a single client API key with optional per-key limits.
//
// A request authenticated against an APIKey consumes its DailyRequests,
// DailyTokens, and DailyCredits counters; a counter is consumed only if the
// corresponding limit is non-zero. A zero limit means "unlimited" for that
// dimension. Counters reset at midnight UTC every day (CountersDate is the
// UTC YYYY-MM-DD the current counters apply to).
//
// Models is an optional whitelist of model IDs the key may invoke. An empty
// list means "any model".
type APIKey struct {
	ID            string  `json:"id"`                      // stable internal identifier (UUID)
	Name          string  `json:"name,omitempty"`          // human-readable label
	Key           string  `json:"key"`                     // secret bearer token
	Enabled       bool    `json:"enabled"`                 // false = revoked
	CreatedAt     int64   `json:"createdAt"`               // Unix seconds
	LastUsedAt    int64   `json:"lastUsedAt,omitempty"`    // Unix seconds
	ExpiresAt     int64   `json:"expiresAt,omitempty"`     // 0 = no expiry
	Models        []string `json:"models,omitempty"`       // optional whitelist
	DailyReqLimit int     `json:"dailyReqLimit,omitempty"` // 0 = unlimited
	DailyTokLimit int     `json:"dailyTokLimit,omitempty"` // 0 = unlimited
	DailyCredLimit float64 `json:"dailyCredLimit,omitempty"` // 0 = unlimited

	// Counters reset every UTC midnight. CountersDate is the YYYY-MM-DD they
	// belong to; mismatched date triggers a reset on next consume call.
	CountersDate    string  `json:"countersDate,omitempty"`
	DailyRequests   int     `json:"dailyRequests,omitempty"`
	DailyTokens     int     `json:"dailyTokens,omitempty"`
	DailyCredits    float64 `json:"dailyCredits,omitempty"`

	// Lifetime totals (persisted, never reset).
	TotalRequests int     `json:"totalRequests,omitempty"`
	TotalTokens   int     `json:"totalTokens,omitempty"`
	TotalCredits  float64 `json:"totalCredits,omitempty"`
}

// generateAPIKeySecret returns a random sk-clb-style key.
func generateAPIKeySecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "sk-kg-" + hex.EncodeToString(buf), nil
}

// generateAPIKeyID returns a 16-byte hex identifier (not a UUID for brevity).
func generateAPIKeyID() string {
	buf := make([]byte, 8)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}

// utcDateString returns today's date in UTC as YYYY-MM-DD. Used for daily
// counter reset comparisons.
func utcDateString() string {
	return time.Now().UTC().Format("2006-01-02")
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

// AddAPIKey appends a new key with a freshly generated id+secret. Returns
// the created key (caller must persist via Save() after — done internally).
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
		CountersDate:   utcDateString(),
	}
	cfg.APIKeys = append(cfg.APIKeys, key)
	if err := Save(); err != nil {
		return nil, err
	}
	return &key, nil
}

// UpdateAPIKey patches a key's metadata (name, models, limits, expiry,
// enabled). The secret is never changed by this call. Returns true if the
// key was found.
func UpdateAPIKey(id string, name *string, enabled *bool, models *[]string, reqLimit, tokLimit *int, credLimit *float64, expiresAt *int64) bool {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, k := range cfg.APIKeys {
		if k.ID != id {
			continue
		}
		if name != nil {
			cfg.APIKeys[i].Name = strings.TrimSpace(*name)
		}
		if enabled != nil {
			cfg.APIKeys[i].Enabled = *enabled
		}
		if models != nil {
			cfg.APIKeys[i].Models = *models
		}
		if reqLimit != nil {
			cfg.APIKeys[i].DailyReqLimit = *reqLimit
		}
		if tokLimit != nil {
			cfg.APIKeys[i].DailyTokLimit = *tokLimit
		}
		if credLimit != nil {
			cfg.APIKeys[i].DailyCredLimit = *credLimit
		}
		if expiresAt != nil {
			cfg.APIKeys[i].ExpiresAt = *expiresAt
		}
		_ = Save()
		return true
	}
	return false
}

// DeleteAPIKey removes a key by id. Returns true if found.
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
// Constant-time over the slice (compares each candidate). Returns nil if not
// found.
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

// ConsumeAPIKey records request usage against a key's daily counters and
// lifetime totals. Returns:
//   - rejected = true and a human reason when a daily limit would be exceeded
//     (in which case the counters are NOT incremented and the request must be
//     refused upstream).
//   - rejected = false on success; counters are incremented and the change is
//     persisted asynchronously by the next Save().
//
// The caller must already have validated the key (FindAPIKeyBySecret + Enabled
// + ExpiresAt). model is the canonical model id for whitelist enforcement; pass
// "" to skip the model check.
func ConsumeAPIKey(id string, tokens int, credits float64, model string) (rejected bool, reason string) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i := range cfg.APIKeys {
		if cfg.APIKeys[i].ID != id {
			continue
		}
		k := &cfg.APIKeys[i]
		// Reset counters if the day rolled over.
		today := utcDateString()
		if k.CountersDate != today {
			k.CountersDate = today
			k.DailyRequests = 0
			k.DailyTokens = 0
			k.DailyCredits = 0
		}
		// Model whitelist check.
		if len(k.Models) > 0 && model != "" {
			allowed := false
			for _, m := range k.Models {
				if m == model {
					allowed = true
					break
				}
			}
			if !allowed {
				return true, "model '" + model + "' not allowed for this key"
			}
		}
		// Daily limit checks BEFORE consumption (atomic-ish via the lock).
		if k.DailyReqLimit > 0 && k.DailyRequests+1 > k.DailyReqLimit {
			return true, "daily request limit reached"
		}
		if k.DailyTokLimit > 0 && k.DailyTokens+tokens > k.DailyTokLimit {
			return true, "daily token limit reached"
		}
		if k.DailyCredLimit > 0 && k.DailyCredits+credits > k.DailyCredLimit {
			return true, "daily credit limit reached"
		}
		k.DailyRequests++
		k.DailyTokens += tokens
		k.DailyCredits += credits
		k.TotalRequests++
		k.TotalTokens += tokens
		k.TotalCredits += credits
		k.LastUsedAt = time.Now().Unix()
		_ = Save()
		return false, ""
	}
	return false, ""
}
