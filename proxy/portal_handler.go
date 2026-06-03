package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Public customer portal endpoint: a key holder presents their own API key and
// gets back a read-only status snapshot (usage vs. limits, expiry, allowed
// models). Presenting the key proves ownership, so returning that key's own
// usage is safe — it's the same trust model as the key working at all.
//
// Security properties (from the prod-readiness audit):
//   - Auth by the key itself via Authorization: Bearer or X-Api-Key; never via
//     a query parameter (would leak into logs).
//   - Constant-time key compare (config.GetKeyStatusBySecret).
//   - No oracle: disabled / expired / unknown all return the same {valid:false}.
//   - Never returns the raw key, internal id, or operator label.
//   - Its own per-IP token-bucket rate limit, independent of the admin
//     brute-force lockout and the inference global limiter.

const (
	// Token-bucket parameters for the per-IP key-status limit. portalBurst is the
	// bucket capacity (max instantaneous burst); portalRefillPerSec is the steady
	// refill rate. ~0.33/s with a burst of 20 = a sustained 20/min that, unlike a
	// fixed window, does NOT allow a double burst across a window boundary (the
	// reviewers' finding). Plenty for a human refreshing a status page while
	// throttling automated enumeration of stolen keys.
	portalBurst        = 20.0
	portalRefillPerSec = 20.0 / 60.0
)

// portalRateEntry is one IP's token bucket. tokens is the current allowance;
// last is when it was last refilled.
type portalRateEntry struct {
	tokens float64
	last   time.Time
}

var (
	portalRateMu sync.Mutex
	portalRates  = map[string]*portalRateEntry{}
)

// allowPortalRequest applies a per-IP token-bucket rate limit. Returns false
// when the IP has no tokens left. Refills continuously, so there is no
// fixed-window boundary an attacker can straddle to double their burst.
func allowPortalRequest(ip string) bool {
	portalRateMu.Lock()
	defer portalRateMu.Unlock()
	now := time.Now()
	rec, ok := portalRates[ip]
	if !ok {
		// Opportunistic sweep so a stream of distinct IPs can't grow the map
		// without bound. Evict buckets that have fully refilled (idle long
		// enough to be back at capacity) — they carry no state worth keeping.
		if len(portalRates) > 4096 {
			for k, v := range portalRates {
				if now.Sub(v.last) > 2*time.Minute {
					delete(portalRates, k)
				}
			}
		}
		// New IP starts with a full bucket, minus this request.
		portalRates[ip] = &portalRateEntry{tokens: portalBurst - 1, last: now}
		return true
	}
	// Refill based on elapsed time, capped at capacity.
	elapsed := now.Sub(rec.last).Seconds()
	rec.tokens = rec.tokens + elapsed*portalRefillPerSec
	if rec.tokens > portalBurst {
		rec.tokens = portalBurst
	}
	rec.last = now
	if rec.tokens < 1 {
		return false
	}
	rec.tokens--
	return true
}

// extractBearerOrApiKey pulls the presented key from Authorization: Bearer or
// the X-Api-Key header. Returns "" when neither is present.
func extractBearerOrApiKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

// handlePortalKeyStatus serves GET /v1/key-status (and /portal/api/key-status).
// It authenticates by the presented key and returns that key's own status.
func (h *Handler) handlePortalKeyStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Method Not Allowed"})
		return
	}

	key := extractBearerOrApiKey(r)
	// Short-circuit a blank key BEFORE charging a rate-limit token, so an
	// unauthenticated ping (no Authorization / X-Api-Key) can't drain a real
	// client's allowance. Same indistinguishable {valid:false} as any other
	// miss — no oracle.
	if key == "" {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"valid": false})
		return
	}

	ip := clientIP(r)
	if !allowPortalRequest(ip) {
		// Token bucket refills continuously; tell the client to back off ~3s
		// (one token at the steady refill rate) rather than a full window.
		setRetryAfter(w, 3*time.Second)
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Too many requests; slow down."})
		return
	}

	// Never log the key value on failure (audit requirement) — log only the IP.
	status, ok := config.GetKeyStatusBySecret(key)
	if !ok {
		// Single indistinguishable invalid outcome — no oracle. 200 with
		// {valid:false} (not 401) so the portal UI can render a clean "key not
		// recognized" state without treating it as a transport error.
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"valid": false})
		return
	}
	_ = json.NewEncoder(w).Encode(status)
}
