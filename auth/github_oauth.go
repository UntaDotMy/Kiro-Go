package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// GitHub Copilot OAuth device flow, ported from 9router's
// src/lib/oauth/providers.js (the `github` handler) and GITHUB_CONFIG.
//
// GitHub Copilot is a TWO-TOKEN provider:
//  1. The OAuth device flow yields a long-lived GitHub access token (gho_...).
//  2. That token is exchanged at copilot_internal/v2/token for a SHORT-LIVED
//     Copilot bearer token (~30 min) which is what actually authenticates the
//     inference calls to api.githubcopilot.com.
//
// The device flow returns no refresh_token, so we persist the GitHub access token
// (in the account's RefreshToken slot) and re-mint the Copilot token from it on
// every refresh. Account.AccessToken holds the current Copilot token used for
// inference; ExpiresAt holds the Copilot token's expiry so the scheduler renews it.
const (
	githubClientID      = "Iv1.b507a08c87ecfe98"
	githubDeviceCodeURL = "https://github.com/login/device/code"
	githubTokenURL      = "https://github.com/login/oauth/access_token"
	githubUserInfoURL   = "https://api.github.com/user"
	githubCopilotTokURL = "https://api.github.com/copilot_internal/v2/token"
	githubScopes        = "read:user"
	githubAPIVersion    = "2022-11-28"
	githubUserAgent     = "GitHubCopilotChat/0.26.7"
	githubGrantDevice   = "urn:ietf:params:oauth:grant-type:device_code"
)

// GitHubDeviceAuth is a started device-authorization request.
type GitHubDeviceAuth struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	ExpiresIn       int
	Interval        int
}

// GitHubTokens is a completed login: the GitHub access token plus the minted
// Copilot token used for inference and the resolved GitHub login.
type GitHubTokens struct {
	GitHubToken        string // long-lived gho_... — re-mints copilot tokens
	CopilotToken       string // short-lived inference bearer
	CopilotExpiresAt   int64  // Unix seconds
	GitHubLogin        string
	GitHubUserID       int64
}

// StartGitHubDeviceAuth posts a device-code request and returns the user code +
// verification URL for the operator to approve.
func StartGitHubDeviceAuth(ctx context.Context) (*GitHubDeviceAuth, error) {
	form := url.Values{}
	form.Set("client_id", githubClientID)
	form.Set("scope", githubScopes)

	req, err := http.NewRequestWithContext(ctx, "POST", githubDeviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("github device-code request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github device-code failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	var d struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("github device-code parse failed: %w", err)
	}
	if d.DeviceCode == "" {
		return nil, fmt.Errorf("github device-code response missing device_code")
	}
	interval := d.Interval
	if interval < 1 {
		interval = 5
	}
	return &GitHubDeviceAuth{
		DeviceCode:      d.DeviceCode,
		UserCode:        d.UserCode,
		VerificationURI: d.VerificationURI,
		ExpiresIn:       d.ExpiresIn,
		Interval:        interval,
	}, nil
}

// PollGitHubToken performs ONE token poll. On approval it also mints the Copilot
// token and fetches the GitHub login. Returns "pending"/"ok"/"" with err.
func PollGitHubToken(ctx context.Context, deviceCode string) (status string, tokens *GitHubTokens, err error) {
	form := url.Values{}
	form.Set("client_id", githubClientID)
	form.Set("device_code", deviceCode)
	form.Set("grant_type", githubGrantDevice)

	req, err := http.NewRequestWithContext(ctx, "POST", githubTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return "pending", nil, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var d struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(body, &d)
	if d.Error == "authorization_pending" || d.Error == "slow_down" {
		return "pending", nil, nil
	}
	if d.Error != "" {
		return "", nil, fmt.Errorf("github token poll error: %s", d.Error)
	}
	if d.AccessToken == "" {
		return "pending", nil, nil
	}

	// Approved: mint the Copilot token and fetch the login.
	copTok, copExp, cerr := MintGitHubCopilotToken(ctx, d.AccessToken)
	if cerr != nil {
		return "", nil, cerr
	}
	login, uid := fetchGitHubUser(ctx, d.AccessToken)
	return "ok", &GitHubTokens{
		GitHubToken:      d.AccessToken,
		CopilotToken:     copTok,
		CopilotExpiresAt: copExp,
		GitHubLogin:      login,
		GitHubUserID:     uid,
	}, nil
}

// MintGitHubCopilotToken exchanges a GitHub access token for a short-lived Copilot
// bearer token (returns the token and its Unix-seconds expiry). Called at login and
// on every refresh.
func MintGitHubCopilotToken(ctx context.Context, githubToken string) (token string, expiresAt int64, err error) {
	req, rerr := http.NewRequestWithContext(ctx, "GET", githubCopilotTokURL, nil)
	if rerr != nil {
		return "", 0, rerr
	}
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", githubUserAgent)

	resp, derr := httpClient().Do(req)
	if derr != nil {
		return "", 0, fmt.Errorf("github copilot-token request failed: %w", derr)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", 0, fmt.Errorf("github copilot-token failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	var d struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return "", 0, fmt.Errorf("github copilot-token parse failed: %w", err)
	}
	if d.Token == "" {
		return "", 0, fmt.Errorf("github copilot-token response missing token")
	}
	return d.Token, d.ExpiresAt, nil
}

func fetchGitHubUser(ctx context.Context, githubToken string) (login string, id int64) {
	req, err := http.NewRequestWithContext(ctx, "GET", githubUserInfoURL, nil)
	if err != nil {
		return "", 0
	}
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", githubUserAgent)
	resp, err := httpClient().Do(req)
	if err != nil {
		return "", 0
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", 0
	}
	var u struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	}
	if err := json.Unmarshal(body, &u); err != nil {
		return "", 0
	}
	return u.Login, u.ID
}
