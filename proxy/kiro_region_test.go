package proxy

import (
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

// TestEndpointChainCapped enforces the maxEndpointChain ceiling so a
// misconfigured many-region failover list cannot produce arbitrarily
// long per-request stalls.
func TestEndpointChainCapped(t *testing.T) {
	prev := kiroEndpointsOverride
	kiroEndpointsOverride = nil
	t.Cleanup(func() { kiroEndpointsOverride = prev })
	t.Setenv("KIRO_API_REGIONS", "us-east-1,us-west-2,eu-west-1,eu-central-1,ap-northeast-1")

	chain := getSortedEndpoints("auto")
	if len(chain) > maxEndpointChain {
		t.Fatalf("chain length %d exceeds cap %d", len(chain), maxEndpointChain)
	}
	if len(chain) <= 3 {
		t.Fatalf("expected multi-region expansion, got only %d entries", len(chain))
	}
}
