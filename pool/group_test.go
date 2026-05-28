package pool

import (
	"kiro-go/config"
	"testing"
)

// TestGroupRestrictsToMembers pins the contract: when a group is supplied
// (the per-API-key Group field), only accounts whose Groups list includes
// that group are eligible. Accounts without groups configured are
// universal (back-compat — existing setups that never use grouping
// continue to work without changes).
func TestGroupRestrictsToMembers(t *testing.T) {
	p := NewForTesting()
	p.setAccounts([]config.Account{
		{ID: "a", Groups: []string{"free"}},
		{ID: "b", Groups: []string{"premium"}},
		{ID: "c", Groups: []string{"premium", "experimental"}},
	})

	for i := 0; i < 30; i++ {
		acc, _, ok := p.GetNextForModelInGroup("claude-opus-4-7", "premium")
		if !ok || acc == nil {
			t.Fatalf("trial %d: expected a pick", i)
		}
		if acc.ID == "a" {
			t.Fatalf("trial %d: free-only account 'a' picked for 'premium' group", i)
		}
	}
}

// TestGroupUngroupedAccountsParticipate documents the back-compat rule:
// an account with empty Groups is eligible for every group restriction.
// This is what keeps configs without explicit grouping working when an
// operator first sets a key's Group.
func TestGroupUngroupedAccountsParticipate(t *testing.T) {
	p := NewForTesting()
	p.setAccounts([]config.Account{
		{ID: "ungrouped"},
		{ID: "premium-member", Groups: []string{"premium"}},
	})

	seen := map[string]int{}
	for i := 0; i < 50; i++ {
		acc, _, ok := p.GetNextForModelInGroup("claude-opus-4-7", "premium")
		if !ok || acc == nil {
			t.Fatalf("trial %d: expected a pick", i)
		}
		seen[acc.ID]++
	}
	if seen["ungrouped"] == 0 {
		t.Fatalf("ungrouped account should participate in every group; got %v", seen)
	}
	if seen["premium-member"] == 0 {
		t.Fatalf("premium-member must also be picked; got %v", seen)
	}
}

// TestEmptyGroupSkipsRestriction ensures that GetNextForModelInGroup with
// group="" behaves exactly like GetNextForModel — every eligible account
// participates. This is the path used when the inbound API key has no
// Group set.
func TestEmptyGroupSkipsRestriction(t *testing.T) {
	p := NewForTesting()
	p.setAccounts([]config.Account{
		{ID: "a", Groups: []string{"free"}},
		{ID: "b", Groups: []string{"premium"}},
	})

	seen := map[string]int{}
	for i := 0; i < 50; i++ {
		acc, _, ok := p.GetNextForModelInGroup("claude-haiku-4-5", "")
		if !ok || acc == nil {
			t.Fatalf("trial %d: expected a pick", i)
		}
		seen[acc.ID]++
	}
	if seen["a"] == 0 || seen["b"] == 0 {
		t.Fatalf("both accounts should be eligible when no group filter applies; got %v", seen)
	}
}

// TestGroupCaseInsensitive verifies the group lookup uses EqualFold so
// admin-typed casing in either the key.Group field or account.Groups list
// doesn't matter.
func TestGroupCaseInsensitive(t *testing.T) {
	p := NewForTesting()
	p.setAccounts([]config.Account{
		{ID: "p", Groups: []string{"Premium"}}, // mixed case in account
	})

	acc, _, ok := p.GetNextForModelInGroup("", "premium")
	if !ok || acc == nil || acc.ID != "p" {
		t.Fatalf("expected case-insensitive match; got %v", acc)
	}
}
