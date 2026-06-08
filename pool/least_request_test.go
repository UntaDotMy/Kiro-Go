package pool

import (
	"kiro-go/config"
	"testing"
	"time"
)

// withLeastRequest sets the strategy resolver to least-request for the duration
// of a test and returns the restore func. The production default is
// least-request, but the cfg==nil test fallback resolves to swr, so the tests
// that exercise the LOR/AIMD paths must opt in explicitly.
func withLeastRequest(t *testing.T) {
	t.Helper()
	restore := SetStrategyResolverForTesting(func() string { return "least-request" })
	t.Cleanup(restore)
}

// --- Least-request (LOR) weighted selection -------------------------------

// TestLeastRequestPicksLowestInflight verifies the core LOR property: with
// equal weights, the picker steers to the account with the fewest outstanding
// in-flight requests. We reserve slots on two of three accounts and confirm the
// untouched one wins.
func TestLeastRequestPicksLowestInflight(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}, {ID: "c"}})

	// Manually load a and b so c is the least-busy.
	p.reserveLocked("a")
	p.reserveLocked("a") // a: inflight 2
	p.reserveLocked("b") // b: inflight 1
	// c: inflight 0

	acc, _, ok := p.AcquireForModelExcluding("", nil)
	if !ok || acc == nil {
		t.Fatalf("expected a pick")
	}
	if acc.ID != "c" {
		t.Fatalf("LOR should pick the least-busy account c, got %s", acc.ID)
	}
	// The winning pick reserves a slot on c.
	if got := p.InflightCount("c"); got != 1 {
		t.Fatalf("expected c inflight=1 after acquire, got %d", got)
	}
}

// TestLeastRequestWeightedScore verifies the Envoy weighted form
// score = weight/(inflight+1): a higher-weight account out-scores a lighter one
// even when both are idle, but enough load on the heavy account flips the pick.
func TestLeastRequestWeightedScore(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	p.setAccounts([]config.Account{
		{ID: "heavy", Weight: 5}, // effectiveWeight 50
		{ID: "light", Weight: 1}, // effectiveWeight 10
	})

	// Both idle: heavy (50/1) beats light (10/1).
	acc, _, ok := p.AcquireForModelExcluding("", nil)
	if !ok || acc.ID != "heavy" {
		t.Fatalf("idle: expected heavy to win on weight, got %v", acc)
	}
	p.Release("heavy")

	// Load heavy enough that its score drops below light's: heavy 50/(n+1) < 10/1
	// => n+1 > 5 => n >= 5. Reserve 5 on heavy.
	for i := 0; i < 5; i++ {
		p.reserveLocked("heavy")
	}
	acc, _, ok = p.AcquireForModelExcluding("", nil)
	if !ok || acc.ID != "light" {
		t.Fatalf("loaded heavy: expected light to win (50/6 < 10/1), got %v", acc)
	}
}

// TestTTFTRoutingPenaltyCap verifies the raised ttftPenaltyCap (10) steers
// traffic to the fast account even when a slow peer carries enough weight that
// the OLD cap of 4 would have kept the slow account winning. Models the live
// pool we measured: one ~1.5s account vs a ~9.6s account (a 6.4x spread that
// fell between the old and new caps).
func TestTTFTRoutingPenaltyCap(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	// Give the SLOW account a 2x weight edge so weight alone favors it. With
	// equal inflight, base scores are slow=20/1, fast=10/1. The TTFT penalty
	// divides the slow account's score by min(ratio, cap):
	//   old cap 4  -> slow 20/4 = 5  vs fast 10  -> fast wins already at 4x...
	// so to make the cap the deciding factor, weight the slow account 5x:
	//   base: slow=50, fast=10; ratio 6.4x.
	//   old cap 4 -> slow 50/4 = 12.5 > fast 10  -> SLOW would win (bug).
	//   new cap 10 (ratio 6.4 applies) -> slow 50/6.4 = 7.8 < fast 10 -> FAST wins.
	p.setAccounts([]config.Account{
		{ID: "slow", Weight: 5}, // effectiveWeight 50
		{ID: "fast", Weight: 1}, // effectiveWeight 10
	})
	p.RecordTTFT("slow", 9600)
	p.RecordTTFT("fast", 1500) // 6.4x faster

	acc, _, ok := p.AcquireForModelExcluding("", nil)
	if !ok || acc == nil {
		t.Fatal("expected a pick")
	}
	if acc.ID != "fast" {
		t.Fatalf("TTFT routing should steer to the fast account at a 6.4x spread (new cap 10), got %s", acc.ID)
	}
}

