package proxy

import (
	"kiro-go/config"
	"kiro-go/logger"
	"time"
)

// streamWorker runs one upstream attempt against a specific account. It
// returns committed=true once it has written ANY byte to the client (a
// successful response, or a mid-stream error it already surfaced). When
// committed=false and err!=nil, nothing reached the client yet, so the
// dispatcher is free to retry on a different account.
//
// The worker owns the full success path (recordSuccess, pool bookkeeping,
// per-key debit). The dispatcher only owns account selection, token refresh,
// failover, and the terminal "all accounts failed" error.
type streamWorker func(account *config.Account) (committed bool, err error)

// admissionWaitBudget bounds how long the dispatcher will wait for an in-flight
// slot to free when every eligible account is at its AIMD concurrency limit
// (least-request strategy). A short bounded wait absorbs the common case where a
// slot frees in a few hundred ms under a burst, without turning the request into
// an unbounded queue. Past the budget we shed with 429 + Retry-After so the
// client backs off coherently. See AWS "Using load shedding to avoid overload".
//
// Status of the adaptive-load-balancing work (A12):
//   DONE — least-request default + AIMD concurrency, in-flight reservation with
//     leak-safe Release, bounded admission wait + saturation shed, and the
//     tokenRefreshFailure rotation sentinel. Dedicated unit tests cover all of
//     these: LOR weighted selection + saturation skip, AIMD grow/shrink math,
//     Acquire/Release accounting incl. decayCountersLocked preserving inflight,
//     half-open single-probe recovery, ConcurrencyState (pool/least_request_test.go);
//     saturation -> admission-wait -> shed and the token-refresh rotation path
//     (proxy/failover_concurrency_test.go); strategy default + alias map
//     (config/pool_strategy_test.go). Admin API accepts "least-request"; the
//     Settings dropdown lists it as the default and the dashboard shows a live
//     per-account in-flight/limit chip over the status WebSocket.
//
//   DEFERRED:
//     - `go test -race`: needs a working cgo toolchain. CI runs it on Linux;
//       the new concurrency paths are otherwise covered by the explicit
//       Acquire/Release accounting tests above.
//     - POOL-WIDE SHEDDING (optional next layer): the Google SRE adaptive-
//       throttle reject probability p=max(0,(req-K*acc)/(req+1)) for when the
//       WHOLE pool is saturated. Per-account AIMD + the bounded admission wait
//       cover most of it; this would be the belt-and-suspenders layer.
//     - LIVE VERIFICATION: confirm against the real account pool that the 429
//       storm is gone under an ultracode parallel-agent burst.
//
// 750ms is enough for a typical 3-4 poll cycles against saturationPollInterval
// (100ms) under healthy load, while keeping a sustained-overload shed below
// 1s end-to-end. A longer budget (the prior 2s) compounded with a low AIMD
// floor into a client-visible stall — shed quickly and let the client retry.
const admissionWaitBudget = 750 * time.Millisecond

