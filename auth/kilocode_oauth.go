package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Kilo Code custom device-authorization flow, ported from 9router's
// src/lib/oauth/providers.js (the `kilocode` handler) and KILOCODE_CONFIG.
// Unlike a standard OAuth device flow, Kilo Code uses a bespoke poll:
//  1. POST /api/device-auth/codes -> { code, verificationUrl, expiresIn }
//  2. GET  /api/device-auth/codes/{code} -> 202 pending, 200 { status, token, ... }
//  3. On approval, fetch /api/profile for the org id (X-Kilocode-OrganizationID header).
//
// There is NO refresh token; the issued bearer token is long-lived and re-login is
// required when it lapses. After auth, inference is ordinary OpenAI-compatible
// /chat/completions at api.kilo.ai with the Bearer token, so the proxy reuses the
// generic OpenAI provider.
const (
	kilocodeAPIBaseURL  = "https://api.kilo.ai"
	kilocodeInitiateURL = "https://api.kilo.ai/api/device-auth/codes"
	kilocodePollURLBase = "https://api.kilo.ai/api/device-auth/codes"
	kilocodeProfileURL  = "https://api.kilo.ai/api/profile"
)

// KilocodeDeviceAuth is a started device-authorization request.
type KilocodeDeviceAuth struct {
	Code            string
	VerificationURL string
	ExpiresIn       int
	Interval        int
}

// KilocodeTokens is a completed token exchange. There is no refresh token.
type KilocodeTokens struct {
	AccessToken string
	UserEmail   string
	OrgID       string
}

// StartKilocodeDeviceAuth requests a device code + verification URL.
func StartKilocodeDeviceAuth(ctx context.Context) (*KilocodeDeviceAuth, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", kilocodeInitiateURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("kilocode device-auth request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("kilocode: too many pending authorization requests, try again later")
	}
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return nil, fmt.Errorf("kilocode device-auth failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	var d struct {
		Code            string `json:"code"`
		VerificationURL string `json:"verificationUrl"`
		ExpiresIn       int    `json:"expiresIn"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("kilocode device-auth parse failed: %w", err)
	}
	if d.Code == "" {
		return nil, fmt.Errorf("kilocode device-auth response missing code")
	}
	expiresIn := d.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 300
	}
	return &KilocodeDeviceAuth{
		Code:            d.Code,
		VerificationURL: d.VerificationURL,
		ExpiresIn:       expiresIn,
		Interval:        3,
	}, nil
}

// PollKilocodeToken performs ONE poll. Returns "pending"/"ok"/"" with err.
// HTTP 202 = pending, 403 = denied, 410 = expired, 200 = approved (with token).
func PollKilocodeToken(ctx context.Context, code string) (status string, tokens *KilocodeTokens, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", kilocodePollURLBase+"/"+code, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return "pending", nil, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case 202:
		return "pending", nil, nil
	case 403:
		return "", nil, fmt.Errorf("kilocode: authorization denied by user")
	case 410:
		return "", nil, fmt.Errorf("kilocode: authorization code expired")
	case 200:
		var d struct {
			Status    string `json:"status"`
			Token     string `json:"token"`
			UserEmail string `json:"userEmail"`
		}
		if err := json.Unmarshal(body, &d); err != nil {
			return "", nil, fmt.Errorf("kilocode poll parse failed: %w", err)
		}
		if d.Status == "approved" && d.Token != "" {
			orgID := fetchKilocodeOrgID(ctx, d.Token)
			return "ok", &KilocodeTokens{AccessToken: d.Token, UserEmail: d.UserEmail, OrgID: orgID}, nil
		}
		return "pending", nil, nil
	default:
		return "", nil, fmt.Errorf("kilocode poll failed: HTTP %d %s", resp.StatusCode, string(body))
	}
}

// fetchKilocodeOrgID looks up the account's first organization id, used as the
// X-Kilocode-OrganizationID inference header. Best-effort: returns "" on any error.
func fetchKilocodeOrgID(ctx context.Context, token string) string {
	req, err := http.NewRequestWithContext(ctx, "GET", kilocodeProfileURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient().Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return ""
	}
	var p struct {
		Organizations []struct {
			ID string `json:"id"`
		} `json:"organizations"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return ""
	}
	if len(p.Organizations) > 0 {
		return p.Organizations[0].ID
	}
	return ""
}