// TestTTFTRoutingExploresUnmeasured verifies an account with NO TTFT measurement
// is not penalized (factor 1.0), so a fresh account still gets traffic to learn
// its latency rather than being starved by a measured-fast peer. We acquire
// WITHOUT releasing so inflight accumulates — mirroring real concurrent load,
// where the least-request term steers the second pick to the idle fresh account
// once the measured one is in flight.
func TestTTFTRoutingExploresUnmeasured(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	p.setAccounts([]config.Account{
		{ID: "measured", Weight: 1},
		{ID: "fresh", Weight: 1},
	})
	p.RecordTTFT("measured", 1500) // fast; fresh stays unmeasured (factor 1.0)

	// Acquire repeatedly without releasing. If the unmeasured account were
	// penalized (factor > 1), it would never win; with factor 1.0 the
	// least-request term hands it the pick once "measured" has an in-flight slot.
	freshPicked := false
	for i := 0; i < 4; i++ {
		acc, _, ok := p.AcquireForModelExcluding("", nil)
		if !ok || acc == nil {
			t.Fatal("expected a pick")
		}
		if acc.ID == "fresh" {
			freshPicked = true
			break
		}
	}
	if !freshPicked {
		t.Fatal("an unmeasured account must still receive traffic to learn its TTFT")
	}
}

// TestLeastRequestSaturationShed verifies that when every eligible account is at
// its AIMD concurrency limit, the reserving picker returns ok=false with the
// saturation poll hint instead of overcommitting a slot.
func TestLeastRequestSaturationShed(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}})

	// Drive both accounts up to the initial limit (aimdInitialLimit).
	for _, id := range []string{"a", "b"} {
		for i := 0; i < aimdInitialLimit; i++ {
			p.reserveLocked(id)
		}
	}

	acc, retryAfter, ok := p.AcquireForModelExcluding("", nil)
	if ok || acc != nil {
		t.Fatalf("expected saturation shed (no pick) when all at limit, got %v", acc)
	}
	if retryAfter != saturationPollInterval {
		t.Fatalf("expected saturation poll hint %s, got %s", saturationPollInterval, retryAfter)
	}
}

// TestNonReservingPickerIgnoresAIMDGate confirms GetNextForModel (the
// non-reserving picker) does NOT apply the concurrency gate: even with both
// accounts "saturated" it still returns a pick and reserves nothing. This is
// the lower-volume single-account path (Responses/Codex, web-search side-call).
func TestNonReservingPickerIgnoresAIMDGate(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	for i := 0; i < aimdInitialLimit+5; i++ {
		p.reserveLocked("a")
	}
	before := p.InflightCount("a")

	acc, _, ok := p.GetNextForModel("")
	if !ok || acc == nil {
		t.Fatal("non-reserving picker should still pick past the AIMD limit")
	}
	if after := p.InflightCount("a"); after != before {
		t.Fatalf("non-reserving picker must not reserve a slot: inflight %d -> %d", before, after)
	}
}

// --- Acquire / Release accounting -----------------------------------------

// TestAcquireReleaseAccounting verifies the in-flight counter increments on a
// reserving acquire and decrements on Release, and that Release floors at zero
// (never goes negative on an extra call).
func TestAcquireReleaseAccounting(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	a1, _, ok := p.AcquireForModelExcluding("", nil)
	if !ok || a1 == nil {
		t.Fatal("expected first acquire to succeed")
	}
	if got := p.InflightCount("a"); got != 1 {
		t.Fatalf("expected inflight=1 after acquire, got %d", got)
	}

	p.Release("a")
	if got := p.InflightCount("a"); got != 0 {
		t.Fatalf("expected inflight=0 after release, got %d", got)
	}
	// Extra release must not underflow.
	p.Release("a")
	if got := p.InflightCount("a"); got != 0 {
		t.Fatalf("extra Release must floor at 0, got %d", got)
	}
}

