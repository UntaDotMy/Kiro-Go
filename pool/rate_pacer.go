package pool

import (
	"hash/crc32"
	"time"
)

// ============================================================================
// Per-account proactive RATE pacing (GCRA) with AIMD rate learning.
//
// WHY THIS EXISTS — concurrency control is not enough.
//
// The existing AIMD *concurrency* limit (cooldownEntry.limit / inflight) caps
// how many requests run on an account SIMULTANEOUSLY. But each account is one
// AWS identity rate-limited by a hidden per-identity TOKEN BUCKET, which is a
// RATE limit (tokens/sec refill + burst), not a concurrency limit. Two fast
// back-to-back requests (each ~200ms) never push inflight above 1, so the
// concurrency gate never sees them — yet they still spend two bucket tokens in
// 400ms. With a large pool you can spread that load around; with only 2
// accounts you cannot, so you bounce off 429s no matter how good the selection
// is. This adds the missing Tier-1 mechanism: pace each account just below its
// own refill rate so it (almost) never hits the wall.
//
// DESIGN — "discover then pace" (the chosen stance):
//
//   - Each account starts UNPACED (rateEstimate == 0): it runs full speed,
//     gated only by concurrency, so a cold start is as fast as possible. We
//     passively learn the achieved success throughput (observedRate, an EWMA).
//   - On the FIRST 429 we snap the paced rate down to observedRate × 0.5 (AIMD
//     multiplicative-decrease) and begin GCRA pacing at that rate. The account
//     comes back PACED rather than just cooled, so it does not immediately
//     re-trip — this is where "smooth" comes from.
//   - On sustained success while paced we probe the rate up by +5% every
//     rateProbeInterval (AIMD additive-increase), sawtoothing just below the
//     real ceiling so we never leave throughput on the table.
//   - An upstream Retry-After still drives the cooldown timer as before; the
//     rate snap-down rides alongside it.
//
// GCRA (Generic Cell Rate Algorithm) is used as the pacer because it needs only
// ONE timestamp per account (the TAT — Theoretical Arrival Time) and produces
// smoother spacing than a chunked token-bucket refill. See the rate-limiting
// literature (GCRA / leaky-bucket-as-meter) for the TAT formulation.
//
// Everything in THIS file is PURE (no locks, no globals, no clock reads except
// where `now` is passed in) so it is fully unit-testable. The stateful wiring
// (reading/advancing TAT under the pool lock, learning on success/error) lives
// in account.go.
// ============================================================================

const (
	// rateDecreaseFactor is the multiplicative-decrease applied to the observed
	// achieved rate on a 429: paced rate = observedRate × 0.85. The 429 fires at
	// roughly the observed rate, so pacing at 0.85× lands at the "balanced
	// ~90-95% of the limit" target — hot enough to stay fast, with enough margin
	// that the gentle probe-up doesn't immediately re-trip. (Was 0.5, which
	// halved throughput on every 429 and then took ~43s of +5% probing to
	// recover — the dominant "too slow after a blip" complaint.)
	rateDecreaseFactor = 0.85

	// rateProbeFactor is the multiplicative-increase: +15% per probe interval of
	// sustained success. Fast enough to recover from a snap-down in ~1-2 probes,
	// gentle enough that it approaches the ceiling without stampeding into it.
	// (Was 1.05 — a 14-probe / ~43s climb.)
	rateProbeFactor = 1.15

	// rateProbeInterval is how often a paced account may probe its rate upward
	// while it keeps succeeding. (Was 3s — combined with the slow factor that
	// made recovery glacial; 1.5s with the faster factor recovers in ~2s.)
	rateProbeInterval = 1500 * time.Millisecond

	// rateMinPaced / rateMaxPaced bound the learned paced rate (requests/sec).
	// The floor keeps a heavily-throttled account from pacing itself to a
	// standstill; the ceiling is a sanity cap so probe-up can't run away.
	rateMinPaced = 0.5
	rateMaxPaced = 1000.0

	// rateBurstTolerance is the GCRA τ as a MULTIPLE of the emission interval T.
	// τ = burstFactor × T allows a burst above the steady rate before pacing
	// kicks in (bucket depth ≈ 1 + τ/T ≈ 1 + burstFactor). A factor of 2 permits
	// a ~3-deep burst so a small fan-out fires immediately instead of being
	// paced from the first request. (Was 1.0 → only ~2-deep.)
	rateBurstTolerance = 2.0

	// rateObservedEWMA is the smoothing factor for the achieved-throughput EWMA
	// (weight on the newest sample). 0.5 tracks recent throughput closely so the
	// snap-down on a 429 is seeded from a current estimate, not a lagging-low one
	// that would over-cut the rate. (Was 0.3.)
	rateObservedEWMA = 0.5

	// rateObservedMaxSample caps a single inter-success instantaneous-rate
	// sample (requests/sec) so two completions landing microseconds apart can't
	// blow the EWMA up to an absurd value that would mis-seed the snap-down.
	rateObservedMaxSample = 500.0

	// ttftEWMAAlpha is the weight on the newest time-to-first-token sample in the
	// per-account TTFT EWMA used for latency-aware "smart laning" selection. 0.5
	// weights the latest sample equally with history, so an account that starts
	// slowing (or recovers) is reflected within ~2 requests instead of ~4. We
	// measured TTFT swinging 1.9s–11.8s for the SAME request against a live pool,
	// so the signal is genuinely noisy and time-varying; a faster-tracking EWMA
	// follows the currently-fast identity rather than lagging on stale history.
	// Single-request outliers are still bounded by ttftMaxSampleMs and the
	// scorer's ttftPenaltyCap, so faster tracking doesn't let one spike dominate.
	ttftEWMAAlpha = 0.5

	// ttftMaxSampleMs clamps a single TTFT sample (milliseconds) so a pathological
	// outlier (a near-stalled request that still eventually produced a token)
	// can't blow the EWMA out and make an otherwise-fast account look permanently
	// slow. 60s is far beyond any healthy first-token latency.
	ttftMaxSampleMs = 60000.0

	// ttftPenaltyCap bounds how much the latency-aware scorer can divide an
	// account's score by relative to the fastest candidate. Raised from 4 to 10
	// after measuring a real spread of ~1.5s (fastest) to ~9.6s (slowest) across
	// one live pool — a 6.4x ratio. At the old cap of 4 the scorer treated a
	// 6.4x-slower account as only 4x slower, so it still steered meaningful
	// traffic onto the slow lane; 10 lets the penalty track the real ratio up to
	// an order of magnitude. It stays a strong PREFERENCE, not a hard ban: even
	// at 10x an account keeps ~10% selection weight, so a recovered account still
	// gets probed and can climb back as its EWMA improves (and unmeasured
	// accounts stay at factor 1.0 to explore).
	ttftPenaltyCap = 10.0
)

