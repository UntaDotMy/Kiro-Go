package pool

import (
	"kiro-go/config"
	"testing"
)

// TestTokenLimitExhaustedDroppedAtReload verifies an account whose TotalTokens has
// reached its operator-set TokenLimit gets effective weight 0, so setAccounts
// (mirroring Reload) never adds it to the rotation slots. A 0 limit (unlimited)
// and a Kiro account are unaffected.
func TestTokenLimitExhaustedDroppedAtReload(t *testing.T) {
	cases := []struct {
		name        string
		acct        config.Account
		wantInSlots bool
	}{
		{"unlimited (0 limit)", config.Account{ID: "a", Backend: "qwen", TotalTokens: 9_999_999}, true},
		{"under limit", config.Account{ID: "b", Backend: "qwen", TokenLimit: 1_000_000, TotalTokens: 500_000}, true},
		{"at limit", config.Account{ID: "c", Backend: "qwen", TokenLimit: 1_000_000, TotalTokens: 1_000_000}, false},
		{"over limit", config.Account{ID: "d", Backend: "qwen", TokenLimit: 1_000_000, TotalTokens: 1_500_000}, false},
		{"kiro unaffected", config.Account{ID: "e", Backend: "kiro", TotalTokens: 50}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := computeEffectiveWeight(c.acct) > 0; got != c.wantInSlots {
				t.Errorf("computeEffectiveWeight>0 = %v, want %v (limit=%d used=%d)",
					got, c.wantInSlots, c.acct.TokenLimit, c.acct.TotalTokens)
			}
		})
	}
}

// TestTokenLimitStacksAcrossKeys is the "stacking" guarantee: with several keys of
// one provider, the picker only ever returns keys that still have budget. As keys
// exhaust mid-session their traffic concentrates on the survivors. We start all
// keys under-limit (so the pool builds correct parallel slots), then spend two of
// them in-memory (as UpdateStats would) and confirm only the survivor serves.
func TestTokenLimitStacksAcrossKeys(t *testing.T) {
	restore := SetStrategyResolverForTesting(func() string { return "fast" })
	defer restore()

	p := newTestPool()
	p.setAccounts([]config.Account{
		{ID: "k1", Backend: "qwen", APIKey: "1", TokenLimit: 1_000_000, TotalTokens: 10},
		{ID: "k2", Backend: "qwen", APIKey: "2", TokenLimit: 1_000_000, TotalTokens: 10},
		{ID: "k3", Backend: "qwen", APIKey: "3", TokenLimit: 1_000_000, TotalTokens: 10},
	})
	// Spend k1 and k2 past their limits (mutate the in-memory accounts the pool
	// holds, exactly as pool.UpdateStats does after a request completes).
	p.mu.Lock()
	for i := range p.accounts {
		if p.accounts[i].ID == "k1" || p.accounts[i].ID == "k2" {
			p.accounts[i].TotalTokens = 1_000_000
		}
	}
	p.mu.Unlock()

	// Only k3 should ever be picked now — k1/k2 are skipped by the live guard.
	for i := 0; i < 8; i++ {
		acc, _, ok := p.GetNextForModel("")
		if !ok || acc == nil {
			t.Fatalf("iteration %d: expected k3, got none", i)
		}
		if acc.ID != "k3" {
			t.Fatalf("iteration %d: expected only k3 to serve, got %s", i, acc.ID)
		}
	}

	// Spend k3 too — now the whole provider pool is exhausted and none serve.
	p.mu.Lock()
	for i := range p.accounts {
		if p.accounts[i].ID == "k3" {
			p.accounts[i].TotalTokens = 1_000_000
		}
	}
	p.mu.Unlock()
	if acc, _, ok := p.GetNextForModel(""); ok && acc != nil {
		t.Fatalf("all keys exhausted, but picker returned %s", acc.ID)
	}
}

// TestTokenLimitLiveSkipBetweenReloads verifies the pick-time guard: when a key's
// TotalTokens crosses its limit mid-session (mutated directly on the in-memory
// account, as UpdateStats does) WITHOUT a Reload, the picker still skips it. We
// force it into the slots with weight by starting under the limit, then bump the
// in-memory counter past the limit and confirm it's no longer returned.
func TestTokenLimitLiveSkipBetweenReloads(t *testing.T) {
	restore := SetStrategyResolverForTesting(func() string { return "fast" })
	defer restore()

	p := newTestPool()
	p.setAccounts([]config.Account{
		{ID: "live", Backend: "qwen", APIKey: "1", TokenLimit: 1_000_000, TotalTokens: 100},
	})
	// Initially eligible.
	if _, _, ok := p.GetNextForModel(""); !ok {
		t.Fatal("key should be eligible while under its limit")
	}
	// Simulate UpdateStats pushing it over the limit between reloads (mutate the
	// in-memory account the pool holds, exactly as pool.UpdateStats does).
	p.mu.Lock()
	for i := range p.accounts {
		if p.accounts[i].ID == "live" {
			p.accounts[i].TotalTokens = 1_000_001
		}
	}
	p.mu.Unlock()
	// Now the pick-time guard must skip it even though slots still contains it.
	if acc, _, ok := p.GetNextForModel(""); ok && acc != nil {
		t.Fatalf("exhausted key must be skipped live, but picker returned %s", acc.ID)
	}
}