// TestReleaseUnknownAccountIsNoOp verifies Release on an id that never reserved
// is a harmless no-op (the non-least-request strategies rely on this).
func TestReleaseUnknownAccountIsNoOp(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})
	p.Release("a")       // no entry yet
	p.Release("ghost")   // never existed
	if got := p.InflightCount("a"); got != 0 {
		t.Fatalf("expected inflight=0, got %d", got)
	}
}

// --- AIMD grow / shrink ----------------------------------------------------

// TestAIMDGrowOnSuccessGatedByUsage verifies the increase only fires when the
// account is actually using its limit (inflight >= limit-1): a success on an idle
// account must NOT grow the limit (which would let the next burst overshoot),
// while a success at the ceiling grows it via slow-start (limit += limit/2).
func TestAIMDGrowOnSuccessGatedByUsage(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	// Seed an entry at the initial limit with no inflight, then a success: the
	// limit must stay put (idle account well below the limit-1 growth gate).
	p.cooldowns["a"] = &cooldownEntry{limit: aimdInitialLimit, inflight: 0}
	p.RecordSuccess("a")
	if _, limit := p.ConcurrencyState("a"); limit != aimdInitialLimit {
		t.Fatalf("idle success must not grow limit; expected %d, got %d", aimdInitialLimit, limit)
	}

	// Now saturate (inflight == limit) and record a success: with no ssthresh
	// yet the account is in slow-start, so the limit grows multiplicatively by
	// limit/2 (12 -> 18), not by +1.
	p.cooldowns["a"] = &cooldownEntry{limit: aimdInitialLimit, inflight: aimdInitialLimit}
	p.RecordSuccess("a")
	want := aimdInitialLimit + aimdInitialLimit/2
	if _, limit := p.ConcurrencyState("a"); limit != want {
		t.Fatalf("at-capacity slow-start success should grow limit to %d, got %d", want, limit)
	}
}

// TestSlowStartReachesMaxFast verifies the headline property of the rework: a
// fresh, fully-saturated account climbs from the initial window to the max
// ceiling in a handful of successes (geometric slow-start), not the ~36 additive
// steps the old +1 loop needed. With initial 12 / max 48 it should take <= 4.
func TestSlowStartReachesMaxFast(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	// Keep the account pinned at capacity so every success counts, no ssthresh
	// (never 429ed) so it stays in slow-start the whole way.
	steps := 0
	for {
		_, limit := p.ConcurrencyState("a")
		if limit >= aimdMaxLimit {
			break
		}
		p.cooldowns["a"] = &cooldownEntry{limit: limit, inflight: limit}
		p.RecordSuccess("a")
		steps++
		if steps > 10 {
			t.Fatalf("slow-start took too many steps to reach max (%d); ramp is not geometric", steps)
		}
	}
	if steps > 4 {
		t.Fatalf("slow-start should reach max in <= 4 saturated successes, took %d", steps)
	}
}

// TestSsthreshMemoryRecoversNearCeiling verifies the remembered-ceiling
// behavior: after a 429 backs the limit off to 3/4 and records that point as
// ssthresh, subsequent at-capacity successes grow ADDITIVELY (+1) rather than
// slow-starting past the discovered ceiling — so the account probes gently right
// where it last throttled instead of slamming back into the bucket.
func TestSsthreshMemoryRecoversNearCeiling(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	// Drive the limit up to the max via slow-start, then trip a 429.
	p.cooldowns["a"] = &cooldownEntry{limit: aimdMaxLimit, inflight: aimdMaxLimit}
	p.RecordError("a", true, time.Second)
	cd := p.cooldowns["a"]
	wantCeiling := aimdMaxLimit * aimdDecreaseNum / aimdDecreaseDen // 48*3/4 = 36
	if cd.ssthresh != wantCeiling {
		t.Fatalf("429 should record ssthresh=%d (the backed-off ceiling), got %d", wantCeiling, cd.ssthresh)
	}
	if cd.limit != wantCeiling {
		t.Fatalf("429 should drop limit to %d (not collapse to 1), got %d", wantCeiling, cd.limit)
	}

	// Clear the cooldown timer so the account is eligible, then a saturated
	// success: limit == ssthresh, so we're in congestion-avoidance -> +1 only.
	cd.until = time.Time{}
	cd.inflight = cd.limit
	p.RecordSuccess("a")
	if _, limit := p.ConcurrencyState("a"); limit != wantCeiling+1 {
		t.Fatalf("at-ceiling success should grow additively to %d, got %d", wantCeiling+1, limit)
	}
}

