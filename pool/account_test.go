package pool

import (
	"kiro-go/config"
	"testing"
	"time"
)

// newTestPool creates an empty pool with no goroutines or config-file side
// effects. Tests populate p.accounts and p.slots directly via setAccounts.
func newTestPool() *AccountPool {
	return &AccountPool{
		cooldowns:  make(map[string]*cooldownEntry),
		modelLists: make(map[string]map[string]bool),
	}
}

// setAccounts mirrors what Reload() does, but accepts arbitrary accounts so
// tests don't have to round-trip through config storage.
func (p *AccountPool) setAccounts(accounts []config.Account) {
	p.accounts = accounts
	p.slots = make([]*accountSlot, 0, len(accounts))
	for _, a := range accounts {
		w := computeEffectiveWeight(a)
		if w <= 0 {
			continue
		}
		p.slots = append(p.slots, &accountSlot{effectiveWeight: w})
	}
}

func TestOverageAccountsAreSkippedByDefault(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{
		{ID: "normal"},
		{ID: "over", UsageCurrent: 10, UsageLimit: 10},
	})

	for i := 0; i < 5; i++ {
		acc, _, ok := p.GetNextForModel("")
		if !ok || acc == nil {
			t.Fatalf("expected an account on iteration %d", i)
		}
		if acc.ID == "over" {
			t.Fatalf("expected over-limit account to be skipped by default")
		}
	}
}

func TestOverageAccountsCanBeSelectedWhenAllowed(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{
		{ID: "over", UsageCurrent: 10, UsageLimit: 10, AllowOverage: true, OverageWeight: 1},
	})

	acc, _, ok := p.GetNextForModel("")
	if !ok || acc == nil {
		t.Fatalf("expected allowed overage account")
	}
	if acc.ID != "over" {
		t.Fatalf("expected overage account, got %q", acc.ID)
	}
}

func TestOverageWeightIsLowerThanNormalWeight(t *testing.T) {
	normalWeight := computeEffectiveWeight(config.Account{ID: "n", Weight: 1})
	overageWeight := computeEffectiveWeight(config.Account{
		ID: "o", UsageCurrent: 10, UsageLimit: 10, AllowOverage: true, OverageWeight: 1,
	})
	if overageWeight >= normalWeight {
		t.Fatalf("expected overage weight %d to be lower than normal weight %d", overageWeight, normalWeight)
	}
}

func TestGetNextKeepsFiveMinuteTokenAvailable(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{
		{ID: "acct-1", AccessToken: "access-token", ExpiresAt: time.Now().Unix() + 300},
	})

	acc, _, ok := p.GetNextForModel("")
	if !ok || acc == nil {
		t.Fatalf("expected five-minute token to be available")
	}
	if acc.ID != "acct-1" {
		t.Fatalf("expected acct-1, got %q", acc.ID)
	}
}

func TestGetNextForModelMatchesClaudeDottedAndDashedAliases(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "acct-1"}})

	p.SetModelList("acct-1", []string{"claude-opus-4-7"})
	if account, _, ok := p.GetNextForModel("claude-opus-4.7"); !ok || account == nil || account.ID != "acct-1" {
		t.Fatalf("expected dotted Kiro id to match dashed picker alias, got account=%#v ok=%v", account, ok)
	}

	p.SetModelList("acct-1", []string{"claude-sonnet-4.6"})
	if account, _, ok := p.GetNextForModel("claude-sonnet-4-6"); !ok || account == nil || account.ID != "acct-1" {
		t.Fatalf("expected dashed picker id to match dotted Kiro id, got account=%#v ok=%v", account, ok)
	}
}

func TestSetModelListKeepsStoredModelListExact(t *testing.T) {
	p := newTestPool()
	p.SetModelList("acct-1", []string{"claude-opus-4-7"})

	models := p.GetModelList("acct-1")
	if len(models) != 1 || models[0] != "claude-opus-4-7" {
		t.Fatalf("expected stored model list to stay exact, got %#v", models)
	}
}

func TestGetNextForModelRejectsUnsupportedKnownModel(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "acct-1"}})
	p.SetModelList("acct-1", []string{"claude-sonnet-4.5"})

	if account, _, ok := p.GetNextForModel("claude-opus-4.7"); ok || account != nil {
		t.Fatalf("expected unsupported model to be rejected, got account=%#v ok=%v", account, ok)
	}
}

