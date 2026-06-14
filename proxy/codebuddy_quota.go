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
	"strconv"
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
	// codeBuddyUsageURLCookie is the billing endpoint the WEB CONSOLE hits (no /v2),
	// used by the cookie fallback path. codeBuddyUsagePathOAuth is the /v2 billing
	// path the IDE OAuth token uses (per 9router open-sse/services/usage.js).
	codeBuddyUsageURLCookie = "https://www.codebuddy.ai/billing/meter/get-user-resource"
	codeBuddyUsagePathOAuth = "/v2/billing/meter/get-user-resource"
	codeBuddyAccountsPath   = "/v2/plugin/accounts"
	codeBuddyProductCN      = "p_tcaca"
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
// PackageCodes is omitempty: the OAuth path omits it (the gateway resolves the
// caller's own packages from the token), while the cookie path sends the full
// list (the web console queries by explicit package id).
type codeBuddyUsageRequest struct {
	PageNumber               int      `json:"PageNumber"`
	PageSize                 int      `json:"PageSize"`
	ProductCode              string   `json:"ProductCode"`
	Status                   []int    `json:"Status"`
	PackageCodes             []string `json:"PackageCodes,omitempty"`
	PackageEndTimeRangeBegin string   `json:"PackageEndTimeRangeBegin"`
	PackageEndTimeRangeEnd   string   `json:"PackageEndTimeRangeEnd"`
}

// codeBuddyBillingAccount is one package row in the billing response. The billing
// API uses Tencent's PascalCase + "*Precise" companion fields; we accept both the
// precise and rounded variants and the wrapped/unwrapped envelopes.
type codeBuddyBillingAccount struct {
	PackageCode string `json:"PackageCode"`

	CycleCapacitySizePrecise   *codeBuddyNum `json:"CycleCapacitySizePrecise"`
	CycleCapacitySize          *codeBuddyNum `json:"CycleCapacitySize"`
	CapacitySizePrecise        *codeBuddyNum `json:"CapacitySizePrecise"`
	CapacitySize               *codeBuddyNum `json:"CapacitySize"`
	CycleCapacityRemainPrecise *codeBuddyNum `json:"CycleCapacityRemainPrecise"`
	CapacityRemainPrecise      *codeBuddyNum `json:"CapacityRemainPrecise"`
	CapacityRemain             *codeBuddyNum `json:"CapacityRemain"`
	CapacityUsedPrecise        *codeBuddyNum `json:"CapacityUsedPrecise"`
	CapacityUsed               *codeBuddyNum `json:"CapacityUsed"`

	CycleEndTime     codeBuddyTime `json:"CycleEndTime"`
	DeductionEndTime codeBuddyTime `json:"DeductionEndTime"`
	ExpiredTime      codeBuddyTime `json:"ExpiredTime"`
}

// codeBuddyNum is a numeric billing field (capacity, remaining, used) that
// CodeBuddy returns inconsistently as a JSON number OR a JSON string (e.g.
// "1000.00"). Unmarshals both forms into a float64; a non-numeric string or null
// becomes 0 (callers gate on the pointer being non-nil for "present").
type codeBuddyNum float64

func (c *codeBuddyNum) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*c = 0
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		str = strings.TrimSpace(str)
		if str == "" {
			*c = 0
			return nil
		}
		f, err := strconv.ParseFloat(str, 64)
		if err != nil {
			return err
		}
		*c = codeBuddyNum(f)
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*c = codeBuddyNum(f)
	return nil
}

// codeBuddyTime is a reset-timestamp field that CodeBuddy returns inconsistently:
// sometimes as an ISO/date string ("2026-07-01 00:00:00"), sometimes as a numeric
// Unix timestamp (seconds or milliseconds), and sometimes null. It unmarshals all
// of those into a normalized string so the rest of the parser can treat resets
// uniformly. A numeric value is rendered as RFC3339 in UTC; a string is kept
// verbatim.
type codeBuddyTime string

func (c *codeBuddyTime) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*c = ""
		return nil
	}
	// String form: strip the surrounding quotes and keep verbatim.
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		*c = codeBuddyTime(strings.TrimSpace(str))
		return nil
	}
	// Numeric form: Unix seconds or milliseconds. Heuristic: >= 1e12 is ms.
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	if n <= 0 {
		*c = ""
		return nil
	}
	secs := int64(n)
	if n >= 1e12 {
		secs = int64(n) / 1000
	}
	*c = codeBuddyTime(time.Unix(secs, 0).UTC().Format(time.RFC3339))
	return nil
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

