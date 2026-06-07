package pool

import (
	"kiro-go/config"
	"testing"
	"time"
)

func TestEmissionInterval(t *testing.T) {
	if got := emissionInterval(0); got != 0 {
		t.Fatalf("rate 0 should be unpaced (0), got %v", got)
	}
	if got := emissionInterval(-1); got != 0 {
		t.Fatalf("negative rate should be unpaced (0), got %v", got)
	}
	// 2 req/sec -> 500ms spacing.
	if got := emissionInterval(2); got != 500*time.Millisecond {
		t.Fatalf("rate 2 -> %v, want 500ms", got)
	}
}

func TestGcraAdmit(t *testing.T) {
	now := time.Now()
	tau := 100 * time.Millisecond

	// Zero TAT always admits (never paced yet).
	if !gcraAdmit(now, time.Time{}, tau) {
		t.Fatal("zero TAT must admit")
	}
	// TAT in the past: admit.
	if !gcraAdmit(now, now.Add(-time.Second), tau) {
		t.Fatal("past TAT must admit")
	}
	// TAT exactly now: admit (not before).
	if !gcraAdmit(now, now, tau) {
		t.Fatal("TAT == now must admit")
	}
	// TAT just within tolerance ahead: admit (now >= tat - tau).
	if !gcraAdmit(now, now.Add(50*time.Millisecond), tau) {
		t.Fatal("TAT within tau must admit")
	}
	// TAT beyond tolerance ahead: reject (too early).
	if gcraAdmit(now, now.Add(200*time.Millisecond), tau) {
		t.Fatal("TAT beyond tau must reject")
	}
}

func TestGcraAdvance(t *testing.T) {
	now := time.Now()
	emission := 500 * time.Millisecond

	// Idle account (TAT in past): advance from now.
	got := gcraAdvance(now, now.Add(-time.Second), emission)
	if got != now.Add(emission) {
		t.Fatalf("idle advance = %v, want now+emission", got)
	}
	// Busy account (TAT in future): advance from TAT, stepping forward.
	future := now.Add(time.Second)
	got = gcraAdvance(now, future, emission)
	if got != future.Add(emission) {
		t.Fatalf("busy advance = %v, want tat+emission", got)
	}
	// Zero TAT: treated as base now (now is after zero time).
	got = gcraAdvance(now, time.Time{}, emission)
	if got != now.Add(emission) {
		t.Fatalf("zero-TAT advance = %v, want now+emission", got)
	}
}

func TestAimdRateDecrease(t *testing.T) {
	// Snap down to rateDecreaseFactor (0.85) of the observed rate — the
	// "balanced ~90-95%" run-hot target, not a blind halving.
	if got := aimdRateDecrease(10); got != 10*rateDecreaseFactor {
		t.Fatalf("decrease(10) = %v, want %v", got, 10*rateDecreaseFactor)
	}
	// Unmeasured (0) stays UNPACED — a 429 before we measured anything tells us
	// nothing about the rate, so we don't guess; the account stays full-speed.
	if got := aimdRateDecrease(0); got != 0 {
		t.Fatalf("decrease(0) = %v, want 0 (stay unpaced)", got)
	}
	// A measured-but-tiny rate floors instead of going to zero.
	if got := aimdRateDecrease(0.1); got != rateMinPaced {
		t.Fatalf("decrease(0.1) = %v, want floor %v", got, rateMinPaced)
	}
	// Capped at max.
	if got := aimdRateDecrease(rateMaxPaced * 4); got != rateMaxPaced {
		t.Fatalf("decrease past max = %v, want %v", got, rateMaxPaced)
	}
}

func TestAimdRateProbe(t *testing.T) {
	got := aimdRateProbe(10)
	want := 10 * rateProbeFactor
	if got <= 10 || got > want+0.01 || got < want-0.01 {
		t.Fatalf("probe(10) = %v, want %v (×%v)", got, want, rateProbeFactor)
	}
	if got := aimdRateProbe(rateMaxPaced); got != rateMaxPaced {
		t.Fatalf("probe at max must stay capped, got %v", got)
	}
}

