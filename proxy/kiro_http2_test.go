package proxy

import (
	"net/http"
	"testing"
)

// TestDirectTransportHasHTTP2Pings is the regression guard for the mid-stream
// "context deadline exceeded … while reading body" disconnect. The direct
// (non-proxied) transport MUST negotiate h2 AND carry active PING
// health-checks, otherwise a silently-dropped upstream connection hangs until
// the 5-minute idleTimeoutReader fires and the cancellation surfaces to the
// client mid-turn.
func TestDirectTransportHasHTTP2Pings(t *testing.T) {
	tr := buildKiroTransport("")

	if !tr.ForceAttemptHTTP2 {
		t.Fatal("direct transport must attempt HTTP/2")
	}
	// ConfigureTransports registers an h2 entry in TLSNextProto; its presence
	// proves the ping-configuration path actually ran on this transport.
	if tr.TLSNextProto == nil {
		t.Fatal("TLSNextProto is nil: http2.ConfigureTransports never ran, so ReadIdleTimeout/PingTimeout were not applied")
	}
	if _, ok := tr.TLSNextProto["h2"]; !ok {
		t.Fatalf("expected an \"h2\" entry in TLSNextProto, got keys %v", keysOf(tr.TLSNextProto))
	}
}

// TestEnableHTTP2PingsAppliesTimeouts asserts the concrete ping budgets land on
// the h2 transport. A future edit that drops the assignment or zeroes a
// constant would silently disable detection; this catches it.
func TestEnableHTTP2PingsAppliesTimeouts(t *testing.T) {
	// buildKiroTransport already configures h2 on its transport, so a second
	// ConfigureTransports call on it would error — exercise the helper on a
	// fresh transport instead to assert the values it sets.
	fresh := &http.Transport{ForceAttemptHTTP2: true}
	h2, err := enableHTTP2Pings(fresh)
	if err != nil {
		t.Fatalf("enableHTTP2Pings failed: %v", err)
	}
	if h2 == nil {
		t.Fatal("expected a non-nil *http2.Transport")
	}
	if h2.ReadIdleTimeout != h2ReadIdleTimeout {
		t.Fatalf("ReadIdleTimeout = %v, want %v", h2.ReadIdleTimeout, h2ReadIdleTimeout)
	}
	if h2.PingTimeout != h2PingTimeout {
		t.Fatalf("PingTimeout = %v, want %v", h2.PingTimeout, h2PingTimeout)
	}
	if h2ReadIdleTimeout <= 0 || h2PingTimeout <= 0 {
		t.Fatal("ping timeouts must be positive or health-checking is disabled")
	}
	// Detection budget (read-idle + ping wait) must stay well under the
	// idleTimeoutReader window, otherwise the slow path still wins the race and
	// the disconnect resurfaces.
	if h2ReadIdleTimeout+h2PingTimeout >= streamIdleTimeout {
		t.Fatalf("h2 detection budget %v must be < streamIdleTimeout %v",
			h2ReadIdleTimeout+h2PingTimeout, streamIdleTimeout)
	}
}

func TestProxiedTransportSkipsHTTP2(t *testing.T) {
	tr := buildKiroTransport("http://127.0.0.1:9")
	if tr.ForceAttemptHTTP2 {
		t.Fatal("proxied transport must not attempt HTTP/2 (h2 can't be negotiated through a forward proxy)")
	}
}

// TestTransportBoundsTLSHandshake guards the fix for an unbounded TLS handshake.
// A hand-built http.Transport does NOT inherit http.DefaultTransport's 10s
// TLSHandshakeTimeout, so leaving it unset means a handshake stalled behind a
// dead middlebox hangs forever on the background-context callers (admin
// warmup/probe, buffered agentic rounds) that have no request ctx deadline.
// DialContext.Timeout covers only the preceding TCP connect, not the handshake.
// Both the direct and proxied transports must carry a positive, bounded value.
func TestTransportBoundsTLSHandshake(t *testing.T) {
	for _, proxyURL := range []string{"", "http://127.0.0.1:9"} {
		tr := buildKiroTransport(proxyURL)
		if tr.TLSHandshakeTimeout <= 0 {
			t.Fatalf("proxyURL=%q: TLSHandshakeTimeout = %v, must be positive (an unset 0 means unbounded)", proxyURL, tr.TLSHandshakeTimeout)
		}
		// Must stay below the dial timeout's sibling budgets so a stalled
		// handshake fails fast rather than approaching the multi-minute windows.
		if tr.TLSHandshakeTimeout > responseHeaderTimeout {
			t.Fatalf("proxyURL=%q: TLSHandshakeTimeout %v should be well under responseHeaderTimeout %v", proxyURL, tr.TLSHandshakeTimeout, responseHeaderTimeout)
		}
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
