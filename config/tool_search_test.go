package config

import "testing"

// TestGetToolSearchEnabledDefaultsOn verifies the default-on semantics: a nil
// pointer (fresh install / config predating the field) reports true, and an
// explicit false opts out.
func TestGetToolSearchEnabledDefaultsOn(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{}) // ToolSearchEnabled is nil

	if !GetToolSearchEnabled() {
		t.Fatal("nil ToolSearchEnabled must default to ON")
	}

	if err := UpdateToolSearchEnabled(false); err != nil {
		t.Fatalf("UpdateToolSearchEnabled(false): %v", err)
	}
	if GetToolSearchEnabled() {
		t.Fatal("explicit false must opt out")
	}

	if err := UpdateToolSearchEnabled(true); err != nil {
		t.Fatalf("UpdateToolSearchEnabled(true): %v", err)
	}
	if !GetToolSearchEnabled() {
		t.Fatal("explicit true must enable")
	}
}
