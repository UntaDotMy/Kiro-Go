package config

import (
	"path/filepath"
	"testing"
)

// initTestConfig boots a fresh config in a per-test temp dir. The package
// state is global, so tests can't run in parallel — t.TempDir at least
// keeps on-disk state per-test.
func initTestConfig(t *testing.T) {
	t.Helper()
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
}

// TestIsQuotaExhausted pins the predicate that drives auto-disable. A zero
// limit is "no limit declared by upstream" and must NOT be treated as full.
func TestIsQuotaExhausted(t *testing.T) {
	cases := []struct {
		name string
		acc  Account
		want bool
	}{
		{"both zero", Account{}, false},
		{"paid under", Account{UsageCurrent: 999, UsageLimit: 1000}, false},
		{"paid at limit", Account{UsageCurrent: 1000, UsageLimit: 1000}, true},
		{"paid over limit", Account{UsageCurrent: 1500, UsageLimit: 1000}, true},
		{"trial under", Account{TrialUsageCurrent: 49, TrialUsageLimit: 50}, false},
		{"trial at limit", Account{TrialUsageCurrent: 50, TrialUsageLimit: 50}, true},
		{"trial only — no paid limit", Account{TrialUsageCurrent: 50, TrialUsageLimit: 50, UsageLimit: 0}, true},
		{"paid healthy, trial full", Account{UsageCurrent: 1, UsageLimit: 1000, TrialUsageCurrent: 50, TrialUsageLimit: 50}, true},
		{"limit zero with usage", Account{UsageCurrent: 100, UsageLimit: 0}, false},

		// Sanity-check that the predicate has no hardcoded threshold — the
		// upstream Kiro API reports any limit per account (Free 50, Pro 1000,
		// Pro+ 2000+, custom enterprise tiers, fractional credit limits, …).
		// All of these must trigger purely off `UsageCurrent >= UsageLimit`,
		// never off a magic constant.
		{"tiny limit at full", Account{Enabled: true, UsageCurrent: 5, UsageLimit: 5}, true},
		{"500 at full", Account{Enabled: true, UsageCurrent: 500, UsageLimit: 500}, true},
		{"2000 at full", Account{Enabled: true, UsageCurrent: 2000, UsageLimit: 2000}, true},
		{"5000 at full", Account{Enabled: true, UsageCurrent: 5000, UsageLimit: 5000}, true},
		{"large limit at full", Account{Enabled: true, UsageCurrent: 100000, UsageLimit: 100000}, true},
		{"fractional limit at full", Account{Enabled: true, UsageCurrent: 12.5, UsageLimit: 12.5}, true},
		{"large limit one credit short", Account{UsageCurrent: 1999, UsageLimit: 2000}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsQuotaExhausted(c.acc); got != c.want {
				t.Errorf("IsQuotaExhausted(%+v) = %v, want %v", c.acc, got, c.want)
			}
		})
	}
}

// TestApplyAutoDisableTransition_Disable covers the (Enabled, fresh, Q=true)
// → (Disabled, AutoDisabledAtFull=true, Q=true) transition. This is the core
// "1000/1000 must auto-disable" case the user asked for.
func TestApplyAutoDisableTransition_Disable(t *testing.T) {
	a := Account{Enabled: true, UsageCurrent: 1000, UsageLimit: 1000}
	if !applyAutoDisableTransition(&a, false) {
		t.Fatal("expected flipped=true")
	}
	if a.Enabled {
		t.Fatal("expected Enabled=false after auto-disable")
	}
	if !a.AutoDisabledAtFull {
		t.Fatal("expected AutoDisabledAtFull=true after auto-disable")
	}
}

// TestApplyAutoDisableTransition_DisableOnTrialOnly covers the trial-only
// quota case — an account whose paid limit is 0 (free trial only) but trial
// is at 50/50 must auto-disable.
func TestApplyAutoDisableTransition_DisableOnTrialOnly(t *testing.T) {
	a := Account{Enabled: true, TrialUsageCurrent: 50, TrialUsageLimit: 50}
	if !applyAutoDisableTransition(&a, false) {
		t.Fatal("expected flipped=true on trial exhaustion")
	}
	if a.Enabled || !a.AutoDisabledAtFull {
		t.Fatalf("expected (false,true) state, got Enabled=%v AutoDisabledAtFull=%v", a.Enabled, a.AutoDisabledAtFull)
	}
}

// TestApplyAutoDisableTransition_Recover covers the auto-recovery side: an
// account that was auto-disabled and now has fresh quota (e.g. billing reset)
// must come back online without operator intervention.
func TestApplyAutoDisableTransition_Recover(t *testing.T) {
	a := Account{Enabled: false, AutoDisabledAtFull: true, UsageCurrent: 0, UsageLimit: 1000}
	if !applyAutoDisableTransition(&a, false) {
		t.Fatal("expected flipped=true on recover")
	}
	if !a.Enabled {
		t.Fatal("expected Enabled=true after recover")
	}
	if a.AutoDisabledAtFull {
		t.Fatal("expected AutoDisabledAtFull cleared after recover")
	}
}

