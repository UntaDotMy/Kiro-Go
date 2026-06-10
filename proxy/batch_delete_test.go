package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"kiro-go/config"
	"kiro-go/pool"
)

// TestBatchDeleteAccounts verifies the select-all + delete path removes exactly
// the requested ids in one request, ignores unknown ids, and rejects an empty
// body — leaving the rest of the pool intact.
func TestBatchDeleteAccounts(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	for _, a := range []config.Account{
		{ID: "a1", Backend: "qwen", APIKey: "k1", Enabled: true},
		{ID: "a2", Backend: "qwen", APIKey: "k2", Enabled: true},
		{ID: "a3", Backend: "qwen", APIKey: "k3", Enabled: true},
		{ID: "a4", Backend: "groq", APIKey: "k4", Enabled: true},
	} {
		if err := config.AddAccount(a); err != nil {
			t.Fatalf("AddAccount %s: %v", a.ID, err)
		}
	}
	h := &Handler{pool: pool.NewForTesting()}

	post := func(body string) (int, map[string]interface{}) {
		req := httptest.NewRequest("POST", "/admin/api/accounts/batch-delete", bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		h.apiDeleteAccountsBatch(rec, req)
		var out map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// Delete two real ids plus one unknown id — only the two real ones count.
	code, out := post(`{"ids":["a1","a3","ghost"]}`)
	if code != http.StatusOK {
		t.Fatalf("batch delete HTTP %d, body=%v", code, out)
	}
	if got := int(out["deleted"].(float64)); got != 2 {
		t.Errorf("deleted = %d, want 2", got)
	}
	got := map[string]bool{}
	for _, a := range config.GetAccounts() {
		got[a.ID] = true
	}
	if len(got) != 2 || !got["a2"] || !got["a4"] {
		t.Fatalf("after delete expected {a2,a4}, got %v", got)
	}

	// Empty body is a 400 and changes nothing.
	code, _ = post(`{"ids":[]}`)
	if code != http.StatusBadRequest {
		t.Errorf("empty ids: HTTP %d, want 400", code)
	}
	if n := len(config.GetAccounts()); n != 2 {
		t.Fatalf("account count changed after empty batch delete: %d", n)
	}
}

// TestDeleteAccountsConfig is the config-layer unit: batch delete removes the
// named ids in a single save, returns the count, and a no-match set is a no-op.
func TestDeleteAccountsConfig(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	for _, id := range []string{"x1", "x2", "x3"} {
		if err := config.AddAccount(config.Account{ID: id, Backend: "qwen", APIKey: id, Enabled: true}); err != nil {
			t.Fatalf("AddAccount %s: %v", id, err)
		}
	}
	n, err := config.DeleteAccounts([]string{"x1", "x3"})
	if err != nil {
		t.Fatalf("DeleteAccounts: %v", err)
	}
	if n != 2 {
		t.Errorf("removed = %d, want 2", n)
	}
	if accts := config.GetAccounts(); len(accts) != 1 || accts[0].ID != "x2" {
		t.Fatalf("expected only x2 to remain, got %v", accts)
	}
	// No-match set: no error, zero removed.
	if n, err := config.DeleteAccounts([]string{"nope"}); err != nil || n != 0 {
		t.Errorf("no-match delete = (%d,%v), want (0,nil)", n, err)
	}
}