// TestConfiguredConcurrencyBounds verifies the picker honors the
// concurrencyResolver: a fresh account starts at the configured initial limit
// and the slow-start ramp caps at the configured max, not the built-in defaults.
func TestConfiguredConcurrencyBounds(t *testing.T) {
	withLeastRequest(t)
	restore := SetConcurrencyResolverForTesting(func() (int, int) { return 3, 5 })
	defer restore()

	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	// First reserving acquire lazily resolves + caches the configured bounds and
	// seeds the entry at the configured initial limit (3).
	acc, _, ok := p.AcquireForModelExcluding("", nil)
	if !ok || acc == nil {
		t.Fatal("expected an acquire")
	}
	p.Release("a")
	if _, limit := p.ConcurrencyState("a"); limit != 3 {
		t.Fatalf("account should start at configured initial 3, got %d", limit)
	}

	// Saturated successes must cap at the configured max (5), never the default 48.
	for i := 0; i < 10; i++ {
		_, limit := p.ConcurrencyState("a")
		p.cooldowns["a"] = &cooldownEntry{limit: limit, inflight: limit}
		p.RecordSuccess("a")
	}
	if _, limit := p.ConcurrencyState("a"); limit != 5 {
		t.Fatalf("limit must cap at configured max 5, got %d", limit)
	}
}

// TestAIMDGrowCappedAtMax verifies additive-increase never climbs past
// aimdMaxLimit no matter how many at-capacity successes land.
func TestAIMDGrowCappedAtMax(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	// Pin inflight at a high value so every success is "at capacity".
	p.cooldowns["a"] = &cooldownEntry{limit: aimdInitialLimit, inflight: aimdMaxLimit + 10}
	for i := 0; i < 50; i++ {
		p.RecordSuccess("a")
	}
	if _, limit := p.ConcurrencyState("a"); limit != aimdMaxLimit {
		t.Fatalf("limit must cap at %d, got %d", aimdMaxLimit, limit)
	}
}

// TestAIMDShrinkOnQuotaError verifies the multiplicative-decrease (x3/4) fires
// on a quota error and floors at aimdMinLimit.
func TestAIMDShrinkOnQuotaError(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	// Start at the max limit, then a 429: limit -> floor(12*3/4) = 9.
	p.cooldowns["a"] = &cooldownEntry{limit: aimdMaxLimit}
	p.RecordError("a", true, time.Second)
	if _, limit := p.ConcurrencyState("a"); limit != aimdMaxLimit*aimdDecreaseNum/aimdDecreaseDen {
		t.Fatalf("expected limit %d after one 429, got %d", aimdMaxLimit*aimdDecreaseNum/aimdDecreaseDen, limit)
	}

	// Many consecutive 429s collapse the limit toward the floor, never below it.
	for i := 0; i < 30; i++ {
		p.RecordError("a", true, time.Second)
	}
	if _, limit := p.ConcurrencyState("a"); limit != aimdMinLimit {
		t.Fatalf("limit must floor at %d, got %d", aimdMinLimit, limit)
	}
}

// TestAIMDNoShrinkOnNonQuotaError verifies a non-quota error does NOT touch the
// AIMD limit — only quota (429) errors signal the bucket is full.
func TestAIMDNoShrinkOnNonQuotaError(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	p.cooldowns["a"] = &cooldownEntry{limit: aimdMaxLimit}
	// Three non-quota strikes (enough to trigger the cooldown) but not the AIMD cut.
	p.RecordError("a", false, 0)
	p.RecordError("a", false, 0)
	p.RecordError("a", false, 0)
	if _, limit := p.ConcurrencyState("a"); limit != aimdMaxLimit {
		t.Fatalf("non-quota error must not shrink AIMD limit; expected %d, got %d", aimdMaxLimit, limit)
	}
}

