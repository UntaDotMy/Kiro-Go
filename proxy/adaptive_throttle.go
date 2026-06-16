package proxy

import (
	"errors"
	"math"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"
)

// errAdaptiveThrottleShed is the sentinel the failover dispatcher returns when a
// request is shed locally by the adaptive throttle (pool-wide overload). It is
// returned directly from runWithFailoverCountedBackend with committed=false and
// a short Retry-After, so the caller surfaces a 429 — it never flows through the
// retryable-error classifier. Kept distinct so callers/tests can identify the
// pool-wide-shed cause separately from a real upstream error.
var errAdaptiveThrottleShed = errors.New("request shed by adaptive throttle (pool-wide overload)")

// Google SRE client-side adaptive throttling.
//
// When the WHOLE account pool is being throttled by the upstream (every account
// returning 429), the per-account AIMD limiter and the bounded admission-wait
// still let each inbound request dial upstream and collect its own 429 before
// shedding. That wastes a round-trip per request and adds to the upstream's
// rejection load — the retry-storm amplification the AWS Builders' Library warns
// about. The fix is the Google SRE "handling overload" adaptive throttle: the
// client sheds a request LOCALLY, before dialing, with probability
//
//	p_reject = max(0, (requests - K*accepts) / (requests + 1))
//
// computed over a rolling window of recent outcomes (per backend). K=2 lets the
// client send up to twice the rate the backend is currently accepting, so the
// feedback channel stays open and the throttle relaxes the instant the backend
// starts accepting again.
//
// SAFETY — this is a no-op under healthy load by construction: when the backend
// accepts everything, accepts == requests, so requests - 2*accepts == -requests
// < 0 and p_reject clamps to 0. The throttle only engages once accepts fall
// behind requests (a real pool-wide 429 storm). It is the pool-wide shedding
// layer explicitly scoped as DEFERRED in failover.go's design notes.
//
// Disable with KIRO_DISABLE_ADAPTIVE_THROTTLE=1 (the rolling counters still
// record, but shouldShed always returns false).

const (
	// adaptiveThrottleK is the SRE multiplier. K=2 ("accept twice what the
	// backend accepts") is the value the SRE book uses as a good default: lower
	// K sheds more aggressively, higher K is more permissive. 2 keeps enough
	// requests flowing to detect recovery quickly.
	adaptiveThrottleK = 2.0

	// adaptiveThrottleHalfLife is the exponential-decay half-life of the rolling
	// requests/accepts counters. ~1 minute approximates the SRE book's 2-minute
	// rolling window while reacting fast enough that a brief 429 burst doesn't
	// keep shedding long after the upstream recovers.
	adaptiveThrottleHalfLife = 60 * time.Second
)

// adaptiveThrottle tracks per-backend (requests, accepts) with lazy exponential
// decay and computes the SRE reject probability. All fields are guarded by mu;
// the per-request hot path takes one short lock, so contention is negligible
// next to an upstream round-trip.
type adaptiveThrottle struct {
	mu       sync.Mutex
	entries  map[string]*throttleEntry
	halfLife time.Duration
	disabled bool
}

type throttleEntry struct {
	requests float64
	accepts  float64
	last     time.Time
}

func newAdaptiveThrottle() *adaptiveThrottle {
	return &adaptiveThrottle{
		entries:  make(map[string]*throttleEntry),
		halfLife: adaptiveThrottleHalfLife,
		disabled: os.Getenv("KIRO_DISABLE_ADAPTIVE_THROTTLE") == "1",
	}
}

// throttleKey normalizes a backend id to a stable counter key. An empty backend
// (the legacy "no constraint" failover path) shares the "kiro" bucket since that
// is the account set it selects from.
func throttleKey(backend string) string {
	b := strings.ToLower(strings.TrimSpace(backend))
	if b == "" {
		return "kiro"
	}
	return b
}

// entryLocked returns the entry for key, creating it if absent, with decay
// already applied to `now`. Caller must hold a.mu.
func (a *adaptiveThrottle) entryLocked(key string, now time.Time) *throttleEntry {
	e := a.entries[key]
	if e == nil {
		e = &throttleEntry{last: now}
		a.entries[key] = e
		return e
	}
	if !e.last.IsZero() {
		if dt := now.Sub(e.last); dt > 0 {
			factor := math.Exp(-float64(dt) / float64(a.halfLife))
			e.requests *= factor
			e.accepts *= factor
		}
	}
	e.last = now
	return e
}

// shouldShed reports whether this request should be rejected locally before
// dialing upstream. It counts the request either way (so a shed request still
// participates in the SRE feedback as a request-without-accept). Returns false
// always when disabled, but still records the request so flipping the env var
// at runtime sees a warm window. The recovery signal — accepts — is recorded by
// recordOutcome when a request actually completes.
func (a *adaptiveThrottle) shouldShed(backend string) bool {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.entryLocked(throttleKey(backend), now)
	p := 0.0
	if e.requests > 0 {
		p = (e.requests - adaptiveThrottleK*e.accepts) / (e.requests + 1)
	}
	if p < 0 {
		p = 0
	}
	shed := !a.disabled && p > 0 && rand.Float64() < p
	if shed {
		// A locally-shed request counts as a request (not an accept), per the
		// SRE model — this is what keeps the probability self-correcting rather
		// than running away.
		e.requests++
	}
	return shed
}

// recordOutcome records a request that actually reached the dispatch path.
// accepted=true means the upstream served it (or failed for a non-throttle
// reason); accepted=false means it was throttled (429/quota). This is the
// signal that drives p_reject.
func (a *adaptiveThrottle) recordOutcome(backend string, accepted bool) {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.entryLocked(throttleKey(backend), now)
	e.requests++
	if accepted {
		e.accepts++
	}
}

// rejectProbability exposes the current p_reject for a backend (for tests and
// the dashboard); does not mutate counters beyond decay.
func (a *adaptiveThrottle) rejectProbability(backend string) float64 {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.entryLocked(throttleKey(backend), now)
	if e.requests <= 0 {
		return 0
	}
	p := (e.requests - adaptiveThrottleK*e.accepts) / (e.requests + 1)
	if p < 0 {
		return 0
	}
	return p
}
