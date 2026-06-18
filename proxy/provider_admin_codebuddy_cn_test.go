package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
	"kiro-go/pool"
)

// TestAPICodeBuddyCNCheckin_NonCNAccount verifies the daily-checkin admin handler
// rejects a non-codebuddy-cn account with HTTP 400. DailyCheckinCodeBuddyCN returns
// (nil, nil) for a non-CN backend before any network call, so this path is
// deterministic and exercises the handler's nil-result guard.
func TestAPICodeBuddyCNCheckin_NonCNAccount(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	const id = "acct-groq"
	if err := config.AddAccount(config.Account{ID: id, Backend: "groq", APIKey: "k", Enabled: true}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	h := &Handler{pool: pool.NewForTesting()}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/codebuddy-cn/checkin/"+id, nil)
	rec := httptest.NewRecorder()
	h.apiCodeBuddyCNCheckin(rec, req, id)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not a codebuddy-cn account") {
		t.Fatalf("body missing guard message: %s", rec.Body.String())
	}
}
