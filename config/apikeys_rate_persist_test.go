package config

import (
	"encoding/json"
	"testing"
)

// Story s10: per-minute / per-hour rate counters must survive a process restart.
// The bug: MinuteBucketKey/HourBucketKey were unexported (not JSON-serialized),
// so after a restart they were empty, the rollover check fired, and the persisted
// MinuteRequests/HourRequests were zeroed — letting a restart loop bypass the
// short-window limit. The fix makes the bucket keys exported JSON fields.

// TestBucketKeysAreSerialized confirms the bucket keys round-trip through JSON
// (the persistence mechanism) so they aren't lost on restart.
func TestBucketKeysAreSerialized(t *testing.T) {
	k := APIKey{
		ID:              "key-1",
		MinuteReqLimit:  10,
		HourReqLimit:    100,
		MinuteBucketKey: "200601021504",
		HourBucketKey:   "2006010215",
		MinuteRequests:  7,
		HourRequests:    42,
	}
	raw, err := json.Marshal(k)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The bucket keys MUST appear in the serialized form.
	var asMap map[string]interface{}
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if _, ok := asMap["minuteBucketKey"]; !ok {
		t.Error("minuteBucketKey must be serialized (was unexported before s10)")
	}
	if _, ok := asMap["hourBucketKey"]; !ok {
		t.Error("hourBucketKey must be serialized (was unexported before s10)")
	}

	var restored APIKey
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if restored.MinuteBucketKey != k.MinuteBucketKey || restored.HourBucketKey != k.HourBucketKey {
		t.Fatalf("bucket keys lost across JSON round-trip: got minute=%q hour=%q",
			restored.MinuteBucketKey, restored.HourBucketKey)
	}
	if restored.MinuteRequests != 7 || restored.HourRequests != 42 {
		t.Fatalf("counters lost across round-trip: minute=%d hour=%d",
			restored.MinuteRequests, restored.HourRequests)
	}
}

// TestPerMinuteLimitSurvivesRestart is the end-to-end regression: consume up to
// the per-minute limit, simulate a restart (JSON round-trip of the whole config
// into a fresh in-memory cfg, exactly as a real reload does), and confirm the
// next pre-flight is STILL rejected within the same window — not bypassed.
func TestPerMinuteLimitSurvivesRestart(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{
		APIKeys: []APIKey{{
			ID:             "key-1",
			Enabled:        true,
			MinuteReqLimit: 2,
			ResetTZ:        "UTC",
		}},
	})

	// Consume the 2 allowed requests in this minute.
	for i := 0; i < 2; i++ {
		if rejected, reason := CheckAPIKeyLimit("key-1", ""); rejected {
			t.Fatalf("request %d should be allowed, got rejected: %s", i, reason)
		}
		ConsumeAPIKey("key-1", 1, 0, "")
	}

	// The 3rd request in the same minute must be rejected.
	if rejected, reason := CheckAPIKeyLimit("key-1", ""); !rejected {
		t.Fatalf("3rd request in the same minute must be rejected, got allowed")
	} else if reason != "per-minute rate limit reached" {
		t.Fatalf("unexpected reason: %q", reason)
	}

	// Simulate a process restart: marshal the live config and reload it into a
	// fresh cfg, exactly the path a real restart takes (load config.json).
	keysBefore := GetAPIKeys()
	raw, err := json.Marshal(&Config{APIKeys: keysBefore})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	var reloaded Config
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	withTestAPIKeyConfig(t, &reloaded)

	// After the "restart", the per-minute limit must STILL be in force for the
	// current window — the bug let this bypass because the bucket key reset.
	if rejected, reason := CheckAPIKeyLimit("key-1", ""); !rejected {
		t.Fatalf("per-minute limit was BYPASSED after restart — counters reset (the s10 bug). reason=%q", reason)
	} else if reason != "per-minute rate limit reached" {
		t.Fatalf("unexpected reason after restart: %q", reason)
	}
}