// TestFloorAfterCooldown verifies the post-storm behavior: after a 429
// storm drives the AIMD limit to the floor (aimdMinLimit, currently 2),
// the account admits up to the floor of concurrent requests before
// shedding further acquires with the saturation hint. The floor exists
// to keep a recovered account productive immediately under a small
// burst; the prior 1-slot floor (single-probe pattern) starved a
// recovered account of any parallelism and compounded the dispatcher's
// admission-wait budget into a client-visible stall.
func TestFloorAfterCooldown(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	// Collapse the limit to the floor with repeated 429s, then clear the
	// cooldown timer so the account is eligible (simulating cooldown expiry).
	for i := 0; i < 10; i++ {
		p.RecordError("a", true, time.Second)
	}
	p.cooldowns["a"].until = time.Time{}
	if _, limit := p.ConcurrencyState("a"); limit != aimdMinLimit {
		t.Fatalf("expected limit floored at %d after storm, got %d", aimdMinLimit, limit)
	}

	// The account now admits up to aimdMinLimit concurrent in-flight before
	// shedding. Acquire exactly that many — all must succeed.
	for i := 0; i < aimdMinLimit; i++ {
		acc, _, ok := p.AcquireForModelExcluding("", nil)
		if !ok || acc == nil {
			t.Fatalf("probe %d/%d should be admitted at the floor, got ok=%v", i+1, aimdMinLimit, ok)
		}
	}
	// The next acquire is shed: the only account is at its limit (= aimdMinLimit).
	acc, retryAfter, ok := p.AcquireForModelExcluding("", nil)
	if ok || acc != nil {
		t.Fatalf("acquire past the floor (inflight=%d) must be shed, got %v", aimdMinLimit, acc)
	}
	if retryAfter != saturationPollInterval {
		t.Fatalf("expected saturation hint %s, got %s", saturationPollInterval, retryAfter)
	}
}

// --- decayCountersLocked preserving inflight -------------------------------

// TestDecayPreservesInflight verifies the leak-safety fix: an entry whose error
// history has aged out but which still has reserved in-flight slots must NOT be
// deleted (that would lose the count and let a burst overshoot the AIMD limit).
// The error fields reset but inflight survives.
func TestDecayPreservesInflight(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	// Stale error history + a live in-flight slot + an expired cooldown.
	p.cooldowns["a"] = &cooldownEntry{
		inflight:        2,
		limit:           5,
		lastSleep:       30 * time.Second,
		consecutiveErrs: 3,
		lastErrorAt:     time.Now().Add(-2 * errorCounterDecay),
		until:           time.Now().Add(-time.Minute), // expired
	}

	// A pick triggers decayCountersLocked.
	_, _, _ = p.AcquireForModelExcluding("", map[string]bool{"a": true}) // exclude so we don't reserve more

	cd, exists := p.cooldowns["a"]
	if !exists {
		t.Fatal("entry with live inflight must NOT be decayed away")
	}
	if cd.inflight != 2 {
		t.Fatalf("inflight must be preserved through decay, got %d", cd.inflight)
	}
	// Error history should have been reset.
	if cd.consecutiveErrs != 0 || cd.lastSleep != 0 || !cd.lastErrorAt.IsZero() {
		t.Fatalf("error history should reset on decay: %+v", cd)
	}
}

// TestDecayDropsIdleStaleEntry is the complement: an entry with NO inflight and
// stale error history past an expired cooldown is dropped entirely (the existing
// behavior, re-pinned here against the inflight-preservation branch).
func TestDecayDropsIdleStaleEntry(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	p.cooldowns["a"] = &cooldownEntry{
		inflight:    0,
		lastSleep:   30 * time.Second,
		lastErrorAt: time.Now().Add(-2 * errorCounterDecay),
	}
	_, _, _ = p.GetNextForModel("")
	if _, exists := p.cooldowns["a"]; exists {
		t.Fatal("idle stale entry should be decayed away")
	}
}

