package proxy

import (
	"testing"
)

// TestParseCodeBuddyQuota_WrappedEnvelope verifies the Tencent-style wrapped
// {Response:{Data:{Accounts:[]}}} shape is parsed and the credit figures summed.
func TestParseCodeBuddyQuota_WrappedEnvelope(t *testing.T) {
	body := []byte(`{
		"Response": {"Data": {"Accounts": [
			{"PackageCode":"TCACA_code_002_AkiJS3ZHF5","CycleCapacitySizePrecise":1000,"CycleCapacityRemainPrecise":250,"CycleEndTime":"2026-07-01 00:00:00"},
			{"PackageCode":"TCACA_code_006_DbXS0lrypC","CapacitySize":500,"CapacityRemain":500,"ExpiredTime":"2026-08-01 00:00:00"}
		]}}
	}`)
	q, err := parseCodeBuddyQuota(body)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if q.Records != 2 {
		t.Errorf("Records = %d, want 2", q.Records)
	}
	if q.Plan != "Pro" {
		t.Errorf("Plan = %q, want Pro (TCACA_code_002 is pro monthly)", q.Plan)
	}
	// total = 1000 + 500, remaining = 250 + 500, used = 750 + 0
	if q.Total != 1500 {
		t.Errorf("Total = %v, want 1500", q.Total)
	}
	if q.Remaining != 750 {
		t.Errorf("Remaining = %v, want 750", q.Remaining)
	}
	if q.Used != 750 {
		t.Errorf("Used = %v, want 750", q.Used)
	}
	// earliest reset wins
	if q.ResetAt != "2026-07-01 00:00:00" {
		t.Errorf("ResetAt = %q, want earliest 2026-07-01", q.ResetAt)
	}
}

// TestParseCodeBuddyQuota_FreeOnly confirms a free-only payload reports Free and
// derives used from total-remaining when CapacityUsed is absent.
func TestParseCodeBuddyQuota_FreeOnly(t *testing.T) {
	body := []byte(`{"data":{"Accounts":[
		{"PackageCode":"TCACA_code_001_PqouKr6QWV","CapacitySize":200,"CapacityRemain":120}
	]}}`)
	q, err := parseCodeBuddyQuota(body)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if q.Plan != "Free" {
		t.Errorf("Plan = %q, want Free", q.Plan)
	}
	if q.Total != 200 || q.Remaining != 120 || q.Used != 80 {
		t.Errorf("got total=%v remaining=%v used=%v, want 200/120/80", q.Total, q.Remaining, q.Used)
	}
}

// TestParseCodeBuddyQuota_Empty handles a payload with no accounts (unauthorized
// cookie or no packages) without erroring.
func TestParseCodeBuddyQuota_Empty(t *testing.T) {
	q, err := parseCodeBuddyQuota([]byte(`{"Response":{"Data":{"Accounts":[]}}}`))
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if q.Records != 0 || q.Total != 0 {
		t.Errorf("empty payload: got records=%d total=%v, want 0/0", q.Records, q.Total)
	}
}

// TestNormalizeCookie collapses multi-line / spaced cookie blobs to one header.
func TestNormalizeCookie(t *testing.T) {
	cases := map[string]string{
		"a=1; b=2":       "a=1; b=2",
		" a=1 ;  b=2 ; ": "a=1; b=2",
		"a=1;;b=2":       "a=1; b=2",
		"  ":             "",
		"single=value":   "single=value",
	}
	for in, want := range cases {
		if got := normalizeCookie(in); got != want {
			t.Errorf("normalizeCookie(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEarlierReset keeps the soonest non-empty timestamp.
func TestEarlierReset(t *testing.T) {
	if got := earlierReset("", "2026-07-01"); got != "2026-07-01" {
		t.Errorf("earlierReset(empty,x) = %q", got)
	}
	if got := earlierReset("2026-07-01", ""); got != "2026-07-01" {
		t.Errorf("earlierReset(x,empty) = %q", got)
	}
	if got := earlierReset("2026-08-01", "2026-07-01"); got != "2026-07-01" {
		t.Errorf("earlierReset picked later, got %q", got)
	}
}
