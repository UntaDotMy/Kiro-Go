package proxy

import "testing"

// Story s5: the per-request failover budget must scale to the addressable pool
// — min(eligibleAccounts, maxFailoverAttempts) — instead of a fixed cap of 3,
// so a burst of dead accounts in a large pool no longer surfaces a 503/429
// while healthy accounts go untried. With only 1 eligible account, rotating is
// meaningless and just wastes latency on retries, so budget = 1.

func TestFailoverBudgetScalesToPool(t *testing.T) {
	cases := []struct {
		name    string
		ids     []string
		wantMin int // budget must be at least this
		wantMax int // and at most this
	}{
		// Single/two accounts: budget tracks eligible count (no floor).
		{"single account", []string{"a"}, 1, 1},
		{"two accounts", []string{"a", "b"}, 2, 2},
		{"three accounts", []string{"a", "b", "c"}, 3, 3},
		// Mid pool: budget tracks the eligible count above the floor.
		{"five accounts", []string{"a", "b", "c", "d", "e"}, 5, 5},
		// Large pool: capped at the ceiling, not unbounded.
		{"twelve accounts capped", []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"}, maxFailoverAttempts, maxFailoverAttempts},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFailoverTestHandler(tc.ids...)
			got := h.failoverBudget("", "")
			if got < tc.wantMin || got > tc.wantMax {
				t.Fatalf("failoverBudget for %d accounts = %d, want in [%d,%d]",
					len(tc.ids), got, tc.wantMin, tc.wantMax)
			}
			if got > maxFailoverAttempts {
				t.Fatalf("budget %d exceeds ceiling %d", got, maxFailoverAttempts)
			}
		})
	}
}

// TestFailoverBudgetNilPoolFallsBackToFloor verifies a handler with no pool
// (defensive) uses the floor rather than panicking.
func TestFailoverBudgetNilPoolFallsBackToFloor(t *testing.T) {
	h := &Handler{}
	if got := h.failoverBudget("", ""); got != minFailoverAttempts {
		t.Fatalf("nil pool should fall back to floor %d, got %d", minFailoverAttempts, got)
	}
}

// TestFailoverCeilingRaisedAboveOldFixedCap is a regression guard: the old hard
// cap was 3; the ceiling must now exceed it so large pools get real headroom.
func TestFailoverCeilingRaisedAboveOldFixedCap(t *testing.T) {
	if maxFailoverAttempts <= 3 {
		t.Fatalf("maxFailoverAttempts ceiling (%d) must exceed the old fixed cap of 3", maxFailoverAttempts)
	}
}
