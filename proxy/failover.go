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

// runWithFailover selects an eligible account for the model and invokes the
// worker, rotating to a different account when the worker reports a
// retryable pre-commit failure. This converts a single unlucky pick onto a
// just-throttled account from a client-visible 5xx into a transparent retry
// while healthy peers exist (the #1 reliability gap surfaced by the pool
// audit).
//
// model + apiKeyID are used to record exactly ONE global failed-request
// counter bump on the terminal path (when every attempt failed), so a
// request that rotates across N accounts still counts as a single failed
// request — matching pre-failover semantics. Per-attempt account cooldown +
// overage bookkeeping is the worker's job (recordAttemptError).
//
// Contract:
//   - selectErr handles the "no account at all / all cooling" case BEFORE the
//     first attempt — it's returned to the caller's protocol-specific error
//     path, with retryAfter set when the pool is merely cooling.
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
		account, poolRetryAfter, ok := h.pool.GetNextForModelExcluding(model, tried)
		if !ok {
			// No (more) eligible accounts. If the pool is merely cooling,
			// surface the soonest recovery hint; otherwise it's a hard
			// "no accounts" condition.
			if poolRetryAfter > 0 {
				recordTerminal()
				return false, poolRetryAfter, lastErr
			}
			// If we already tried at least one account, return its error so
			// the caller's envelope is informative; else signal empty pool.
			recordTerminal()
			return false, lastRetryAfter, lastErr
		}
		tried[account.ID] = true

		// Token refresh is a pre-commit step; a failure here is retryable on
		// a peer account (its token may be valid).
		if tokErr := h.ensureValidToken(account); tokErr != nil {
			lastErr = tokErr
			logger.Warnf("[Failover] Token refresh failed for %s (attempt %d/%d): %v",
				redactForLog(account.Email), attempt+1, maxFailoverAttempts, tokErr)
			continue
		}

		didCommit, workErr := worker(account)
		if didCommit {
			// Bytes are on the wire — the worker fully owns the outcome.
			return true, 0, workErr
		}
		if workErr == nil {
			// Worker chose not to commit but reported success (shouldn't
			// normally happen); treat as done.
			return false, 0, nil
		}
		lastErr = workErr
		if ra := retryAfterFromErr(workErr); ra > 0 {
			lastRetryAfter = ra
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
