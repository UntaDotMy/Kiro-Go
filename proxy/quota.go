// Helpers shared by the Anthropic, OpenAI Chat, OpenAI Responses, and
// WebSocket handlers for surfacing 429 / Retry-After signals and translating
// upstream *QuotaError values into pool cooldown calls.
package proxy

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// recordPoolError feeds an upstream error back into the pool's cooldown
// state machine. We discriminate three tiers:
//
//   - *QuotaError → 429 throttling. Honor upstream Retry-After.
//   - 402 OVERAGE → monthly quota exhausted. Long cooldown (~1h) so the
//     SWRR walk doesn't keep selecting an account that will keep failing
//     until the next billing reset.
//   - everything else → generic non-quota error. Soft cooldown after 3
//     consecutive failures.
//
// Substring matching on err.Error() is the fallback for paths that wrap
// before returning; it covers the legacy "429" / "quota" surface plus the
// 402 OVERAGE messages from CodeWhisperer.
func (h *Handler) recordPoolError(accountID string, err error) {
	if err == nil {
		return
	}
	var qe *QuotaError
	if errors.As(err, &qe) {
		h.pool.RecordError(accountID, true, qe.RetryAfter)
		return
	}
	msg := err.Error()
	if strings.Contains(msg, "402") && strings.Contains(msg, "OVERAGE") {
		// Monthly quota exhausted — short cooldowns won't help, schedule
		// a long park instead.
		h.pool.RecordQuotaExhaustion(accountID)
		return
	}
	isQuota := strings.Contains(msg, "429") || strings.Contains(msg, "quota")
	h.pool.RecordError(accountID, isQuota, 0)
}

// handleUpstreamError records a failed CallKiroAPI invocation against the
// stats counters, the pool cooldown state, and the per-account overage flag.
// Use this for a TERMINAL failure (no failover will follow): it bumps the
// global failed-request counter exactly once. For an intermediate failover
// attempt that may be retried on another account, use recordAttemptError
// (cooldown + overage only) so one client request isn't counted as N failures.
// effort is the resolved reasoning-effort level for the request ("" when none)
// so the per-model-effort failure bucket matches the success accounting.
func (h *Handler) handleUpstreamError(err error, accountID, model, apiKeyID, effort string) {
	if err == nil {
		return
	}
	h.recordFailure(model, apiKeyID, effort)
	h.recordPoolError(accountID, err)
	h.checkOverageError(err, accountID)
}

// recordAttemptError applies the per-account bookkeeping for ONE failed
// failover attempt: it cools the account (so the dispatcher's exclude set +
// the SWRR walk both skip it) and flips the per-account overage flag on a
// 402, but does NOT touch the global failed-request counter. The dispatcher's
// caller records a single global failure once every attempt is exhausted, so
// a request that rotates across several accounts counts as one failed request,
// matching pre-failover semantics.
func (h *Handler) recordAttemptError(err error, accountID string) {
	if err == nil {
		return
	}
	h.recordPoolError(accountID, err)
	h.checkOverageError(err, accountID)
}

// maxFailoverAttempts bounds how many distinct accounts a single request will
// try before giving up. Each attempt picks a different eligible account
// (the failed ones are excluded), so this also caps worst-case added latency
// to maxFailoverAttempts upstream round-trips. 3 is enough to ride over a
// couple of accounts that throttle at call time without turning one client
// request into a pool-wide stampede.
const maxFailoverAttempts = 3

// isRetryableUpstreamError reports whether a CallKiroAPI error is worth
// retrying on a *different* account. Retryable: 429/quota throttles and
// transient 5xx / connection errors — a peer account can likely serve the
// request. NOT retryable: 401/403 (auth) and 402 (payment/overage) are
// per-account terminal conditions; retrying them just burns more accounts
// and latency for the same failure. Context cancellation (client gone) is
// also not retryable.
func isRetryableUpstreamError(err error) bool {
	if err == nil {
		return false
	}
	var qe *QuotaError
	if errors.As(err, &qe) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := err.Error()
	// Auth / payment are terminal for this account — do not fail over.
	if strings.Contains(msg, "HTTP 401") || strings.Contains(msg, "HTTP 403") || strings.Contains(msg, "HTTP 402") {
		return false
	}
	if strings.Contains(msg, "OVERAGE") {
		return false
	}
	// 429 throttles and 5xx server errors are retryable on a peer account.
	if strings.Contains(msg, "429") || strings.Contains(msg, "quota") {
		return true
	}
	if strings.Contains(msg, "HTTP 500") || strings.Contains(msg, "HTTP 502") ||
		strings.Contains(msg, "HTTP 503") || strings.Contains(msg, "HTTP 504") {
		return true
	}
	// Connection-level failures (dial/reset/timeout) — a different account
	// (possibly a different endpoint/region) may succeed.
	if strings.Contains(msg, "connection") || strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "timeout") || strings.Contains(msg, "reset") {
		return true
	}
	return false
}

// retryAfterFromErr extracts the upstream Retry-After hint from a *QuotaError,
// or 0 if the error isn't a quota error / carried no hint. Used by the
// failover dispatcher to surface a real Retry-After when every attempted
// account was throttled.
func retryAfterFromErr(err error) time.Duration {
	var qe *QuotaError
	if errors.As(err, &qe) {
		return qe.RetryAfter
	}
	return 0
}

// retryAfterSeconds rounds a duration up to a whole number of seconds, with
// a sane floor of 1. RFC 7231 Retry-After is an integer number of seconds.
func retryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	secs := int((d + time.Second - 1) / time.Second)
	if secs < 1 {
		return 1
	}
	return secs
}

// setRetryAfter writes Retry-After to the response header. Safe to call
// before WriteHeader.
func setRetryAfter(w http.ResponseWriter, d time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(d)))
}
