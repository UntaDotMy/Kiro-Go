package proxy

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"kiro-go/config"
	"kiro-go/pool"
)

// initBatchTestHandler boots a fresh config in a temp dir, seeds it with the
// given accounts, and returns a Handler wired to a real pool. Config state is
// global so these tests cannot run in parallel.
func initBatchTestHandler(t *testing.T, accounts ...config.Account) *Handler {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	for _, a := range accounts {
		if err := config.AddAccount(a); err != nil {
			t.Fatalf("seed account %s: %v", a.ID, err)
		}
	}
	p := pool.NewForTesting()
	p.Reload()
	return &Handler{pool: p, startTime: 0}
}

func postBatch(t *testing.T, h *Handler, ids []string, action string) map[string]interface{} {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{"ids": ids, "action": action})
	req := httptest.NewRequest("POST", "/admin/api/accounts/batch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiBatchAccounts(rec, req)
	if rec.Code != 200 {
		t.Fatalf("action %q: status = %d, body = %s", action, rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, rec.Body.String())
	}
	return out
}

// TestBatchOverageOnOff verifies overage-on / overage-off flip AllowOverage on
// exactly the selected accounts and leave the unselected one untouched.
func TestBatchOverageOnOff(t *testing.T) {
	h := initBatchTestHandler(t,
		config.Account{ID: "a", RefreshToken: "rt-a", Enabled: true, AllowOverage: false},
		config.Account{ID: "b", RefreshToken: "rt-b", Enabled: true, AllowOverage: false},
		config.Account{ID: "c", RefreshToken: "rt-c", Enabled: true, AllowOverage: true},
	)

	resp := postBatch(t, h, []string{"a", "b"}, "overage-on")
	if resp["success"] != true || resp["count"].(float64) != 2 {
		t.Fatalf("overage-on result mismatch: %+v", resp)
	}
	got := overageMap(config.GetAccounts())
	if !got["a"] || !got["b"] {
		t.Fatalf("a and b should have overage on, got %+v", got)
	}
	if !got["c"] {
		t.Fatalf("c (unselected, already on) must stay on, got %+v", got)
	}

	// Now turn b and c off; a must remain on.
	resp = postBatch(t, h, []string{"b", "c"}, "overage-off")
	if resp["count"].(float64) != 2 {
		t.Fatalf("overage-off count mismatch: %+v", resp)
	}
	got = overageMap(config.GetAccounts())
	if !got["a"] {
		t.Fatalf("a must remain on (unselected), got %+v", got)
	}
	if got["b"] || got["c"] {
		t.Fatalf("b and c should be off, got %+v", got)
	}
}

// TestBatchDelete verifies delete removes exactly the selected accounts and
// reports the count; unselected accounts survive.
func TestBatchDelete(t *testing.T) {
	h := initBatchTestHandler(t,
		config.Account{ID: "a", RefreshToken: "rt-a", Enabled: true},
		config.Account{ID: "b", RefreshToken: "rt-b", Enabled: true},
		config.Account{ID: "c", RefreshToken: "rt-c", Enabled: true},
	)

	resp := postBatch(t, h, []string{"a", "c"}, "delete")
	if resp["success"] != true {
		t.Fatalf("delete should succeed: %+v", resp)
	}
	if resp["deleted"].(float64) != 2 || resp["failed"].(float64) != 0 {
		t.Fatalf("expected deleted=2 failed=0, got %+v", resp)
	}
	ids := idSetOf(config.GetAccounts())
	if ids["a"] || ids["c"] {
		t.Fatalf("a and c should be gone, remaining: %+v", ids)
	}
	if !ids["b"] {
		t.Fatalf("b must survive, remaining: %+v", ids)
	}
}

// TestBatchDeleteCountsMissingAsFailed verifies a delete that targets an id
// which no longer exists is reported as failed, not silently dropped.
func TestBatchDeleteCountsMissingAsFailed(t *testing.T) {
	h := initBatchTestHandler(t,
		config.Account{ID: "a", RefreshToken: "rt-a", Enabled: true},
	)
	resp := postBatch(t, h, []string{"a", "does-not-exist"}, "delete")
	if resp["deleted"].(float64) != 1 {
		t.Fatalf("expected deleted=1, got %+v", resp)
	}
	if resp["failed"].(float64) != 1 {
		t.Fatalf("expected failed=1 for the missing id, got %+v", resp)
	}
	if resp["success"] != false {
		t.Fatalf("success must be false when a row failed, got %+v", resp)
	}
}

// TestBatchRejectsUnknownAction guards the switch default.
func TestBatchRejectsUnknownAction(t *testing.T) {
	h := initBatchTestHandler(t, config.Account{ID: "a", RefreshToken: "rt-a", Enabled: true})
	body, _ := json.Marshal(map[string]interface{}{"ids": []string{"a"}, "action": "frobnicate"})
	req := httptest.NewRequest("POST", "/admin/api/accounts/batch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiBatchAccounts(rec, req)
	if rec.Code != 400 {
		t.Fatalf("unknown action should 400, got %d", rec.Code)
	}
}

func overageMap(accounts []config.Account) map[string]bool {
	m := make(map[string]bool, len(accounts))
	for _, a := range accounts {
		m[a.ID] = a.AllowOverage
	}
	return m
}

func idSetOf(accounts []config.Account) map[string]bool {
	m := make(map[string]bool, len(accounts))
	for _, a := range accounts {
		m[a.ID] = true
	}
	return m
}
