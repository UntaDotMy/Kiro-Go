package pool

import (
	"kiro-go/config"
	"testing"
)

// TestBackendFilterScopesSelection verifies the provider-aware pool filter: a
// backend-scoped pick only ever returns accounts whose resolved backend matches,
// and a backend with no accounts yields no pick.
func TestBackendFilterScopesSelection(t *testing.T) {
	withFast(t, 1)
	p := NewForTesting()
	p.setAccounts([]config.Account{
		{ID: "k1"},                    // Backend "" -> kiro
		{ID: "k2", Backend: "kiro"},   // explicit kiro
		{ID: "c1", Backend: "codex"},  // codex
		{ID: "o1", Backend: "openai"}, // openai
	})

	// Backend "kiro" must only ever return k1 or k2.
	for i := 0; i < 20; i++ {
		acc, _, ok := p.GetNextForBackendModelExcluding("kiro", "", nil)
		if !ok || acc == nil {
			t.Fatalf("kiro pick #%d failed", i)
		}
		if acc.ID != "k1" && acc.ID != "k2" {
			t.Fatalf("kiro-scoped pick returned non-kiro account %s", acc.ID)
		}
	}

	// Backend "codex" must only ever return c1.
	for i := 0; i < 5; i++ {
		acc, _, ok := p.GetNextForBackendModelExcluding("codex", "", nil)
		if !ok || acc == nil || acc.ID != "c1" {
			t.Fatalf("codex-scoped pick should return c1, got ok=%v acc=%v", ok, acc)
		}
	}

	// A backend with no accounts yields no pick (and no retryAfter — it's "none",
	// not "cooling").
	acc, retryAfter, ok := p.GetNextForBackendModelExcluding("gemini", "", nil)
	if ok || acc != nil {
		t.Fatalf("gemini-scoped pick should find nothing, got acc=%v ok=%v", acc, ok)
	}
	if retryAfter != 0 {
		t.Fatalf("empty-backend pick should not report a cooling retryAfter, got %v", retryAfter)
	}
}

// TestEmptyBackendMatchesAllAccounts verifies the legacy contract: an unscoped
// ("") pick considers every account regardless of backend, so existing callers
// that don't pass a backend are unaffected.
func TestEmptyBackendMatchesAllAccounts(t *testing.T) {
	withFast(t, 1)
	p := NewForTesting()
	p.setAccounts([]config.Account{
		{ID: "k1"},
		{ID: "c1", Backend: "codex"},
	})

	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		acc, _, ok := p.GetNextForModel("")
		if !ok {
			t.Fatalf("unscoped pick #%d failed", i)
		}
		seen[acc.ID] = true
	}
	if !seen["k1"] || !seen["c1"] {
		t.Fatalf("unscoped pick should reach both backends, saw %v", seen)
	}
}

// TestFastStrategyBackendScopedStickyRotation verifies the fast strategy's
// sticky-round-robin behaves correctly when selection is backend-scoped: within
// a single backend the picker rotates across that backend's accounts as the
// sticky cap is hit, and never crosses into another backend's accounts.
func TestFastStrategyBackendScopedStickyRotation(t *testing.T) {
	withFast(t, 2) // sticky cap 2
	p := NewForTesting()
	p.setAccounts([]config.Account{
		{ID: "k1"},                  // kiro
		{ID: "k2", Backend: "kiro"}, // kiro
		{ID: "g1", Backend: "groq"}, // groq
		{ID: "g2", Backend: "groq"}, // groq
	})

	// 8 groq-scoped picks: every pick must be a groq account, and across the run
	// both g1 and g2 must be used (sticky cap 2 forces rotation).
	groqSeen := map[string]int{}
	for i := 0; i < 8; i++ {
		acc, _, ok := p.GetNextForBackendModelExcluding("groq", "", nil)
		if !ok || acc == nil {
			t.Fatalf("groq pick #%d failed", i)
		}
		if acc.Backend != "groq" {
			t.Fatalf("groq-scoped pick returned non-groq account %s (backend=%q)", acc.ID, acc.Backend)
		}
		groqSeen[acc.ID]++
	}
	if groqSeen["g1"] == 0 || groqSeen["g2"] == 0 {
		t.Fatalf("sticky cap 2 should rotate across both groq accounts, saw %v", groqSeen)
	}

	// Interleaved kiro-scoped picks must only ever return kiro accounts, proving
	// the backend filter holds even though the pool's sticky state is shared.
	for i := 0; i < 4; i++ {
		acc, _, ok := p.GetNextForBackendModelExcluding("kiro", "", nil)
		if !ok || acc == nil {
			t.Fatalf("kiro pick #%d failed", i)
		}
		if acc.ID != "k1" && acc.ID != "k2" {
			t.Fatalf("kiro-scoped pick returned non-kiro account %s", acc.ID)
		}
	}
}
