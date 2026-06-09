package proxy

import (
	"fmt"
	"kiro-go/config"
	"strings"
	"testing"
)

// TestKiroEndpointsForRegionUsEast1 pins the us-east-1 endpoint chain. Per
// Kiro's firewall allowlist the modern runtime.<region>.kiro.dev host is the
// PRIMARY; the legacy q.* and codewhisperer.* hosts are kept as fallback (the
// operator asked NOT to drop the legacy endpoint). codewhisperer.* is included
// only in us-east-1 because it returns NXDOMAIN elsewhere (jwadow #58).
func TestKiroEndpointsForRegionUsEast1(t *testing.T) {
	prev := kiroEndpointsOverride
	kiroEndpointsOverride = nil
	t.Cleanup(func() { kiroEndpointsOverride = prev })

	endpoints := kiroEndpointsForRegion("us-east-1")
	if len(endpoints) != 4 {
		t.Fatalf("expected 4 endpoints (runtime primary + 3 legacy), got %d", len(endpoints))
	}
	// The PRIMARY (first) endpoint must be the modern runtime host.
	if !strings.Contains(endpoints[0].URL, "runtime.us-east-1.kiro.dev") {
		t.Errorf("primary endpoint must be runtime.us-east-1.kiro.dev, got %s", endpoints[0].URL)
	}
	hosts := []string{}
	for _, e := range endpoints {
		hosts = append(hosts, e.URL)
	}
	joined := strings.Join(hosts, " | ")
	// Legacy endpoints must still be present (kept as fallback).
	if !strings.Contains(joined, "q.us-east-1.amazonaws.com") {
		t.Errorf("missing legacy q.us-east-1: %s", joined)
	}
	if !strings.Contains(joined, "codewhisperer.us-east-1.amazonaws.com") {
		t.Errorf("missing legacy codewhisperer.us-east-1: %s", joined)
	}
}

// TestKiroEndpointsForRegionEU pins the non-us-east-1 chain: runtime primary,
// legacy q.* kept as fallback, and codewhisperer.* absent (NXDOMAIN outside
// us-east-1).
func TestKiroEndpointsForRegionEU(t *testing.T) {
	prev := kiroEndpointsOverride
	kiroEndpointsOverride = nil
	t.Cleanup(func() { kiroEndpointsOverride = prev })

	endpoints := kiroEndpointsForRegion("eu-west-1")
	if len(endpoints) != 3 {
		t.Fatalf("expected 3 endpoints (runtime primary + 2 legacy), got %d", len(endpoints))
	}
	if !strings.Contains(endpoints[0].URL, "runtime.eu-west-1.kiro.dev") {
		t.Errorf("primary endpoint must be runtime.eu-west-1.kiro.dev, got %s", endpoints[0].URL)
	}
	hosts := []string{}
	for _, e := range endpoints {
		hosts = append(hosts, e.URL)
	}
	joined := strings.Join(hosts, " | ")
	if strings.Contains(joined, "codewhisperer.eu-west-1") {
		t.Errorf("codewhisperer.* must not be used outside us-east-1: %s", joined)
	}
	if !strings.Contains(joined, "runtime.eu-west-1.kiro.dev") {
		t.Errorf("expected runtime.eu-west-1.kiro.dev host: %s", joined)
	}
	if !strings.Contains(joined, "q.eu-west-1.amazonaws.com") {
		t.Errorf("expected legacy q.eu-west-1 fallback: %s", joined)
	}
}

// TestKiroRESTBaseSwitchesByRegion confirms the account-MANAGEMENT REST base:
// us-east-1 uses codewhisperer.us-east-1.amazonaws.com (the host that actually
// serves /getUsageLimits, /GetUserInfo, /ListAvailableModels,
// /ListAvailableProfiles); other regions fall back to runtime.<region>.kiro.dev
// because codewhisperer.* is NXDOMAIN outside us-east-1. This is deliberately
// NOT the runtime host for us-east-1 — pointing the management paths there
// breaks account refresh (the runtime host only serves the inference action).
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

