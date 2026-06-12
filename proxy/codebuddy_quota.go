package proxy

import (
	"bytes"
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

// CodeBuddy quota tracking.
//
// CodeBuddy inference auth (the CLI OAuth access token in Account.AccessToken)
// and CodeBuddy QUOTA are two different worlds. The OAuth token speaks to the
// /v1/chat/completions gateway, but credit/usage figures are served ONLY by the
// web console billing API (https://www.codebuddy.ai/billing/meter/get-user-resource),
// which is gated behind a Keycloak browser session cookie. The OAuth token gets
// 401/403 there. That is precisely why quota could never be tracked without
// guessing: the value lives behind a cookie the CLI flow never captures.
//
// The account's CodeBuddy web session cookie is stored in Account.WebCookie
// (supplied via manual cookie import). This file turns that cookie into REAL
// numbers: it POSTs the documented billing query (ProductCode p_tcaca + the
// TCACA_code_* package codes) and folds the returned CycleCapacity figures into
// the account's UsageCurrent/UsageLimit/UsagePercent fields — the same fields the
// dashboard already renders for every other provider.
//
// Ported from 9router_wyx0's open-sse/services/usage.js (getCodeBuddyUsage /
// parseCodeBuddyUsage) and src/app/api/oauth/codebuddy/quota-cookie/route.js.

const (
	codeBuddyUsageURL  = "https://www.codebuddy.ai/billing/meter/get-user-resource"
	codeBuddyProductCN = "p_tcaca"
)

// codeBuddyPackageCodes are the billing "package" identifiers the web console
// queries for. Each maps to a plan bucket (free / pro-monthly / gift / ...). The
// billing API returns one Accounts[] row per active package, so we ask for all
// of them and sum the credit figures. Source: 9router CODEBUDDY_CONFIG.packageCodes.
var codeBuddyPackageCodes = []string{
	"TCACA_code_001_PqouKr6QWV", // free
	"TCACA_code_002_AkiJS3ZHF5", // pro monthly
	"TCACA_code_006_DbXS0lrypC", // gift
	"TCACA_code_007_nzdH5h4Nl0", // activity
	"TCACA_code_003_FAnt7lcmRT", // pro yearly
	"TCACA_code_008_cfWoLwvjU4", // free monthly
	"TCACA_code_009_0XmEQc2xOf", // extra
}

// codeBuddyProPackages are the package codes that indicate a paid (Pro) plan, so
// the synced subscription tier reflects reality rather than a default.
var codeBuddyProPackages = map[string]bool{
	"TCACA_code_002_AkiJS3ZHF5": true, // pro monthly
	"TCACA_code_003_FAnt7lcmRT": true, // pro yearly
}

// CodeBuddyQuota is the normalized result of one billing poll.
type CodeBuddyQuota struct {
	Plan      string  // "Pro" | "Free"
	Used      float64 // credits consumed this cycle (summed across packages)
	Total     float64 // credit allowance this cycle
	Remaining float64 // credits left
	ResetAt   string  // earliest cycle-end across packages (RFC3339 or raw)
	Records   int     // number of billing rows returned (0 ⇒ cookie unauthorized/empty)
}

// codeBuddyUsageRequest is the billing query body. The time range is "now" to
// ~101 years out so every active package is in-window (mirrors 9router).
type codeBuddyUsageRequest struct {
	PageNumber               int      `json:"PageNumber"`
	PageSize                 int      `json:"PageSize"`
	ProductCode              string   `json:"ProductCode"`
	Status                   []int    `json:"Status"`
	PackageCodes             []string `json:"PackageCodes"`
	PackageEndTimeRangeBegin string   `json:"PackageEndTimeRangeBegin"`
	PackageEndTimeRangeEnd   string   `json:"PackageEndTimeRangeEnd"`
}

// codeBuddyBillingAccount is one package row in the billing response. The billing
// API uses Tencent's PascalCase + "*Precise" companion fields; we accept both the
// precise and rounded variants and the wrapped/unwrapped envelopes.
type codeBuddyBillingAccount struct {
	PackageCode string `json:"PackageCode"`

	CycleCapacitySizePrecise   *float64 `json:"CycleCapacitySizePrecise"`
	CycleCapacitySize          *float64 `json:"CycleCapacitySize"`
	CapacitySizePrecise        *float64 `json:"CapacitySizePrecise"`
	CapacitySize               *float64 `json:"CapacitySize"`
	CycleCapacityRemainPrecise *float64 `json:"CycleCapacityRemainPrecise"`
	CapacityRemainPrecise      *float64 `json:"CapacityRemainPrecise"`
	CapacityRemain             *float64 `json:"CapacityRemain"`
	CapacityUsedPrecise        *float64 `json:"CapacityUsedPrecise"`
	CapacityUsed               *float64 `json:"CapacityUsed"`

	CycleEndTime     string `json:"CycleEndTime"`
	DeductionEndTime string `json:"DeductionEndTime"`
	ExpiredTime      string `json:"ExpiredTime"`
}

// normalizeCookie collapses a raw cookie blob into a single "k=v; k=v" header
// line, trimming empties — defensive against pasted multi-line cookie exports.
func normalizeCookie(cookie string) string {
	parts := strings.Split(cookie, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "; ")
}

func codeBuddyUsageBody() []byte {
	now := time.Now()
	end := now.AddDate(101, 0, 0)
	fmtTime := func(t time.Time) string { return t.Format("2006-01-02 15:04:05") }
	b, _ := json.Marshal(codeBuddyUsageRequest{
		PageNumber:               1,
		PageSize:                 200,
		ProductCode:              codeBuddyProductCN,
		Status:                   []int{0, 3},
		PackageCodes:             codeBuddyPackageCodes,
		PackageEndTimeRangeBegin: fmtTime(now),
		PackageEndTimeRangeEnd:   fmtTime(end),
	})
	return b
}

// FetchCodeBuddyQuota queries the web-console billing API with the account's
// stored session cookie and returns normalized credit figures. The cookie (not
// the OAuth token) is the credential; an account with no WebCookie returns an
// error so the caller can prompt a (re-)import of the cookie. The account's own
// proxy is honored so the poll exits the same egress as inference.
func FetchCodeBuddyQuota(ctx context.Context, acct *config.Account) (*CodeBuddyQuota, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cookie := normalizeCookie(acct.WebCookie)
	if cookie == "" {
		return nil, fmt.Errorf("no CodeBuddy web session cookie stored for account %s; import one via the cookie-import flow to enable quota tracking", acct.ID)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", codeBuddyUsageURL, bytes.NewReader(codeBuddyUsageBody()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-Domain", "www.codebuddy.ai")
	req.Header.Set("Origin", "https://www.codebuddy.ai")
	req.Header.Set("Referer", "https://www.codebuddy.ai/profile/usage")

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(acct)).Do(req)
	if err != nil {
		return nil, fmt.Errorf("codebuddy quota request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("codebuddy web session cookie is not authorized (HTTP %d); re-import a fresh cookie via the cookie-import flow", resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("codebuddy quota endpoint returned HTTP %d", resp.StatusCode)
	}
	return parseCodeBuddyQuota(raw)
}

// parseCodeBuddyQuota extracts and sums the credit figures from the billing
// response. Tolerates the wrapped {Response:{Data:{Accounts:[]}}}, the
// {data:{accounts:[]}}, and the bare {Accounts:[]} shapes.
func parseCodeBuddyQuota(raw []byte) (*CodeBuddyQuota, error) {
	var env struct {
		Response struct {
			Data struct {
				Accounts []codeBuddyBillingAccount `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
		Data struct {
			Accounts []codeBuddyBillingAccount `json:"Accounts"`
			Lower    []codeBuddyBillingAccount `json:"accounts"`
		} `json:"data"`
		Accounts []codeBuddyBillingAccount `json:"Accounts"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("codebuddy quota parse failed: %w", err)
	}
	accounts := env.Response.Data.Accounts
	if len(accounts) == 0 {
		accounts = env.Data.Accounts
	}
	if len(accounts) == 0 {
		accounts = env.Data.Lower
	}
	if len(accounts) == 0 {
		accounts = env.Accounts
	}

	q := &CodeBuddyQuota{Plan: "Free", Records: len(accounts)}
	for _, a := range accounts {
		if codeBuddyProPackages[a.PackageCode] {
			q.Plan = "Pro"
		}
		total := firstFloat(a.CycleCapacitySizePrecise, a.CycleCapacitySize, a.CapacitySizePrecise, a.CapacitySize)
		remaining := firstFloat(a.CycleCapacityRemainPrecise, a.CapacityRemainPrecise, a.CapacityRemain)
		used := firstFloat(a.CapacityUsedPrecise, a.CapacityUsed)

		// Derive any missing leg from the other two so a partial row still counts.
		if total == nil && used != nil && remaining != nil {
			t := *used + *remaining
			total = &t
		}
		if total == nil && remaining == nil && used == nil {
			continue
		}
		safeTotal := 0.0
		if total != nil {
			safeTotal = *total
		} else if used != nil || remaining != nil {
			safeTotal = deref(used) + deref(remaining)
		}
		safeRemaining := deref(remaining)
		if remaining == nil {
			safeRemaining = max0(safeTotal - deref(used))
		}
		safeUsed := deref(used)
		if used == nil {
			safeUsed = max0(safeTotal - safeRemaining)
		}

		q.Total += max0(safeTotal)
		q.Remaining += max0(safeRemaining)
		q.Used += max0(safeUsed)
		q.ResetAt = earlierReset(q.ResetAt, firstNonBlank(a.CycleEndTime, a.DeductionEndTime, a.ExpiredTime))
	}
	return q, nil
}

func firstFloat(vals ...*float64) *float64 {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func deref(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func max0(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// earlierReset keeps the soonest of two reset timestamps (string compare works
// for the YYYY-MM-DD... lexicographic ordering the API returns; falls back
// gracefully if one is blank).
func earlierReset(current, next string) string {
	if current == "" {
		return next
	}
	if next == "" {
		return current
	}
	if next < current {
		return next
	}
	return current
}

// SyncCodeBuddyQuota fetches live quota for one account and folds it into the
// account's persisted Usage* fields, so the dashboard's existing quota UI shows
// real CodeBuddy credits. Returns the quota for callers that want to report it.
// A non-CodeBuddy account is a no-op (nil, nil).
func SyncCodeBuddyQuota(ctx context.Context, acctID string) (*CodeBuddyQuota, error) {
	acct, ok := config.GetAccount(acctID)
	if !ok {
		return nil, fmt.Errorf("account %s not found", acctID)
	}
	backend := config.GetAccountBackend(&acct)
	if backend != "codebuddy" && backend != "codebuddy-ai" {
		return nil, nil
	}
	q, err := FetchCodeBuddyQuota(ctx, &acct)
	if err != nil {
		return nil, err
	}

	update := acct
	update.UsageLimit = q.Total
	update.UsageCurrent = q.Used
	if q.Total > 0 {
		update.UsagePercent = q.Used / q.Total
	} else {
		update.UsagePercent = 0
	}
	if q.ResetAt != "" {
		update.NextResetDate = q.ResetAt
	}
	if q.Plan == "Pro" {
		update.SubscriptionType = "PRO"
	} else {
		update.SubscriptionType = "FREE"
	}
	update.LastRefresh = time.Now().Unix()
	if err := config.UpdateAccount(acctID, update); err != nil {
		return q, err
	}
	logger.Infof("[CodeBuddy] quota synced for %s: used=%.0f/%.0f (%s plan, %d records)", acctID, q.Used, q.Total, q.Plan, q.Records)
	return q, nil
}
