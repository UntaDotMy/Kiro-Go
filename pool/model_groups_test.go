package pool

import (
	"kiro-go/config"
	"testing"
)

// TestModelGroupRestrictsToMembers pins the contract: when a model is
// mapped to a group, only accounts whose Groups list includes that group
// are eligible. Accounts without groups are universal (historical
// behavior) so existing setups don't break.
func TestModelGroupRestrictsToMembers(t *testing.T) {
	restore := SetModelGroupResolverForTesting(func(m string) string {
		if m == "claude-opus-4-7" {
			return "premium"
		}
		return ""
	})
	defer restore()

	p := NewForTesting()
	p.setAccounts([]config.Account{
		{ID: "a", Groups: []string{"free"}},
		{ID: "b", Groups: []string{"premium"}},
		{ID: "c", Groups: []string{"premium", "experimental"}},
	})

	// Premium model: only b and c are eligible.
	for i := 0; i < 30; i++ {
		acc, _, ok := p.GetNextForModel("claude-opus-4-7")
		if !ok || acc == nil {
			t.Fatalf("trial %d: expected a pick", i)
		}
		if acc.ID == "a" {
			t.Fatalf("trial %d: free-only account 'a' picked for premium model", i)
		}
	}
}

// TestModelGroupUngroupedAccountsParticipate documents the back-compat
// rule: if an account has no Groups configured, it's eligible for every
// model regardless of group restriction. This is what keeps old configs
// working when an operator turns on a group rule for the first time.
func TestModelGroupUngroupedAccountsParticipate(t *testing.T) {
	restore := SetModelGroupResolverForTesting(func(m string) string { return "premium" })
	defer restore()

	p := NewForTesting()
	p.setAccounts([]config.Account{
		{ID: "ungrouped"}, // no Groups set
		{ID: "premium-member", Groups: []string{"premium"}},
	})

	seen := map[string]int{}
	for i := 0; i < 50; i++ {
		acc, _, ok := p.GetNextForModel("claude-opus-4-7")
		if !ok || acc == nil {
			t.Fatalf("trial %d: expected a pick", i)
		}
		seen[acc.ID]++
	}
	if seen["ungrouped"] == 0 {
		t.Fatalf("ungrouped account should participate in every model; got %v", seen)
	}
	if seen["premium-member"] == 0 {
		t.Fatalf("premium-member must also be picked; got %v", seen)
	}
}

// TestModelGroupNoMappingFallsBackToFullPool ensures the absence of a
// model->group mapping skips group filtering entirely.
func TestModelGroupNoMappingFallsBackToFullPool(t *testing.T) {
	restore := SetModelGroupResolverForTesting(func(m string) string { return "" })
	defer restore()

	p := NewForTesting()
	p.setAccounts([]config.Account{
		{ID: "a", Groups: []string{"free"}},
		{ID: "b", Groups: []string{"premium"}},
	})

	seen := map[string]int{}
	for i := 0; i < 50; i++ {
		acc, _, ok := p.GetNextForModel("claude-haiku-4-5")
		if !ok || acc == nil {
			t.Fatalf("trial %d: expected a pick", i)
		}
		seen[acc.ID]++
	}
	if seen["a"] == 0 || seen["b"] == 0 {
		t.Fatalf("both accounts should be eligible when no group rule applies; got %v", seen)
	}
}
