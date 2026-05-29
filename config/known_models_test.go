package config

import (
	"testing"
)

// TestKnownModelsRoundTrip verifies persistence of the last-known-good model
// catalog: SetKnownModels stores a deduped list, GetKnownModels returns a copy.
func TestKnownModelsRoundTrip(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{})

	if got := GetKnownModels(); got != nil {
		t.Fatalf("fresh config should have no known models, got %v", got)
	}

	if err := SetKnownModels([]string{"claude-opus-4.7", "claude-sonnet-4.5", "claude-opus-4.7"}); err != nil {
		t.Fatalf("SetKnownModels: %v", err)
	}
	got := GetKnownModels()
	if len(got) != 2 {
		t.Fatalf("expected 2 deduped models, got %v", got)
	}
}

// TestSetKnownModelsEmptyIsNoOp ensures an empty fetch result never clobbers a
// previously-persisted catalog (cold-start fetch failures must not wipe the
// last-known-good list).
func TestSetKnownModelsEmptyIsNoOp(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{})

	if err := SetKnownModels([]string{"claude-opus-4.7"}); err != nil {
		t.Fatalf("seed SetKnownModels: %v", err)
	}
	if err := SetKnownModels(nil); err != nil {
		t.Fatalf("empty SetKnownModels should be a no-op, got err %v", err)
	}
	if err := SetKnownModels([]string{}); err != nil {
		t.Fatalf("empty slice SetKnownModels should be a no-op, got err %v", err)
	}
	if got := GetKnownModels(); len(got) != 1 || got[0] != "claude-opus-4.7" {
		t.Fatalf("empty set must preserve prior catalog, got %v", got)
	}
}

// TestGetKnownModelsReturnsCopy verifies callers can't mutate config state
// through the returned slice.
func TestGetKnownModelsReturnsCopy(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{})
	if err := SetKnownModels([]string{"claude-opus-4.7"}); err != nil {
		t.Fatalf("SetKnownModels: %v", err)
	}
	got := GetKnownModels()
	got[0] = "mutated"
	again := GetKnownModels()
	if again[0] != "claude-opus-4.7" {
		t.Fatalf("GetKnownModels must return a copy; config was mutated to %v", again)
	}
}
