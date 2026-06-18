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

// codeBuddyCNBillingBase is the billing/checkin host for codebuddy-cn. Unlike
// inference (codebuddy.cn), the credit/checkin meter lives on copilot.tencent.com
// (api_client.py:37 BILLING_API_BASE) — do NOT route these through
// codeBuddyBillingHost (which defaults to www.codebuddy.ai).
const codeBuddyCNBillingBase = "https://copilot.tencent.com"

// SyncCodeBuddyCNQuota fetches live billing quota for one codebuddy-cn account
// from https://copilot.tencent.com/v2/billing/meter/get-user-resource using
// the ck_ API key as a Bearer token. It folds the returned CycleCapacity figures
// into the account's persisted Usage* fields (same fields the dashboard already
// renders). The ck_ key resolves its own packages — no explicit package codes
// are needed. A non-codebuddy-cn account is a no-op (nil, nil).
//
// Reuses the existing parseCodeBuddyQuota helper from codebuddy_quota.go (same
// package).
func SyncCodeBuddyCNQuota(ctx context.Context, acctID string) (*CodeBuddyQuota, error) {
	acct, ok := config.GetAccount(acctID)
	if !ok {
		return nil, fmt.Errorf("account %s not found", acctID)
	}
	backend := config.GetAccountBackend(&acct)
	if backend != "codebuddy-cn" {
		return nil, nil
	}

	q, err := fetchCodeBuddyCNQuota(ctx, &acct)
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
	logger.Infof("[CodeBuddyCN] quota synced for %s: used=%.0f/%.0f (%s plan, %d records)", acctID, q.Used, q.Total, q.Plan, q.Records)
	return q, nil
}

// codeBuddyCNBearer resolves the Bearer credential for a codebuddy-cn account:
// the ck_ API key first, falling back to the OAuth access token.
func codeBuddyCNBearer(acct *config.Account) string {
	key := strings.TrimSpace(acct.APIKey)
	if key == "" {
		key = strings.TrimSpace(acct.AccessToken)
	}
	return key
}

// codeBuddyCNBillingPost POSTs body to baseURL+path with API-key headers only
// (api_client.py:113-119): Authorization Bearer, Content-Type, Accept — no
// X-User-Id / X-Domain. It returns the HTTP status and the (bounded) response
// body. The body is read EVEN ON non-2xx because the billing gateway returns a
// 400 with a JSON business code (e.g. daily-checkin code 10001 "already checked
// in") that the caller must inspect — discarding the 400 body would turn a
// normal already-signed response into a false error (api_client.py:184, 200-217).
func codeBuddyCNBillingPost(ctx context.Context, acct *config.Account, baseURL, path string, body []byte) (int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	url := strings.TrimRight(baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+codeBuddyCNBearer(acct))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(acct)).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, nil
}

// fetchCodeBuddyCNQuota POSTs to copilot.tencent.com/v2/billing/meter/get-user-resource
// with the ck_ API key as Bearer.
func fetchCodeBuddyCNQuota(ctx context.Context, acct *config.Account) (*CodeBuddyQuota, error) {
	return fetchCodeBuddyCNQuotaAt(ctx, acct, codeBuddyCNBillingBase)
}

// fetchCodeBuddyCNQuotaAt is the baseURL-parameterized core so it can be driven
// against an httptest server without global config.
func fetchCodeBuddyCNQuotaAt(ctx context.Context, acct *config.Account, baseURL string) (*CodeBuddyQuota, error) {
	// BUG #2: api-key billing POSTs an empty {} body (api_client.py:180). Do NOT
	// use codeBuddyUsageBody(false) here — its ProductCode + 101-year range
	// scope-filters the ck_ key's packages to empty → quota 0.
	status, raw, err := codeBuddyCNBillingPost(ctx, acct, baseURL, "/v2/billing/meter/get-user-resource", []byte("{}"))
	if err != nil {
		return nil, fmt.Errorf("codebuddy-cn quota request failed: %w", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("codebuddy-cn quota endpoint returned HTTP %d: %s", status, string(raw))
	}
	// Reuse the existing billing parser — it handles the {Response:{Data:{Accounts:[]}}}
	// envelope and all its variants (see codebuddy_quota.go:parseCodeBuddyQuota).
	q, err := parseCodeBuddyQuota(raw)
	if err != nil {
		return nil, fmt.Errorf("codebuddy-cn quota parse failed: %w", err)
	}
	return q, nil
}

// CodeBuddyCNCheckinStatus is the daily-checkin status for a codebuddy-cn
// account (api_client.py:347-361 CheckinStatus).
type CodeBuddyCNCheckinStatus struct {
	Active         bool
	TodayCheckedIn bool
	StreakDays     int
	DailyCredit    float64
	TodayCredit    float64
	IsStreakDay    bool
}

// CodeBuddyCNCheckinResult is the outcome of a daily check-in
// (api_client.py:367-409 daily_checkin).
type CodeBuddyCNCheckinResult struct {
	Success     bool
	Already     bool    // today already checked in (code 10001 or HTTP 400)
	Credit      float64 // credit awarded this checkin
	StreakDays  int
	IsStreakDay bool
	Message     string
}

// codeBuddyCNCheckinEnvelope is the common billing response envelope.
type codeBuddyCNCheckinEnvelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Active         bool    `json:"active"`
		TodayCheckedIn bool    `json:"today_checked_in"`
		StreakDays     int     `json:"streak_days"`
		DailyCredit    float64 `json:"daily_credit"`
		TodayCredit    float64 `json:"today_credit"`
		IsStreakDay    bool    `json:"is_streak_day"`
		Credit         float64 `json:"credit"`
	} `json:"data"`
}

