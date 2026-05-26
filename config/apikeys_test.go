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

func TestCheckAPIKeyLimitAllowsEmptyModelWhenWhitelistExists(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{
		APIKeys: []APIKey{{
			ID:      "key-1",
			Enabled: true,
			Models:  []string{"claude-opus-4.7"},
		}},
	})

	rejected, reason := CheckAPIKeyLimit("key-1", "")
	if rejected || reason != "" {
		t.Fatalf("expected empty model to bypass whitelist, got rejected=%v reason=%q", rejected, reason)
	}
}