// runWithFailover selects an eligible account for the model and invokes the
// worker, rotating to a different account when the worker reports a
// retryable pre-commit failure. This converts a single unlucky pick onto a
// just-throttled account from a client-visible 5xx into a transparent retry
// while healthy peers exist (the #1 reliability gap surfaced by the pool
// audit).
//
// Concurrency control: account selection goes through the pool's RESERVING
// picker (AcquireForModelExcluding). Under the least-request strategy this
// reserves an in-flight slot on the chosen account and skips accounts already at
// their AIMD concurrency limit; the reserved slot is always released via a defer
// before the function returns (whether the attempt commits, fails over, or the
// worker panics). Under the other strategies the picker reserves nothing and
// Release is a no-op, so behavior is unchanged.
//
// model + apiKeyID are used to record exactly ONE global failed-request
// counter bump on the terminal path (when every attempt failed), so a
// request that rotates across N accounts still counts as a single failed
// request — matching pre-failover semantics. Per-attempt account cooldown +
// overage bookkeeping is the worker's job (recordAttemptError).
//
// Contract:
//   - selectErr handles the "no account at all / all cooling / all saturated"
//     case BEFORE the first attempt — it's returned to the caller's
//     protocol-specific error path, with retryAfter set when the pool is merely
//     cooling or saturated.
//   - Once the worker commits (writes to the client), its result is final;
//     no failover happens after the first byte (we can't un-send SSE frames).
//   - ensureValidToken failures are treated as retryable pre-commit errors:
//     a different account may have a healthy token.
//
// Returns:
//   - committed: whether any worker wrote to the client (caller should NOT
//     also write an error envelope when true).
//   - retryAfter: when every attempt was throttled, the soonest upstream hint
//     so the caller can emit a real Retry-After with its 429.
//   - err: the last upstream error when no attempt committed; nil on success.
func (h *Handler) runWithFailover(model, apiKeyID, effort string, worker streamWorker) (committed bool, retryAfter time.Duration, err error) {
	tried := make(map[string]bool, maxFailoverAttempts)
	var lastErr error
	var lastRetryAfter time.Duration

	// recordTerminal bumps the single global failed-request counter once, for
	// the request as a whole, when we're about to give up without committing.
	recordTerminal := func() {
		if lastErr != nil {
			h.recordFailure(model, apiKeyID, effort)
		}
	}

	for attempt := 0; attempt < maxFailoverAttempts; attempt++ {
		account, poolRetryAfter, ok := h.acquireWithAdmissionWait(model, tried)
		if !ok {
			// No (more) eligible accounts. If the pool is merely cooling or
			// saturated, surface the soonest recovery hint; otherwise it's a
			// hard "no accounts" condition.
			if poolRetryAfter > 0 {
				recordTerminal()
				return false, poolRetryAfter, lastErr
			}
			recordTerminal()
			return false, lastRetryAfter, lastErr
		}
		tried[account.ID] = true

		// A slot may have been reserved on this account (least-request strategy);
		// release it exactly once when this attempt is done, regardless of how
		// the iteration exits — including a worker panic. The defer inside this
		// closure is what guarantees that: a bare sequential Release (the prior
		// form) was skipped when the worker panicked, permanently leaking the
		// reserved in-flight slot and slowly shrinking the account out of the
		// least-request rotation (decayCountersLocked preserves entries while
		// inflight>0). The panic still propagates to net/http's per-request
		// recover; we only ensure the slot is freed on the way out. Release is a
		// no-op for non-reserving strategies and for an account that reserved
		// nothing.
		committedThisAttempt, workErr := func() (bool, error) {
			defer h.pool.Release(account.ID)
			return h.runOneAttempt(account, worker, attempt)
		}()

		if committedThisAttempt {
			// Bytes are on the wire — the worker fully owns the outcome.
			return true, 0, workErr
		}
		if workErr == nil {
			// Either a token-refresh failover signal (handled below) or a
			// non-committing success. runOneAttempt returns a sentinel for the
			// token case; a genuine nil here means done.
			return false, 0, nil
		}
		lastErr = workErr
		if ra := retryAfterFromErr(workErr); ra > 0 {
			lastRetryAfter = ra
		}
		if isTokenRefreshFailure(workErr) {
			// Token refresh is a pre-commit step; a peer account may have a
			// valid token. Already logged in runOneAttempt; just rotate.
			continue
		}
		if !isRetryableUpstreamError(workErr) {
			// Terminal for this request (auth/payment/client-cancel). Don't
			// burn more accounts on a failure a peer can't fix.
			recordTerminal()
			return false, lastRetryAfter, workErr
		}
		logger.Infof("[Failover] Account %s failed with retryable error (attempt %d/%d), rotating: %v",
			redactForLog(account.Email), attempt+1, maxFailoverAttempts, workErr)
	}

	recordTerminal()
	return false, lastRetryAfter, lastErr
}

// acquireWithAdmissionWait wraps the pool's reserving picker with a bounded wait
// for a free in-flight slot. When the picker reports saturation (every eligible
// account at its AIMD limit, signalled by a small retryAfter with ok=false and
// no account), it retries for up to admissionWaitBudget before giving up — a
// slot frees as concurrent requests complete. A cooling-pool retryAfter (longer
// than the saturation poll) is returned immediately, since waiting won't help on
// that timescale.
func (h *Handler) acquireWithAdmissionWait(model string, tried map[string]bool) (*config.Account, time.Duration, bool) {
	deadline := time.Now().Add(admissionWaitBudget)
	for {
		account, retryAfter, ok := h.pool.AcquireForModelExcluding(model, tried)
		if ok {
			return account, 0, true
		}
		// Distinguish "busy, try again very soon" (saturation poll) from
		// "cooling / empty" (return now). Saturation uses the small poll
		// interval; anything larger is a real cooldown the wait can't beat.
		if retryAfter <= 0 || retryAfter > saturationPollHint {
			return nil, retryAfter, false
		}
		if time.Now().After(deadline) {
			// Waited our budget and still no slot. Shed with the poll hint so
			// the caller emits a short Retry-After.
			return nil, retryAfter, false
		}
		time.Sleep(retryAfter)
	}
}

// runOneAttempt performs token refresh + a single worker invocation for one
// account. It returns (committed, err). A token-refresh failure is returned as a
// retryable error tagged via tokenRefreshFailure so the dispatcher rotates
// without treating it as an upstream error.
func (h *Handler) runOneAttempt(account *config.Account, worker streamWorker, attempt int) (bool, error) {
	if tokErr := h.ensureValidToken(account); tokErr != nil {
		logger.Warnf("[Failover] Token refresh failed for %s (attempt %d/%d): %v",
			redactForLog(account.Email), attempt+1, maxFailoverAttempts, tokErr)
		return false, tokenRefreshFailure{tokErr}
	}
	return worker(account)
}
