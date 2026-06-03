package proxy

import (
	"errors"
	"kiro-go/config"
	"kiro-go/pool"
	"sync/atomic"
	"testing"
	"time"
)

// These tests cover the adaptive-load-balancing additions on the proxy side:
// the saturation -> admission-wait -> shed path in acquireWithAdmissionWait /
// runWithFailover, and the tokenRefreshFailure sentinel that lets a pre-commit
// token miss rotate to a peer instead of being misclassified as a terminal
// upstream error.

// saturatePool drives every account in the pool up to its AIMD concurrency
// limit by repeatedly acquiring (and never releasing) reserving slots under the
// least-request strategy. It returns once the next acquire would be shed.
func saturatePool(t *testing.T, p *pool.AccountPool, ids ...string) {
	t.Helper()
	// aimdInitialLimit is 2 per account; acquire generously and stop when the
	// pool starts shedding (ok=false with the saturation hint).
	for i := 0; i < len(ids)*8; i++ {
		_, _, ok := p.AcquireForModelExcluding("", nil)
		if !ok {
			return
		}
	}
	t.Fatalf("pool never saturated after %d acquires", len(ids)*8)
}

// TestAcquireAdmissionWaitShedsWhenSaturated verifies that when every account is
// pinned at its concurrency limit and no slot frees, acquireWithAdmissionWait
// waits up to its budget and then sheds (ok=false) with the pool's saturation
// poll hint, so the caller can emit a short Retry-After.
func TestAcquireAdmissionWaitShedsWhenSaturated(t *testing.T) {
	restore := pool.SetStrategyResolverForTesting(func() string { return "least-request" })
	defer restore()

	h := newFailoverTestHandler("a", "b")
	saturatePool(t, h.pool, "a", "b")

	start := time.Now()
	acc, retryAfter, ok := h.acquireWithAdmissionWait("", map[string]bool{})
	elapsed := time.Since(start)

	if ok || acc != nil {
		t.Fatalf("expected shed (no account) when saturated, got %v", acc)
	}
	if retryAfter <= 0 {
		t.Fatalf("expected a positive saturation retry hint, got %s", retryAfter)
	}
	// It must have actually waited roughly the admission budget before shedding.
	if elapsed < admissionWaitBudget-200*time.Millisecond {
		t.Fatalf("expected to wait ~%s before shedding, only waited %s", admissionWaitBudget, elapsed)
	}
}

// TestAcquireAdmissionWaitAdmitsWhenSlotFrees verifies the happy path of the
// bounded wait: when the pool is saturated but a slot frees mid-wait, the
// dispatcher admits the freed account instead of shedding.
func TestAcquireAdmissionWaitAdmitsWhenSlotFrees(t *testing.T) {
	restore := pool.SetStrategyResolverForTesting(func() string { return "least-request" })
	defer restore()

	h := newFailoverTestHandler("a", "b")
	saturatePool(t, h.pool, "a", "b")

	// Free a slot on "a" shortly after the wait begins.
	go func() {
		time.Sleep(60 * time.Millisecond)
		h.pool.Release("a")
	}()

	start := time.Now()
	acc, _, ok := h.acquireWithAdmissionWait("", map[string]bool{})
	elapsed := time.Since(start)

	if !ok || acc == nil {
		t.Fatal("expected the freed slot to be admitted within the budget")
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("admission should have waited for the slot to free, returned in %s", elapsed)
	}
	if elapsed > admissionWaitBudget {
		t.Fatalf("admission exceeded the budget (%s): %s", admissionWaitBudget, elapsed)
	}
}

// TestAcquireAdmissionWaitReturnsImmediatelyWhenCooling verifies that a COOLING
// pool (retryAfter larger than the saturation poll hint) is surfaced
// immediately — waiting can't beat a multi-second cooldown, so the dispatcher
// returns the cooldown hint at once instead of polling for the full budget.
func TestAcquireAdmissionWaitReturnsImmediatelyWhenCooling(t *testing.T) {
	restore := pool.SetStrategyResolverForTesting(func() string { return "least-request" })
	defer restore()

	h := newFailoverTestHandler("a", "b")
	// Cool both accounts for 30s (well past saturationPollHint).
	h.pool.RecordError("a", true, 30*time.Second)
	h.pool.RecordError("b", true, 30*time.Second)

	start := time.Now()
	acc, retryAfter, ok := h.acquireWithAdmissionWait("", map[string]bool{})
	elapsed := time.Since(start)

	if ok || acc != nil {
		t.Fatalf("expected no account while all cooling, got %v", acc)
	}
	if retryAfter < time.Second {
		t.Fatalf("expected the ~30s cooldown hint, got %s", retryAfter)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("a cooling pool must return immediately, waited %s", elapsed)
	}
}