// --- ConcurrencyState ------------------------------------------------------

// TestConcurrencyStateReportsInitialForUnknown verifies an account with no
// cooldown entry reports (0 inflight, aimdInitialLimit) — what the picker would
// enforce on its first use — so the dashboard shows a sensible default.
func TestConcurrencyStateReportsInitialForUnknown(t *testing.T) {
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "fresh"}})
	inflight, limit := p.ConcurrencyState("fresh")
	if inflight != 0 || limit != aimdInitialLimit {
		t.Fatalf("expected (0, %d) for a fresh account, got (%d, %d)", aimdInitialLimit, inflight, limit)
	}
}

// TestConcurrencyStateReflectsLiveCounts verifies it reports the live reserved
// count and the current (possibly grown/shrunk) limit.
func TestConcurrencyStateReflectsLiveCounts(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	p.cooldowns["a"] = &cooldownEntry{inflight: 3, limit: 7}
	inflight, limit := p.ConcurrencyState("a")
	if inflight != 3 || limit != 7 {
		t.Fatalf("expected (3, 7), got (%d, %d)", inflight, limit)
	}
}

// --- Strategy isolation ----------------------------------------------------

// TestSwrStrategyReservesNothing confirms that under the swr strategy the
// reserving picker does NOT reserve a slot (the AIMD path is least-request only),
// so swr behavior is byte-for-byte unchanged.
func TestSwrStrategyReservesNothing(t *testing.T) {
	restore := SetStrategyResolverForTesting(func() string { return "swr" })
	defer restore()
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}, {ID: "b"}})

	acc, _, ok := p.AcquireForModelExcluding("", nil)
	if !ok || acc == nil {
		t.Fatal("expected a swr pick")
	}
	if got := p.InflightCount(acc.ID); got != 0 {
		t.Fatalf("swr must not reserve an in-flight slot, got inflight=%d for %s", got, acc.ID)
	}
}

// --- event-driven admission: Release signals waiters ----------------------

// TestReleaseSignalsWaiter verifies the Phase-B event-driven admission wakeup:
// releasing a real in-flight slot pushes a signal on ReleaseSignal so an
// admission waiter wakes immediately instead of polling.
func TestReleaseSignalsWaiter(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	// Reserve a slot, then drain any signal the setup may have produced.
	acc, _, ok := p.AcquireForModelExcluding("", nil)
	if !ok || acc == nil {
		t.Fatal("expected an acquire")
	}
	select {
	case <-p.ReleaseSignal():
	default:
	}

	// Release the slot — a signal must be delivered.
	p.Release(acc.ID)
	select {
	case <-p.ReleaseSignal():
		// good: woken
	case <-time.After(time.Second):
		t.Fatal("Release did not signal ReleaseSignal within 1s")
	}
}

// TestReleaseSignalCoalescesAndNeverBlocks verifies the non-blocking,
// buffered-1 contract: many concurrent releases never block the releaser, and
// they coalesce into at most one pending wakeup (a woken waiter re-attempts
// Acquire, so a dropped duplicate signal is harmless).
func TestReleaseSignalCoalescesAndNeverBlocks(t *testing.T) {
	withLeastRequest(t)
	p := newTestPool()
	p.setAccounts([]config.Account{{ID: "a"}})

	// Reserve several slots so each Release frees one (and thus signals).
	for i := 0; i < 5; i++ {
		if _, _, ok := p.AcquireForModelExcluding("", nil); !ok {
			// limit may cap before 5; that's fine for this test.
			break
		}
	}
	// Fire many releases with no reader draining between them. Each must return
	// promptly (non-blocking send); the buffer-1 channel coalesces them.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			p.Release("a")
		}
		close(done)
	}()
	select {
	case <-done:
		// good: never blocked despite no one draining the signal between sends
	case <-time.After(2 * time.Second):
		t.Fatal("Release blocked — signal send must be non-blocking")
	}
	// At most one pending signal remains.
	<-p.ReleaseSignal()
	select {
	case <-p.ReleaseSignal():
		t.Fatal("expected signals to coalesce to at most one pending wakeup")
	default:
	}
}
