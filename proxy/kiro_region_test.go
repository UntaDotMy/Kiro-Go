package proxy

import (
	"fmt"
	"kiro-go/config"
	"strings"
	"testing"
)

// TestKiroEndpointsForRegionUsEast1 pins the us-east-1 endpoint chain:
// the codewhisperer.<region> hostname is included only here because per
// jwadow/kiro-gateway #58 it returns NXDOMAIN in every other region.
func TestKiroEndpointsForRegionUsEast1(t *testing.T) {
	prev := kiroEndpointsOverride
	kiroEndpointsOverride = nil
	t.Cleanup(func() { kiroEndpointsOverride = prev })

	endpoints := kiroEndpointsForRegion("us-east-1")
	if len(endpoints) != 3 {
		t.Fatalf("expected 3 endpoints, got %d", len(endpoints))
	}
	hosts := []string{}
	for _, e := range endpoints {
		hosts = append(hosts, e.URL)
	}
	joined := strings.Join(hosts, " | ")
	if !strings.Contains(joined, "q.us-east-1.amazonaws.com") {
		t.Errorf("missing q.us-east-1: %s", joined)
	}
	if !strings.Contains(joined, "codewhisperer.us-east-1.amazonaws.com") {
		t.Errorf("missing codewhisperer.us-east-1: %s", joined)
	}
}

// TestKiroEndpointsForRegionEU substitutes runtime.<region>.kiro.dev for
// codewhisperer.<region> outside us-east-1.
func TestKiroEndpointsForRegionEU(t *testing.T) {
	prev := kiroEndpointsOverride
	kiroEndpointsOverride = nil
	t.Cleanup(func() { kiroEndpointsOverride = prev })

	endpoints := kiroEndpointsForRegion("eu-west-1")
	hosts := []string{}
	for _, e := range endpoints {
		hosts = append(hosts, e.URL)
	}
	joined := strings.Join(hosts, " | ")
	if strings.Contains(joined, "codewhisperer.eu-west-1") {
		t.Errorf("codewhisperer.* must not be used outside us-east-1: %s", joined)
	}
	if !strings.Contains(joined, "runtime.eu-west-1.kiro.dev") {
		t.Errorf("expected runtime.eu-west-1.kiro.dev fallback host: %s", joined)
	}
	if !strings.Contains(joined, "q.eu-west-1.amazonaws.com") {
		t.Errorf("expected q.eu-west-1: %s", joined)
	}
}

// TestKiroRESTBaseSwitchesByRegion mirrors the streaming chain logic for
// the REST base used by usage / profile queries.
func TestKiroRESTBaseSwitchesByRegion(t *testing.T) {
	if got := kiroRESTBaseForRegion("us-east-1"); !strings.Contains(got, "codewhisperer.us-east-1.amazonaws.com") {
		t.Errorf("us-east-1 REST base wrong: %s", got)
	}
	if got := kiroRESTBaseForRegion("ap-northeast-1"); !strings.Contains(got, "runtime.ap-northeast-1.kiro.dev") {
		t.Errorf("non-us-east-1 REST base must use runtime.<region>.kiro.dev: %s", got)
	}
	if got := kiroRESTBaseForRegion(""); !strings.Contains(got, "us-east-1") {
		t.Errorf("empty region should default to us-east-1: %s", got)
	}
}

// TestEndpointChainCapped enforces the maxEndpointChain ceiling and the
// per-account region-pinning contract: one account's chain covers ONLY
// its pinned region's service actions (3), never a multi-region
// expansion. Cross-region spreading happens across ACCOUNTS, not within
// a single identity, so no one OAuth identity ever crosses regions.
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
	// One identity is pinned to ONE region: exactly that region's 3
	// service actions, not a multi-region chain.
	if len(chain) != 3 {
		t.Fatalf("expected exactly one region's 3 endpoints (per-account pinning), got %d", len(chain))
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