// TestRunWithFailoverShedsUnderSaturation verifies the end-to-end shed: when the
// pool is fully saturated, runWithFailover never invokes the worker, reports
// committed=false with a positive Retry-After, and (because no upstream attempt
// was actually made) does NOT bump the global failed-request counter.
func TestRunWithFailoverShedsUnderSaturation(t *testing.T) {
	restore := pool.SetStrategyResolverForTesting(func() string { return "least-request" })
	defer restore()

	h := newFailoverTestHandler("a", "b")
	saturatePool(t, h.pool, "a", "b")

	var workerCalls int32
	worker := func(account *config.Account) (bool, error) {
		atomic.AddInt32(&workerCalls, 1)
		return true, nil
	}

	committed, retryAfter, err := h.runWithFailover("claude-sonnet-4.5", "key-1", "", worker)
	if committed {
		t.Fatal("expected no commit when the pool is saturated")
	}
	if err != nil {
		t.Fatalf("saturation shed is not an upstream error; expected nil err, got %v", err)
	}
	if retryAfter <= 0 {
		t.Fatalf("expected a positive Retry-After on shed, got %s", retryAfter)
	}
	if got := atomic.LoadInt32(&workerCalls); got != 0 {
		t.Fatalf("worker must not run when shedding, was called %d times", got)
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != 0 {
		t.Fatalf("a shed with no upstream attempt must not count as a failed request, got %d", got)
	}
}

// TestRunOneAttemptTagsTokenRefreshFailure verifies that when an account's token
// refresh fails (here deterministically: a past expiry forces a refresh and the
// account has no OIDC client credentials, so the refresh errors offline before
// any network call), runOneAttempt returns committed=false and tags the error as
// a tokenRefreshFailure — and never invokes the worker.
func TestRunOneAttemptTagsTokenRefreshFailure(t *testing.T) {
	h := newFailoverTestHandler() // empty pool; GetByID returns nil so no re-fetch
	// Account not in the pool, expiry in the past => ensureValidToken attempts a
	// refresh; empty ClientID/Secret => refreshOIDCToken errors immediately.
	acct := &config.Account{ID: "a", ExpiresAt: time.Now().Unix() - 100}

	var workerCalls int32
	worker := func(account *config.Account) (bool, error) {
		atomic.AddInt32(&workerCalls, 1)
		return true, nil
	}

	committed, err := h.runOneAttempt(acct, worker, 0)
	if committed {
		t.Fatal("a pre-commit token-refresh failure must not be committed")
	}
	if err == nil {
		t.Fatal("expected a token-refresh error")
	}
	if !isTokenRefreshFailure(err) {
		t.Fatalf("error should be tagged as a tokenRefreshFailure, got %T: %v", err, err)
	}
	if got := atomic.LoadInt32(&workerCalls); got != 0 {
		t.Fatalf("worker must not run after a failed token refresh, was called %d times", got)
	}
}

// TestTokenRefreshFailureBypassesUpstreamClassification is the regression that
// justifies the sentinel: the raw token-refresh error string is NOT a retryable
// upstream error, so without the tokenRefreshFailure tag the dispatcher would
// treat it as terminal and refuse to rotate. The tag lets the dispatcher
// recognise it (isTokenRefreshFailure) and rotate to a peer with a healthy
// token, while errors.Unwrap still exposes the underlying cause.
func TestTokenRefreshFailureBypassesUpstreamClassification(t *testing.T) {
	raw := errors.New("OIDC refresh requires clientId and clientSecret")

	// The raw error would be classified terminal (no 429/5xx/connection marker),
	// which is exactly why a dedicated sentinel + explicit dispatcher branch is
	// needed to rotate on it.
	if isRetryableUpstreamError(raw) {
		t.Fatal("precondition: the raw token error should NOT look like a retryable upstream error")
	}

	tagged := tokenRefreshFailure{raw}
	if !isTokenRefreshFailure(tagged) {
		t.Fatal("isTokenRefreshFailure should recognise the sentinel")
	}
	if !errors.Is(tagged, raw) {
		t.Fatal("errors.Is should unwrap the sentinel to its cause")
	}
	// A genuine upstream quota error must NOT be mistaken for a token failure.
	if isTokenRefreshFailure(&QuotaError{Endpoints: []string{"Kiro IDE"}, RetryAfter: time.Second}) {
		t.Fatal("a QuotaError must not be classified as a token-refresh failure")
	}
}