// updateTTFT folds one time-to-first-token sample (milliseconds) into the EWMA.
// A non-positive sample is ignored (returns prev); the sample is clamped to
// ttftMaxSampleMs before blending; a zero prev seeds directly. Pure.
func updateTTFT(prev, sampleMs float64) float64 {
	if sampleMs <= 0 {
		return prev
	}
	if sampleMs > ttftMaxSampleMs {
		sampleMs = ttftMaxSampleMs
	}
	if prev <= 0 {
		return sampleMs
	}
	return ttftEWMAAlpha*sampleMs + (1-ttftEWMAAlpha)*prev
}

// emissionInterval converts a rate (requests/sec) into the GCRA emission
// interval T = 1/rate. Returns 0 for a non-positive rate (meaning "unpaced").
func emissionInterval(rate float64) time.Duration {
	if rate <= 0 {
		return 0
	}
	return time.Duration(float64(time.Second) / rate)
}

// gcraAdmit reports whether a request may be admitted at `now` under GCRA
// pacing, given the current theoretical arrival time `tat` and burst tolerance
// `tau`. The rule is: admit iff now >= tat - tau (the request is not "too
// early"). A zero tat (never advanced) always admits. Pure.
func gcraAdmit(now, tat time.Time, tau time.Duration) bool {
	if tat.IsZero() {
		return true
	}
	return !now.Before(tat.Add(-tau))
}

// gcraAdvance returns the TAT after admitting a request at `now` with emission
// interval `emission`. TAT moves forward by one emission interval from
// max(now, tat), so an idle account's TAT snaps to now (no accumulated credit
// beyond τ) while a busy one's TAT keeps stepping forward. Pure.
func gcraAdvance(now, tat time.Time, emission time.Duration) time.Time {
	base := tat
	if now.After(base) {
		base = now
	}
	return base.Add(emission)
}

// aimdRateDecrease returns the new paced rate after a throttle: half the
// observed achieved rate, floored at rateMinPaced and capped at rateMaxPaced.
// Returns 0 ("stay unpaced") when the observed rate is unknown — a 429 before
// we ever measured throughput tells us nothing about the rate to pace at, so we
// leave the account unpaced and let the existing cooldown + concurrency-AIMD
// machinery handle it (exactly the pre-pacer behavior). Pacing only engages
// once there is a measured rate to halve — true "discover then pace". Pure.
func aimdRateDecrease(observedRate float64) float64 {
	if observedRate <= 0 {
		return 0
	}
	r := observedRate * rateDecreaseFactor
	if r < rateMinPaced {
		r = rateMinPaced
	}
	if r > rateMaxPaced {
		r = rateMaxPaced
	}
	return r
}

// aimdRateProbe returns the new paced rate after a sustained-success probe:
// +5%, capped at rateMaxPaced. Pure.
func aimdRateProbe(rate float64) float64 {
	r := rate * rateProbeFactor
	if r > rateMaxPaced {
		r = rateMaxPaced
	}
	return r
}

// updateObservedRate folds one inter-success interval into the throughput EWMA.
// `interval` is the time since the previous success on this account; the
// instantaneous rate 1/interval is clamped to rateObservedMaxSample before
// blending so a microsecond-apart pair can't distort the estimate. A
// non-positive interval or a zero previous estimate seeds directly. Pure.
func updateObservedRate(prev float64, interval time.Duration) float64 {
	if interval <= 0 {
		return prev
	}
	inst := float64(time.Second) / float64(interval)
	if inst > rateObservedMaxSample {
		inst = rateObservedMaxSample
	}
	if prev <= 0 {
		return inst
	}
	return rateObservedEWMA*inst + (1-rateObservedEWMA)*prev
}

// phaseOffset returns a deterministic per-account fraction of the emission
// interval, in [0, emission), used to stagger two accounts' GCRA cycles so a
// synchronized client burst doesn't drain both buckets at the same instant
// (the dominant small-pool failure mode). Derived from a hash of the account id
// so it is stable across calls but differs between accounts. Pure.
func phaseOffset(accountID string, emission time.Duration) time.Duration {
	if emission <= 0 {
		return 0
	}
	h := crc32.ChecksumIEEE([]byte(accountID))
	return time.Duration(uint64(h) % uint64(emission))
}
