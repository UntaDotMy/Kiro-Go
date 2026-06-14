package proxy

import (
	"context"
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"strings"
	"time"
)

// CodeBuddy quota admin API.
//
// Quota sync is a standalone feature: it reads CodeBuddy credit/usage figures from
// a web-console session cookie stored on the account (Account.WebCookie) and folds
// them into the dashboard's usage fields. The cookie is supplied via manual cookie
// import (see the cookie-import flow); this handler just triggers a re-fetch.

// apiSyncCodeBuddyQuota POST /admin/api/codebuddy/quota/{id} — re-fetch quota for
// one CodeBuddy account using its stored web cookie.
func (h *Handler) apiSyncCodeBuddyQuota(w http.ResponseWriter, r *http.Request, id string) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	q, err := SyncCodeBuddyQuota(ctx, id)
	if err != nil {
		writeJSONError(w, 400, err.Error())
		return
	}
	if q == nil {
		writeJSONError(w, 400, "not a CodeBuddy account")
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "ok",
		"plan":      q.Plan,
		"used":      q.Used,
		"total":     q.Total,
		"remaining": q.Remaining,
		"resetAt":   q.ResetAt,
		"records":   q.Records,
	})
}

// apiImportCodeBuddyCookie POST /admin/api/codebuddy/cookie/{id} {cookie} — stores
// a CodeBuddy web-console session cookie on the account so quota tracking works,
// then immediately syncs quota so the caller gets live figures (or a clear error
// if the cookie is unauthorized). The cookie is the billing credential; the OAuth
// inference token cannot reach the billing API (see codebuddy_quota.go).
func (h *Handler) apiImportCodeBuddyCookie(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Cookie string `json:"cookie"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, 400, "invalid JSON")
		return
	}
	cookie := strings.TrimSpace(req.Cookie)
	if cookie == "" {
		writeJSONError(w, 400, "cookie value is required")
		return
	}
	acct, ok := config.GetAccount(id)
	if !ok {
		writeJSONError(w, 404, "account not found")
		return
	}
	backend := config.GetAccountBackend(&acct)
	if backend != "codebuddy" && backend != "codebuddy-ai" {
		writeJSONError(w, 400, "not a CodeBuddy account")
		return
	}
	if err := config.SetAccountWebCookie(id, cookie, time.Now().Unix()); err != nil {
		writeJSONError(w, 400, err.Error())
		return
	}

	// Sync immediately so the response reflects whether the cookie actually works.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	q, err := SyncCodeBuddyQuota(ctx, id)
	if err != nil {
		// Cookie stored, but the poll failed (likely unauthorized/expired). Report
		// it so the operator knows to re-capture, without losing the stored value.
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "stored",
			"warning": err.Error(),
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "ok",
		"plan":      q.Plan,
		"used":      q.Used,
		"total":     q.Total,
		"remaining": q.Remaining,
		"resetAt":   q.ResetAt,
		"records":   q.Records,
	})
}

// writeJSONError writes a {"error": msg} body with the given status.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