// CheckinStatusCodeBuddyCN reports the daily-checkin status for a codebuddy-cn
// account. A non-codebuddy-cn account is a no-op (nil, nil), mirroring
// SyncCodeBuddyCNQuota.
func CheckinStatusCodeBuddyCN(ctx context.Context, acctID string) (*CodeBuddyCNCheckinStatus, error) {
	acct, ok := config.GetAccount(acctID)
	if !ok {
		return nil, fmt.Errorf("account %s not found", acctID)
	}
	if config.GetAccountBackend(&acct) != "codebuddy-cn" {
		return nil, nil
	}
	return checkinStatusCodeBuddyCNAt(ctx, &acct, codeBuddyCNBillingBase)
}

// checkinStatusCodeBuddyCNAt is the baseURL-parameterized core for unit tests.
func checkinStatusCodeBuddyCNAt(ctx context.Context, acct *config.Account, baseURL string) (*CodeBuddyCNCheckinStatus, error) {
	status, raw, err := codeBuddyCNBillingPost(ctx, acct, baseURL, "/v2/billing/meter/checkin-status", []byte("{}"))
	if err != nil {
		return nil, fmt.Errorf("codebuddy-cn checkin-status request failed: %w", err)
	}
	var env codeBuddyCNCheckinEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("codebuddy-cn checkin-status parse failed: %w (HTTP %d)", err, status)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("codebuddy-cn checkin-status failed: %s (code %d)", env.Msg, env.Code)
	}
	return &CodeBuddyCNCheckinStatus{
		Active:         env.Data.Active,
		TodayCheckedIn: env.Data.TodayCheckedIn,
		StreakDays:     env.Data.StreakDays,
		DailyCredit:    env.Data.DailyCredit,
		TodayCredit:    env.Data.TodayCredit,
		IsStreakDay:    env.Data.IsStreakDay,
	}, nil
}

// DailyCheckinCodeBuddyCN performs the daily check-in for a codebuddy-cn account.
// A non-codebuddy-cn account is a no-op (nil, nil), mirroring SyncCodeBuddyCNQuota.
func DailyCheckinCodeBuddyCN(ctx context.Context, acctID string) (*CodeBuddyCNCheckinResult, error) {
	acct, ok := config.GetAccount(acctID)
	if !ok {
		return nil, fmt.Errorf("account %s not found", acctID)
	}
	if config.GetAccountBackend(&acct) != "codebuddy-cn" {
		return nil, nil
	}
	return dailyCheckinCodeBuddyCNAt(ctx, &acct, codeBuddyCNBillingBase)
}

// dailyCheckinCodeBuddyCNAt is the baseURL-parameterized core for unit tests.
func dailyCheckinCodeBuddyCNAt(ctx context.Context, acct *config.Account, baseURL string) (*CodeBuddyCNCheckinResult, error) {
	status, raw, err := codeBuddyCNBillingPost(ctx, acct, baseURL, "/v2/billing/meter/daily-checkin", []byte("{}"))
	if err != nil {
		return nil, fmt.Errorf("codebuddy-cn checkin request failed: %w", err)
	}

	// Parse the body even on non-2xx: daily-checkin + HTTP 400 + code 10001 is
	// the documented "already checked in" signal (api_client.py:184, 397-405).
	var env codeBuddyCNCheckinEnvelope
	if jsonErr := json.Unmarshal(raw, &env); jsonErr != nil {
		return nil, fmt.Errorf("codebuddy-cn checkin parse failed: %w (HTTP %d: %s)", jsonErr, status, string(raw))
	}

	switch {
	case env.Code == 0:
		return &CodeBuddyCNCheckinResult{
			Success:     true,
			Credit:      env.Data.Credit,
			StreakDays:  env.Data.StreakDays,
			IsStreakDay: env.Data.IsStreakDay,
		}, nil
	case env.Code == 10001:
		return &CodeBuddyCNCheckinResult{Success: true, Already: true, Message: env.Msg}, nil
	case status == 400 && (env.Code == 10001 || codeBuddyCNAlreadyMarker(env.Msg)):
		return &CodeBuddyCNCheckinResult{Success: true, Already: true, Message: env.Msg}, nil
	default:
		return nil, fmt.Errorf("codebuddy-cn checkin failed: %s (code %d)", env.Msg, env.Code)
	}
}

// codeBuddyCNAlreadyMarker reports whether msg contains an already-checked-in
// keyword (api_client.py:402).
func codeBuddyCNAlreadyMarker(msg string) bool {
	lower := strings.ToLower(msg)
	markers := []string{"already", "已签", "已领", "重复签到", "今日已"}
	for _, m := range markers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}
