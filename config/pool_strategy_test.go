package config

import "testing"

// TestGetPoolStrategyDefaultsToFast verifies the production default:
// an initialized config with an empty PoolStrategy resolves to "fast"
// (the 9router-style low-latency default), NOT "least-request" or "swr".
// The cfg==nil test-only fallback ("swr") is a separate path.
func TestGetPoolStrategyDefaultsToFast(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{}) // PoolStrategy == ""
	if got := GetPoolStrategy(); got != "fast" {
		t.Fatalf("empty PoolStrategy on an initialized config should default to fast, got %q", got)
	}
}

// TestGetPoolStrategyNormalization pins the alias map: every recognized spelling
// of each strategy normalizes to its canonical form, and an unrecognized value
// falls back to the fast default.
func TestGetPoolStrategyNormalization(t *testing.T) {
	cases := map[string]string{
		// fast and its aliases.
		"fast":       "fast",
		"fill-first": "fast",
		"fillfirst":  "fast",
		"fill_first": "fast",
		"9router":    "fast",
		"FAST":       "fast",
		"  fast  ":   "fast",
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
		// unknown -> default (fast).
		"round-robin": "fast",
		"garbage":     "fast",
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

// TestGetPoolStickyLimitDefaultAndClamp verifies the "fast" strategy sticky cap:
// unset resolves to the default (3), valid values pass through, and the value is
// clamped to [1, 1000].
func TestGetPoolStickyLimitDefaultAndClamp(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{}) // unset
	if got := GetPoolStickyLimit(); got != defaultPoolStickyLimit {
		t.Fatalf("unset sticky limit should default to %d, got %d", defaultPoolStickyLimit, got)
	}
	withTestAPIKeyConfig(t, &Config{PoolStickyLimit: 5})
	if got := GetPoolStickyLimit(); got != 5 {
		t.Fatalf("sticky limit 5 should pass through, got %d", got)
	}
	withTestAPIKeyConfig(t, &Config{PoolStickyLimit: 1})
	if got := GetPoolStickyLimit(); got != 1 {
		t.Fatalf("sticky limit 1 (stickiness off) should pass through, got %d", got)
	}
	withTestAPIKeyConfig(t, &Config{PoolStickyLimit: 99999})
	if got := GetPoolStickyLimit(); got != 1000 {
		t.Fatalf("sticky limit should clamp to 1000, got %d", got)
	}
}
