package config

import (
	"crypto/subtle"
	"time"
)

// KeyStatusPeriod is the periodic-budget view of a key's status: current
// window counters, their limits (0 = unlimited), and when the window resets.
type KeyStatusPeriod struct {
	ResetPeriod   string  `json:"resetPeriod"`
	ResetTZ       string  `json:"resetTZ"`
	NextReset     int64   `json:"nextReset"` // unix seconds; 0 if unknown
	Requests      int     `json:"requests"`
	Tokens        int     `json:"tokens"`
	Credits       float64 `json:"credits"`
	ReqLimit      int     `json:"reqLimit"`
	TokLimit      int     `json:"tokLimit"`
	CredLimit     float64 `json:"credLimit"`
}

// KeyStatusLifetime is the never-resets view: cumulative usage and hard caps.
type KeyStatusLifetime struct {
	Requests  int     `json:"requests"`
	Tokens    int     `json:"tokens"`
	Credits   float64 `json:"credits"`
	ReqLimit  int     `json:"reqLimit"`
	TokLimit  int     `json:"tokLimit"`
	CredLimit float64 `json:"credLimit"`
}

// KeyStatus is the safe, customer-facing view of an API key returned by the
// public portal endpoint. It deliberately OMITS the raw key, the internal id,
// and the human label (Name) so the response leaks nothing a customer
// shouldn't see about the operator's setup. It reports only what the holder of
// the key needs: is it active, when does it expire, what can it call, and how
// much of each quota dimension is left.
type KeyStatus struct {
	Valid       bool              `json:"valid"`
	Enabled     bool              `json:"enabled"`
	ExpiresAt   int64             `json:"expiresAt"`   // absolute expiry, 0 = none
	LazyExpiry  int64             `json:"lazyExpiry"`  // computed absolute expiry from first-use, 0 = none
	Models      []string          `json:"models"`      // allowed model whitelist; empty = all
	Period      KeyStatusPeriod   `json:"period"`
	Lifetime    KeyStatusLifetime `json:"lifetime"`
	MinuteLimit int               `json:"minuteLimit"`
	HourLimit   int               `json:"hourLimit"`
}

// nextPeriodReset returns the unix-seconds timestamp of the next reset boundary
// for the given period + timezone. Mirrors periodBucketKey's bucketing so the
// customer sees exactly when their periodic counters roll over.
func nextPeriodReset(period, tz string) int64 {
	loc := resolveResetTZ(tz)
	now := time.Now().In(loc)
	switch period {
	case "weekly":
		// Next Monday 00:00 local. Go: Sunday=0 .. Saturday=6.
		daysUntilMon := (8 - int(now.Weekday())) % 7
		if daysUntilMon == 0 {
			daysUntilMon = 7
		}
		next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, daysUntilMon)
		return next.Unix()
	case "monthly":
		// First day of next month, 00:00 local.
		y, m := now.Year(), now.Month()
		next := time.Date(y, m, 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
		return next.Unix()
	default: // daily or ""
		next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
		return next.Unix()
	}
}

// GetKeyStatusBySecret looks up a key by its raw secret (constant-time compare
// against every configured key) and returns a customer-safe status snapshot.
// Returns (KeyStatus{Valid:false}, false) when no key matches, when the key is
// disabled, or when it has expired — a single indistinguishable "invalid"
// outcome so the endpoint can't be used as an oracle to enumerate which keys
// exist vs. which are merely disabled.
//
// It rolls over any stale periodic/rate buckets in the returned view (without
// persisting) so the reported counters reflect the CURRENT window, matching
// what the next real request would see.
func GetKeyStatusBySecret(secret string) (KeyStatus, bool) {
	if secret == "" {
		return KeyStatus{Valid: false}, false
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	now := time.Now().Unix()
	for i := range cfg.APIKeys {
		k := &cfg.APIKeys[i]
		// Constant-time compare so timing can't reveal a near-match prefix.
		if subtle.ConstantTimeCompare([]byte(secret), []byte(k.Key)) != 1 {
			continue
		}
		// Matched. Treat disabled / expired as "invalid" with no detail so the
		// endpoint is not an oracle.
		if !k.Enabled {
			return KeyStatus{Valid: false}, false
		}
		if k.ExpiresAt > 0 && now > k.ExpiresAt {
			return KeyStatus{Valid: false}, false
		}
		var lazyAbs int64
		if k.LazyExpirySeconds > 0 && k.FirstUsedAt > 0 {
			lazyAbs = k.FirstUsedAt + k.LazyExpirySeconds
			if now > lazyAbs {
				return KeyStatus{Valid: false}, false
			}
		}

		// Compute the current-window counters as a read-only view: if the stored
		// CountersDate is for an older window, the live counters are effectively
		// zero now.
		periodReq, periodTok, periodCred := k.DailyRequests, k.DailyTokens, k.DailyCredits
		if k.CountersDate != periodBucketKey(k.ResetPeriod, k.ResetTZ) {
			periodReq, periodTok, periodCred = 0, 0, 0
		}

		models := make([]string, len(k.Models))
		copy(models, k.Models)

		resetPeriod := k.ResetPeriod
		if resetPeriod == "" {
			resetPeriod = "daily"
		}
		resetTZ := k.ResetTZ
		if resetTZ == "" {
			resetTZ = "UTC"
		}

		return KeyStatus{
			Valid:      true,
			Enabled:    true,
			ExpiresAt:  k.ExpiresAt,
			LazyExpiry: lazyAbs,
			Models:     models,
			Period: KeyStatusPeriod{
				ResetPeriod: resetPeriod,
				ResetTZ:     resetTZ,
				NextReset:   nextPeriodReset(resetPeriod, resetTZ),
				Requests:    periodReq,
				Tokens:      periodTok,
				Credits:     periodCred,
				ReqLimit:    k.DailyReqLimit,
				TokLimit:    k.DailyTokLimit,
				CredLimit:   k.DailyCredLimit,
			},
			Lifetime: KeyStatusLifetime{
				Requests:  k.TotalRequests,
				Tokens:    k.TotalTokens,
				Credits:   k.TotalCredits,
				ReqLimit:  k.LifetimeReqLimit,
				TokLimit:  k.LifetimeTokLimit,
				CredLimit: k.LifetimeCredLimit,
			},
			MinuteLimit: k.MinuteReqLimit,
			HourLimit:   k.HourReqLimit,
		}, true
	}
	return KeyStatus{Valid: false}, false
}
