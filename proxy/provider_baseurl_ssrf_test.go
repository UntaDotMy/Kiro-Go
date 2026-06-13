package proxy

import (
	"os"
	"testing"
)

// Story s4: the SSRF guard on operator-supplied provider base URLs must block
// link-local / cloud-metadata addresses by default (the server-side /models
// fetch is the exploit path), while preserving the tested localhost/LAN flow.
// KIRO_STRICT_PROVIDER_URLS=1 additionally blocks loopback + private ranges.

func TestValidateProviderBaseURLDefaultPolicy(t *testing.T) {
	os.Unsetenv("KIRO_STRICT_PROVIDER_URLS")
	cases := []struct {
		name      string
		url       string
		wantError bool
	}{
		// Always blocked: link-local / cloud metadata.
		{"metadata v4", "http://169.254.169.254/v1", true},
		{"metadata exact", "http://169.254.169.254", true},
		{"link-local ipv6", "http://[fe80::1]:8080/v1", true},
		{"unspecified v4", "http://0.0.0.0:8080/v1", true},
		// Always blocked: bad scheme / schemeless (the original guard).
		{"ftp scheme", "ftp://api.example.com", true},
		{"schemeless", "api.example.com/v1", true},
		{"empty", "", true},
		// Allowed by default: public endpoints.
		{"public https", "https://api.openai.com/v1", false},
		{"public http", "http://api.example.com/v1", false},
		// Allowed by default: self-hosted localhost / LAN (the common legit case).
		{"localhost ollama", "http://127.0.0.1:11434/v1", false},
		{"loopback ipv6", "http://[::1]:11434/v1", false},
		{"private LAN", "http://192.168.1.10:11434/v1", false},
		{"private 10.x", "http://10.0.0.5:8080/v1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := validateProviderBaseURL(tc.url)
			if tc.wantError && msg == "" {
				t.Fatalf("expected %q to be rejected, got allowed", tc.url)
			}
			if !tc.wantError && msg != "" {
				t.Fatalf("expected %q to be allowed, got error %q", tc.url, msg)
			}
		})
	}
}

func TestValidateProviderBaseURLStrictMode(t *testing.T) {
	os.Setenv("KIRO_STRICT_PROVIDER_URLS", "1")
	defer os.Unsetenv("KIRO_STRICT_PROVIDER_URLS")

	// In strict mode loopback + private are now blocked, but public still works
	// and metadata is still blocked.
	blocked := []string{
		"http://127.0.0.1:11434/v1",
		"http://[::1]:11434/v1",
		"http://192.168.1.10:11434/v1",
		"http://10.0.0.5:8080/v1",
		"http://169.254.169.254/", // still blocked
	}
	for _, u := range blocked {
		if msg := validateProviderBaseURL(u); msg == "" {
			t.Errorf("strict mode must reject %q", u)
		}
	}
	if msg := validateProviderBaseURL("https://api.openai.com/v1"); msg != "" {
		t.Errorf("strict mode must still allow public %q, got %q", "https://api.openai.com/v1", msg)
	}
}

// TestValidateProviderBaseURLHostnameResolvesToMetadata documents the
// re-validation-after-DNS intent: a literal metadata IP is always refused. (A
// hostname that resolves to metadata is also refused via LookupHost; we assert
// the literal-IP path here since DNS in tests is environment-dependent.)
func TestValidateProviderBaseURLLiteralMetadataAlwaysRefused(t *testing.T) {
	os.Unsetenv("KIRO_STRICT_PROVIDER_URLS")
	if msg := validateProviderBaseURL("http://169.254.169.254/latest/meta-data/"); msg == "" {
		t.Fatal("literal cloud-metadata IP must always be refused regardless of mode")
	}
}
