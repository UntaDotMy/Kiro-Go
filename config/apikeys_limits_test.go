package config

import (
	"testing"
	"time"
)

// TestConsumeAPIKeyRecordsUsageThatCrossesPeriodicTokenLimit is the regression
// test for the periodic-token-cap bypass bug. The OLD ConsumeAPIKey returned
// WITHOUT recording usage whenever a request would push DailyTokens over the
// limit, so the counter froze just below the threshold and CheckAPIKeyLimit's
// `DailyTokens >= limit` gate never tripped — the key served forever. The fix
// records actual usage unconditionally so the crossing is persisted and the
// NEXT pre-flight check rejects.
func TestConsumeAPIKeyRecordsUsageThatCrossesPeriodicTokenLimit(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{
		APIKeys: []APIKey{{
			ID:            "key-1",
			Enabled:       true,
			DailyTokLimit: 1000,
			ResetPeriod:   "daily",
			ResetTZ:       "UTC",
		}},
	})

	// First request uses 600 tokens — under the 1000 cap, so pre-flight allows.
	if rejected, reason := CheckAPIKeyLimit("key-1", ""); rejected {
		t.Fatalf("first pre-flight should allow, got rejected=%v reason=%q", rejected, reason)
	}
	ConsumeAPIKey("key-1", 600, 0, "")

	// Second request uses 600 tokens — this CROSSES the cap (600+600=1200).
	// Pre-flight still allows because 600 < 1000 (it can't know the size yet).
	if rejected, _ := CheckAPIKeyLimit("key-1", ""); rejected {
		t.Fatalf("second pre-flight should still allow (600 < 1000)")
	}
	ConsumeAPIKey("key-1", 600, 0, "")

	// The crossing MUST have been recorded → DailyTokens == 1200.
	keys := GetAPIKeys()
	if keys[0].DailyTokens != 1200 {
		t.Fatalf("expected DailyTokens=1200 (crossing recorded), got %d — the bypass bug is back", keys[0].DailyTokens)
	}

	// Now the THIRD request must be rejected by pre-flight: 1200 >= 1000.
	rejected, reason := CheckAPIKeyLimit("key-1", "")
	if !rejected {
		t.Fatalf("third pre-flight MUST reject once DailyTokens >= limit, but it allowed (DailyTokens=%d)", keys[0].DailyTokens)
	}
	if reason != "periodic token limit reached" {
		t.Fatalf("unexpected reason: %q", reason)
	}
}

// TestConsumeAPIKeyRecordsCreditsThatCrossPeriodicLimit mirrors the token test
// for the periodic credit cap.
func TestConsumeAPIKeyRecordsCreditsThatCrossPeriodicLimit(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{
		APIKeys: []APIKey{{
			ID:             "key-1",
			Enabled:        true,
			DailyCredLimit: 10,
			ResetPeriod:    "daily",
			ResetTZ:        "UTC",
		}},
	})

	ConsumeAPIKey("key-1", 0, 6, "") // 6 credits
	ConsumeAPIKey("key-1", 0, 6, "") // crosses 10 → recorded as 12

	if keys := GetAPIKeys(); keys[0].DailyCredits != 12 {
		t.Fatalf("expected DailyCredits=12 (crossing recorded), got %v", keys[0].DailyCredits)
	}
	if rejected, reason := CheckAPIKeyLimit("key-1", ""); !rejected || reason != "periodic credit limit reached" {
		t.Fatalf("expected periodic credit rejection, got rejected=%v reason=%q", rejected, reason)
	}
}

// TestConsumeAPIKeyPeriodicLimitResetsOnNewBucket confirms a periodic token cap
// is NOT permanent: when the period rolls over, the counter resets and the key
// serves again. The key must remain Enabled throughout (periodic caps never
// disable the key — only lifetime caps and expiry do).
func TestConsumeAPIKeyPeriodicLimitResetsOnNewBucket(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{
		APIKeys: []APIKey{{
			ID:            "key-1",
			Enabled:       true,
			DailyTokLimit: 1000,
			ResetPeriod:   "daily",
			ResetTZ:       "UTC",
			CountersDate:  "1999-01-01", // stale bucket → forces rollover on next use
			DailyTokens:   9999,         // pretend we were way over yesterday
		}},
	})

	// A new request rolls the bucket over, resets DailyTokens to 0, then records.
	ConsumeAPIKey("key-1", 200, 0, "")
	keys := GetAPIKeys()
	if keys[0].DailyTokens != 200 {
		t.Fatalf("expected DailyTokens reset to 200 after period rollover, got %d", keys[0].DailyTokens)
	}
	if !keys[0].Enabled {
		t.Fatal("periodic cap must never disable the key")
	}
	if rejected, _ := CheckAPIKeyLimit("key-1", ""); rejected {
		t.Fatal("after rollover and 200 tokens, key should serve again")
	}
}

