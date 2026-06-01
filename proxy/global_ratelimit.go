package proxy

import (
	"sync"
	"time"
)

// ============================================================================
// Global request rate limiter (token bucket).
//
// We already enforce per-API-key limits (minute/hour/periodic/lifetime) and
// per-account cooldowns. What we lacked is a single GLOBAL throttle that caps
// the proxy's total inbound request rate regardless of key — a backstop against
// a runaway client or a thundering herd hammering the whole account pool at
// once. This is the classic token-bucket: a bucket of `capacity` tokens refills
// at `ratePerSec`; each request takes one; an empty bucket rejects with 429.
//
// OPT-IN: disabled by default (config GlobalRateLimitPerMinute == 0). When off,
// allow() is a cheap no-op returning true, so the stable baseline behavior is
// completely unchanged. Only when an operator sets a positive per-minute limit
// does the bucket engage.
// ============================================================================

// globalRateLimiter is a thread-safe token bucket. Zero value is a disabled
// limiter (allow() always true) until Configure is called with a positive rate.
type globalRateLimiter struct {
	mu         sync.Mutex
	enabled    bool
	capacity   float64   // max burst (tokens)
	tokens     float64   // current tokens
	ratePerSec float64   // refill rate
	last       time.Time // last refill timestamp
}

// Configure sets the limiter from a per-minute request budget. perMinute <= 0
// disables it. The burst capacity is the full per-minute budget (so a client
// can spend a minute's worth at once, then is paced to the steady rate), with a
// floor of 1 so a tiny limit still admits single requests.
func (g *globalRateLimiter) Configure(perMinute int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if perMinute <= 0 {
		g.enabled = false
		return
	}
	capacity := float64(perMinute)
	if capacity < 1 {
		capacity = 1
	}
	g.enabled = true
	g.capacity = capacity
	g.ratePerSec = float64(perMinute) / 60.0
	// Start full so enabling the limiter doesn't immediately reject traffic.
	g.tokens = capacity
	g.last = time.Now()
}

// allow attempts to take one token. Returns (allowed, retryAfter). When the
// limiter is disabled it always allows with zero wait. When throttled,
// retryAfter is the time until the next token becomes available.
func (g *globalRateLimiter) allow() (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.enabled {
		return true, 0
	}

	now := time.Now()
	elapsed := now.Sub(g.last).Seconds()
	if elapsed > 0 {
		g.tokens += elapsed * g.ratePerSec
		if g.tokens > g.capacity {
			g.tokens = g.capacity
		}
		g.last = now
	}

	if g.tokens >= 1 {
		g.tokens--
		return true, 0
	}

	// Not enough tokens: time until one full token accrues.
	needed := 1 - g.tokens
	var wait time.Duration
	if g.ratePerSec > 0 {
		wait = time.Duration(needed/g.ratePerSec*1000) * time.Millisecond
	}
	if wait < time.Second {
		wait = time.Second // never advertise a sub-second Retry-After
	}
	return false, wait
}
