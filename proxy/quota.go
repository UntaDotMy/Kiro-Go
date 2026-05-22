// Helpers shared by the Anthropic, OpenAI Chat, OpenAI Responses, and
// WebSocket handlers for surfacing 429 / Retry-After signals and translating
// upstream *QuotaError values into pool cooldown calls.
package proxy

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// recordPoolError feeds an upstream error back into the pool's cooldown
// state machine. If the error is a *QuotaError, we honor the Retry-After
// from the upstream response; otherwise we fall back to substring matching
// on legacy error strings ("429" / "quota") for paths that wrap before
// returning.
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
	isQuota := strings.Contains(msg, "429") || strings.Contains(msg, "quota")
	h.pool.RecordError(accountID, isQuota, 0)
}

// handleUpstreamError records a failed CallKiroAPI invocation against the
// stats counters, the pool cooldown state, and the per-account overage flag.
// Centralizing the three calls keeps every protocol handler (Anthropic /
// OpenAI Chat / OpenAI Responses / WebSocket) in lock-step — a future change
// (e.g. emitting a metric on every upstream failure) lands once.
func (h *Handler) handleUpstreamError(err error, accountID, model, apiKeyID string) {
	if err == nil {
		return
	}
	h.recordFailure(model, apiKeyID)
	h.recordPoolError(accountID, err)
	h.checkOverageError(err, accountID)
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