// TestSWRRSpreadsLoadEvenlyAcrossEqualWeightAccounts verifies that with equal
// weights, 100 sequential picks distribute close to evenly across N accounts —
// the property that protects us from the 429-cascade. The previous slot-
// duplicating algorithm would have produced 10×A, 10×B, 10×C bursts.
func TestSWRRSpreadsLoadEvenlyAcrossEqualWeightAccounts(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{
		{ID: "a"}, {ID: "b"}, {ID: "c"},
	})

	const total = 99
	counts := map[string]int{}
	for i := 0; i < total; i++ {
		acc, _, ok := p.GetNextForModel("")
		if !ok {
			t.Fatalf("expected account on iteration %d", i)
		}
		counts[acc.ID]++
	}

	// With equal weights and SWRR, each account should get exactly total/N picks.
	expected := total / 3
	for id, c := range counts {
		if c != expected {
			t.Fatalf("expected %s to be picked exactly %d times, got %d", id, expected, c)
		}
	}
}

// TestSWRRRespectsWeights confirms that a higher-weighted account gets
// proportionally more picks than its peers.
func TestSWRRRespectsWeights(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{
		{ID: "heavy", Weight: 5},
		{ID: "light", Weight: 1},
	})
	// effectiveWeight: heavy=50, light=10 → 5:1 ratio.

	const total = 60
	counts := map[string]int{}
	for i := 0; i < total; i++ {
		acc, _, ok := p.GetNextForModel("")
		if !ok {
			t.Fatalf("expected account on iteration %d", i)
		}
		counts[acc.ID]++
	}

	if counts["heavy"] != 50 || counts["light"] != 10 {
		t.Fatalf("expected 50/10 split, got heavy=%d light=%d", counts["heavy"], counts["light"])
	}
}

// TestSWRRDoesNotBurstOnFirstFewPicks is the regression test for the original
// 429 cascade: the OLD algorithm appended 10 copies of each account and would
// pick the SAME account for the first 10 calls. SWRR must rotate immediately.
func TestSWRRDoesNotBurstOnFirstFewPicks(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{
		{ID: "a"}, {ID: "b"}, {ID: "c"},
	})

	picks := make([]string, 6)
	for i := 0; i < 6; i++ {
		acc, _, ok := p.GetNextForModel("")
		if !ok {
			t.Fatalf("expected account on iteration %d", i)
		}
		picks[i] = acc.ID
	}

	// First 3 picks must visit all 3 accounts at least once. The old algorithm
	// would have produced ["a","a","a","a","a","a"] here.
	seen := map[string]bool{picks[0]: true, picks[1]: true, picks[2]: true}
	if len(seen) != 3 {
		t.Fatalf("first 3 picks should cover all 3 accounts, got %v", picks[:3])
	}
}

// TestRecordErrorTieredCooldown verifies the cooldown ladder: 1st 429 lands
// in the decorrelated-jitter starting window, and the consecutive run is
// always clamped at softCooldownMax (no overflow).
func TestRecordErrorTieredCooldown(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	// 1st quota error without Retry-After: decorrelated jitter on the
	// initial step uses random(retryAfterMin, softCooldownBase × 3) since
	// lastSleep is zero and we seed it with softCooldownBase. Clamping to
	// retryAfterMin on the floor and softCooldownMax on the cap, the first
	// draw must satisfy retryAfterMin ≤ d1 ≤ min(softCooldownBase*3, softCooldownMax).
	p.RecordError("a", true, 0)
	d1 := p.CooldownRemaining("a")
	upper := softCooldownBase * 3
	if upper > softCooldownMax {
		upper = softCooldownMax
	}
	if d1 < retryAfterMin-time.Second || d1 > upper+time.Second {
		t.Fatalf("1st cooldown should land in [%s, %s], got %s", retryAfterMin, upper, d1)
	}

	// Many consecutive errors: cooldown ceiling grows but is clamped at softCooldownMax.
	for i := 0; i < 20; i++ {
		p.RecordError("a", true, 0)
	}
	dN := p.CooldownRemaining("a")
	if dN > softCooldownMax+time.Second {
		t.Fatalf("cooldown should be clamped at %s, got %s", softCooldownMax, dN)
	}
}

