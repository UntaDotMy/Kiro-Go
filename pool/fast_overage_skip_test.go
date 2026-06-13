package pool

import (
	"kiro-go/config"
	"testing"
	"time"
)

// Story s8: the default "fast" strategy must still EXCLUDE OVERAGE-parked
// accounts (402, billed past cap) even though it deliberately bypasses short
// soft-429 cooldowns. RecordQuotaExhaustion sets the hard park; fast must honor
// it while continuing to route to soft-cooled accounts.

func TestFastStrategySkipsOverageParkedAccount(t *testing.T) {
	withFast(t, 1)
	p := NewForTesting()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}})

	// Park "a" via the OVERAGE path. Fast must never pick it while parked.
	p.RecordQuotaExhaustion("a")

	for i := 0; i < 20; i++ {
		acc, _, ok := p.GetNextForModel("")
		if !ok || acc == nil {
			t.Fatalf("pick #%d failed — a healthy peer (b) should always be available", i)
		}
		if acc.ID == "a" {
			t.Fatalf("fast strategy routed to an OVERAGE-parked account (a); it must be skipped")
		}
	}
}

// TestFastStrategyStillIgnoresSoftCooldown is the guard that the s8 change did
// NOT regress the deliberate soft-cooldown bypass: a soft-429-cooled account is
// still routable under fast.
func TestFastStrategyStillIgnoresSoftCooldown(t *testing.T) {
	withFast(t, 1)
	p := NewForTesting()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}})

	// Soft cooldown (NOT an overage park): fast must still be willing to pick a.
	p.cooldowns["a"] = &cooldownEntry{until: time.Now().Add(time.Minute)}

	sawA := false
	for i := 0; i < 6; i++ {
		acc, _, ok := p.GetNextForModel("")
		if !ok || acc == nil {
			t.Fatalf("pick #%d failed", i)
		}
		if acc.ID == "a" {
			sawA = true
		}
	}
	if !sawA {
		t.Fatal("fast must still route to a soft-cooled account; the OVERAGE-park exclusion must not block soft cooldowns")
	}
}

// TestRecordSuccessClearsOveragePark verifies a successful request rejoins the
// account to rotation (the park clears), so a recovered account isn't stuck out
// for the whole hour.
func TestRecordSuccessClearsOveragePark(t *testing.T) {
	withFast(t, 1)
	p := NewForTesting()
	p.setAccounts([]config.Account{{ID: "a"}})

	p.RecordQuotaExhaustion("a")
	// While parked, fast finds nothing.
	if _, _, ok := p.GetNextForModel(""); ok {
		t.Fatal("a should be parked (OVERAGE) and not selectable")
	}

	// A success clears the park.
	p.RecordSuccess("a")
	if _, _, ok := p.GetNextForModel(""); !ok {
		t.Fatal("after RecordSuccess the OVERAGE park must be cleared and a selectable again")
	}
}

// TestNonFastStrategyAlsoSkipsOveragePark confirms the park is honored by the
// non-fast strategies too (they already skipped via `until`, but the dedicated
// park field must not regress that).
func TestNonFastStrategyAlsoSkipsOveragePark(t *testing.T) {
	restore := SetStrategyResolverForTesting(func() string { return "least-request" })
	defer restore()
	p := NewForTesting()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}})
	p.RecordQuotaExhaustion("a")

	for i := 0; i < 10; i++ {
		acc, _, ok := p.GetNextForModel("")
		if !ok || acc == nil {
			t.Fatalf("pick #%d failed", i)
		}
		if acc.ID == "a" {
			t.Fatal("least-request must also skip an OVERAGE-parked account")
		}
	}
	_ = config.Account{} // keep config import used
}
