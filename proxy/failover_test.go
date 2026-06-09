package proxy

import (
	"context"
	"errors"
	"fmt"
	"kiro-go/config"
	"kiro-go/pool"
	"sync/atomic"
	"testing"
	"time"
)

// TestIsRetryableUpstreamError pins the failover classifier contract: 429 /
// quota / 5xx / connection errors AND auth (401/403) are retryable on a PEER
// account (tokens are per-account, and the dead account is already disabled +
// excluded, so failover rotates to a healthy peer rather than 503-ing). Payment
// (402/OVERAGE) and client cancellation stay terminal.
func TestIsRetryableUpstreamError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"quota error type", &QuotaError{Endpoints: []string{"Kiro IDE"}, RetryAfter: time.Second}, true},
		{"429 string", errors.New("HTTP 429 from Kiro IDE: throttled"), true},
		{"rate-limited string", errors.New("rate limited (HTTP 429) on Kiro IDE"), true},
		{"quota string", errors.New("quota exhausted on Kiro IDE"), true},
		{"500", errors.New("HTTP 500 from Kiro IDE: internal"), true},
		{"502", errors.New("HTTP 502 from Kiro IDE"), true},
		{"503", errors.New("HTTP 503 from Kiro IDE"), true},
		{"504", errors.New("HTTP 504 from Kiro IDE"), true},
		{"connection reset", errors.New("read tcp: connection reset by peer"), true},
		{"eof", errors.New("unexpected EOF"), true},
		{"timeout", errors.New("net/http: request timeout"), true},
		{"401 retryable on peer", errors.New("HTTP 401 from Kiro IDE: unauthorized"), true},
		{"403 retryable on peer", errors.New("HTTP 403 from Kiro IDE: forbidden"), true},
		{"402 payment terminal", errors.New("HTTP 402 from Kiro IDE: payment required"), false},
		{"overage terminal", errors.New("HTTP 402 OVERAGE limit reached"), false},
		{"context canceled", context.Canceled, false},
		{"deadline exceeded", context.DeadlineExceeded, false},
		{"unknown 4xx not retried", errors.New("HTTP 400 from Kiro IDE: bad request"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableUpstreamError(tc.err); got != tc.want {
				t.Fatalf("isRetryableUpstreamError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsRetryableWrappedQuotaError verifies errors.As matching survives
// fmt.Errorf %w wrapping — the dispatcher must still recognize a wrapped
// QuotaError as retryable.
func TestIsRetryableWrappedQuotaError(t *testing.T) {
	base := &QuotaError{Endpoints: []string{"Kiro IDE"}, RetryAfter: 5 * time.Second}
	wrapped := fmt.Errorf("call failed: %w", base)
	if !isRetryableUpstreamError(wrapped) {
		t.Fatal("wrapped QuotaError should be retryable")
	}
	if got := retryAfterFromErr(wrapped); got != 5*time.Second {
		t.Fatalf("retryAfterFromErr(wrapped) = %s, want 5s", got)
	}
}

// TestRetryAfterFromErr verifies the helper only extracts a hint from a
// *QuotaError and returns 0 otherwise.
func TestRetryAfterFromErr(t *testing.T) {
	if got := retryAfterFromErr(errors.New("HTTP 500")); got != 0 {
		t.Fatalf("non-quota error should yield 0, got %s", got)
	}
	if got := retryAfterFromErr(nil); got != 0 {
		t.Fatalf("nil should yield 0, got %s", got)
	}
	qe := &QuotaError{Endpoints: []string{"X"}, RetryAfter: 12 * time.Second}
	if got := retryAfterFromErr(qe); got != 12*time.Second {
		t.Fatalf("expected 12s, got %s", got)
	}
}

// newFailoverTestHandler builds a Handler backed by a pool seeded with the
// given account IDs (ExpiresAt=0 so ensureValidToken never tries to refresh).
func newFailoverTestHandler(ids ...string) *Handler {
	p := pool.NewForTesting()
	accts := make([]config.Account, 0, len(ids))
	for _, id := range ids {
		accts = append(accts, config.Account{ID: id, AccessToken: "tok-" + id})
	}
	p.SetAccountsForTesting(accts)
	return &Handler{pool: p}
}

// TestRunWithFailoverRotatesOnRetryableError verifies the dispatcher retries
// on a second account after the first returns a retryable (429) error, and
// that the eventual success is reported committed with no error.
func TestRunWithFailoverRotatesOnRetryableError(t *testing.T) {
	h := newFailoverTestHandler("a", "b")

	var attempts int32
	firstFailed := false
	worker := func(account *config.Account) (bool, error) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			firstFailed = true
			// Pre-commit retryable failure — cool this account, signal failover.
			h.recordAttemptError(&QuotaError{Endpoints: []string{"Kiro IDE"}, RetryAfter: time.Second}, account.ID)
			return false, &QuotaError{Endpoints: []string{"Kiro IDE"}, RetryAfter: time.Second}
		}
		// Second account commits successfully.
		return true, nil
	}

	committed, _, err := h.runWithFailover("claude-sonnet-4.5", "key-1", "", worker)
	if !committed || err != nil {
		t.Fatalf("expected committed success after failover, got committed=%v err=%v", committed, err)
	}
	if !firstFailed || atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("expected exactly 2 attempts with first failing, got %d", attempts)
	}
	// Failover-then-succeed must NOT bump the global failed counter.
	if got := atomic.LoadInt64(&h.failedRequests); got != 0 {
		t.Fatalf("successful failover should record 0 failures, got %d", got)
	}
}

// TestRunWithFailoverCountsSingleFailure verifies that when every account
// fails with a retryable error, the request is counted as exactly ONE failed
// request (not one per attempt), while each distinct account is cooled.
func TestRunWithFailoverCountsSingleFailure(t *testing.T) {
	h := newFailoverTestHandler("a", "b", "c")

	worker := func(account *config.Account) (bool, error) {
		err := &QuotaError{Endpoints: []string{"Kiro IDE"}, RetryAfter: 2 * time.Second}
		h.recordAttemptError(err, account.ID)
		return false, err
	}

	committed, retryAfter, err := h.runWithFailover("claude-sonnet-4.5", "key-1", "", worker)
	if committed {
		t.Fatal("expected no commit when all accounts fail")
	}
	if err == nil {
		t.Fatal("expected an error when all accounts fail")
	}
	if retryAfter <= 0 {
		t.Fatalf("expected a Retry-After hint from the quota errors, got %s", retryAfter)
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != 1 {
		t.Fatalf("a single client request must count as 1 failure regardless of attempts, got %d", got)
	}
}

// TestRunWithFailoverStopsOnTerminalError verifies a non-retryable error
// (e.g. 402 payment) stops after the FIRST account — no peer accounts are
// burned — and counts one failure.
func TestRunWithFailoverStopsOnTerminalError(t *testing.T) {
	h := newFailoverTestHandler("a", "b", "c")

	var attempts int32
	worker := func(account *config.Account) (bool, error) {
		atomic.AddInt32(&attempts, 1)
		err := errors.New("HTTP 402 from Kiro IDE: payment required")
		h.recordAttemptError(err, account.ID)
		return false, err
	}

	committed, _, err := h.runWithFailover("claude-sonnet-4.5", "key-1", "", worker)
	if committed || err == nil {
		t.Fatalf("expected terminal failure, got committed=%v err=%v", committed, err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("terminal (402) error must not fail over; expected 1 attempt, got %d", got)
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != 1 {
		t.Fatalf("expected exactly 1 failure, got %d", got)
	}
}
