package proxy

import (
	"fmt"
	"kiro-go/config"
	"strings"
	"testing"
)

// TestKiroEndpointsForRegionRuntimeOnly pins the runtime-only inference chain:
// every region resolves to exactly ONE endpoint, the modern
// runtime.<region>.kiro.dev host carrying the GenerateAssistantResponse target.
// The legacy q.* / codewhisperer.* / AmazonQ hosts were removed (codewhisperer.*
// carried the identical target as runtime, so it was redundant; q.* and AmazonQ
// were deprecated legacy). No fallback chain.
func TestKiroEndpointsForRegionRuntimeOnly(t *testing.T) {
	prev := kiroEndpointsOverride
	kiroEndpointsOverride = nil
	t.Cleanup(func() { kiroEndpointsOverride = prev })

	for _, region := range []string{"us-east-1", "eu-west-1", "ap-northeast-1"} {
		endpoints := kiroEndpointsForRegion(region)
		if len(endpoints) != 1 {
			t.Fatalf("[%s] expected exactly 1 runtime endpoint (no fallback), got %d", region, len(endpoints))
		}
		ep := endpoints[0]
		if !strings.Contains(ep.URL, fmt.Sprintf("runtime.%s.kiro.dev", region)) {
			t.Errorf("[%s] endpoint must be the runtime host, got %s", region, ep.URL)
		}
		if ep.AmzTarget != "AmazonCodeWhispererStreamingService.GenerateAssistantResponse" {
			t.Errorf("[%s] runtime endpoint must carry the GenerateAssistantResponse target, got %q", region, ep.AmzTarget)
		}
		// No legacy hosts may appear.
		if strings.Contains(ep.URL, "amazonaws.com") {
			t.Errorf("[%s] legacy amazonaws.com host must be gone from inference, got %s", region, ep.URL)
		}
	}
}

// TestKiroRESTBaseSwitchesByRegion confirms the account-MANAGEMENT REST base is
// UNCHANGED by the runtime-only inference move: us-east-1 still uses the LEGACY
// codewhisperer.us-east-1.amazonaws.com host (the host that actually serves
// /getUsageLimits, /GetUserInfo, /ListAvailableModels, /ListAvailableProfiles);
// other regions fall back to runtime.<region>.kiro.dev because codewhisperer.* is
// NXDOMAIN outside us-east-1. This is deliberately NOT the runtime host for
// us-east-1 — pointing the management paths there breaks account refresh (the
// runtime host only serves the inference action). "Refresh uses the legacy one,
// requests use runtime."
func TestKiroRESTBaseSwitchesByRegion(t *testing.T) {
	if got := kiroRESTBaseForRegion("us-east-1"); !strings.Contains(got, "codewhisperer.us-east-1.amazonaws.com") {
		t.Errorf("us-east-1 REST base must use codewhisperer.us-east-1.amazonaws.com (runtime host doesn't serve management paths): %s", got)
	}
	if got := kiroRESTBaseForRegion("ap-northeast-1"); !strings.Contains(got, "runtime.ap-northeast-1.kiro.dev") {
		t.Errorf("non-us-east-1 REST base must use runtime.<region>.kiro.dev: %s", got)
	}
	if got := kiroRESTBaseForRegion(""); !strings.Contains(got, "codewhisperer.us-east-1.amazonaws.com") {
		t.Errorf("empty region should default to the us-east-1 management host: %s", got)
	}
}

// TestGetSortedEndpointsRuntimeOnly verifies the per-account endpoint resolution:
// one account resolves to exactly its pinned region's single runtime endpoint
// (no preferred/fallback ordering — that machinery was removed with the legacy
// hosts). Cross-region spreading still happens across ACCOUNTS via
// resolveAccountRegion, never within one identity.
func TestGetSortedEndpointsRuntimeOnly(t *testing.T) {
	prev := kiroEndpointsOverride
	kiroEndpointsOverride = nil
	t.Cleanup(func() { kiroEndpointsOverride = prev })
	t.Setenv("KIRO_API_REGIONS", "us-east-1,us-west-2,eu-west-1,eu-central-1,ap-northeast-1")

	acct := &config.Account{ID: "acct-runtime-only"}
	chain := getSortedEndpoints(acct)
	if len(chain) != 1 {
		t.Fatalf("expected exactly one runtime endpoint per account, got %d", len(chain))
	}
	region := resolveAccountRegion(acct)
	if !strings.Contains(chain[0].URL, fmt.Sprintf("runtime.%s.kiro.dev", region)) {
		t.Errorf("account endpoint must be the runtime host for its pinned region %s, got %s", region, chain[0].URL)
	}
}

// TestResolveAccountRegionStableAndSpreads verifies the two core
// properties of per-account region pinning:
//  1. STABLE — the same account always resolves to the same region, so an
//     identity never hops regions request-to-request (no impossible-travel
//     signal).
//  2. SPREAD — different accounts deterministically land on different
//     regions across the configured list, so pool traffic is distributed
//     over regional rate buckets without any single identity moving.
func TestResolveAccountRegionStableAndSpreads(t *testing.T) {
	t.Setenv("KIRO_API_REGIONS", "us-east-1,eu-west-1,ap-northeast-1")

	// Stability: many resolutions of the same id must be identical.
	first := resolveAccountRegion(&config.Account{ID: "stable-acct"})
	for i := 0; i < 50; i++ {
		if got := resolveAccountRegion(&config.Account{ID: "stable-acct"}); got != first {
			t.Fatalf("region must be stable per account; got %q then %q", first, got)
		}
	}

	// Spread: across many distinct ids we must touch more than one region.
	seen := map[string]bool{}
	for i := 0; i < 60; i++ {
		seen[resolveAccountRegion(&config.Account{ID: fmt.Sprintf("acct-%d", i)})] = true
	}
	if len(seen) < 2 {
		t.Fatalf("expected accounts to spread across multiple regions, only saw %v", seen)
	}
	for r := range seen {
		if r != "us-east-1" && r != "eu-west-1" && r != "ap-northeast-1" {
			t.Fatalf("resolved region %q outside the configured list", r)
		}
	}
}

// TestResolveAccountRegionFallback confirms the safe fallbacks: with no
// KIRO_API_REGIONS configured, or a nil/empty-id account, resolution
// returns the single global region (never panics, never empty).
func TestResolveAccountRegionFallback(t *testing.T) {
	t.Setenv("KIRO_API_REGIONS", "")

	if got := resolveAccountRegion(&config.Account{ID: "any"}); got == "" {
		t.Fatal("expected a non-empty global region when no list is configured")
	}
	if got := resolveAccountRegion(nil); got == "" {
		t.Fatal("nil account must fall back to the global region, not empty")
	}
	if got := resolveAccountRegion(&config.Account{ID: ""}); got == "" {
		t.Fatal("empty-id account must fall back to the global region, not empty")
	}
}