// TestApplyAutoDisableTransition_RespectsManualDisable covers the safety
// invariant: an operator-disabled account (Enabled=false, AutoDisabledAtFull=false)
// must NOT be auto-re-enabled by a refresh that shows healthy quota.
func TestApplyAutoDisableTransition_RespectsManualDisable(t *testing.T) {
	a := Account{Enabled: false, AutoDisabledAtFull: false, UsageCurrent: 0, UsageLimit: 1000}
	if applyAutoDisableTransition(&a, false) {
		t.Fatal("manually-disabled account must not auto-recover")
	}
	if a.Enabled {
		t.Fatal("Enabled must remain false")
	}
}

// TestApplyAutoDisableTransition_NoOps covers stable states that must NOT
// flip on every refresh (would cause flap and pointless Save() calls).
func TestApplyAutoDisableTransition_NoOps(t *testing.T) {
	cases := []Account{
		{Enabled: true, UsageCurrent: 50, UsageLimit: 1000},                              // healthy — no change
		{Enabled: false, AutoDisabledAtFull: true, UsageCurrent: 1000, UsageLimit: 1000}, // still over limit — stay disabled
		{Enabled: false, AutoDisabledAtFull: false, UsageCurrent: 1000, UsageLimit: 1000}, // manual + over limit — leave alone
	}
	for _, a := range cases {
		before := a
		if applyAutoDisableTransition(&a, false) {
			t.Errorf("unexpected flip for %+v → %+v", before, a)
		}
	}
}

// TestApplyAutoDisableTransition_PerAccountOverageSuppresses covers the
// interaction with the per-account AllowOverage flag. An account flagged for
// overage must stay Enabled at full usage so it can keep serving at its
// reduced overage weight (1..10) — auto-disable would defeat the operator's
// explicit choice to allow overage on that account.
func TestApplyAutoDisableTransition_PerAccountOverageSuppresses(t *testing.T) {
	a := Account{Enabled: true, AllowOverage: true, UsageCurrent: 1000, UsageLimit: 1000}
	if applyAutoDisableTransition(&a, false) {
		t.Fatal("overage-enabled account must not auto-disable at full")
	}
	if !a.Enabled {
		t.Fatal("Enabled must remain true")
	}
	if a.AutoDisabledAtFull {
		t.Fatal("AutoDisabledAtFull must remain false")
	}
}

// TestApplyAutoDisableTransition_GlobalOverageSuppresses mirrors the above
// for the global cfg.AllowOverUsage flag, passed in by the caller.
func TestApplyAutoDisableTransition_GlobalOverageSuppresses(t *testing.T) {
	a := Account{Enabled: true, UsageCurrent: 1000, UsageLimit: 1000}
	if applyAutoDisableTransition(&a, true) {
		t.Fatal("global overage must suppress auto-disable")
	}
	if !a.Enabled {
		t.Fatal("Enabled must remain true under global overage")
	}
}

// TestApplyAutoDisableTransition_OverageRecoversAutoDisabled covers a subtle
// recovery path: an account was auto-disabled before AllowOverage was turned
// on. The next refresh under overage must bring it back without waiting for
// quota reset, otherwise the operator's flag flip is silently ineffective.
func TestApplyAutoDisableTransition_OverageRecoversAutoDisabled(t *testing.T) {
	a := Account{
		Enabled:            false,
		AutoDisabledAtFull: true,
		AllowOverage:       true,
		UsageCurrent:       1000,
		UsageLimit:         1000,
	}
	if !applyAutoDisableTransition(&a, false) {
		t.Fatal("expected flip back to enabled when overage gets turned on")
	}
	if !a.Enabled {
		t.Fatal("Enabled must be restored")
	}
	if a.AutoDisabledAtFull {
		t.Fatal("AutoDisabledAtFull must be cleared on overage recovery")
	}
}

// TestApplyAutoDisableTransition_OverageDoesNotOverrideManualDisable pins
// the invariant that overage NEVER causes a manually-disabled account
// (Enabled=false, AutoDisabledAtFull=false) to flip back on. Recovery only
// applies to accounts the auto-disable feature itself disabled.
func TestApplyAutoDisableTransition_OverageDoesNotOverrideManualDisable(t *testing.T) {
	cases := []struct {
		name string
		acc  Account
		glob bool
	}{
		{"per-account overage on, manually disabled, full quota",
			Account{Enabled: false, AutoDisabledAtFull: false, AllowOverage: true, UsageCurrent: 1000, UsageLimit: 1000},
			false},
		{"global overage on, manually disabled, full quota",
			Account{Enabled: false, AutoDisabledAtFull: false, UsageCurrent: 1000, UsageLimit: 1000},
			true},
		{"per-account overage on, manually disabled, healthy quota",
			Account{Enabled: false, AutoDisabledAtFull: false, AllowOverage: true, UsageCurrent: 0, UsageLimit: 1000},
			false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := c.acc
			if applyAutoDisableTransition(&a, c.glob) {
				t.Fatalf("manually-disabled account flipped under overage: %+v", a)
			}
			if a.Enabled {
				t.Fatal("Enabled must remain false")
			}
		})
	}
}