// TestEndpointChainCapped enforces the maxEndpointChain ceiling and the
// per-account region-pinning contract: one account's chain covers ONLY its
// pinned region's service actions, never a multi-region expansion. Cross-region
// spreading happens across ACCOUNTS, not within a single identity.
func TestEndpointChainCapped(t *testing.T) {
	prev := kiroEndpointsOverride
	kiroEndpointsOverride = nil
	t.Cleanup(func() { kiroEndpointsOverride = prev })
	t.Setenv("KIRO_API_REGIONS", "us-east-1,us-west-2,eu-west-1,eu-central-1,ap-northeast-1")

	acct := &config.Account{ID: "acct-chain-cap"}
	chain := getSortedEndpoints("auto", acct)
	if len(chain) > maxEndpointChain {
		t.Fatalf("chain length %d exceeds cap %d", len(chain), maxEndpointChain)
	}
	// One identity is pinned to ONE region: exactly that region's service
	// actions (3 outside us-east-1, 4 in us-east-1), not a multi-region chain.
	if len(chain) != 3 && len(chain) != 4 {
		t.Fatalf("expected one region's 3-4 endpoints (per-account pinning), got %d", len(chain))
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

// TestSortRegionalEndpoints pins the preferred-endpoint resolver across every
// preferred value and both fallback modes, for both the us-east-1 chain (4
// endpoints incl. codewhisperer) and a non-us-east-1 chain (3 endpoints). The
// resolver matches by endpoint NAME substring, so this guards against a rename
// silently breaking selection.
func TestSortRegionalEndpoints(t *testing.T) {
	prev := kiroEndpointsOverride
	kiroEndpointsOverride = nil
	t.Cleanup(func() { kiroEndpointsOverride = prev })

	firstHostContains := func(eps []kiroEndpoint, sub string) bool {
		return len(eps) > 0 && strings.Contains(eps[0].URL, sub)
	}

	for _, region := range []string{"us-east-1", "eu-west-1"} {
		chain := kiroEndpointsForRegion(region)

		// "auto" → declared order (runtime primary), full chain when fallback on.
		auto := sortRegionalEndpoints(chain, "auto", true)
		if !firstHostContains(auto, "runtime."+region+".kiro.dev") {
			t.Errorf("[%s] auto primary should be the runtime host, got %s", region, auto[0].URL)
		}
		if len(auto) != len(chain) {
			t.Errorf("[%s] auto+fallback should keep the full chain (%d), got %d", region, len(chain), len(auto))
		}

		// "kiro" → the modern Kiro Runtime host as primary.
		kiro := sortRegionalEndpoints(chain, "kiro", true)
		if !firstHostContains(kiro, "runtime."+region+".kiro.dev") {
			t.Errorf("[%s] preferred=kiro primary should be the runtime host, got %s", region, kiro[0].URL)
		}
		if len(kiro) != len(chain) {
			t.Errorf("[%s] preferred=kiro+fallback should keep the full chain, got %d", region, len(kiro))
		}

		// "amazonq" → the AmazonQ SendMessage endpoint as primary.
		aq := sortRegionalEndpoints(chain, "amazonq", true)
		if !strings.Contains(aq[0].Name, "AmazonQ") {
			t.Errorf("[%s] preferred=amazonq primary should be AmazonQ, got %q", region, aq[0].Name)
		}

		// fallback=false collapses to a single endpoint.
		single := sortRegionalEndpoints(chain, "kiro", false)
		if len(single) != 1 {
			t.Errorf("[%s] fallback=false must yield exactly one endpoint, got %d", region, len(single))
		}
		if !firstHostContains(single, "runtime."+region+".kiro.dev") {
			t.Errorf("[%s] fallback=false preferred=kiro must be the runtime host, got %s", region, single[0].URL)
		}
	}

	// "codewhisperer": us-east-1 has a real CodeWhisperer endpoint; outside
	// us-east-1 it falls back to the runtime host (the regional equivalent that
	// carries the same GenerateAssistantResponse target).
	usEast := kiroEndpointsForRegion("us-east-1")
	cw := sortRegionalEndpoints(usEast, "codewhisperer", true)
	if !strings.Contains(cw[0].Name, "CodeWhisperer") {
		t.Errorf("us-east-1 preferred=codewhisperer should select the CodeWhisperer endpoint, got %q", cw[0].Name)
	}
	eu := kiroEndpointsForRegion("eu-west-1")
	cwEU := sortRegionalEndpoints(eu, "codewhisperer", true)
	if !strings.Contains(cwEU[0].URL, "runtime.eu-west-1.kiro.dev") {
		t.Errorf("non-us-east-1 preferred=codewhisperer should fall back to the runtime host, got %s", cwEU[0].URL)
	}
}