func TestUpdateObservedRate(t *testing.T) {
	// First sample seeds directly: 250ms interval -> 4 req/sec.
	first := updateObservedRate(0, 250*time.Millisecond)
	if first < 3.9 || first > 4.1 {
		t.Fatalf("seed sample = %v, want ~4", first)
	}
	// Non-positive interval is ignored (returns prev).
	if got := updateObservedRate(4, 0); got != 4 {
		t.Fatalf("zero interval should return prev, got %v", got)
	}
	// A microsecond-apart pair is clamped, not allowed to explode the EWMA.
	clamped := updateObservedRate(4, time.Microsecond)
	if clamped > rateObservedMaxSample {
		t.Fatalf("EWMA after tiny interval = %v, must be <= clamp %v", clamped, rateObservedMaxSample)
	}
	// EWMA blends toward a new steady sample over repeated observations.
	r := 4.0
	for i := 0; i < 50; i++ {
		r = updateObservedRate(r, 100*time.Millisecond) // 10 req/sec
	}
	if r < 9 || r > 11 {
		t.Fatalf("EWMA converged to %v, want ~10", r)
	}
}

func TestPhaseOffset(t *testing.T) {
	emission := 500 * time.Millisecond
	// Stable for the same id.
	a1 := phaseOffset("account-a", emission)
	a2 := phaseOffset("account-a", emission)
	if a1 != a2 {
		t.Fatalf("phase offset not stable: %v vs %v", a1, a2)
	}
	// Within [0, emission).
	if a1 < 0 || a1 >= emission {
		t.Fatalf("phase offset %v out of range [0,%v)", a1, emission)
	}
	// Two different accounts should (very likely) differ, proving the stagger.
	b := phaseOffset("account-b", emission)
	if a1 == b {
		t.Fatal("expected different accounts to get different phase offsets")
	}
	// Unpaced (emission 0) -> no offset.
	if got := phaseOffset("account-a", 0); got != 0 {
		t.Fatalf("unpaced phase offset = %v, want 0", got)
	}
}

// ---- integration: rate-learning lifecycle through the pool API -------------

// TestRatePacerUnpacedUntilFirst429 verifies an account starts UNPACED (paced
// rate 0) and only becomes paced after a quota error.
func TestRatePacerUnpacedUntilFirst429(t *testing.T) {
	p := NewForTesting()
	p.SetAccountsForTesting([]config.Account{{ID: "a", AccessToken: "t"}})

	// Fresh account: no paced rate.
	if paced, _ := p.RateState("a"); paced != 0 {
		t.Fatalf("fresh account should be unpaced, got paced=%v", paced)
	}

	// Record a couple successes so observedRate has a measurement to snap from.
	p.RecordSuccess("a")
	time.Sleep(5 * time.Millisecond)
	p.RecordSuccess("a")
	if paced, _ := p.RateState("a"); paced != 0 {
		t.Fatalf("success alone must not pace the account, got paced=%v", paced)
	}

	// First quota error → account becomes paced at rateDecreaseFactor of the
	// observed rate (the balanced run-hot snap-down).
	p.RecordError("a", true, 0)
	paced, observed := p.RateState("a")
	if paced <= 0 {
		t.Fatalf("after 429 account must be paced, got paced=%v (observed=%v)", paced, observed)
	}
	if observed > 0 && paced > observed*rateDecreaseFactor+0.01 {
		t.Fatalf("paced rate %v should be ~%v× observed %v", paced, rateDecreaseFactor, observed)
	}
}

// TestRatePacerStaysUnpacedWhenUnmeasured verifies that a 429 before any
// throughput was measured leaves the account UNPACED (rate 0) — we don't guess
// a rate we never observed; the cooldown + concurrency AIMD handle that case as
// they did before the pacer existed.
func TestRatePacerStaysUnpacedWhenUnmeasured(t *testing.T) {
	p := NewForTesting()
	p.SetAccountsForTesting([]config.Account{{ID: "a", AccessToken: "t"}})

	p.RecordError("a", true, 0) // 429 with no prior success
	paced, _ := p.RateState("a")
	if paced != 0 {
		t.Fatalf("unmeasured 429 should stay unpaced (0), got %v", paced)
	}
}

