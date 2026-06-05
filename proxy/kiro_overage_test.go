package proxy

import (
	"io"
	"kiro-go/config"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestFetchOverageStatusParsesUpstream verifies FetchOverageStatus reads the
// real AWS Overages switch state and billing figures (cap/rate/current $) from
// a /getUsageLimits-shaped response.
func TestFetchOverageStatusParsesUpstream(t *testing.T) {
	prev := kiroRestHttpStore.Load()
	t.Cleanup(func() { kiroRestHttpStore.Store(prev) })

	const body = `{
		"overageConfiguration": {"overageStatus": "ENABLED"},
		"subscriptionInfo": {"overageCapability": "OVERAGE_CAPABLE", "subscriptionTitle": "KIRO PRO+"},
		"usageBreakdownList": [
			{"resourceType": "AGENTIC_REQUEST", "overageCap": 50.0, "overageRate": 0.04, "currentOverages": 3.20}
		]
	}`
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("expected GET, got %s", req.Method)
			}
			if !strings.Contains(req.URL.Path, "/getUsageLimits") {
				t.Fatalf("expected getUsageLimits path, got %s", req.URL.Path)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		}),
	})

	acct := &config.Account{ID: "ov-1", Email: "ov@example.com", AccessToken: "x", Region: "us-east-1"}
	snap, err := FetchOverageStatus(acct)
	if err != nil {
		t.Fatalf("FetchOverageStatus: %v", err)
	}
	if snap.Status != "ENABLED" {
		t.Errorf("status = %q, want ENABLED", snap.Status)
	}
	if snap.Capability != "OVERAGE_CAPABLE" {
		t.Errorf("capability = %q, want OVERAGE_CAPABLE", snap.Capability)
	}
	if snap.SubscriptionTitle != "KIRO PRO+" {
		t.Errorf("title = %q, want KIRO PRO+", snap.SubscriptionTitle)
	}
	if snap.OverageCap != 50.0 || snap.OverageRate != 0.04 || snap.CurrentOverages != 3.20 {
		t.Errorf("billing = cap %v rate %v current %v, want 50/0.04/3.20", snap.OverageCap, snap.OverageRate, snap.CurrentOverages)
	}
	if snap.CheckedAt == 0 {
		t.Error("CheckedAt should be set")
	}
}

// TestFetchOverageStatusUnknownWhenAbsent verifies a response without an
// overageConfiguration yields status UNKNOWN rather than a crash or "".
func TestFetchOverageStatusUnknownWhenAbsent(t *testing.T) {
	prev := kiroRestHttpStore.Load()
	t.Cleanup(func() { kiroRestHttpStore.Store(prev) })
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"usageBreakdownList":[]}`)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	acct := &config.Account{ID: "ov-2", AccessToken: "x", Region: "us-east-1"}
	snap, err := FetchOverageStatus(acct)
	if err != nil {
		t.Fatalf("FetchOverageStatus: %v", err)
	}
	if snap.Status != "UNKNOWN" {
		t.Errorf("status = %q, want UNKNOWN", snap.Status)
	}
}

// TestFetchOverageStatusNilAccount guards the nil path.
func TestFetchOverageStatusNilAccount(t *testing.T) {
	if _, err := FetchOverageStatus(nil); err == nil {
		t.Fatal("expected error for nil account")
	}
}

// TestPersistOverageSnapshotRoundTrip verifies the persister writes the cached
// AWS overage fields to config and they survive a read-back, without touching
// the local AllowOverage routing flag.
func TestPersistOverageSnapshotRoundTrip(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "ov-3", Email: "p@example.com", AllowOverage: true}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	snap := &OverageSnapshot{
		Status:          "ENABLED",
		Capability:      "OVERAGE_CAPABLE",
		OverageCap:      25.5,
		OverageRate:     0.20,
		CurrentOverages: 7.5,
		CheckedAt:       1700000000,
	}
	if err := PersistOverageSnapshot("ov-3", snap); err != nil {
		t.Fatalf("persist: %v", err)
	}

	var found *config.Account
	for _, a := range config.GetAccounts() {
		if a.ID == "ov-3" {
			ac := a
			found = &ac
			break
		}
	}
	if found == nil {
		t.Fatal("account not found after persist")
	}
	if found.OverageStatus != "ENABLED" || found.OverageCap != 25.5 || found.OverageRate != 0.20 || found.CurrentOverages != 7.5 || found.OverageCheckedAt != 1700000000 {
		t.Errorf("persisted overage fields wrong: %+v", found)
	}
	// The AWS cache must NOT clobber the local routing flag.
	if !found.AllowOverage {
		t.Error("PersistOverageSnapshot must not touch the local AllowOverage routing flag")
	}
}
