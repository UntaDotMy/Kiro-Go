package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"kiro-go/pool"
	"path/filepath"
	"testing"
)

// TestDashboardSnapshotIncludesPerAccountStats pins the realtime per-account
// push: dashboardSnapshot must carry an accountStats array with each account's
// live counters (credits, tokens, requests, quota), so the dashboard updates
// account cards without a manual refresh. Before this, the snapshot only sent
// aggregate totals and the per-account credit/usage numbers only changed when
// the operator hit refresh.
func TestDashboardSnapshotIncludesPerAccountStats(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "acc-1",
		Email:        "a@example.com",
		RefreshToken: "rt",
		Enabled:      true,
		UsageCurrent: 12,
		UsageLimit:   1000,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	p := pool.NewForTesting()
	p.Reload() // pull the account from config into the pool
	// Simulate a served request so the pool carries live per-account counters.
	p.UpdateStats("acc-1", 1500, 3.5)

	h := &Handler{pool: p, startTime: 0}

	raw := h.dashboardSnapshot()
	if raw == nil {
		t.Fatal("dashboardSnapshot returned nil")
	}
	var snap struct {
		Type         string `json:"type"`
		AccountStats []struct {
			ID           string  `json:"id"`
			TotalCredits float64 `json:"totalCredits"`
			TotalTokens  int     `json:"totalTokens"`
			RequestCount int     `json:"requestCount"`
			UsageCurrent float64 `json:"usageCurrent"`
			UsageLimit   float64 `json:"usageLimit"`
		} `json:"accountStats"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.Type != "status" {
		t.Fatalf("expected type=status, got %q", snap.Type)
	}
	if len(snap.AccountStats) != 1 {
		t.Fatalf("expected 1 account in accountStats, got %d", len(snap.AccountStats))
	}
	a := snap.AccountStats[0]
	if a.ID != "acc-1" {
		t.Fatalf("expected acc-1, got %q", a.ID)
	}
	// Live counters from the pool must be present so cards update in realtime.
	if a.TotalCredits != 3.5 {
		t.Fatalf("expected totalCredits=3.5 (live pool counter), got %v", a.TotalCredits)
	}
	if a.TotalTokens != 1500 {
		t.Fatalf("expected totalTokens=1500, got %d", a.TotalTokens)
	}
	if a.RequestCount != 1 {
		t.Fatalf("expected requestCount=1, got %d", a.RequestCount)
	}
	// Quota fields from config must also be present.
	if a.UsageCurrent != 12 || a.UsageLimit != 1000 {
		t.Fatalf("expected usage 12/1000, got %v/%v", a.UsageCurrent, a.UsageLimit)
	}
}
