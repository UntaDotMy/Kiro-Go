// Helpers shared by the Anthropic, OpenAI Chat, OpenAI Responses, and
// WebSocket handlers for surfacing 429 / Retry-After signals and translating
// upstream *QuotaError values into pool cooldown calls.
package proxy

import (
	"context"
	"errors"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
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
	// Terminal per-account conditions (suspension / revoked-or-invalid auth)
	// can't be fixed by a cooldown or a peer retry — the account is dead until
	// the operator intervenes (or, for a transient token issue, the next
	// background refresh re-validates it). Disable it on THIS request instead of
	// leaving the picker to keep selecting it until the next refresh tick. The
	// background RefreshAccountInfo path still auto-clears BanStatus back to
	// ACTIVE on a later healthy refresh, so a transient blip self-heals.
	if isSuspensionErrorMessage(msg) {
		h.disableAccountByID(accountID, "AWS temporarily suspended - unusual user activity detected")
		h.pool.RecordError(accountID, false, 0)
		return
	}
	if isAuthErrorMessage(msg) {
		h.disableAccountByID(accountID, "Authentication failed - token invalid or expired")
		h.pool.RecordError(accountID, false, 0)
		return
	}
	isQuota := strings.Contains(msg, "429") || strings.Contains(msg, "quota")
	h.pool.RecordError(accountID, isQuota, 0)
}

// isSuspensionErrorMessage reports whether an upstream error indicates AWS has
// suspended the account (a terminal, operator-must-intervene condition).
func isSuspensionErrorMessage(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "temporarily_suspended") ||
		strings.Contains(m, "temporarily is suspended") ||
		strings.Contains(m, "account suspended")
}

// isAuthErrorMessage reports whether an upstream error indicates the account's
// credentials are no longer valid (401/403, invalid/expired token, bad grant).
func isAuthErrorMessage(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "http 401") ||
		strings.Contains(m, "http 403") ||
		strings.Contains(m, "unauthorized") ||
		strings.Contains(m, "forbidden") ||
		strings.Contains(m, "authentication failed") ||
		strings.Contains(m, "token invalid") ||
		strings.Contains(m, "token expired") ||
		strings.Contains(m, "invalid_grant") ||
		strings.Contains(m, "access token expired") ||
		strings.Contains(m, "refresh token expired")
}

// disableAccountByID disables an account on the request path (BanStatus=BANNED,
// Enabled=false) and reloads the pool so the picker stops selecting it
// immediately. Idempotent: a no-op if the account is already disabled with the
// same ban reason, so a burst of failing requests doesn't thrash config.Save or
// the pool reload. The next healthy background RefreshAccountInfo re-enables it.
func (h *Handler) disableAccountByID(accountID, banReason string) {
	acc := h.pool.GetByID(accountID)
	if acc == nil {
		return
	}
	if !acc.Enabled && acc.BanStatus == "BANNED" && acc.BanReason == banReason {
		return // already disabled for this reason — avoid Save/Reload thrash
	}
	updated := *acc
	updated.Enabled = false
	updated.BanStatus = "BANNED"
	updated.BanReason = banReason
	updated.BanTime = time.Now().Unix()
	if err := config.UpdateAccount(accountID, updated); err != nil {
		logger.Warnf("[Failover] failed to disable %s: %v", redactForLog(acc.Email), err)
		return
	}
	logger.Warnf("[Failover] disabled %s on request path: %s", redactForLog(acc.Email), banReason)
	h.pool.Reload()
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

// maxFailoverAttempts is the CEILING on how many distinct accounts a single
// request will try before giving up. The actual budget per request is
// min(eligibleAccounts, maxFailoverAttempts) — see failoverBudget — so a small
// pool isn't over-tried and a large pool isn't artificially capped at a tiny
// constant while many healthy accounts go untried (the reliability gap the pool
// audit flagged). Each attempt picks a different eligible account (failed ones
// are excluded), so this also bounds worst-case added latency to this many
// upstream round-trips. The ceiling keeps a pathological pool (hundreds of
// dead accounts) from turning one client request into a pool-wide stampede.
const maxFailoverAttempts = 10

// minFailoverAttempts is the FLOOR on the per-request attempt budget. It applies
// when the addressable pool size can't be determined (count returns 0, e.g. a
// bespoke test pool) so failover still has the historical headroom to ride over
// a couple of just-throttled accounts. It also guarantees a single-account pool
// still gets a real attempt.
const minFailoverAttempts = 3

// saturationPollHint is the upper bound (inclusive) on a pool retryAfter that
// the dispatcher treats as "busy, slot will free shortly" rather than "cooling,
// come back later". It must be >= the pool's saturationPollInterval (25ms); a
// retryAfter at or below this is polled within admissionWaitBudget, anything
// larger is surfaced to the client immediately. Kept here (proxy package) so the
// dispatcher doesn't hard-depend on the pool's internal constant for its own
// threshold value.
const saturationPollHint = 250 * time.Millisecond

// Compile-time invariant: the dispatcher's "wait for a freed slot" threshold
// must be >= the value the pool actually returns on saturation. Converting the
// difference to an unsigned constant fails the build if it goes negative (a
// negative constant overflows uint) — far better than silently classifying
// every saturated-pool return as a real cooldown and shedding immediately,
// which would break the "wait until an account frees" contract.
const _ = uint(saturationPollHint - pool.SaturationPollInterval)

// tokenRefreshFailure tags a pre-commit token-refresh error so the failover
// dispatcher rotates to a peer account without classifying it as an upstream
// (quota/5xx) error. It is never surfaced to the client.
type tokenRefreshFailure struct{ err error }

func (e tokenRefreshFailure) Error() string { return "token refresh failed: " + e.err.Error() }
func (e tokenRefreshFailure) Unwrap() error { return e.err }

// isTokenRefreshFailure reports whether err is the token-refresh sentinel.
func isTokenRefreshFailure(err error) bool {
	var t tokenRefreshFailure
	return errors.As(err, &t)
}

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
	// HTTP/2 stream reset (RST_STREAM / GOAWAY) from the upstream — a
	// fresh transport on a peer account may skip the bad stream. The
	// context-canceled check below still runs first via the substring
	// fallback in the rare case the cause is itself a context error,
	// but typed *context.Canceled never reaches this branch.
	var sre *ErrUpstreamStreamReset
	if errors.As(err, &sre) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := err.Error()
	// Payment / overage is terminal: a 402 or OVERAGE is a billing state that a
	// peer retry should not burn more accounts trying to beat. Keep it terminal.
	if strings.Contains(msg, "HTTP 402") || strings.Contains(msg, "OVERAGE") {
		return false
	}
	// Auth (401/403) is terminal for the FAILED account but retryable across
	// accounts: tokens are per-account, so a revoked/expired token on account A
	// says nothing about account B. recordPoolError has already disabled the
	// dead account (disableAccountByID on isAuthErrorMessage) and it's in the
	// dispatcher's `tried` set, so failover rotates to a healthy peer instead of
	// returning a 503 while other accounts are fine — it cannot loop back onto
	// the dead one, and maxFailoverAttempts bounds the rotation.
	if strings.Contains(msg, "HTTP 401") || strings.Contains(msg, "HTTP 403") {
		return true
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
