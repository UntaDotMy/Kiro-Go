package config

import "testing"

// TestGetPoolConcurrencyDefaults verifies that an unset (0) initial/max resolves
// to the built-in defaults (12/48), both on an initialized config and on the
// cfg==nil pre-Init path that unit tests hit.
func TestGetPoolConcurrencyDefaults(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{}) // both fields 0
	if got := GetPoolInitialConcurrency(); got != defaultPoolInitialConcurrency {
		t.Fatalf("unset initial concurrency should default to %d, got %d", defaultPoolInitialConcurrency, got)
	}
	if got := GetPoolMaxConcurrency(); got != defaultPoolMaxConcurrency {
		t.Fatalf("unset max concurrency should default to %d, got %d", defaultPoolMaxConcurrency, got)
	}
}

// TestGetPoolConcurrencyClamps verifies out-of-range stored values are clamped
// on read so a bad persisted value can never strand or stampede the limiter.
func TestGetPoolConcurrencyClamps(t *testing.T) {
	// Below the floor clamps up to minPoolConcurrency.
	withTestAPIKeyConfig(t, &Config{PoolInitialConcurrency: -5, PoolMaxConcurrency: -5})
	if got := GetPoolInitialConcurrency(); got != minPoolConcurrency {
		t.Fatalf("negative initial should clamp to %d, got %d", minPoolConcurrency, got)
	}

	// Above the ceiling clamps down to maxPoolConcurrency.
	withTestAPIKeyConfig(t, &Config{PoolInitialConcurrency: 9999, PoolMaxConcurrency: 9999})
	if got := GetPoolInitialConcurrency(); got != maxPoolConcurrency {
		t.Fatalf("oversized initial should clamp to %d, got %d", maxPoolConcurrency, got)
	}
	if got := GetPoolMaxConcurrency(); got != maxPoolConcurrency {
		t.Fatalf("oversized max should clamp to %d, got %d", maxPoolConcurrency, got)
	}
}

// TestGetPoolMaxConcurrencyFlooredToInitial verifies the cross-field guarantee:
// a max below the resolved initial is floored UP to initial so the ramp can
// never start above its own ceiling.
func TestGetPoolMaxConcurrencyFlooredToInitial(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{PoolInitialConcurrency: 20, PoolMaxConcurrency: 5})
	if got := GetPoolMaxConcurrency(); got != 20 {
		t.Fatalf("max (5) below initial (20) should floor to initial 20, got %d", got)
	}
}

// TestUpdatePoolConcurrencyValidation pins the setter's validation: in-range
// values persist, an inverted range (max < initial, both non-zero) is rejected,
// out-of-bounds values are rejected, and 0 is accepted as "reset to default".
func TestUpdatePoolConcurrencyValidation(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{})

	// Valid pair persists and reads back.
	if err := UpdatePoolConcurrency(8, 32); err != nil {
		t.Fatalf("valid pair should persist, got %v", err)
	}
	if got := GetPoolInitialConcurrency(); got != 8 {
		t.Fatalf("expected initial 8, got %d", got)
	}
	if got := GetPoolMaxConcurrency(); got != 32 {
		t.Fatalf("expected max 32, got %d", got)
	}

	// Inverted range is rejected.
	if err := UpdatePoolConcurrency(20, 10); err == nil {
		t.Fatal("max < initial should be rejected")
	}

	// Out-of-bounds is rejected.
	if err := UpdatePoolConcurrency(0, maxPoolConcurrency+1); err == nil {
		t.Fatal("max beyond the ceiling should be rejected")
	}
	if err := UpdatePoolConcurrency(-1, 0); err == nil {
		t.Fatal("negative initial should be rejected")
	}

	// 0 / 0 resets to defaults.
	if err := UpdatePoolConcurrency(0, 0); err != nil {
		t.Fatalf("0/0 reset should be accepted, got %v", err)
	}
	if got := GetPoolInitialConcurrency(); got != defaultPoolInitialConcurrency {
		t.Fatalf("after reset expected default %d, got %d", defaultPoolInitialConcurrency, got)
	}
}
