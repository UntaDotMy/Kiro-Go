package proxy

import "testing"

// TestIsSuspensionErrorMessage covers the classifier that triggers an
// immediate request-path account disable for AWS suspension.
func TestIsSuspensionErrorMessage(t *testing.T) {
	yes := []string{
		"HTTP 403 from Kiro IDE: TEMPORARILY_SUSPENDED",
		"Account suspended: unusual activity",
		"this account temporarily is suspended",
		"ERROR: temporarily_suspended",
	}
	no := []string{
		"HTTP 429 throttled",
		"rate limited (HTTP 429) on Kiro IDE",
		"connection reset by peer",
		"",
	}
	for _, m := range yes {
		if !isSuspensionErrorMessage(m) {
			t.Errorf("isSuspensionErrorMessage(%q) = false, want true", m)
		}
	}
	for _, m := range no {
		if isSuspensionErrorMessage(m) {
			t.Errorf("isSuspensionErrorMessage(%q) = true, want false", m)
		}
	}
}

// TestIsAuthErrorMessage covers the classifier that triggers an immediate
// request-path disable for revoked/invalid credentials.
func TestIsAuthErrorMessage(t *testing.T) {
	yes := []string{
		"HTTP 401 from Kiro IDE: unauthorized",
		"HTTP 403 forbidden",
		"authentication failed",
		"token invalid",
		"token expired",
		"invalid_grant",
		"refresh token expired",
		"access token expired",
	}
	no := []string{
		"HTTP 429 throttled",
		"HTTP 500 internal server error",
		"rate limited (HTTP 429) on Kiro IDE",
		"context deadline exceeded",
		"",
	}
	for _, m := range yes {
		if !isAuthErrorMessage(m) {
			t.Errorf("isAuthErrorMessage(%q) = false, want true", m)
		}
	}
	for _, m := range no {
		if isAuthErrorMessage(m) {
			t.Errorf("isAuthErrorMessage(%q) = true, want false", m)
		}
	}
}

// TestAuthAndSuspensionDisjointFromQuota guards against a future edit making a
// quota/429 string accidentally classify as a terminal disable condition (which
// would wrongly ban an account that just needs a cooldown).
func TestAuthAndSuspensionDisjointFromQuota(t *testing.T) {
	quotaish := []string{
		"rate limited (HTTP 429) on Kiro IDE (retry after 5s)",
		"HTTP 429 Too Many Requests",
		"402 OVERAGE limit reached",
	}
	for _, m := range quotaish {
		if isAuthErrorMessage(m) || isSuspensionErrorMessage(m) {
			t.Errorf("quota-style message %q must not classify as auth/suspension", m)
		}
	}
}
