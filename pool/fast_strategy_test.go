package pool

import (
	"kiro-go/config"
	"testing"
	"time"
)

// withFast pins the strategy resolver to "fast" and the sticky limit to the
// given value for the duration of a test.
func withFast(t *testing.T, stickyLimit int) {
	t.Helper()
	restoreStrat := SetStrategyResolverForTesting(func() string { return "fast" })
	restoreSticky := SetStickyLimitResolverForTesting(func() int { return stickyLimit })
	t.Cleanup(restoreStrat)
	t.Cleanup(restoreSticky)
}

// TestFastStrategyNoStallOnSelection verifies the headline property of the fast
// strategy: a reserving acquire returns an account IMMEDIATELY (ok=true,
// retryAfter=0) with no admission-wait / saturation signal, even when several
// requests are already in flight on every account. The least-request path would
// gate on the AIMD concurrency limit and could return the saturation poll hint;
// fast never does.
func TestFastStrategyNoStallOnSelection(t *testing.T) {
	withFast(t, 1) // stickiness off -> spread
	p := NewForTesting()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}})

	// Pile far more in-flight reservations than any AIMD limit would allow.
	for i := 0; i < 100; i++ {
		acc, retryAfter, ok := p.AcquireForModelExcluding("", nil)
		if !ok || acc == nil {
			t.Fatalf("fast acquire #%d should always succeed immediately, got ok=%v retryAfter=%v", i, ok, retryAfter)
		}
		if retryAfter != 0 {
			t.Fatalf("fast acquire #%d should never return a retryAfter (no admission gate), got %v", i, retryAfter)
		}
	}
}

// TestFastStrategyFillFirstByWeight verifies that with stickiness off the fast
// strategy prefers the highest-weight eligible account.
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
		t.Fatalf("fast fill-first should pick the highest-weight account, got %s", acc.ID)
	}
}

// TestFastStrategyRotatesWhenStickinessOff verifies that equal-weight accounts
// rotate (least-recently-picked) when the sticky limit is 1, instead of pinning
// to slot 0. Over an even number of picks each account should get an equal share.
func TestFastStrategyRotatesWhenStickinessOff(t *testing.T) {
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

// TestFastStrategyStickyHoldsThenRotates verifies sticky-round-robin: with a
// sticky limit of 3, the strategy stays on one account for 3 consecutive picks,
// then rotates to the least-recently-picked peer for the next 3, and so on.
func TestFastStrategyStickyHoldsThenRotates(t *testing.T) {
	withFast(t, 3)
	p := NewForTesting()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}})

	var seq []string
	for i := 0; i < 6; i++ {
		acc, _, ok := p.GetNextForModel("")
		if !ok {
			t.Fatalf("pick #%d failed", i)
		}
		seq = append(seq, acc.ID)
	}
	// First 3 picks stay on one account, next 3 on the other.
	first := seq[0]
	for i := 1; i < 3; i++ {
		if seq[i] != first {
			t.Fatalf("expected first 3 picks sticky on %s, got %v", first, seq)
		}
	}
	second := seq[3]
	if second == first {
		t.Fatalf("expected rotation to the other account after the sticky cap, got %v", seq)
	}
	for i := 4; i < 6; i++ {
		if seq[i] != second {
			t.Fatalf("expected next 3 picks sticky on %s, got %v", second, seq)
		}
	}
}

// TestFastStrategySkipsCooldownAccounts verifies that fast selection respects
// the shared cooldown filter: an account in soft cooldown is never picked, and
// once every account is cooling the picker returns the soonest-recovery hint
// (same contract as the other strategies).
func TestFastStrategySkipsCooldownAccounts(t *testing.T) {
	withFast(t, 1)
	p := NewForTesting()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}})

	// Cool "a" hard; only "b" should be returned.
	p.cooldowns["a"] = &cooldownEntry{until: time.Now().Add(time.Minute)}
	for i := 0; i < 5; i++ {
		acc, _, ok := p.GetNextForModel("")
		if !ok || acc == nil {
			t.Fatalf("pick #%d should still find b", i)
		}
		if acc.ID != "a" {
			continue
		}
		t.Fatalf("fast strategy returned a cooling account")
	}

	// Cool "b" too — now every account is cooling, picker returns retryAfter>0.
	p.cooldowns["b"] = &cooldownEntry{until: time.Now().Add(30 * time.Second)}
	acc, retryAfter, ok := p.GetNextForModel("")
	if ok || acc != nil {
		t.Fatalf("expected no pick when all accounts cooling, got acc=%v ok=%v", acc, ok)
	}
	if retryAfter <= 0 {
		t.Fatalf("expected a positive soonest-recovery retryAfter, got %v", retryAfter)
	}
}

// TestFastStrategyRotatesAwayFromCooledStickyAccount verifies that if the
// current sticky account falls into cooldown mid-streak, the next pick rotates
// to an eligible peer rather than getting stuck.
func TestFastStrategyRotatesAwayFromCooledStickyAccount(t *testing.T) {
	withFast(t, 10) // long sticky window
	p := NewForTesting()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}})

	first, _, ok := p.GetNextForModel("")
	if !ok {
		t.Fatalf("first pick failed")
	}
	// Cool the account we just stuck to.
	p.cooldowns[first.ID] = &cooldownEntry{until: time.Now().Add(time.Minute)}

	acc, _, ok := p.GetNextForModel("")
	if !ok || acc == nil {
		t.Fatalf("expected a pick after sticky account cooled")
	}
	if acc.ID == first.ID {
		t.Fatalf("expected rotation away from cooled sticky account %s", first.ID)
	}
}
