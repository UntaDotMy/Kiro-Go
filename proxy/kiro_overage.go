package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

// kiroQAPIBaseForAccount returns the AWS Q Developer endpoint that owns the
// user-level Overages switch, pinned to the account's stable region (A13's
// resolveAccountRegion) so an identity's overage calls hit the same region as
// its inference traffic. The Q host is q.<region>.amazonaws.com.
func kiroQAPIBaseForAccount(account *config.Account) string {
	region := resolveAccountRegion(account)
	if region == "" {
		region = "us-east-1"
	}
	return fmt.Sprintf("https://q.%s.amazonaws.com", region)
}

// OverageSnapshot captures the upstream Overages state for an account.
type OverageSnapshot struct {
	Status            string  `json:"status"`            // "ENABLED" | "DISABLED" | "UNKNOWN"
	Capability        string  `json:"capability"`        // "OVERAGE_CAPABLE" | ...
	SubscriptionTitle string  `json:"subscriptionTitle"` // e.g. "KIRO PRO+"
	OverageCap        float64 `json:"overageCap"`        // USD upper bound
	OverageRate       float64 `json:"overageRate"`       // per-invocation USD
	CurrentOverages   float64 `json:"currentOverages"`   // accumulated overage USD
	CheckedAt         int64   `json:"checkedAt"`         // Unix seconds
}

// upstreamOverageResponse mirrors the parts of /getUsageLimits we need for the
// Overages switch UI. Other fields are parsed elsewhere (RefreshAccountInfo).
type upstreamOverageResponse struct {
	OverageConfiguration *struct {
		OverageStatus string `json:"overageStatus"`
	} `json:"overageConfiguration"`
	SubscriptionInfo *struct {
		OverageCapability string `json:"overageCapability"`
		SubscriptionTitle string `json:"subscriptionTitle"`
	} `json:"subscriptionInfo"`
	UsageBreakdownList []struct {
		ResourceType    string  `json:"resourceType"`
		OverageCap      float64 `json:"overageCap"`
		OverageRate     float64 `json:"overageRate"`
		CurrentOverages float64 `json:"currentOverages"`
	} `json:"usageBreakdownList"`
}

// FetchOverageStatus calls AWS Q GET /getUsageLimits and extracts the Overages
// switch state plus the real billing figures (cap / rate / accumulated $). This
// is read-only and does NOT change any billing behavior.
func FetchOverageStatus(account *config.Account) (*OverageSnapshot, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}

	rawURL := kiroQAPIBaseForAccount(account) + "/getUsageLimits?origin=AI_EDITOR&resourceType=AGENTIC_REQUEST&isEmailRequired=true"
	if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
		rawURL += "&profileArn=" + neturl.QueryEscape(profileArn)
	}

	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	setKiroHeaders(req, account)

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var parsed upstreamOverageResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode getUsageLimits: %w", err)
	}

	snap := &OverageSnapshot{
		Status:    "UNKNOWN",
		CheckedAt: time.Now().Unix(),
	}
	if parsed.OverageConfiguration != nil && parsed.OverageConfiguration.OverageStatus != "" {
		snap.Status = strings.ToUpper(parsed.OverageConfiguration.OverageStatus)
	}
	if parsed.SubscriptionInfo != nil {
		snap.Capability = parsed.SubscriptionInfo.OverageCapability
		snap.SubscriptionTitle = parsed.SubscriptionInfo.SubscriptionTitle
	}
	for _, bd := range parsed.UsageBreakdownList {
		if bd.OverageCap > 0 || bd.OverageRate > 0 || bd.CurrentOverages > 0 {
			snap.OverageCap = bd.OverageCap
			snap.OverageRate = bd.OverageRate
			snap.CurrentOverages = bd.CurrentOverages
			break
		}
	}
	return snap, nil
}

// SetOverageStatus calls AWS Q POST /setUserPreference to flip the REAL
// user-level Overages billing switch, then re-fetches the snapshot for cache
// write-through. enabled=true → "ENABLED", enabled=false → "DISABLED".
//
// WARNING: enabling overage authorizes real overage billing on the account.
// The admin endpoint that calls this requires an explicit confirm.
func SetOverageStatus(account *config.Account, enabled bool) (*OverageSnapshot, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}

	profileArn, err := ResolveProfileArn(account)
	if err != nil {
		return nil, fmt.Errorf("resolve profileArn: %w", err)
	}

	status := "DISABLED"
	if enabled {
		status = "ENABLED"
	}
	payload := map[string]interface{}{
		"overageConfiguration": map[string]string{
			"overageStatus": status,
		},
		"profileArn": profileArn,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", kiroQAPIBaseForAccount(account)+"/setUserPreference", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	setKiroHeaders(req, account)
	req.Header.Set("Content-Type", "application/json")

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("setUserPreference HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	logger.Infof("[Overage] account=%s flipped overageStatus=%s upstream", redactForLog(account.Email), status)

	// Best-effort re-read so cached fields (cap/rate/current) stay accurate.
	snap, fetchErr := FetchOverageStatus(account)
	if fetchErr != nil {
		// The POST succeeded; return a synthesized snapshot.
		logger.Warnf("[Overage] re-fetch after switch failed for %s: %v", redactForLog(account.Email), fetchErr)
		return &OverageSnapshot{Status: status, CheckedAt: time.Now().Unix()}, nil
	}
	// AWS can lag; force the just-set value.
	snap.Status = status
	return snap, nil
}

// PersistOverageSnapshot writes a snapshot back to config.json for an account.
func PersistOverageSnapshot(accountID string, snap *OverageSnapshot) error {
	if snap == nil {
		return nil
	}
	return config.UpdateAccountOverageStatus(
		accountID,
		snap.Status,
		snap.Capability,
		snap.OverageCap,
		snap.OverageRate,
		snap.CurrentOverages,
		snap.CheckedAt,
	)
}
