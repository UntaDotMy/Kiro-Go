package pool

import (
	"kiro-go/config"
	"testing"
	"time"
)

// withFast pins the strategy resolver to "fast" and the per-account
// fast-concurrency cap to the given value for the duration of a test.
func withFast(t *testing.T, fastConcurrency int) {
	t.Helper()
	restoreStrat := SetStrategyResolverForTesting(func() string { return "fast" })
	restoreFast := SetFastConcurrencyResolverForTesting(func() int { return fastConcurrency })
	t.Cleanup(restoreStrat)
	t.Cleanup(restoreFast)
}

// TestFastStrategyNonReservingNeverGates verifies that the NON-reserving picker
// (GetNextForModel) always returns an account immediately with no saturation
// hint, even when far more in-flight reservations exist than any cap would
// allow. Non-reserving callers (diagnostics / single-account paths) have no slot
// to reserve, so the cap gate doesn't apply to them.
func TestFastStrategyNonReservingNeverGates(t *testing.T) {
	withFast(t, 1)
	p := NewForTesting()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}})

	for i := 0; i < 100; i++ {
		acc, retryAfter, ok := p.GetNextForModel("")
		if !ok || acc == nil {
			t.Fatalf("non-reserving fast pick #%d should always succeed, got ok=%v retryAfter=%v", i, ok, retryAfter)
		}
		if retryAfter != 0 {
			t.Fatalf("non-reserving fast pick #%d should never return a retryAfter, got %v", i, retryAfter)
		}
	}
}

// TestFastStrategyFansOutAcrossFreeAccounts is the headline property (WiFi-7
// multi-link): with the per-account cap at 1 (send-and-ack) and 4 free accounts,
// 4 concurrent reserving acquires each land on a DISTINCT account in parallel,
// instead of queueing on one. A 5th acquire — with all 4 now at their cap —
// returns the saturation poll hint so the dispatcher waits for a slot to free.
func TestFastStrategyFansOutAcrossFreeAccounts(t *testing.T) {
	withFast(t, 1)
	p := NewForTesting()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}})

	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		acc, retryAfter, ok := p.AcquireForModelExcluding("", nil)
		if !ok || acc == nil {
			t.Fatalf("acquire #%d should succeed while a free account remains, got ok=%v retryAfter=%v", i, ok, retryAfter)
		}
		if seen[acc.ID] {
			t.Fatalf("acquire #%d reused account %s — fast must fan out to a distinct free account", i, acc.ID)
		}
		seen[acc.ID] = true
	}
	if len(seen) != 4 {
		t.Fatalf("expected 4 distinct accounts under send-and-ack fan-out, got %v", seen)
	}

	// All 4 are now at their cap of 1 — the next acquire must shed with the
	// saturation poll hint (the dispatcher turns this into a short wait).
	acc, retryAfter, ok := p.AcquireForModelExcluding("", nil)
	if ok || acc != nil {
		t.Fatalf("acquire with every account at cap should not pick, got acc=%v ok=%v", acc, ok)
	}
	if retryAfter != saturationPollInterval {
		t.Fatalf("expected saturation poll hint %s when all accounts at cap, got %s", saturationPollInterval, retryAfter)
	}
}

// TestFastStrategyWaitsThenRoutesToFreedSlot verifies the "wait until one frees"
// contract: with cap 1 and 2 accounts both in flight, the next acquire saturates;
// after a Release frees a slot, the following acquire succeeds and routes to the
// freed account.
func TestFastStrategyWaitsThenRoutesToFreedSlot(t *testing.T) {
	withFast(t, 1)
	p := NewForTesting()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}})

	first, _, ok := p.AcquireForModelExcluding("", nil)
	if !ok {
		t.Fatalf("first acquire failed")
	}
	second, _, ok := p.AcquireForModelExcluding("", nil)
	if !ok || second.ID == first.ID {
		t.Fatalf("second acquire should land on the other account, got %v (first %v)", second, first)
	}
	// Both at cap → saturate.
	if _, ra, ok := p.AcquireForModelExcluding("", nil); ok || ra != saturationPollInterval {
		t.Fatalf("expected saturation when both at cap, got ok=%v retryAfter=%v", ok, ra)
	}
	// Free one slot; the next acquire must succeed and route to the freed account.
	p.Release(first.ID)
	acc, _, ok := p.AcquireForModelExcluding("", nil)
	if !ok || acc == nil {
		t.Fatalf("acquire after Release should succeed")
	}
	if acc.ID != first.ID {
		t.Fatalf("acquire after Release should route to the freed account %s, got %s", first.ID, acc.ID)
	}
}

