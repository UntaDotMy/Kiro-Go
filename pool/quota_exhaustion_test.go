package pool

import (
	"kiro-go/config"
	"testing"
	"time"
)

// TestRecordQuotaExhaustionLongCooldown pins the contract that 402
// OVERAGE / monthly-quota exhaustion produces an hour-long cooldown,
// not a soft 5s..5min one. This is the path that prevents the SWRR
// walk from re-selecting an account that's already been billed past
// its monthly cap.
func TestRecordQuotaExhaustionLongCooldown(t *testing.T) {
	p := NewForTesting()
	p.setAccounts([]config.Account{{ID: "a"}})

	p.RecordQuotaExhaustion("a")
	d := p.CooldownRemaining("a")
	if d < 55*time.Minute || d > 65*time.Minute {
		t.Fatalf("expected ~1h cooldown, got %s", d)
	}
}