// TestRecordErrorHonorsRetryAfter checks that a server-supplied Retry-After
// is used directly (clamped) instead of computing a backoff.
func TestRecordErrorHonorsRetryAfter(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	// Server says retry in 45 seconds. We should use exactly that (within ±1s).
	p.RecordError("a", true, 45*time.Second)
	d := p.CooldownRemaining("a")
	if d < 44*time.Second || d > 46*time.Second {
		t.Fatalf("expected ~45s cooldown, got %s", d)
	}

	// Tiny Retry-After (1s) is now honored directly: dropping the 5s clamp
	// floor is intentional. Upstream sometimes hints "retry in 1s" during
	// light throttling and burning four extra seconds wasted free capacity.
	// We still honor retryAfterAbsoluteMin (1s) so a misformed 0/negative
	// header can't pin the account on a hot loop.
	p2 := newTestPool()
	p2.setAccounts([]config.Account{{ID: "b"}})
	p2.RecordError("b", true, 1*time.Second)
	d2 := p2.CooldownRemaining("b")
	if d2 < retryAfterAbsoluteMin-time.Second || d2 > retryAfterAbsoluteMin+time.Second {
		t.Fatalf("expected cooldown ~%s, got %s", retryAfterAbsoluteMin, d2)
	}

	// Huge Retry-After should be clamped down to retryAfterMax.
	p3 := newTestPool()
	p3.setAccounts([]config.Account{{ID: "c"}})
	p3.RecordError("c", true, 24*time.Hour)
	d3 := p3.CooldownRemaining("c")
	if d3 > retryAfterMax+time.Second {
		t.Fatalf("expected cooldown clamped to %s, got %s", retryAfterMax, d3)
	}
}

// TestRecordSuccessClearsCooldown ensures one good response unbenches the account.
func TestRecordSuccessClearsCooldown(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	p.RecordError("a", true, 60*time.Second)
	if p.CooldownRemaining("a") == 0 {
		t.Fatal("expected cooldown after RecordError")
	}

	p.RecordSuccess("a")
	if p.CooldownRemaining("a") != 0 {
		t.Fatal("RecordSuccess should clear cooldown")
	}
}

// TestDecorrelatedJitterDesyncsSiblings is a probabilistic regression test:
// when many accounts 429 simultaneously and we redraw their cooldowns, the
// resulting expiries should be spread across the window, not clustered. With
// pure-exponential backoff every account would land in the same shifted
// window every step; with decorrelated jitter, each account's next sleep is
// bounded by its OWN previous sleep, so the spread persists.
//
// We measure spread by counting unique cooldown buckets across 8 accounts
// after 4 consecutive 429s each. We require ≥4 distinct buckets out of 8
// (≥50% diversity) — a comfortable margin over the ~1/8 collision floor.
func TestDecorrelatedJitterDesyncsSiblings(t *testing.T) {
	p := newTestPool()
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	accts := make([]config.Account, len(ids))
	for i, id := range ids {
		accts[i] = config.Account{ID: id}
	}
	p.setAccounts(accts)

	// Hammer all accounts with 4 consecutive 429s.
	for step := 0; step < 4; step++ {
		for _, id := range ids {
			p.RecordError(id, true, 0)
		}
	}

	// Bucket cooldown remainings into 1-second slots.
	buckets := make(map[int]bool, len(ids))
	for _, id := range ids {
		d := p.CooldownRemaining(id)
		buckets[int(d/time.Second)] = true
	}
	if len(buckets) < 4 {
		t.Fatalf("decorrelated jitter should desync siblings; got only %d distinct buckets across %d accounts", len(buckets), len(ids))
	}
}

// TestNonQuotaErrorsRequireThreeStrikes confirms that a transient non-quota
// error (e.g. transport blip) doesn't immediately bench the account.
func TestNonQuotaErrorsRequireThreeStrikes(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	p.RecordError("a", false, 0)
	if p.CooldownRemaining("a") > 0 {
		t.Fatal("1st non-quota error should not cool down")
	}
	p.RecordError("a", false, 0)
	if p.CooldownRemaining("a") > 0 {
		t.Fatal("2nd non-quota error should not cool down")
	}
	p.RecordError("a", false, 0)
	if p.CooldownRemaining("a") == 0 {
		t.Fatal("3rd non-quota error should cool down")
	}
}