// TestConsumeAPIKeyLifetimeTokenLimitDisables confirms lifetime caps DO disable
// the key permanently (unlike periodic caps).
func TestConsumeAPIKeyLifetimeTokenLimitDisables(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{
		APIKeys: []APIKey{{
			ID:               "key-1",
			Enabled:          true,
			LifetimeTokLimit: 1000,
		}},
	})

	ConsumeAPIKey("key-1", 600, 0, "")
	rejected, reason := ConsumeAPIKey("key-1", 600, 0, "") // crosses lifetime cap
	if !rejected {
		t.Fatal("expected lifetime token cap to report rejection")
	}
	if reason != "lifetime token limit reached (key disabled)" {
		t.Fatalf("unexpected reason: %q", reason)
	}
	keys := GetAPIKeys()
	if keys[0].Enabled {
		t.Fatal("lifetime cap MUST disable the key")
	}
	if keys[0].TotalTokens != 1200 {
		t.Fatalf("expected TotalTokens=1200 recorded, got %d", keys[0].TotalTokens)
	}
	// Pre-flight now rejects on "key disabled".
	if rejected, reason := CheckAPIKeyLimit("key-1", ""); !rejected || reason != "key disabled" {
		t.Fatalf("expected 'key disabled' pre-flight rejection, got rejected=%v reason=%q", rejected, reason)
	}
}

// TestConsumeAPIKeyAbsoluteExpiryDisables confirms a key past its absolute
// expiry is disabled when used, and pre-flight then rejects.
func TestConsumeAPIKeyAbsoluteExpiryDisables(t *testing.T) {
	past := time.Now().Add(-time.Hour).Unix()
	withTestAPIKeyConfig(t, &Config{
		APIKeys: []APIKey{{
			ID:        "key-1",
			Enabled:   true,
			ExpiresAt: past,
		}},
	})

	// Pre-flight already rejects an expired key (and disables it).
	if rejected, reason := CheckAPIKeyLimit("key-1", ""); !rejected || reason != "key expired" {
		t.Fatalf("expected pre-flight 'key expired', got rejected=%v reason=%q", rejected, reason)
	}
	if GetAPIKeys()[0].Enabled {
		t.Fatal("expired key must be disabled by pre-flight")
	}
}

// TestConsumeAPIKeyLazyExpiryDisables confirms lazy expiry (countdown from
// first use) disables the key once the window passes.
func TestConsumeAPIKeyLazyExpiryDisables(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{
		APIKeys: []APIKey{{
			ID:                "key-1",
			Enabled:           true,
			LazyExpirySeconds: 3600,
			FirstUsedAt:       time.Now().Add(-2 * time.Hour).Unix(), // used 2h ago, 1h window
		}},
	})

	if rejected, reason := CheckAPIKeyLimit("key-1", ""); !rejected || reason != "key expired (lazy)" {
		t.Fatalf("expected pre-flight 'key expired (lazy)', got rejected=%v reason=%q", rejected, reason)
	}
	if GetAPIKeys()[0].Enabled {
		t.Fatal("lazily-expired key must be disabled")
	}
}

// TestConsumeAPIKeyDisabledKeyStillRecords is a subtle invariant: even if a key
// got disabled between pre-flight and consume (e.g. a concurrent lifetime trip),
// the usage that already happened is still recorded for accurate accounting.
func TestConsumeAPIKeyDisabledKeyStillRecords(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{
		APIKeys: []APIKey{{
			ID:          "key-1",
			Enabled:     false, // already disabled
			DailyTokens: 100,
			ResetPeriod: "daily",
			ResetTZ:     "UTC",
			// Pin the current bucket so the rollover logic doesn't reset the
			// 100 we seeded — we're testing recording, not rollover.
			CountersDate: periodBucketKey("daily", "UTC"),
		}},
	})
	ConsumeAPIKey("key-1", 50, 0, "")
	if got := GetAPIKeys()[0].DailyTokens; got != 150 {
		t.Fatalf("usage on an already-completed request must still be recorded, got DailyTokens=%d want 150", got)
	}
}
