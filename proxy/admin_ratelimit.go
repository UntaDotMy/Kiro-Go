package proxy

import (
	"net"
	"net/http"
	"os"
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

// trustedProxies is the parsed KIRO_TRUSTED_PROXIES allowlist (CIDRs or bare
// IPs). When the immediate peer (RemoteAddr) is in this set, we trust the
// X-Forwarded-For header for client identification; otherwise XFF is ignored.
// Parsed once at first use.
var (
	trustedProxiesOnce sync.Once
	trustedProxyNets   []*net.IPNet
)

// resetTrustedProxiesForTest re-reads KIRO_TRUSTED_PROXIES, bypassing the
// sync.Once cache. Test-only: lets a test flip the env var and observe the new
// behavior. Not used in production code paths.
func resetTrustedProxiesForTest() {
	trustedProxiesOnce = sync.Once{}
	trustedProxyNets = nil
}

func loadTrustedProxies() {
	raw := strings.TrimSpace(os.Getenv("KIRO_TRUSTED_PROXIES"))
	if raw == "" {
		return
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Accept both "10.0.0.0/8" and a bare "10.1.2.3".
		if !strings.Contains(part, "/") {
			if ip := net.ParseIP(part); ip != nil {
				if ip.To4() != nil {
					part += "/32"
				} else {
					part += "/128"
				}
			}
		}
		if _, ipnet, err := net.ParseCIDR(part); err == nil {
			trustedProxyNets = append(trustedProxyNets, ipnet)
		}
	}
}

// peerIsTrustedProxy reports whether the request's immediate peer (RemoteAddr)
// is in the configured trusted-proxy allowlist. Only then is X-Forwarded-For
// honored.
func peerIsTrustedProxy(r *http.Request) bool {
	trustedProxiesOnce.Do(loadTrustedProxies)
	if len(trustedProxyNets) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range trustedProxyNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

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
	// Opportunistic sweep of expired records while we hold the lock. Without
	// this the map would only shrink lazily when a given IP retries, so a
	// stream of distinct source IPs would grow it without bound. The map is
	// small in practice, so an O(n) sweep on each failure is cheap.
	sweepAdminFailsLocked()
	rec, ok := adminFails[ip]
	if !ok || time.Since(rec.earliest) > adminFailureWindow {
		adminFails[ip] = &adminFailureRecord{count: 1, earliest: time.Now()}
		return
	}
	rec.count++
}

// sweepAdminFailsLocked deletes failure records older than the window. Caller
// must hold adminFailMu.
func sweepAdminFailsLocked() {
	for ip, rec := range adminFails {
		if time.Since(rec.earliest) > adminFailureWindow {
			delete(adminFails, ip)
		}
	}
}

// startAdminFailSweeper periodically sweeps expired records independent of the
// failure path. The opportunistic sweep in recordAdminFailure only fires when
// SOME IP fails again, so a distributed attack from many distinct single-attempt
// IPs would accumulate entries that are never swept (each IP never retries). A
// background ticker bounds the map to entries from the last adminFailureWindow
// regardless of attack pattern. Stops on the handler's shutdown signal.
func (h *Handler) startAdminFailSweeper() {
	safeGo("adminFailSweeper", func() {
		ticker := time.NewTicker(adminFailureWindow)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				adminFailMu.Lock()
				sweepAdminFailsLocked()
				adminFailMu.Unlock()
			case <-h.stopRefresh:
				return
			}
		}
	})
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

// clientIP returns the source IP used for admin brute-force keying. By default
// it is the immediate peer (RemoteAddr) — X-Forwarded-For is NOT trusted,
// because an unauthenticated attacker can forge it to get a fresh lockout
// budget per spoofed value, defeating the lockout entirely. XFF is honored
// ONLY when the immediate peer is in the KIRO_TRUSTED_PROXIES allowlist (set
// this when the proxy genuinely runs behind a known reverse proxy / load
// balancer). On localhost / direct exposure, leave it unset.
func clientIP(r *http.Request) string {
	if peerIsTrustedProxy(r) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// First entry is the original client.
			if comma := strings.IndexByte(xff, ','); comma > 0 {
				return strings.TrimSpace(xff[:comma])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