// TestUpdateAccountInfo_AutoDisablesAtFull is the integration test through
// the public seam. It mirrors what the lazy-refresh path does in production:
// pull info, hand it to UpdateAccountInfo, expect Enabled to flip and the
// caller to be told (via the bool return) that it should Reload.
func TestUpdateAccountInfo_AutoDisablesAtFull(t *testing.T) {
	initTestConfig(t)
	if err := AddAccount(Account{
		ID:           "a1",
		RefreshToken: "rt",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	flipped, err := UpdateAccountInfo("a1", AccountInfo{
		UsageCurrent: 1000,
		UsageLimit:   1000,
	})
	if err != nil {
		t.Fatalf("UpdateAccountInfo: %v", err)
	}
	if !flipped {
		t.Fatal("expected flipped=true on auto-disable")
	}

	saved := GetAccounts()
	if len(saved) != 1 {
		t.Fatalf("expected 1 account, got %d", len(saved))
	}
	if saved[0].Enabled {
		t.Fatal("auto-disable must persist Enabled=false")
	}
	if !saved[0].AutoDisabledAtFull {
		t.Fatal("auto-disable must persist AutoDisabledAtFull=true")
	}
}

// TestUpdateAccountInfo_AutoRecovers covers the round trip through
// UpdateAccountInfo: an auto-disabled account with fresh quota recovers and
// the caller is signalled.
func TestUpdateAccountInfo_AutoRecovers(t *testing.T) {
	initTestConfig(t)
	if err := AddAccount(Account{
		ID:                 "a1",
		RefreshToken:       "rt",
		Enabled:            false,
		AutoDisabledAtFull: true,
		UsageCurrent:       1000,
		UsageLimit:         1000,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	flipped, err := UpdateAccountInfo("a1", AccountInfo{
		UsageCurrent: 0,
		UsageLimit:   1000,
	})
	if err != nil {
		t.Fatalf("UpdateAccountInfo: %v", err)
	}
	if !flipped {
		t.Fatal("expected flipped=true on auto-recover")
	}

	saved := GetAccounts()[0]
	if !saved.Enabled {
		t.Fatal("auto-recover must persist Enabled=true")
	}
	if saved.AutoDisabledAtFull {
		t.Fatal("auto-recover must clear AutoDisabledAtFull")
	}
}

// TestUpdateAccountInfo_RespectsManualDisable confirms the integration-level
// safety: a refresh on a manually-disabled account never flips Enabled, even
// when quota is healthy.
func TestUpdateAccountInfo_RespectsManualDisable(t *testing.T) {
	initTestConfig(t)
	if err := AddAccount(Account{
		ID:                 "a1",
		RefreshToken:       "rt",
		Enabled:            false,
		AutoDisabledAtFull: false, // operator disabled
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	flipped, err := UpdateAccountInfo("a1", AccountInfo{
		UsageCurrent: 0,
		UsageLimit:   1000,
	})
	if err != nil {
		t.Fatalf("UpdateAccountInfo: %v", err)
	}
	if flipped {
		t.Fatal("manually-disabled account must not flip on refresh")
	}
	if GetAccounts()[0].Enabled {
		t.Fatal("Enabled must remain false")
	}
}

// TestUpdateAccountInfo_GlobalOverageSuppressesAutoDisable confirms that the
// public seam reads cfg.AllowOverUsage and forwards it into the transition.
// Without this wiring the global flag would be silently ignored on the
// refresh-driven auto-disable path.
func TestUpdateAccountInfo_GlobalOverageSuppressesAutoDisable(t *testing.T) {
	initTestConfig(t)
	if err := UpdateAllowOverUsage(true); err != nil {
		t.Fatalf("UpdateAllowOverUsage: %v", err)
	}
	if err := AddAccount(Account{
		ID:           "a1",
		RefreshToken: "rt",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	flipped, err := UpdateAccountInfo("a1", AccountInfo{
		UsageCurrent: 1000,
		UsageLimit:   1000,
	})
	if err != nil {
		t.Fatalf("UpdateAccountInfo: %v", err)
	}
	if flipped {
		t.Fatal("global overage must suppress refresh-driven auto-disable")
	}
	if !GetAccounts()[0].Enabled {
		t.Fatal("Enabled must remain true under global overage")
	}
}
