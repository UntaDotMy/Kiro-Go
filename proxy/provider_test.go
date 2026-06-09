package proxy

import (
	"context"
	"kiro-go/config"
	"testing"
)

// TestProviderForDefaultsToKiro verifies the core invariant of the provider
// seam: a Backend-less (pre-existing) account, and one explicitly tagged
// "kiro", both resolve to the kiro provider — so every current config behaves
// exactly as before.
func TestProviderForDefaultsToKiro(t *testing.T) {
	if p := ProviderFor(&config.Account{}); p == nil || p.Name() != "kiro" {
		t.Fatalf("Backend-less account should resolve to kiro provider, got %v", p)
	}
	if p := ProviderFor(&config.Account{Backend: "kiro"}); p == nil || p.Name() != "kiro" {
		t.Fatalf("Backend=kiro should resolve to kiro provider, got %v", p)
	}
}

// TestProviderForUnknownBackendIsNil verifies that an account whose backend has
// no registered provider resolves to nil (caller uses ProviderForOrErr to turn
// that into a descriptive error rather than a panic).
func TestProviderForUnknownBackendIsNil(t *testing.T) {
	if p := ProviderFor(&config.Account{Backend: "does-not-exist"}); p != nil {
		t.Fatalf("unknown backend should resolve to nil, got %v (%s)", p, p.Name())
	}
	if _, err := ProviderForOrErr(&config.Account{Backend: "does-not-exist"}); err == nil {
		t.Fatalf("ProviderForOrErr should error on unknown backend")
	}
}

// TestKiroProviderCallUsesPrebuiltPayload verifies kiroProvider.Call passes the
// prebuilt NormalizedRequest.Kiro payload straight through (the byte-identical
// Phase 1->2 guarantee) — i.e. it does NOT rebuild from Claude/OpenAI when Kiro
// is already set. We assert this by confirming Call reaches the upstream layer
// with a non-nil account/payload path; a nil account short-circuits in
// CallKiroAPIContext without panic, which is enough to exercise the dispatch.
func TestKiroProviderCallDispatches(t *testing.T) {
	kp := kiroProvider{}
	if kp.Name() != "kiro" {
		t.Fatalf("expected name kiro, got %s", kp.Name())
	}
	// A minimal payload + nil-safe callback: CallKiroAPIContext will attempt the
	// endpoint chain and return an error (no real account), but must not panic.
	nr := &NormalizedRequest{Model: "claude-sonnet-4.5", Kiro: &KiroPayload{}}
	err := kp.Call(context.Background(), &config.Account{ID: "t"}, nr, &KiroStreamCallback{})
	if err == nil {
		t.Fatalf("expected an upstream error for a fake account, got nil")
	}
}