// TestGetNextForModelReportsRetryAfterWhenAllCool is the safety net for the
// dry-pool path. With every account cooling, the pool must surface
// (nil, retryAfter, false) so the handler returns 429 instead of trying a
// known-throttled account and amplifying the cascade.
func TestGetNextForModelReportsRetryAfterWhenAllCool(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}})

	p.RecordError("a", true, 30*time.Second)
	p.RecordError("b", true, 60*time.Second)

	acc, retryAfter, ok := p.GetNextForModel("")
	if ok || acc != nil {
		t.Fatalf("expected no account when all are cooling, got ok=%v acc=%v", ok, acc)
	}
	// retryAfter should be ~30s (the soonest expiry).
	if retryAfter < 25*time.Second || retryAfter > 35*time.Second {
		t.Fatalf("expected retryAfter ~30s, got %s", retryAfter)
	}
}

// TestClearSoftCooldownIfHealthy: a successful refresh should let the account
// re-enter rotation immediately.
func TestClearSoftCooldownIfHealthy(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})
	p.RecordError("a", true, 5*time.Minute)
	if p.CooldownRemaining("a") == 0 {
		t.Fatal("expected cooldown")
	}
	p.ClearSoftCooldownIfHealthy("a")
	if p.CooldownRemaining("a") != 0 {
		t.Fatal("ClearSoftCooldownIfHealthy should remove cooldown")
	}
}

// TestErrorCounterDecays: after errorCounterDecay of no errors, the
// consecutive-error counter resets so old failures don't keep amplifying
// backoff long after the account recovered.
func TestErrorCounterDecays(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	// Inject a stale error and zero out the cooldown so decay is the only
	// thing keeping the entry alive.
	p.cooldowns["a"] = &cooldownEntry{
		lastSleep:   30 * time.Second,
		lastErrorAt: time.Now().Add(-2 * errorCounterDecay),
	}

	// A pick triggers decayCountersLocked, which should drop the entry.
	_, _, _ = p.GetNextForModel("")

	if _, exists := p.cooldowns["a"]; exists {
		t.Fatal("expected stale cooldown entry to be decayed")
	}
}

// TestErrorCounterDecaysAfterExpiredCooldown is the regression for a bug
// where decayCountersLocked only fired when until.IsZero() — meaning entries
// that completed their cooldown naturally kept their lastSleep counter
// indefinitely, and the next 429 would resume backoff at the cap.
// After errorCounterDecay of quiet, an expired-cooldown entry must be
// dropped so the next failure starts fresh.
func TestErrorCounterDecaysAfterExpiredCooldown(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	// Cooldown that already expired, with a stale lastErrorAt.
	p.cooldowns["a"] = &cooldownEntry{
		lastSleep:   2 * time.Minute,
		lastErrorAt: time.Now().Add(-2 * errorCounterDecay),
		until:       time.Now().Add(-10 * time.Minute),
	}

	_, _, _ = p.GetNextForModel("")

	if _, exists := p.cooldowns["a"]; exists {
		t.Fatal("expected expired-cooldown entry to be decayed after errorCounterDecay")
	}
}

// TestErrorCounterDecayDoesNotInterruptActiveCooldown protects against a
// regression where decay could prematurely drop a still-cooling account,
// making it look eligible mid-cooldown.
func TestErrorCounterDecayDoesNotInterruptActiveCooldown(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	// Active cooldown but an old lastErrorAt (e.g., the upstream told us
	// to retry in 10 min, and that 10 min started 6 min ago).
	p.cooldowns["a"] = &cooldownEntry{
		lastSleep:   10 * time.Minute,
		lastErrorAt: time.Now().Add(-2 * errorCounterDecay),
		until:       time.Now().Add(4 * time.Minute),
	}

	_, _, _ = p.GetNextForModel("")

	if _, exists := p.cooldowns["a"]; !exists {
		t.Fatal("active cooldown entry must not be decayed while still cooling")
	}
}

// TestSWRRSkipsCoolingAccount ensures a cooling account is removed from
// the rotation while its sibling continues to be picked.
func TestSWRRSkipsCoolingAccount(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}})

	p.RecordError("a", true, 60*time.Second)

	// Every pick for the next minute should hit b.
	for i := 0; i < 5; i++ {
		acc, _, ok := p.GetNextForModel("")
		if !ok || acc == nil {
			t.Fatalf("expected account on iteration %d", i)
		}
		if acc.ID != "b" {
			t.Fatalf("expected b while a is cooling, got %s", acc.ID)
		}
	}
}
