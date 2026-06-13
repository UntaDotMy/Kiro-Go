package pool

import (
	"kiro-go/config"
	"testing"
	"time"
)

// Story s5: failover must size its attempt budget to the addressable pool, not
// a fixed cap of 3. EligibleCountForBackendModel is the pool-side count the
// dispatcher uses for that. It mirrors the picker's eligibility predicate.

func TestEligibleCountCountsHealthyAccounts(t *testing.T) {
	// Default (non-fast) strategy.
	restore := SetStrategyResolverForTesting(func() string { return "least-request" })
	defer restore()

	p := newTestPool()
	p.setAccounts([]config.Account{
		{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}, {ID: "e"},
	})
	if got := p.EligibleCountForBackendModel("", ""); got != 5 {
		t.Fatalf("all 5 healthy accounts should be eligible, got %d", got)
	}
}

func TestEligibleCountSkipsCooledAccountsNonFast(t *testing.T) {
	restore := SetStrategyResolverForTesting(func() string { return "least-request" })
	defer restore()

	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}, {ID: "c"}})
	// Cool two accounts; only one remains eligible under a non-fast strategy.
	p.cooldowns["a"] = &cooldownEntry{until: time.Now().Add(time.Minute)}
	p.cooldowns["b"] = &cooldownEntry{until: time.Now().Add(time.Minute)}

	if got := p.EligibleCountForBackendModel("", ""); got != 1 {
		t.Fatalf("only the 1 uncooled account should be eligible (non-fast), got %d", got)
	}
}

func TestEligibleCountFastIgnoresCooldown(t *testing.T) {
	restore := SetStrategyResolverForTesting(func() string { return "fast" })
	defer restore()

	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}, {ID: "c"}})
	// Cooled accounts are still routable under fast (it routes by free capacity,
	// not cooldown), so they count as eligible — matching pick().
	p.cooldowns["a"] = &cooldownEntry{until: time.Now().Add(time.Minute)}

	if got := p.EligibleCountForBackendModel("", ""); got != 3 {
		t.Fatalf("fast must count cooled accounts as eligible, got %d", got)
	}
}

func TestEligibleCountScopesByBackend(t *testing.T) {
	restore := SetStrategyResolverForTesting(func() string { return "least-request" })
	defer restore()

	p := newTestPool()
	p.setAccounts([]config.Account{
		{ID: "k1", Backend: "kiro"},
		{ID: "k2", Backend: "kiro"},
		{ID: "o1", Backend: "openai"},
	})
	if got := p.EligibleCountForBackendModel("kiro", ""); got != 2 {
		t.Fatalf("only kiro accounts should count for backend=kiro, got %d", got)
	}
	if got := p.EligibleCountForBackendModel("openai", ""); got != 1 {
		t.Fatalf("only openai accounts should count for backend=openai, got %d", got)
	}
}
