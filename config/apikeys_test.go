package config

import (
	"path/filepath"
	"testing"
)

func withTestAPIKeyConfig(t *testing.T, testConfig *Config) {
	t.Helper()

	cfgLock.Lock()
	previousConfig := cfg
	previousPath := cfgPath
	cfg = testConfig
	cfgPath = filepath.Join(t.TempDir(), "config.json")
	cfgLock.Unlock()

	t.Cleanup(func() {
		cfgLock.Lock()
		cfg = previousConfig
		cfgPath = previousPath
		cfgLock.Unlock()
	})
}

func TestCheckAPIKeyLimitAcceptsClaudeModelAliases(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{
		APIKeys: []APIKey{{
			ID:      "key-1",
			Enabled: true,
			Models:  []string{"claude-opus-4.7"},
		}},
	})

	rejected, reason := CheckAPIKeyLimit("key-1", "claude-opus-4-7")
	if rejected || reason != "" {
		t.Fatalf("expected dashed picker id to match dotted whitelist, got rejected=%v reason=%q", rejected, reason)
	}

	rejected, reason = ConsumeAPIKey("key-1", 1, 0.5, "claude-opus-4-7")
	if rejected || reason != "" {
		t.Fatalf("expected consume path to accept dashed picker id, got rejected=%v reason=%q", rejected, reason)
	}
}

func TestCheckAPIKeyLimitRejectsUnsupportedModel(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{
		APIKeys: []APIKey{{
			ID:      "key-1",
			Enabled: true,
			Models:  []string{"claude-sonnet-4.5"},
		}},
	})

	rejected, reason := CheckAPIKeyLimit("key-1", "claude-opus-4.7")
	if !rejected {
		t.Fatalf("expected unsupported model to be rejected")
	}
	if reason == "" {
		t.Fatalf("expected rejection reason for unsupported model")
	}
}

// TestCheckAPIKeyLimitRejectsEmptyModelWhenWhitelistExists locks in the
// empty-model bypass FIX. A key with a model whitelist must NOT be served an
// empty model on the inference gate — an empty model previously short-circuited
// the `model != ""` whitelist guard, letting a restricted key reach any model.
// The model-agnostic rate gate (CheckAPIKeyRateLimit, used by /v1/models and
// /v1/stats) still allows it, since those routes don't invoke a model.
func TestCheckAPIKeyLimitRejectsEmptyModelWhenWhitelistExists(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{
		APIKeys: []APIKey{{
			ID:      "key-1",
			Enabled: true,
			Models:  []string{"claude-opus-4.7"},
		}},
	})

	// Inference gate: empty model + whitelist => REJECT (this is the fix).
	rejected, reason := CheckAPIKeyLimit("key-1", "")
	if !rejected {
		t.Fatalf("expected empty model to be rejected for a whitelisted key, got rejected=%v reason=%q", rejected, reason)
	}

	// Metadata gate: empty model is fine — it doesn't invoke a model.
	rejected, reason = CheckAPIKeyRateLimit("key-1")
	if rejected {
		t.Fatalf("rate-only gate must allow a whitelisted key with no model, got rejected=%v reason=%q", rejected, reason)
	}
}

// TestCheckAPIKeyRateLimitStillEnforcesNonModelLimits verifies the metadata
// gate skips ONLY the model dimension — disable/expiry/rate/quota still apply.
func TestCheckAPIKeyRateLimitStillEnforcesNonModelLimits(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{
		APIKeys: []APIKey{{
			ID:      "key-1",
			Enabled: false, // disabled key must be rejected even on metadata routes
			Models:  []string{"claude-opus-4.7"},
		}},
	})
	if rejected, _ := CheckAPIKeyRateLimit("key-1"); !rejected {
		t.Fatal("disabled key must be rejected by the rate-only gate")
	}
}

// TestGatesRejectUnknownKeyID locks in the deleted-key fix: a key that was
// matched at auth time but DELETED before the gate runs must be rejected, not
// treated as unrestricted. Both the inference gate and the rate gate must fail
// closed for an id that isn't present.
func TestGatesRejectUnknownKeyID(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{
		APIKeys: []APIKey{{ID: "key-1", Enabled: true}},
	})

	if rejected, _ := CheckAPIKeyLimit("deleted-id", "claude-opus-4.7"); !rejected {
		t.Fatal("CheckAPIKeyLimit must fail closed for an unknown/deleted key id")
	}
	if rejected, _ := CheckAPIKeyRateLimit("deleted-id"); !rejected {
		t.Fatal("CheckAPIKeyRateLimit must fail closed for an unknown/deleted key id")
	}
	// The real key must still pass.
	if rejected, _ := CheckAPIKeyRateLimit("key-1"); rejected {
		t.Fatal("a present, enabled key must still pass the rate gate")
	}
}
