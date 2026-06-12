package proxy

import (
	"context"
	"encoding/json"
	"net/http"
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

// writeJSONError writes a {"error": msg} body with the given status.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
