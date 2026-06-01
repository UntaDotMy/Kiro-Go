package proxy

import (
	"testing"
	"time"
)

// TestGlobalRateLimiterDisabledByDefault confirms the zero-value limiter (and a
// Configure(0)) never throttles — preserving the stable default behavior.
func TestGlobalRateLimiterDisabledByDefault(t *testing.T) {
	var g globalRateLimiter
	for i := 0; i < 1000; i++ {
		if ok, _ := g.allow(); !ok {
			t.Fatalf("zero-value limiter must always allow, rejected at %d", i)
		}
	}
	g.Configure(0)
	for i := 0; i < 1000; i++ {
		if ok, _ := g.allow(); !ok {
			t.Fatalf("Configure(0) must disable, rejected at %d", i)
		}
	}
}

// TestGlobalRateLimiterBurstThenThrottle verifies the bucket admits a full
// burst up to capacity, then rejects with a positive Retry-After.
func TestGlobalRateLimiterBurstThenThrottle(t *testing.T) {
	var g globalRateLimiter
	g.Configure(60) // 60/min => capacity 60, refill 1/s, starts full

	allowed := 0
	for i := 0; i < 60; i++ {
		if ok, _ := g.allow(); ok {
			allowed++
		}
	}
	if allowed != 60 {
		t.Fatalf("expected full burst of 60 to be allowed, got %d", allowed)
	}

	// 61st request within the same instant must be throttled.
	ok, retryAfter := g.allow()
	if ok {
		t.Fatal("expected throttle after burst exhausted")
	}
	if retryAfter <= 0 {
		t.Fatalf("expected positive Retry-After, got %v", retryAfter)
	}
}

// TestGlobalRateLimiterRefills confirms tokens accrue over time so a throttled
// limiter recovers. We simulate elapsed time by manipulating last.
func TestGlobalRateLimiterRefills(t *testing.T) {
	var g globalRateLimiter
	g.Configure(60) // 1 token/sec

	// Drain the bucket.
	for i := 0; i < 60; i++ {
		g.allow()
	}
	if ok, _ := g.allow(); ok {
		t.Fatal("bucket should be empty")
	}

	// Pretend 2 seconds elapsed → ~2 tokens refill.
	g.mu.Lock()
	g.last = time.Now().Add(-2 * time.Second)
	g.mu.Unlock()

	if ok, _ := g.allow(); !ok {
		t.Fatal("expected allow after refill")
	}
	if ok, _ := g.allow(); !ok {
		t.Fatal("expected second allow after ~2 tokens refilled")
	}
	// Third should be throttled again (only ~2 refilled).
	if ok, _ := g.allow(); ok {
		t.Fatal("expected throttle after the ~2 refilled tokens are spent")
	}
}

// TestGlobalRateLimiterReconfigure confirms changing the limit takes effect and
// re-enabling fills the bucket (so toggling on doesn't instantly reject).
func TestGlobalRateLimiterReconfigure(t *testing.T) {
	var g globalRateLimiter
	g.Configure(2)
	g.allow()
	g.allow()
	if ok, _ := g.allow(); ok {
		t.Fatal("expected throttle at capacity 2")
	}
	// Disable → always allow.
	g.Configure(0)
	if ok, _ := g.allow(); !ok {
		t.Fatal("expected allow after disable")
	}
	// Re-enable with a larger budget → starts full.
	g.Configure(100)
	allowed := 0
	for i := 0; i < 100; i++ {
		if ok, _ := g.allow(); ok {
			allowed++
		}
	}
	if allowed != 100 {
		t.Fatalf("expected 100 allowed after re-enable (starts full), got %d", allowed)
	}
}
