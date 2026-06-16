package proxy

import (
	"testing"
	"time"
)

// TestAdaptiveThrottleNoOpUnderHealthyLoad is the core safety guarantee: when
// the backend accepts everything, the SRE formula clamps p_reject to 0, so the
// throttle NEVER sheds under healthy load. This is what lets it ship on by
// default without risking the tuned happy path.
func TestAdaptiveThrottleNoOpUnderHealthyLoad(t *testing.T) {
	at := newAdaptiveThrottle()
	for i := 0; i < 500; i++ {
		if at.shouldShed("kiro") {
			t.Fatalf("shed under all-accept load at i=%d (must never happen)", i)
		}
		at.recordOutcome("kiro", true)
	}
	if p := at.rejectProbability("kiro"); p != 0 {
		t.Fatalf("reject probability under healthy load = %v, want 0", p)
	}
}

// TestAdaptiveThrottleEngagesUnderRejection verifies the throttle actually sheds
// once the backend rejects sustained traffic (accepts fall far behind requests).
func TestAdaptiveThrottleEngagesUnderRejection(t *testing.T) {
	at := newAdaptiveThrottle()
	// Simulate a pool-wide 429 storm: many requests, zero accepts.
	for i := 0; i < 200; i++ {
		at.recordOutcome("kiro", false)
	}
	p := at.rejectProbability("kiro")
	if p <= 0 {
		t.Fatalf("reject probability under all-reject load = %v, want > 0", p)
	}
	// With requests>>accepts, p approaches 1; assert it's meaningfully high.
	if p < 0.9 {
		t.Fatalf("reject probability under all-reject = %v, want near 1", p)
	}
	// And shedding should actually fire across a batch.
	shed := 0
	for i := 0; i < 200; i++ {
		if at.shouldShed("kiro") {
			shed++
		}
	}
	if shed == 0 {
		t.Fatal("expected the throttle to shed at least some requests under all-reject load")
	}
}

// TestAdaptiveThrottleDisabledEnvSkipsShedding verifies the kill switch: when
// disabled, shouldShed always returns false even under heavy rejection, while
// counters still record so flipping it back on sees a warm window.
func TestAdaptiveThrottleDisabledEnvSkipsShedding(t *testing.T) {
	at := newAdaptiveThrottle()
	at.disabled = true
	for i := 0; i < 300; i++ {
		at.recordOutcome("kiro", false)
	}
	for i := 0; i < 300; i++ {
		if at.shouldShed("kiro") {
			t.Fatal("disabled throttle must never shed")
		}
	}
	// rejectProbability still reflects the (high) computed value — disable only
	// gates the shed decision, not the math.
	if p := at.rejectProbability("kiro"); p <= 0 {
		t.Fatalf("disabled throttle should still compute p_reject for observability, got %v", p)
	}
}

// TestAdaptiveThrottleDecayRecovers verifies the rolling counters decay, so a
// brief 429 burst doesn't keep shedding long after the backend recovers.
func TestAdaptiveThrottleDecayRecovers(t *testing.T) {
	at := newAdaptiveThrottle()
	at.halfLife = 10 * time.Millisecond // shrink for a fast test
	for i := 0; i < 200; i++ {
		at.recordOutcome("kiro", false)
	}
	if at.rejectProbability("kiro") <= 0 {
		t.Fatal("expected shedding pressure right after the burst")
	}
	// Let several half-lives elapse, then feed accepts (recovery).
	time.Sleep(80 * time.Millisecond)
	for i := 0; i < 50; i++ {
		at.recordOutcome("kiro", true)
	}
	if p := at.rejectProbability("kiro"); p != 0 {
		t.Fatalf("after decay + recovery, p_reject = %v, want 0", p)
	}
}

// TestAdaptiveThrottlePerBackendIsolation verifies one backend's 429 storm does
// not shed traffic destined for a different, healthy backend.
func TestAdaptiveThrottlePerBackendIsolation(t *testing.T) {
	at := newAdaptiveThrottle()
	for i := 0; i < 200; i++ {
		at.recordOutcome("codebuddy", false) // codebuddy is being throttled
	}
	if at.rejectProbability("codebuddy") <= 0 {
		t.Fatal("codebuddy should show shedding pressure")
	}
	if p := at.rejectProbability("kiro"); p != 0 {
		t.Fatalf("kiro (untouched) must not inherit codebuddy's pressure, got %v", p)
	}
	// Empty backend shares the kiro bucket (legacy no-constraint path).
	if at.rejectProbability("") != at.rejectProbability("kiro") {
		t.Fatal(`empty backend must map to the "kiro" bucket`)
	}
}
