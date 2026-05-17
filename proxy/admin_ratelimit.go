package proxy

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// adminFailureWindow + adminFailureMax control the per-IP lockout for failed
// admin auth attempts. 10 failures in 5 minutes = locked out until the
// window expires. The store is in-memory only; restart clears it. This is
// not a substitute for a real WAF / fail2ban, but it stops the trivial
// online brute-force attempts.
const (
	adminFailureWindow = 5 * time.Minute
	adminFailureMax    = 10
)

type adminFailureRecord struct {
	count    int
	earliest time.Time
}

var (
	adminFailMu sync.Mutex
	adminFails  = map[string]*adminFailureRecord{}
)

// allowAdminAttempt returns true when the IP is below the failure threshold.
// false means the IP is currently locked out.
func (h *Handler) allowAdminAttempt(r *http.Request) bool {
	ip := clientIP(r)
	adminFailMu.Lock()
	defer adminFailMu.Unlock()
	rec, ok := adminFails[ip]
	if !ok {
		return true
	}
	if time.Since(rec.earliest) > adminFailureWindow {
		delete(adminFails, ip)
		return true
	}
	return rec.count < adminFailureMax
}

// recordAdminFailure increments the failure counter for the IP. Call this on
// every wrong-password attempt.
func (h *Handler) recordAdminFailure(r *http.Request) {
	ip := clientIP(r)
	adminFailMu.Lock()
	defer adminFailMu.Unlock()
	rec, ok := adminFails[ip]
	if !ok || time.Since(rec.earliest) > adminFailureWindow {
		adminFails[ip] = &adminFailureRecord{count: 1, earliest: time.Now()}
		return
	}
	rec.count++
}

// resetAdminFailures clears the failure counter for the IP after a successful
// auth, so a previously-suspect IP that gets the password right doesn't stay
// rate-limited.
func (h *Handler) resetAdminFailures(r *http.Request) {
	ip := clientIP(r)
	adminFailMu.Lock()
	defer adminFailMu.Unlock()
	delete(adminFails, ip)
}

// clientIP returns the source IP of the request. Honors X-Forwarded-For when
// the proxy is itself behind a reverse proxy (only the first token is used).
// Falls back to RemoteAddr.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// First entry is the original client.
		if comma := strings.IndexByte(xff, ','); comma > 0 {
			return strings.TrimSpace(xff[:comma])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