// pacedViaMeasured429 drives an account to a known paced state the way
// production does: measure throughput via successes, then trip a 429 so the
// snap-down has a real observed rate to halve. Returns the resulting paced rate.
func pacedViaMeasured429(t *testing.T, p *AccountPool, id string) float64 {
	t.Helper()
	// Two successes ~100ms apart => observed ~10 req/sec.
	p.RecordSuccess(id)
	p.mu.Lock()
	p.cooldowns[id].lastSuccessAt = time.Now().Add(-100 * time.Millisecond)
	p.mu.Unlock()
	p.RecordSuccess(id)
	p.RecordError(id, true, 0)
	paced, _ := p.RateState(id)
	if paced <= 0 {
		t.Fatalf("precondition: account should be paced after measured 429, got %v", paced)
	}
	return paced
}

// TestRatePacerProbesUpOnSustainedSuccess verifies the paced rate climbs back
// toward the ceiling (+5% per probe interval) once the account is succeeding.
func TestRatePacerProbesUpOnSustainedSuccess(t *testing.T) {
	p := NewForTesting()
	p.SetAccountsForTesting([]config.Account{{ID: "a", AccessToken: "t"}})

	start := pacedViaMeasured429(t, p, "a")

	// A success immediately after should NOT probe (within rateProbeInterval).
	p.RecordSuccess("a")
	if paced, _ := p.RateState("a"); paced != start {
		t.Fatalf("probe should be rate-limited; rate moved %v -> %v too soon", start, paced)
	}

	// Reach past the probe interval by backdating lastProbeAt, then a success
	// should bump the rate +5%.
	p.mu.Lock()
	p.cooldowns["a"].lastProbeAt = time.Now().Add(-2 * rateProbeInterval)
	p.mu.Unlock()
	p.RecordSuccess("a")
	paced, _ := p.RateState("a")
	if paced <= start {
		t.Fatalf("paced rate should have probed up from %v, got %v", start, paced)
	}
}

// TestRatePacerSurvivesDecay verifies a learned paced rate is NOT wiped by the
// error-history decay sweep — otherwise the account would reset to unpaced and
// re-trip the 429 it learned to avoid (a decay-period sawtooth).
func TestRatePacerSurvivesDecay(t *testing.T) {
	p := NewForTesting()
	p.SetAccountsForTesting([]config.Account{{ID: "a", AccessToken: "t"}})

	paced := pacedViaMeasured429(t, p, "a")

	// Backdate the error well past the decay window AND clear the cooldown timer,
	// then trigger the decay sweep.
	p.mu.Lock()
	cd := p.cooldowns["a"]
	cd.lastErrorAt = time.Now().Add(-2 * errorCounterDecay)
	cd.until = time.Time{}
	p.decayCountersLocked(time.Now())
	p.mu.Unlock()

	if paced2, _ := p.RateState("a"); paced2 != paced {
		t.Fatalf("decay must preserve learned paced rate %v, got %v", paced, paced2)
	}
}

// TestRatePacerGatesAcquire verifies that once an account is paced, a burst of
// Acquires beyond the burst tolerance is rate-gated (the pool reports
// saturation) rather than all admitted at once.
func TestRatePacerGatesAcquire(t *testing.T) {
	withLeastRequest(t) // the rate gate lives in the least-request reserve branch
	p := NewForTesting()
	p.SetAccountsForTesting([]config.Account{{ID: "a", AccessToken: "t"}})

	// Pace the account at a slow, known rate so the gate is observable: set it
	// directly to 2 req/sec (emission 500ms, tau ~500ms => ~2-deep burst), with a
	// generous concurrency limit so the RATE gate is the only thing that can shed.
	p.mu.Lock()
	p.cooldowns["a"] = &cooldownEntry{limit: aimdMaxLimit, rateEstimate: 2}
	p.mu.Unlock()

	admitted := 0
	var lastRetry time.Duration
	for i := 0; i < 6; i++ {
		acc, retry, ok := p.AcquireForModelExcluding("", nil)
		if ok && acc != nil {
			admitted++
		} else {
			lastRetry = retry
		}
	}
	if admitted == 0 {
		t.Fatal("rate pacer should admit at least the burst tolerance")
	}
	if admitted >= 6 {
		t.Fatal("rate pacer should have gated part of the burst, but admitted all 6")
	}
	if lastRetry <= 0 {
		t.Fatalf("a rate-gated pick should return a positive retry hint, got %v", lastRetry)
	}
}