// codeBuddyUsageBody builds the billing query body. When includePackages is true
// (cookie path) the full package-code list is sent; when false (OAuth path) it is
// omitted, matching the IDE CLI which lets the gateway resolve the caller's own
// packages from the token.
func codeBuddyUsageBody(includePackages bool) []byte {
	now := time.Now()
	end := now.AddDate(101, 0, 0)
	fmtTime := func(t time.Time) string { return t.Format("2006-01-02 15:04:05") }
	body := codeBuddyUsageRequest{
		PageNumber:               1,
		PageSize:                 200,
		ProductCode:              codeBuddyProductCN,
		Status:                   []int{0, 3},
		PackageEndTimeRangeBegin: fmtTime(now),
		PackageEndTimeRangeEnd:   fmtTime(end),
	}
	if includePackages {
		body.PackageCodes = codeBuddyPackageCodes
	}
	b, _ := json.Marshal(body)
	return b
}

// codeBuddyBillingHost returns the bare host (no scheme) the billing + identity
// calls target for an account. The international site (codebuddy-ai) and the CN
// gateway (codebuddy) both serve billing from www.codebuddy.ai per the reference
// implementation, so that is the default; a per-account BaseURLOverride host wins
// if one is set.
func codeBuddyBillingHost(acct *config.Account) string {
	if acct != nil && strings.TrimSpace(acct.BaseURLOverride) != "" {
		u := strings.TrimSpace(acct.BaseURLOverride)
		u = strings.TrimPrefix(u, "https://")
		u = strings.TrimPrefix(u, "http://")
		if i := strings.IndexAny(u, "/"); i >= 0 {
			u = u[:i]
		}
		if u != "" {
			return u
		}
	}
	return "www.codebuddy.ai"
}

// CodeBuddyIdentity is the account profile served by /v2/plugin/accounts: the
// display email/nickname shown in the dashboard plus the uid/enterpriseId the
// billing call needs as headers.
type CodeBuddyIdentity struct {
	UID          string
	EnterpriseID string
	Email        string
	Nickname     string
}