// TestFastStrategyPipelinesWhenCapAboveOne verifies that raising the per-account
// cap lets a single account pipeline multiple concurrent requests before the
// burst spills to a peer. With cap 2 and 2 accounts, four acquires distribute as
// two per account (the least-loaded account always wins the next slot).
func TestFastStrategyPipelinesWhenCapAboveOne(t *testing.T) {
	withFast(t, 2)
	p := NewForTesting()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}})

	counts := map[string]int{}
	for i := 0; i < 4; i++ {
		acc, _, ok := p.AcquireForModelExcluding("", nil)
		if !ok || acc == nil {
			t.Fatalf("acquire #%d should succeed under cap 2 with 2 accounts", i)
		}
		counts[acc.ID]++
	}
	if counts["a"] != 2 || counts["b"] != 2 {
		t.Fatalf("expected 2 in-flight per account at cap 2, got %v", counts)
	}
	// Fifth acquire — both at cap 2 — saturates.
	if _, ra, ok := p.AcquireForModelExcluding("", nil); ok || ra != saturationPollInterval {
		t.Fatalf("expected saturation at cap 2 once both accounts full, got ok=%v retryAfter=%v", ok, ra)
	}
}

// TestFastStrategyFillFirstByWeight verifies that on the first pick (equal
// in-flight) the higher-weight account is preferred.
func TestFastStrategyFillFirstByWeight(t *testing.T) {
	withFast(t, 1)
	p := NewForTesting()
	p.setAccounts([]config.Account{
		{ID: "low", Weight: 1},  // effectiveWeight 10
		{ID: "high", Weight: 5}, // effectiveWeight 50
	})

	acc, _, ok := p.GetNextForModel("")
	if !ok || acc == nil {
		t.Fatalf("expected a pick")
	}
	if acc.ID != "high" {
		t.Fatalf("fast should prefer the highest-weight account when equally loaded, got %s", acc.ID)
	}
}

// TestFastStrategyRotatesEvenly verifies that equal-weight, equally-loaded
// accounts rotate by least-recently-picked so a stream of non-reserving picks
// spreads evenly instead of pinning to slot 0.
func TestFastStrategyRotatesEvenly(t *testing.T) {
	withFast(t, 1)
	p := NewForTesting()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}, {ID: "c"}})

	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		acc, _, ok := p.GetNextForModel("")
		if !ok {
			t.Fatalf("pick #%d failed", i)
		}
		counts[acc.ID]++
	}
	for _, id := range []string{"a", "b", "c"} {
		if counts[id] != 3 {
			t.Fatalf("expected even rotation 3/3/3 across a,b,c, got %v", counts)
		}
	}
}

// TestFastStrategyIgnoresCooldown verifies the deliberate design choice that the
// fast strategy routes purely by free capacity and does NOT skip a soft-cooled
// account — an account that recently hit a 429 is steered around by load, not by
// a timed cooldown. (The other strategies still honor the cooldown; see
// TestFastStrategy* vs the least-request tests.)
func TestFastStrategyIgnoresCooldown(t *testing.T) {
	withFast(t, 1)
	p := NewForTesting()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}})

	// Put "a" in a long soft cooldown. Fast must still be willing to pick it.
	p.cooldowns["a"] = &cooldownEntry{until: time.Now().Add(time.Minute)}

	sawA := false
	for i := 0; i < 4; i++ {
		acc, _, ok := p.GetNextForModel("")
		if !ok || acc == nil {
			t.Fatalf("pick #%d failed", i)
		}
		if acc.ID == "a" {
			sawA = true
		}
	}
	if !sawA {
		t.Fatalf("fast strategy must route to a cooling account (it ignores soft cooldown); never picked a")
	}
}
