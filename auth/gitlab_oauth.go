package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// GitLab Duo OAuth — authorization-code + PKCE, ported from 9router's
// src/lib/oauth/providers.js (the `gitlab` handler) and GITLAB_CONFIG. GitLab is
// SELF-HOSTABLE, so the operator supplies their instance baseURL + the OAuth app's
// clientId (and optional clientSecret for confidential apps) registered on that
// instance. After auth, inference is OpenAI-compatible at
// {baseURL}/api/v4/chat/completions with a Bearer token, so the proxy reuses the
// generic OpenAI provider; this package owns the login + token refresh.
//
// Flow: StartGitLabLogin builds the authorize URL (manual-paste — GitLab shows the
// code), ExchangeGitLabCode swaps it for tokens and fetches the username.
const (
	gitlabDefaultBaseURL = "https://gitlab.com"
	gitlabAuthorizePath  = "/oauth/authorize"
	gitlabTokenPath      = "/oauth/token"
	gitlabUserInfoPath   = "/api/v4/user"
	gitlabScope          = "api read_user"
	// gitlabRedirectURI is GitLab's standard OOB redirect that displays the code.
	gitlabRedirectURI = "urn:ietf:wg:oauth:2.0:oob"
)

// GitLabAuthStart carries the data to surface the login and complete the exchange.
type GitLabAuthStart struct {
	AuthURL      string
	State        string
	CodeVerifier string
}

// GitLabTokens is a completed GitLab exchange/refresh.
type GitLabTokens struct {
	AccessToken  string
	RefreshToken string
	Username     string
	ExpiresIn    int
}

func gitlabBase(baseURL string) string {
	b := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if b == "" {
		return gitlabDefaultBaseURL
	}
	if !strings.HasPrefix(strings.ToLower(b), "http") {
		b = "https://" + b
	}
	return b
}

// StartGitLabLogin builds the PKCE authorize URL for the operator's instance + app.
func StartGitLabLogin(baseURL, clientID string) (*GitLabAuthStart, error) {
	if strings.TrimSpace(clientID) == "" {
		return nil, fmt.Errorf("gitlab clientId is required")
	}
	verifier := generateCodeVerifier()
	challenge := gitlabPKCEChallenge(verifier)
	state := generateCodeVerifier()

	params := url.Values{}
	params.Set("client_id", clientID)
	params.Set("redirect_uri", gitlabRedirectURI)
	params.Set("response_type", "code")
	params.Set("state", state)
	params.Set("scope", gitlabScope)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")

	return &GitLabAuthStart{
		AuthURL:      gitlabBase(baseURL) + gitlabAuthorizePath + "?" + params.Encode(),
		State:        state,
		CodeVerifier: verifier,
	}, nil
}

// ExchangeGitLabCode swaps a pasted authorization code for tokens, then fetches the
// username. clientSecret is optional (empty for public PKCE apps).
func ExchangeGitLabCode(ctx context.Context, baseURL, clientID, clientSecret, code, codeVerifier string) (*GitLabTokens, error) {
	base := gitlabBase(baseURL)
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "authorization_code")
	form.Set("code", strings.TrimSpace(code))
	form.Set("redirect_uri", gitlabRedirectURI)
	form.Set("code_verifier", codeVerifier)
	if strings.TrimSpace(clientSecret) != "" {
		form.Set("client_secret", clientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", base+gitlabTokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitlab token exchange request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gitlab token exchange failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	t, err := parseGitLabTokenResponse(body)
	if err != nil {
		return nil, err
	}
	t.Username = fetchGitLabUsername(ctx, base, t.AccessToken)
	return t, nil
}

// RefreshGitLabToken renews tokens via the refresh grant.
func RefreshGitLabToken(ctx context.Context, baseURL, clientID, clientSecret, refreshToken string) (*GitLabTokens, error) {
	base := gitlabBase(baseURL)
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("redirect_uri", gitlabRedirectURI)
	if strings.TrimSpace(clientSecret) != "" {
		form.Set("client_secret", clientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", base+gitlabTokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitlab token refresh request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gitlab token refresh failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	t, err := parseGitLabTokenResponse(body)
	if err != nil {
		return nil, err
	}
	if t.RefreshToken == "" {
		t.RefreshToken = refreshToken
	}
	return t, nil
}

func parseGitLabTokenResponse(body []byte) (*GitLabTokens, error) {
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("gitlab token response parse failed: %w", err)
	}
	if raw.AccessToken == "" {
		return nil, fmt.Errorf("gitlab token response missing access_token")
	}
	return &GitLabTokens{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		ExpiresIn:    raw.ExpiresIn,
	}, nil
}

func fetchGitLabUsername(ctx context.Context, base, accessToken string) string {
	req, err := http.NewRequestWithContext(ctx, "GET", base+gitlabUserInfoPath, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
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
	var u struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(body, &u); err != nil {
		return ""
	}
	return u.Username
}

func gitlabPKCEChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
