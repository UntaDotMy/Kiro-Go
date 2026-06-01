package stats

import (
	"path/filepath"
	"testing"
)

// withTempDB initializes the package-level DB against a fresh temp file and
// resets it after the test. The stats package uses a single global DB handle
// guarded by a once-style Init, so we close and nil it out around each test to
// keep them isolated.
func withTempDB(t *testing.T) {
	t.Helper()
	dbMu.Lock()
	if db != nil {
		_ = db.Close()
		db = nil
	}
	dbMu.Unlock()
	path := filepath.Join(t.TempDir(), "stats_test.sqlite")
	if err := Init(path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		_ = Close()
	})
}

func TestCanonicalEffort(t *testing.T) {
	cases := map[string]string{
		"":        EffortBucketDefault,
		"   ":     EffortBucketDefault,
		"HIGH":    "high",
		" xhigh ": "xhigh",
		"Max":     "max",
		"low":     "low",
		"weird":   "weird", // storage layer doesn't validate, only normalizes
	}
	for in, want := range cases {
		if got := CanonicalEffort(in); got != want {
			t.Errorf("CanonicalEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestByModelEffortBreakdown records several requests at different effort
// levels and verifies the per-effort breakdown both buckets correctly and sums
// to the per-model total (the key invariant for the analytics view).
func TestByModelEffortBreakdown(t *testing.T) {
	withTempDB(t)

	// Two requests at high, one at xhigh, one with no effort (-> default).
	// Different model spellings collapse to one canonical model row.
	Record("claude-opus-4.7", "k1", "high", true, 100, 50, 1.0)
	Record("claude-opus-4-7", "k1", "high", true, 200, 80, 2.0)
	Record("claude-opus-4.7", "k1", "xhigh", true, 300, 120, 3.0)
	Record("claude-opus-4.7", "k1", "", true, 10, 5, 0.1) // -> default bucket
	// A failed request still records (counts as a request, zero tokens/credits).
	Record("claude-opus-4.7", "k1", "max", false, 0, 0, 0)

	byEffort, err := ByModelEffort()
	if err != nil {
		t.Fatalf("ByModelEffort: %v", err)
	}
	levels := byEffort["claude-opus-4-7"]
	if levels == nil {
		t.Fatalf("no effort breakdown for canonical model; got keys %v", keysOf(byEffort))
	}

	// high: 2 requests merged, tokens 100+50+200+80 = 430, credits 3.0
	if got := levels["high"]; got.Requests != 2 || got.TokensIn+got.TokensOut != 430 || got.Credits != 3.0 {
		t.Errorf("high bucket = %+v, want 2 req / 430 tok / 3.0 cr", got)
	}
	// xhigh: 1 request, 420 tok, 3.0 cr
	if got := levels["xhigh"]; got.Requests != 1 || got.TokensIn+got.TokensOut != 420 || got.Credits != 3.0 {
		t.Errorf("xhigh bucket = %+v, want 1 req / 420 tok / 3.0 cr", got)
	}
	// default: 1 request, 15 tok
	if got := levels[EffortBucketDefault]; got.Requests != 1 || got.TokensIn+got.TokensOut != 15 {
		t.Errorf("default bucket = %+v, want 1 req / 15 tok", got)
	}
	// max: 1 request (the failed one), zero tokens/credits.
	if got := levels["max"]; got.Requests != 1 || got.TokensIn+got.TokensOut != 0 || got.Credits != 0 {
		t.Errorf("max bucket = %+v, want 1 req / 0 tok / 0 cr (failed)", got)
	}

	// Invariant: per-effort sums equal the per-model total.
	byModel, err := ByModel()
	if err != nil {
		t.Fatalf("ByModel: %v", err)
	}
	model := byModel["claude-opus-4-7"]
	var sumReq, sumTok int
	var sumCred float64
	for _, v := range levels {
		sumReq += v.Requests
		sumTok += v.TokensIn + v.TokensOut
		sumCred += v.Credits
	}
	if sumReq != model.Requests {
		t.Errorf("effort request sum %d != model total %d", sumReq, model.Requests)
	}
	if sumTok != model.TokensIn+model.TokensOut {
		t.Errorf("effort token sum %d != model total %d", sumTok, model.TokensIn+model.TokensOut)
	}
	if sumCred != model.Credits {
		t.Errorf("effort credit sum %.4f != model total %.4f", sumCred, model.Credits)
	}
}

// TestRecordSkipsEffortScopeWithoutModel ensures a model_effort row is only
// written when a model id is present (an empty model id should not create a
// dangling "\x1fdefault" bucket).
func TestRecordSkipsEffortScopeWithoutModel(t *testing.T) {
	withTempDB(t)
	Record("", "k1", "high", true, 10, 10, 1.0) // no model
	byEffort, err := ByModelEffort()
	if err != nil {
		t.Fatalf("ByModelEffort: %v", err)
	}
	if len(byEffort) != 0 {
		t.Errorf("expected no effort rows for empty model, got %v", byEffort)
	}
}

func TestSplitEffortScopeID(t *testing.T) {
	model, effort, ok := splitEffortScopeID("claude-opus-4-7" + effortScopeSep + "high")
	if !ok || model != "claude-opus-4-7" || effort != "high" {
		t.Errorf("split = (%q,%q,%v), want (claude-opus-4-7,high,true)", model, effort, ok)
	}
	if _, _, ok := splitEffortScopeID("no-separator-here"); ok {
		t.Error("expected ok=false for scope id without separator")
	}
}

func keysOf(m map[string]map[string]Totals) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
