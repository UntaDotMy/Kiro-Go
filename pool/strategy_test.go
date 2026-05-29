package pool

import (
	"kiro-go/config"
	"testing"
)

// TestLeastUsedStrategyPicksLowestRequestCount pins the contract that
// "least-used" routes the next request to whichever eligible account has
// the lowest RequestCount. This is what makes mixed-quota pools drain
// the fresh accounts before re-burning the long-tenured ones.
func TestLeastUsedStrategyPicksLowestRequestCount(t *testing.T) {
	restore := SetStrategyResolverForTesting(func() string { return "least-used" })
	defer restore()

	p := NewForTesting()
	p.setAccounts([]config.Account{
		{ID: "a", RequestCount: 100, LastUsed: 100},
		{ID: "b", RequestCount: 5, LastUsed: 50}, // least-used winner
		{ID: "c", RequestCount: 80, LastUsed: 200},
	})

	acc, _, ok := p.GetNextForModel("")
	if !ok || acc == nil {
		t.Fatalf("expected a pick")
	}
	if acc.ID != "b" {
		t.Fatalf("expected least-used pick=b, got %s", acc.ID)
	}
}

// TestLeastUsedStrategyTiesBreakOnLastUsed verifies the secondary sort:
// when two accounts have the same RequestCount, the one with the older
// LastUsed wins so we don't keep re-picking the most-recent one.
func TestLeastUsedStrategyTiesBreakOnLastUsed(t *testing.T) {
	restore := SetStrategyResolverForTesting(func() string { return "least-used" })
	defer restore()

	p := NewForTesting()
	p.setAccounts([]config.Account{
		{ID: "newer", RequestCount: 5, LastUsed: 200},
		{ID: "older", RequestCount: 5, LastUsed: 100}, // tied count, older LastUsed
	})

	acc, _, ok := p.GetNextForModel("")
	if !ok || acc == nil {
		t.Fatalf("expected a pick")
	}
	if acc.ID != "older" {
		t.Fatalf("expected tie-break by LastUsed, got %s", acc.ID)
	}
}

// TestRandomStrategyPicksFromEligible exercises the random branch over
// many trials and confirms it eventually picks every eligible account
// (i.e. it doesn't get stuck on one). Statistical: chance of not seeing
// 'a' or 'b' in 200 picks is < 1e-60 per uniform RNG.
func TestRandomStrategyPicksFromEligible(t *testing.T) {
	restore := SetStrategyResolverForTesting(func() string { return "random" })
	defer restore()

	p := NewForTesting()
	p.setAccounts([]config.Account{
		{ID: "a"}, {ID: "b"},
	})

	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		acc, _, ok := p.GetNextForModel("")
		if !ok || acc == nil {
			t.Fatalf("trial %d: expected a pick", i)
		}
		seen[acc.ID]++
	}
	if seen["a"] == 0 || seen["b"] == 0 {
		t.Fatalf("expected both accounts to be picked over 200 trials, got %v", seen)
	}
}
