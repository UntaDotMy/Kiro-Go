package proxy

import (
	"errors"
	"fmt"
	"kiro-go/pool"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRetryAfterDeltaSeconds(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "30")
	if d := parseRetryAfter(h); d != 30*time.Second {
		t.Fatalf("expected 30s, got %s", d)
	}
}

func TestParseRetryAfterAmazonHeader(t *testing.T) {
	// AWS internal services use x-amz-retry-after in milliseconds.
	h := http.Header{}
	h.Set("x-amz-retry-after", "2500")
	if d := parseRetryAfter(h); d != 2500*time.Millisecond {
		t.Fatalf("expected 2.5s, got %s", d)
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	h := http.Header{}
	future := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
	h.Set("Retry-After", future)
	d := parseRetryAfter(h)
	if d < 40*time.Second || d > 50*time.Second {
		t.Fatalf("expected ~45s, got %s", d)
	}
}

func TestParseRetryAfterMissing(t *testing.T) {
	h := http.Header{}
	if d := parseRetryAfter(h); d != 0 {
		t.Fatalf("expected 0 when header absent, got %s", d)
	}
}

func TestParseRetryAfterPastDateReturnsZero(t *testing.T) {
	// An HTTP-date in the past should not yield a negative cooldown.
	h := http.Header{}
	h.Set("Retry-After", time.Now().Add(-1*time.Hour).UTC().Format(http.TimeFormat))
	if d := parseRetryAfter(h); d != 0 {
		t.Fatalf("expected 0 for past date, got %s", d)
	}
}

func TestQuotaErrorErrorsAs(t *testing.T) {
	qe := &QuotaError{Endpoints: []string{"Kiro IDE"}, RetryAfter: 30 * time.Second}
	var e error = qe

	var got *QuotaError
	if !errors.As(e, &got) {
		t.Fatalf("errors.As should match *QuotaError")
	}
	if got.RetryAfter != 30*time.Second {
		t.Fatalf("expected RetryAfter 30s, got %s", got.RetryAfter)
	}
}

func TestRetryAfterSecondsRoundsUp(t *testing.T) {
	if got := retryAfterSeconds(500 * time.Millisecond); got != 1 {
		t.Fatalf("sub-second cooldown should round up to 1, got %d", got)
	}
	if got := retryAfterSeconds(2*time.Second + 100*time.Millisecond); got != 3 {
		t.Fatalf("expected round-up to 3, got %d", got)
	}
	if got := retryAfterSeconds(0); got != 1 {
		t.Fatalf("zero should floor to 1, got %d", got)
	}
}

// TestHandleUpstreamErrorIsNoOpOnNil verifies that the helper short-circuits
// for nil errors so callers can invoke it unconditionally without leaking
// failure counts.
func TestHandleUpstreamErrorIsNoOpOnNil(t *testing.T) {
	h := &Handler{pool: pool.NewForTesting()}
	beforeTotal := atomic.LoadInt64(&h.totalRequests)
	beforeFailed := atomic.LoadInt64(&h.failedRequests)

	h.handleUpstreamError(nil, "acct-1", "claude-sonnet-4.5", "key-1", "")

	if got := atomic.LoadInt64(&h.totalRequests); got != beforeTotal {
		t.Fatalf("nil err must not bump totalRequests, was %d → %d", beforeTotal, got)
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != beforeFailed {
		t.Fatalf("nil err must not bump failedRequests, was %d → %d", beforeFailed, got)
	}
	if d := h.pool.CooldownRemaining("acct-1"); d != 0 {
		t.Fatalf("nil err must not seed cooldown, got %s", d)
	}
}

// TestHandleUpstreamErrorBumpsCountersAndCooldown verifies that a *QuotaError
// flows the upstream Retry-After through to the pool's cooldown state and
// records a failed request in the per-handler counters.
func TestHandleUpstreamErrorBumpsCountersAndCooldown(t *testing.T) {
	h := &Handler{pool: pool.NewForTesting()}
	id := "test-acct-quota-" + fmt.Sprintf("%d", time.Now().UnixNano())
	beforeTotal := atomic.LoadInt64(&h.totalRequests)
	beforeFailed := atomic.LoadInt64(&h.failedRequests)

	qe := &QuotaError{Endpoints: []string{"Kiro IDE"}, RetryAfter: 30 * time.Second}
	h.handleUpstreamError(qe, id, "claude-sonnet-4.5", "key-x", "")

	if got := atomic.LoadInt64(&h.totalRequests); got != beforeTotal+1 {
		t.Fatalf("expected totalRequests to bump by 1, was %d → %d", beforeTotal, got)
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != beforeFailed+1 {
		t.Fatalf("expected failedRequests to bump by 1, was %d → %d", beforeFailed, got)
	}
	d := h.pool.CooldownRemaining(id)
	// RecordError clamps to [retryAfterMin, retryAfterMax]; we only assert the
	// cooldown is in the right ballpark, since the clamps live in pool/.
	if d <= 0 || d > time.Minute {
		t.Fatalf("expected cooldown ~30s, got %s", d)
	}
}

// TestHandleUpstreamErrorTreatsNonQuotaAsTransient verifies that a generic
// upstream error doesn't immediately seed a cooldown — non-quota errors only
// trip cooldown after several consecutive failures (per pool.RecordError).
func TestHandleUpstreamErrorTreatsNonQuotaAsTransient(t *testing.T) {
	h := &Handler{pool: pool.NewForTesting()}
	id := "test-acct-transient-" + fmt.Sprintf("%d", time.Now().UnixNano())

	h.handleUpstreamError(errors.New("HTTP 500: internal server error"), id, "claude-sonnet-4.5", "key-y", "")

	if d := h.pool.CooldownRemaining(id); d != 0 {
		t.Fatalf("first non-quota error should not cool down, got %s", d)
	}
	// Counter still bumped — the failure is recorded even if cooldown didn't trip.
	if atomic.LoadInt64(&h.failedRequests) == 0 {
		t.Fatalf("expected failedRequests to bump for non-quota error too")
	}
}
