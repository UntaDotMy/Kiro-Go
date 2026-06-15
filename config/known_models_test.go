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

// TestKnownModelEffortRoundTrip verifies the per-model effort-level map persists
// and reads back intact, and that GetKnownModelEffort returns a deep copy.
func TestKnownModelEffortRoundTrip(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{})

	if got := GetKnownModelEffort(); got != nil {
		t.Fatalf("fresh config should have no effort map, got %v", got)
	}

	in := map[string][]string{
		"claude-opus-4.7":   {"low", "medium", "high", "xhigh", "max"},
		"claude-sonnet-4.6": {"low", "medium", "high", "max"},
	}
	if err := SetKnownModelEffort(in); err != nil {
		t.Fatalf("SetKnownModelEffort: %v", err)
	}
	got := GetKnownModelEffort()
	if len(got) != 2 || len(got["claude-opus-4.7"]) != 5 || got["claude-sonnet-4.6"][3] != "max" {
		t.Fatalf("effort map did not round-trip, got %v", got)
	}

	// Deep copy: mutating the returned map/slice must not affect stored state.
	got["claude-opus-4.7"][0] = "mutated"
	again := GetKnownModelEffort()
	if again["claude-opus-4.7"][0] != "low" {
		t.Fatalf("GetKnownModelEffort must return a deep copy; config was mutated to %v", again)
	}
}

// TestSetKnownModelEffortIdentityIsNoOp ensures re-persisting an identical map
// doesn't churn the config (refreshModelsCache calls it on every fetch).
func TestSetKnownModelEffortIdentityIsNoOp(t *testing.T) {
	withTestAPIKeyConfig(t, &Config{})
	in := map[string][]string{"claude-opus-4.7": {"low", "high", "max"}}
	if err := SetKnownModelEffort(in); err != nil {
		t.Fatalf("seed SetKnownModelEffort: %v", err)
	}
	// An equal-but-distinct map should be detected as identical (no error path
	// to assert directly, but it must not corrupt the stored value).
	if err := SetKnownModelEffort(map[string][]string{"claude-opus-4.7": {"low", "high", "max"}}); err != nil {
		t.Fatalf("identical SetKnownModelEffort: %v", err)
	}
	if got := GetKnownModelEffort(); len(got["claude-opus-4.7"]) != 3 {
		t.Fatalf("identical re-set must preserve the map, got %v", got)
	}
}

// TestSameModelEffortOrderInsensitive guards m35: the level order carries no
// meaning (resolveModelEffort ranks via effortRank, not position), so the same
// set of levels in a different order must compare equal — otherwise an upstream
// schema reordering churns config.json with a spurious fsync on every refresh.
func TestSameModelEffortOrderInsensitive(t *testing.T) {
	a := map[string][]string{"claude-opus-4.7": {"low", "medium", "high", "xhigh", "max"}}
	b := map[string][]string{"claude-opus-4.7": {"max", "high", "low", "xhigh", "medium"}}
	if !sameModelEffort(a, b) {
		t.Errorf("same level set in different order must compare equal")
	}

	// A genuinely different set (one level swapped) must still register as a change.
	c := map[string][]string{"claude-opus-4.7": {"low", "medium", "high", "xhigh", "ultra"}}
	if sameModelEffort(a, c) {
		t.Errorf("different level set must NOT compare equal")
	}
	// Duplicate-vs-distinct multiset of equal length must not false-match.
	d := map[string][]string{"m": {"low", "low", "high"}}
	e := map[string][]string{"m": {"low", "high", "high"}}
	if sameModelEffort(d, e) {
		t.Errorf("differing multisets of equal length must NOT compare equal")
	}
}
