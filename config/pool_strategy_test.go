package config

import "testing"

// TestGetPoolStrategyDefaultsToLeastRequest verifies the production default:
// an initialized config with an empty PoolStrategy resolves to "least-request"
// (the adaptive-load-balancing default), NOT "swr". The cfg==nil fallback is a
// separate test-only path covered below.
func TestGetPoolStrategyDefaultsToLeastRequest(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{}) // PoolStrategy == ""
	if got := GetPoolStrategy(); got != "least-request" {
		t.Fatalf("empty PoolStrategy on an initialized config should default to least-request, got %q", got)
	}
}

// TestGetPoolStrategyNormalization pins the alias map: every recognized spelling
// of each strategy normalizes to its canonical form, and an unrecognized value
// falls back to the least-request default.
func TestGetPoolStrategyNormalization(t *testing.T) {
	cases := map[string]string{
		// least-request and its aliases.
		"least-request": "least-request",
		"least-conn":    "least-request",
		"leastrequest":  "least-request",
		"least_request": "least-request",
		"lor":           "least-request",
		"LEAST-REQUEST": "least-request",
		"  least-request  ": "least-request",
		// swr family.
		"swr":  "swr",
		"swrr": "swr",
		// least-used family.
		"least-used": "least-used",
		"leastused":  "least-used",
		"least_used": "least-used",
		// random.
		"random": "random",
		// unknown -> default.
		"round-robin": "least-request",
		"garbage":     "least-request",
	}
	for input, want := range cases {
		withTestAPIKeyConfig(t, &Config{PoolStrategy: input})
		if got := GetPoolStrategy(); got != want {
			t.Fatalf("GetPoolStrategy(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestUpdatePoolStrategyPersists verifies UpdatePoolStrategy writes the value
// through so the next GetPoolStrategy reads it back (trimmed).
func TestUpdatePoolStrategyPersists(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{})
	if err := UpdatePoolStrategy("  least-request  "); err != nil {
		t.Fatalf("UpdatePoolStrategy: %v", err)
	}
	if got := GetPoolStrategy(); got != "least-request" {
		t.Fatalf("after update expected least-request, got %q", got)
	}
	// Switching back to swr fully opts out of the adaptive path.
	if err := UpdatePoolStrategy("swr"); err != nil {
		t.Fatalf("UpdatePoolStrategy(swr): %v", err)
	}
	if got := GetPoolStrategy(); got != "swr" {
		t.Fatalf("after update expected swr, got %q", got)
	}
}
