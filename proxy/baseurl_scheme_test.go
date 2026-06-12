package proxy

import "testing"

// TestIsValidBaseURLScheme confirms base URLs accept BOTH http and https (not
// everyone serves TLS — self-hosted/LAN/localhost gateways often use plain http)
// while still rejecting schemeless input and non-web schemes (SSRF guard).
func TestIsValidBaseURLScheme(t *testing.T) {
	cases := map[string]bool{
		"https://api.example.com/v1":   true,
		"http://api.example.com/v1":    true,
		"HTTP://API.EXAMPLE.COM":       true, // case-insensitive
		"https://localhost:8080":       true,
		"http://192.168.1.10:11434/v1": true, // LAN / on-prem
		"  http://host/v1  ":           true, // trimmed
		"ftp://api.example.com":        false,
		"file:///etc/passwd":           false,
		"api.example.com/v1":           false, // schemeless
		"":                             false,
		"ws://api.example.com":         false,
	}
	for in, want := range cases {
		if got := isValidBaseURLScheme(in); got != want {
			t.Errorf("isValidBaseURLScheme(%q) = %v, want %v", in, got, want)
		}
	}
}
