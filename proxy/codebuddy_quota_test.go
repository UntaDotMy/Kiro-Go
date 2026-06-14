package proxy

import (
	"strings"
	"testing"
)

// TestParseCodeBuddyQuota_StringNumericFields verifies the parser tolerates
// capacity fields returned as JSON STRINGS (e.g. "1000.00", "600") rather than
// numbers — the live OAuth payload sends them as strings, which previously caused
// "cannot unmarshal string into Go struct field ...CapacitySizePrecise".
func TestParseCodeBuddyQuota_StringNumericFields(t *testing.T) {
	body := []byte(`{"data":{"Response":{"Data":{"Accounts":[
		{"PackageCode":"TCACA_code_002_AkiJS3ZHF5","CapacitySizePrecise":"1000.00","CapacityRemain":"600","CapacityUsed":"400.00","CycleEndTime":"2026-07-01 00:00:00"}
	]}}}}`)
	q, err := parseCodeBuddyQuota(body)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if q.Records != 1 {
		t.Errorf("Records = %d, want 1", q.Records)
	}
	if q.Total != 1000 || q.Remaining != 600 || q.Used != 400 {
		t.Errorf("got total=%v remaining=%v used=%v, want 1000/600/400", q.Total, q.Remaining, q.Used)
	}
}

// TestParseCodeBuddyQuota_NumericTimeFields verifies the parser tolerates time
// fields returned as numeric Unix timestamps (seconds AND milliseconds) and null,
// not just date strings — the live OAuth payload mixes these, which previously
// caused "cannot unmarshal number into Go struct field ...DeductionEndTime".
func TestParseCodeBuddyQuota_NumericTimeFields(t *testing.T) {
	body := []byte(`{"data":{"Response":{"Data":{"Accounts":[
		{"PackageCode":"TCACA_code_002_AkiJS3ZHF5","CycleCapacitySize":1000,"CapacityRemain":600,"CapacityUsed":400,"CycleEndTime":1782000000,"DeductionEndTime":1782000000000,"ExpiredTime":null}
	]}}}}`)
	q, err := parseCodeBuddyQuota(body)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if q.Records != 1 {
		t.Errorf("Records = %d, want 1", q.Records)
	}
	if q.Total != 1000 || q.Remaining != 600 {
		t.Errorf("got total=%v remaining=%v, want 1000/600", q.Total, q.Remaining)
	}
	// A numeric reset must normalize to a non-empty RFC3339 string.
	if q.ResetAt == "" {
		t.Errorf("ResetAt should be derived from the numeric CycleEndTime, got empty")
	}
}

// TestParseCodeBuddyQuota_OAuthNestedEnvelope verifies the OAuth-path shape,
// where the accounts sit under data.Response.Data.Accounts (the IDE token call
// returns the billing payload nested one level deeper than the web console).
func TestParseCodeBuddyQuota_OAuthNestedEnvelope(t *testing.T) {
	body := []byte(`{"data":{"Response":{"Data":{"Accounts":[
		{"PackageCode":"TCACA_code_001_PqouKr6QWV","CycleCapacitySize":100,"CycleCapacityRemain":80,"CapacityUsed":20}
	]}}}}`)
	q, err := parseCodeBuddyQuota(body)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if q.Records != 1 {
		t.Errorf("Records = %d, want 1", q.Records)
	}
	if q.Total != 100 || q.Remaining != 80 || q.Used != 20 {
		t.Errorf("got total=%v remaining=%v used=%v, want 100/80/20", q.Total, q.Remaining, q.Used)
	}
}

// TestCodeBuddyUsageBody_PackageCodesGating confirms the OAuth body omits
// PackageCodes (gateway resolves them from the token) while the cookie body
// includes the full list (web console queries by explicit id).
func TestCodeBuddyUsageBody_PackageCodesGating(t *testing.T) {
	oauth := string(codeBuddyUsageBody(false))
	if strings.Contains(oauth, "PackageCodes") {
		t.Errorf("OAuth body must omit PackageCodes, got: %s", oauth)
	}
	cookie := string(codeBuddyUsageBody(true))
	if !strings.Contains(cookie, "PackageCodes") {
		t.Errorf("cookie body must include PackageCodes, got: %s", cookie)
	}
}

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
