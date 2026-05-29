package config

import "testing"

// TestIsModelAllowedForAPIKeyFutureMinor pins the per-key allowlist
// forwards-compat: a key with Models:["claude-opus-4-8"] must accept
// inbound calls in either dashed (claude-opus-4-8) or dotted
// (claude-opus-4.8) form without a hardcoded alias entry. If this
// regresses, every "new Anthropic minor version" event becomes a
// code change in the allowlist matcher.
func TestIsModelAllowedForAPIKeyFutureMinor(t *testing.T) {
	k := APIKey{Models: []string{"claude-opus-4-8"}}
	if !IsModelAllowedForAPIKey(k, "claude-opus-4-8") {
		t.Errorf("dashed should match itself")
	}
	if !IsModelAllowedForAPIKey(k, "claude-opus-4.8") {
		t.Errorf("dotted twin should match dashed entry")
	}
	if IsModelAllowedForAPIKey(k, "claude-opus-4-7") {
		t.Errorf("different minor must not match")
	}
	if IsModelAllowedForAPIKey(k, "claude-sonnet-4-8") {
		t.Errorf("different family must not match")
	}

	// Reverse direction — allowlist holds dotted, request comes dashed.
	k2 := APIKey{Models: []string{"claude-sonnet-5.0"}}
	if !IsModelAllowedForAPIKey(k2, "claude-sonnet-5-0") {
		t.Errorf("dashed inbound should match dotted allowlist entry")
	}

	// Double-digit minors must work — the original len==3 check
	// silently rejected these. Pin so the next "no code change for
	// future minors" claim actually survives past minor 9.
	k3 := APIKey{Models: []string{"claude-opus-4-10"}}
	if !IsModelAllowedForAPIKey(k3, "claude-opus-4.10") {
		t.Errorf("dotted twin of double-digit minor should match")
	}
	if !IsModelAllowedForAPIKey(k3, "claude-opus-4-10") {
		t.Errorf("dashed double-digit minor should match itself")
	}
	if IsModelAllowedForAPIKey(k3, "claude-opus-4-1") {
		t.Errorf("4-1 must not match 4-10 (different minor, would be a real prefix bug)")
	}
}
