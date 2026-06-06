package config

import "testing"

// TestGetToolSearchEnabledDefaultsOff verifies the default-off semantics: a nil
// pointer (fresh install / config predating an explicit toggle) reports false,
// and an explicit true opts in. The emulation defaults off because it depends on
// the upstream model reliably calling a synthetic search tool to discover its
// deferred tools — an unreliable indirection that otherwise makes the model
// narrate an action then end the turn without any tool_use.
func TestGetToolSearchEnabledDefaultsOff(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{}) // ToolSearchEnabled is nil

	if GetToolSearchEnabled() {
		t.Fatal("nil ToolSearchEnabled must default to OFF")
	}

	if err := UpdateToolSearchEnabled(true); err != nil {
		t.Fatalf("UpdateToolSearchEnabled(true): %v", err)
	}
	if !GetToolSearchEnabled() {
		t.Fatal("explicit true must enable")
	}

	if err := UpdateToolSearchEnabled(false); err != nil {
		t.Fatalf("UpdateToolSearchEnabled(false): %v", err)
	}
	if GetToolSearchEnabled() {
		t.Fatal("explicit false must opt out")
	}
}
