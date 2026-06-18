package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"
)

// Codex usage polling + capacity-weighted routing (codex-lb parity).
//
// codex-lb routes Codex accounts by remaining quota across two windows (the 5h
// "primary" and the weekly "secondary"), polled from GET /backend-api/wham/usage.
// Kiro-Go's default "fast"/"least-request" strategies are latency/load based and
// provider-agnostic; for Codex specifically we layer this usage signal on top:
//   - the poller records used% + reset_at per window onto the account,
//   - RefreshCodexUsage drives an auto-cooldown when a window is exhausted (so the
//     pool skips it until reset_at), which is the cheap reactive equivalent of
//     codex-lb's capacity-weighted selection without a second scheduler.
//
// This keeps Kiro accounts entirely unaffected: nothing here runs for a non-codex
// account.

// codexUsageResponse is the subset of GET /backend-api/wham/usage we read.
type codexUsageResponse struct {
	PlanType  string `json:"plan_type"`
	RateLimit *struct {
		PrimaryWindow   *codexUsageWindow `json:"primary_window"`
		SecondaryWindow *codexUsageWindow `json:"secondary_window"`
	} `json:"rate_limit"`
}

type codexUsageWindow struct {
	UsedPercent float64 `json:"used_percent"`
	ResetAt     int64   `json:"reset_at"`
}

// pollCodexUsage calls the ChatGPT usage endpoint for one Codex account and
// returns the parsed windows. Best-effort: a transport / non-200 returns an
// error the caller logs and ignores (usage routing degrades to reactive 429
// cooldown, which is always present).
func pollCodexUsage(ctx context.Context, acct *config.Account) (*codexUsageResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", codexUsageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(acct.AccessToken))
	if acct.CodexAccountID != "" {
		req.Header.Set("chatgpt-account-id", acct.CodexAccountID)
	}
	req.Header.Set("User-Agent", codexUserAgent)

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(acct)).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != 200 {
		// Plain error — this is a best-effort REST poll, not an inference call, so
		// it must NOT masquerade as a QuotaError (which carries cooldown semantics
		// for the streaming path). The caller treats any error as "skip this poll".
		return nil, fmt.Errorf("codex usage poll: HTTP %d", resp.StatusCode)
	}
	var u codexUsageResponse
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// refreshCodexAccountUsage polls usage for one Codex account, persists the
// windows onto the account, and — if either window is exhausted — parks the
// account in cooldown until the soonest reset so the pool skips it. Returns true
// if the account's persisted usage fields changed.
func (h *Handler) refreshCodexAccountUsage(ctx context.Context, acct *config.Account) bool {
	u, err := pollCodexUsage(ctx, acct)
	if err != nil || u == nil || u.RateLimit == nil {
		return false
	}
	now := time.Now()
	info := config.Account{
		CodexUsageCheckedAt: now.Unix(),
		CodexPlanType:       u.PlanType,
	}
	var soonestExhaustedReset int64
	consider := func(win *codexUsageWindow, setPct *float64, setReset *int64) {
		if win == nil {
			return
		}
		*setPct = win.UsedPercent
		*setReset = win.ResetAt
		if win.UsedPercent >= 100 && win.ResetAt > now.Unix() {
			if soonestExhaustedReset == 0 || win.ResetAt < soonestExhaustedReset {
				soonestExhaustedReset = win.ResetAt
			}
		}
	}
	consider(u.RateLimit.PrimaryWindow, &info.CodexPrimaryUsedPct, &info.CodexPrimaryResetAt)
	consider(u.RateLimit.SecondaryWindow, &info.CodexSecondaryUsedPct, &info.CodexSecondaryResetAt)

	config.UpdateCodexUsage(acct.ID, info)

	// If a window is exhausted, cool the account until its reset so the pool skips
	// it — the cheap reactive equivalent of codex-lb's capacity-weighted skip.
	if soonestExhaustedReset > 0 {
		cooldown := time.Until(time.Unix(soonestExhaustedReset, 0))
		if cooldown > 0 {
			h.pool.RecordError(acct.ID, true, cooldown)
		}
	}
	return true
}

// backgroundRefreshNonKiro is the per-tick refresh path for a non-Kiro account.
// It refreshes credentials through the account's provider (when near expiry) and,
// for Codex, polls the usage windows so quota-based routing stays current. It
// deliberately does NOT call the AWS RefreshAccountInfo path. Errors are logged
// and swallowed so one bad account never stalls the sweep.
func (h *Handler) backgroundRefreshNonKiro(account *config.Account) {
	p := ProviderFor(account)
	if p == nil {
		return
	}
	// Refresh credentials if near expiry (api-key providers report ExpiresAt 0 and
	// are skipped). Reuse the wider background skew like the Kiro path.
	if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-backgroundTokenRefreshSkewSeconds {
		ts, err := p.RefreshToken(context.Background(), account)
		if err != nil {
			logger.Warnf("[BackgroundRefresh] %s token refresh failed for %s: %v", config.GetAccountBackend(account), redactForLog(account.Email), err)
		} else {
			account.AccessToken = ts.AccessToken
			if ts.RefreshToken != "" {
				account.RefreshToken = ts.RefreshToken
			}
			if ts.ExpiresAt > 0 {
				account.ExpiresAt = ts.ExpiresAt
			}
			applyTokenExtras(account, ts.Extra)
			config.UpdateAccountToken(account.ID, account.AccessToken, account.RefreshToken, account.ExpiresAt)
			h.pool.UpdateToken(account.ID, account.AccessToken, account.RefreshToken, account.ExpiresAt)
		}
	}

	// Codex: poll the usage windows (quota-based routing signal).
	if config.GetAccountBackend(account) == "codex" {
		h.refreshCodexAccountUsage(context.Background(), account)
	}

	// CodeBuddy: quota is served by the web-console billing API, reachable with the
	// IDE OAuth access token (the same credential inference uses) — so this is fully
	// automatic, no manual cookie needed. Fold the live credit figures into the
	// account's Usage* fields so the dashboard tracks real usage.
	if backend := config.GetAccountBackend(account); backend == "codebuddy" || backend == "codebuddy-ai" {
		if strings.TrimSpace(account.AccessToken) != "" || strings.TrimSpace(account.WebCookie) != "" {
			if _, err := SyncCodeBuddyQuota(context.Background(), account.ID); err != nil {
				logger.Debugf("[BackgroundRefresh] CodeBuddy quota sync failed for %s: %v", redactForLog(account.Email), err)
			}
		}
	}

	// CodeBuddy-CN: quota is fetched from copilot.tencent.com using the ck_ API key.
	// Reuses the same Usage* fields for dashboard tracking.
	if config.GetAccountBackend(account) == "codebuddy-cn" {
		if strings.TrimSpace(account.APIKey) != "" || strings.TrimSpace(account.AccessToken) != "" {
			if _, err := SyncCodeBuddyCNQuota(context.Background(), account.ID); err != nil {
				logger.Debugf("[BackgroundRefresh] CodeBuddy-CN quota sync failed for %s: %v", redactForLog(account.Email), err)
			}
		}
	}
}