// FetchCodeBuddyIdentity looks up the account profile (email, nickname, uid,
// enterpriseId) via the authenticated /v2/plugin/accounts endpoint. Used both at
// login (to populate the dashboard email) and by the quota poll (to carry the
// X-User-Id / X-Enterprise-Id headers the gateway expects). A failure is
// non-fatal — callers fall back to whatever they already have.
func FetchCodeBuddyIdentity(ctx context.Context, acct *config.Account) (CodeBuddyIdentity, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	host := codeBuddyBillingHost(acct)
	req, err := http.NewRequestWithContext(ctx, "GET", "https://"+host+codeBuddyAccountsPath, nil)
	if err != nil {
		return CodeBuddyIdentity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(acct.AccessToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Domain", host)

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(acct)).Do(req)
	if err != nil {
		return CodeBuddyIdentity{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return CodeBuddyIdentity{}, fmt.Errorf("codebuddy accounts endpoint returned HTTP %d", resp.StatusCode)
	}
	var d struct {
		Data struct {
			Accounts []struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Email        string `json:"email"`
				Nickname     string `json:"nickname"`
				LastLogin    any    `json:"lastLogin"`
			} `json:"accounts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return CodeBuddyIdentity{}, fmt.Errorf("codebuddy accounts parse failed: %w", err)
	}
	if len(d.Data.Accounts) == 0 {
		return CodeBuddyIdentity{}, nil
	}
	// Prefer the account flagged lastLogin, else the first row (mirrors 9router).
	chosen := d.Data.Accounts[0]
	for _, a := range d.Data.Accounts {
		if a.LastLogin != nil {
			chosen = a
			break
		}
	}
	return CodeBuddyIdentity{
		UID:          chosen.UID,
		EnterpriseID: chosen.EnterpriseID,
		Email:        chosen.Email,
		Nickname:     chosen.Nickname,
	}, nil
}

// FetchCodeBuddyQuota returns normalized credit figures for a CodeBuddy account.
// It uses the IDE OAuth access token as the PRIMARY credential — the same token
// inference uses — so quota tracking is automatic with no manual step (this is
// what the official CLI and 9router do; the earlier "cookie only" assumption was
// wrong). When a web session cookie has been imported it is used as a fallback if
// the OAuth path is rejected. The account's own proxy is honored so the poll exits
// the same egress as inference.
func FetchCodeBuddyQuota(ctx context.Context, acct *config.Account) (*CodeBuddyQuota, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	// Primary: IDE OAuth token (automatic — no cookie needed).
	if strings.TrimSpace(acct.AccessToken) != "" {
		q, err := fetchCodeBuddyQuotaOAuth(ctx, acct)
		if err == nil {
			return q, nil
		}
		// OAuth failed. Fall through to the cookie path only if a cookie exists;
		// otherwise surface the OAuth error so the operator knows the token is stale.
		if normalizeCookie(acct.WebCookie) == "" {
			return nil, err
		}
		logger.Debugf("[CodeBuddy] OAuth quota fetch failed for %s, trying stored cookie: %v", acct.ID, err)
	}

	// Fallback: imported web session cookie.
	return fetchCodeBuddyQuotaCookie(ctx, acct)
}

// fetchCodeBuddyQuotaOAuth fetches quota using the IDE OAuth access token, with
// the identity headers resolved from /v2/plugin/accounts. This is the automatic
// path: it needs nothing beyond the token the login flow already captured.
func fetchCodeBuddyQuotaOAuth(ctx context.Context, acct *config.Account) (*CodeBuddyQuota, error) {
	host := codeBuddyBillingHost(acct)
	identity, _ := FetchCodeBuddyIdentity(ctx, acct)
	uid, enterpriseID := identity.UID, identity.EnterpriseID

	req, err := http.NewRequestWithContext(ctx, "POST", "https://"+host+codeBuddyUsagePathOAuth, bytes.NewReader(codeBuddyUsageBody(false)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(acct.AccessToken))
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Domain", host)
	if uid != "" {
		req.Header.Set("X-User-Id", uid)
	}
	if enterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", enterpriseID)
		req.Header.Set("X-Tenant-Id", enterpriseID)
	}

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(acct)).Do(req)
	if err != nil {
		return nil, fmt.Errorf("codebuddy quota request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("codebuddy IDE OAuth token was rejected (HTTP %d); the token may be expired", resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("codebuddy quota endpoint returned HTTP %d", resp.StatusCode)
	}
	return parseCodeBuddyQuota(raw)
}

// fetchCodeBuddyQuotaCookie fetches quota using a manually-imported web session
// cookie. Retained as a fallback for accounts whose OAuth token can't reach the
// billing API (rare). An account with no cookie returns an error.
func fetchCodeBuddyQuotaCookie(ctx context.Context, acct *config.Account) (*CodeBuddyQuota, error) {
	cookie := normalizeCookie(acct.WebCookie)
	if cookie == "" {
		return nil, fmt.Errorf("no CodeBuddy credential available for account %s: OAuth token rejected and no web session cookie stored", acct.ID)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", codeBuddyUsageURLCookie, bytes.NewReader(codeBuddyUsageBody(true)))
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
		return nil, fmt.Errorf("codebuddy web session cookie is not authorized (HTTP %d); re-import a fresh cookie", resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("codebuddy quota endpoint returned HTTP %d", resp.StatusCode)
	}
	return parseCodeBuddyQuota(raw)
}

// parseCodeBuddyQuota extracts and sums the credit figures from the billing
// response. Tolerates the OAuth-path {data:{Response:{Data:{Accounts:[]}}}}, the
// web-console {Response:{Data:{Accounts:[]}}}, the {data:{accounts:[]}}, and the
// bare {Accounts:[]} shapes (mirrors 9router parseCodeBuddyUsage envelope order).
func parseCodeBuddyQuota(raw []byte) (*CodeBuddyQuota, error) {
	var env struct {
		Response struct {
			Data struct {
				Accounts []codeBuddyBillingAccount `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
		Data struct {
			Response struct {
				Data struct {
					Accounts []codeBuddyBillingAccount `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
			Accounts []codeBuddyBillingAccount `json:"Accounts"`
			Lower    []codeBuddyBillingAccount `json:"accounts"`
		} `json:"data"`
		Accounts []codeBuddyBillingAccount `json:"Accounts"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("codebuddy quota parse failed: %w", err)
	}
	accounts := env.Data.Response.Data.Accounts
	if len(accounts) == 0 {
		accounts = env.Response.Data.Accounts
	}
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
			safeTotal = float64(*total)
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
		q.ResetAt = earlierReset(q.ResetAt, firstNonBlank(string(a.CycleEndTime), string(a.DeductionEndTime), string(a.ExpiredTime)))
	}
	return q, nil
}

func firstFloat(vals ...*codeBuddyNum) *codeBuddyNum {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func deref(v *codeBuddyNum) float64 {
	if v == nil {
		return 0
	}
	return float64(*v)
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
	// Backfill the display email/nickname for accounts created before identity
	// capture (or where login's best-effort lookup failed). Only fills blanks so a
	// user-edited nickname is never overwritten.
	if strings.TrimSpace(acct.Email) == "" {
		if id, ierr := FetchCodeBuddyIdentity(ctx, &acct); ierr == nil {
			if id.Email != "" {
				update.Email = id.Email
			}
			if strings.TrimSpace(acct.Nickname) == "" && id.Nickname != "" {
				update.Nickname = id.Nickname
			}
		}
	}
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
