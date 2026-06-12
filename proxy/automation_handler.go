package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"kiro-go/auth"
	"kiro-go/automation"
	"kiro-go/config"
	"kiro-go/logger"
)

// Automation admin API.
//
// The Automation tab drives stealth-browser logins for providers whose quota
// can't be tracked from the CLI token alone (CodeBuddy today; others "coming
// soon"). This file is the HTTP surface: start/stop bulk jobs, poll progress,
// finish manual-assist accounts, and re-sync quota on demand. The heavy lifting
// lives in the automation package; here we adapt it to the existing admin-router
// conventions and own the persist+pool-reload+quota-sync side effects.

// codeBuddyPersist is the OnPersist callback the job machinery calls when a login
// succeeds. It creates the account with the captured OAuth tokens + web cookie,
// reloads the pool, seeds the model list, and folds in real quota.
func (h *Handler) codeBuddyPersist(ctx context.Context, res automation.CodeBuddyLoginResult, email string) (string, string, error) {
	backend := res.Backend
	if backend == "" {
		backend = "codebuddy"
	}
	_, nickname := codeBuddyBackendHost(backend)

	acct := config.Account{
		ID:           auth.GenerateAccountID(),
		Backend:      backend,
		Email:        email,
		Nickname:     nickname,
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		Enabled:      true,
		WebCookie:    res.WebCookie,
	}
	if res.WebCookie != "" {
		acct.WebCookieAt = nowUnixSeconds()
	}
	if res.ExpiresIn > 0 {
		acct.ExpiresAt = nowUnixSeconds() + int64(res.ExpiresIn)
	}
	if err := config.AddAccount(acct); err != nil {
		return "", "", err
	}
	h.pool.Reload()
	logger.Infof("[Automation] saved CodeBuddy account %s (%s) for %s", acct.ID, backend, email)

	// Seed models (advisory fallback) so the account is routable immediately.
	if ids, advisory, ferr := codeBuddyInference.FetchModelsForAccount(ctx, &acct); ferr == nil && len(ids) > 0 {
		if advisory {
			h.pool.SetAdvisoryModelList(acct.ID, ids)
		} else {
			h.pool.SetModelList(acct.ID, ids)
		}
	}

	// Fold in real quota from the captured cookie. Non-fatal: the account is
	// already saved and usable for inference even if quota can't be read.
	quotaMsg := ""
	if res.WebCookie != "" {
		if q, qerr := SyncCodeBuddyQuota(ctx, acct.ID); qerr != nil {
			quotaMsg = "quota unavailable: " + qerr.Error()
		} else if q != nil {
			quotaMsg = fmt.Sprintf("%s plan, %.0f/%.0f credits used", q.Plan, q.Used, q.Total)
		}
	} else {
		quotaMsg = "no web cookie captured; quota not tracked"
	}
	return acct.ID, quotaMsg, nil
}

// apiStartAutomation POST /admin/api/automation/start
// body: {backend, concurrency, headless, proxyURL, accounts:[]string}
// Starts a bulk CodeBuddy login job and returns {jobId}.
func (h *Handler) apiStartAutomation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Backend     string   `json:"backend"`
		Concurrency int      `json:"concurrency"`
		Headless    *bool    `json:"headless"`
		ProxyURL    string   `json:"proxyURL"`
		Accounts    []string `json:"accounts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, 400, "Invalid JSON")
		return
	}
	backend := strings.TrimSpace(req.Backend)
	if backend == "" {
		backend = "codebuddy"
	}
	if backend != "codebuddy" && backend != "codebuddy-ai" {
		writeJSONError(w, 400, "automation currently supports only CodeBuddy (codebuddy / codebuddy-ai)")
		return
	}
	if len(req.Accounts) == 0 {
		writeJSONError(w, 400, "no accounts provided (one email|password per line)")
		return
	}
	// Default headless on (bulk runs unattended); operator can force visible.
	headless := true
	if req.Headless != nil {
		headless = *req.Headless
	}

	job, err := automation.GetManager().Start(automation.StartInput{
		Backend:     backend,
		Concurrency: req.Concurrency,
		Headless:    headless,
		ProxyURL:    strings.TrimSpace(req.ProxyURL),
		Lines:       req.Accounts,
	}, h.codeBuddyPersist)
	if err != nil {
		writeJSONError(w, 409, err.Error())
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"jobId": job.ID})
}

// apiAutomationStatus GET /admin/api/automation/status — current job snapshot.
func (h *Handler) apiAutomationStatus(w http.ResponseWriter, r *http.Request) {
	snap := automation.GetManager().Snapshot()
	if snap == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"job": nil})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"job": snap})
}

// apiAutomationCancel POST /admin/api/automation/cancel — cancel the current job.
func (h *Handler) apiAutomationCancel(w http.ResponseWriter, r *http.Request) {
	job := automation.GetManager().Current()
	if job == nil {
		writeJSONError(w, 404, "no job running")
		return
	}
	job.Cancel()
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "cancelling"})
}

// apiAutomationComplete POST /admin/api/automation/complete {line} — signal that
// the operator finished a needs-manual account in its browser window.
func (h *Handler) apiAutomationComplete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Line int `json:"line"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, 400, "Invalid JSON")
		return
	}
	job := automation.GetManager().Current()
	if job == nil {
		writeJSONError(w, 404, "no job running")
		return
	}
	if !job.CompleteManual(req.Line) {
		writeJSONError(w, 404, "no manual account waiting on that line")
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "completing"})
}

// apiSyncCodeBuddyQuota POST /admin/api/automation/quota/{id} — re-fetch quota for
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
