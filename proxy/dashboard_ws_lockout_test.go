package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Story s3: the dashboard WebSocket auth path must enforce the same per-IP
// brute-force lockout as the HTTP admin path, so the WS upgrade can't be an
// unthrottled password-guessing oracle.

// recordFailuresFor drives the per-IP failure counter to the lockout threshold
// for a given source IP, using the same method the auth path calls.
func recordFailuresFor(h *Handler, ip string, n int) {
	for i := 0; i < n; i++ {
		r := httptest.NewRequest(http.MethodGet, "/admin/ws", nil)
		r.RemoteAddr = ip + ":40000"
		h.recordAdminFailure(r)
	}
}

func TestDashboardWSEnforcesBruteForceLockout(t *testing.T) {
	h := &Handler{}
	const ip = "203.0.113.77" // TEST-NET-3, unique to this test

	// Sanity: a fresh IP is allowed to attempt.
	fresh := httptest.NewRequest(http.MethodGet, "/admin/ws", nil)
	fresh.RemoteAddr = ip + ":40000"
	if !h.allowAdminAttempt(fresh) {
		t.Fatalf("fresh IP should be allowed before any failures")
	}

	// Drive the counter to the lockout threshold.
	recordFailuresFor(h, ip, adminFailureMax)

	// The WS upgrade from the locked-out IP must be refused with 429 BEFORE any
	// password check — i.e. the upgrade never succeeds and we get the lockout
	// status, not a 401/101.
	req := httptest.NewRequest(http.MethodGet, "/admin/ws", nil)
	req.RemoteAddr = ip + ":40000"
	// Offer a password-bearing subprotocol so we'd reach VerifyPassword if the
	// gate were absent.
	req.Header.Set("Sec-WebSocket-Protocol", dashboardWSAuthSubprotocolPrefix+"whatever")
	rec := httptest.NewRecorder()

	h.handleDashboardWS(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked-out IP: got status %d, want 429 (lockout must gate the WS upgrade)", rec.Code)
	}

	// Cleanup so the package-global failure map doesn't leak into other tests.
	h.resetAdminFailures(req)
}

// TestDashboardWSWrongPasswordCountsTowardLockout verifies a wrong-password WS
// attempt increments the same counter, so repeated WS guesses eventually trip
// the lockout (the oracle is closed).
func TestDashboardWSWrongPasswordCountsTowardLockout(t *testing.T) {
	h := &Handler{}
	const ip = "203.0.113.88"

	probe := httptest.NewRequest(http.MethodGet, "/admin/ws", nil)
	probe.RemoteAddr = ip + ":40000"

	// One short of the threshold: still allowed.
	recordFailuresFor(h, ip, adminFailureMax-1)
	if !h.allowAdminAttempt(probe) {
		t.Fatalf("IP one failure below threshold should still be allowed")
	}

	// One more failure crosses the threshold.
	recordFailuresFor(h, ip, 1)
	if h.allowAdminAttempt(probe) {
		t.Fatalf("IP at the failure threshold must be locked out")
	}

	h.resetAdminFailures(probe)
}
