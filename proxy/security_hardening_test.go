package proxy

import (
	"net/http/httptest"
	"os"
	"testing"
)

// TestClientIPIgnoresXFFByDefault locks in the brute-force keying fix: without
// a configured trusted-proxy allowlist, X-Forwarded-For must NOT be trusted, so
// an attacker can't rotate the header to get a fresh lockout budget per value.
func TestClientIPIgnoresXFFByDefault(t *testing.T) {
	// Ensure no trusted proxies configured and the once-cache reflects that.
	os.Unsetenv("KIRO_TRUSTED_PROXIES")
	resetTrustedProxiesForTest()

	r := httptest.NewRequest("POST", "/admin/api/settings", nil)
	r.RemoteAddr = "203.0.113.7:5555"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")

	if got := clientIP(r); got != "203.0.113.7" {
		t.Fatalf("XFF must be ignored by default; expected peer IP 203.0.113.7, got %q", got)
	}
}

// TestClientIPHonorsXFFFromTrustedProxy verifies that when the immediate peer
// is in KIRO_TRUSTED_PROXIES, the real client IP from XFF is used.
func TestClientIPHonorsXFFFromTrustedProxy(t *testing.T) {
	os.Setenv("KIRO_TRUSTED_PROXIES", "203.0.113.0/24")
	defer os.Unsetenv("KIRO_TRUSTED_PROXIES")
	resetTrustedProxiesForTest()

	r := httptest.NewRequest("POST", "/admin/api/settings", nil)
	r.RemoteAddr = "203.0.113.7:5555" // in the trusted range
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.7")

	if got := clientIP(r); got != "1.2.3.4" {
		t.Fatalf("trusted proxy must surface the real client IP from XFF, got %q", got)
	}
}

// TestValidateProxyURL covers the SSRF guard: scheme enforcement + link-local
// rejection (cloud metadata), while allowing normal/private proxies.
func TestValidateProxyURL(t *testing.T) {
	os.Unsetenv("KIRO_ALLOW_LINKLOCAL_PROXY")
	cases := []struct {
		name      string
		url       string
		wantError bool
	}{
		{"valid http", "http://proxy.example:8080", false},
		{"valid socks5", "socks5://127.0.0.1:1080", false},   // localhost proxy is legit
		{"valid private LAN", "http://10.1.2.3:3128", false}, // corporate proxy is legit
		{"bad scheme", "ftp://proxy.example", true},
		{"no scheme", "proxy.example:8080", true},
		{"link-local metadata", "http://169.254.169.254/", true},
		{"link-local ipv6", "http://[fe80::1]:80/", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := validateProxyURL(tc.url)
			if tc.wantError && msg == "" {
				t.Fatalf("expected %q to be rejected", tc.url)
			}
			if !tc.wantError && msg != "" {
				t.Fatalf("expected %q to be allowed, got error %q", tc.url, msg)
			}
		})
	}
}

// TestValidateProxyURLLinkLocalOverride verifies the escape hatch.
func TestValidateProxyURLLinkLocalOverride(t *testing.T) {
	os.Setenv("KIRO_ALLOW_LINKLOCAL_PROXY", "1")
	defer os.Unsetenv("KIRO_ALLOW_LINKLOCAL_PROXY")
	if msg := validateProxyURL("http://169.254.169.254/"); msg != "" {
		t.Fatalf("override must allow link-local, got %q", msg)
	}
}
