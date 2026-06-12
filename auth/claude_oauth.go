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

// Claude (Anthropic) Code OAuth — authorization-code + PKCE against claude.ai,
// ported from 9router's src/lib/oauth/providers.js (the `claude` handler) and
// CLAUDE_CONFIG. Claude Code uses a MANUAL-CODE paste flow (not a localhost
// callback): the browser redirects to the Anthropic console callback page which
// displays a `code#state` string the operator copies back into the dashboard.
//
// After auth, Claude is the Anthropic Messages dialect at api.anthropic.com with a
// Bearer OAuth access token (NOT x-api-key), so the proxy reuses the generic
// Anthropic provider for inference and only needs this package for the login +
// token refresh.
const (
	claudeClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeAuthorizeURL = "https://claude.ai/oauth/authorize"
	claudeTokenURL     = "https://api.anthropic.com/v1/oauth/token"
	claudeRedirectURI  = "https://console.anthropic.com/oauth/code/callback"
	claudeScopes       = "org:create_api_key user:profile user:inference"
)

// ClaudeAuthStart carries the data needed to surface the browser login and later
// complete the manual-code exchange.
type ClaudeAuthStart struct {
	AuthURL      string
	State        string
	CodeVerifier string
}

// ClaudeTokens is a completed Claude OAuth exchange or refresh.
type ClaudeTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
	Scope        string
}

// StartClaudeLogin builds the PKCE authorize URL for the manual-code flow.
func StartClaudeLogin() (*ClaudeAuthStart, error) {
	verifier := generateCodeVerifier()
	challenge := claudePKCEChallenge(verifier)
	state := generateCodeVerifier()

	params := url.Values{}
	params.Set("code", "true")
	params.Set("client_id", claudeClientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", claudeRedirectURI)
	params.Set("scope", claudeScopes)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("state", state)

	return &ClaudeAuthStart{
		AuthURL:      claudeAuthorizeURL + "?" + params.Encode(),
		State:        state,
		CodeVerifier: verifier,
	}, nil
}

// ExchangeClaudeCode swaps a pasted authorization code for tokens. The pasted code
// may carry the state after a '#' (code#state); we split it the way 9router does.
func ExchangeClaudeCode(ctx context.Context, pastedCode, codeVerifier, state string) (*ClaudeTokens, error) {
	authCode := strings.TrimSpace(pastedCode)
	codeState := ""
	if i := strings.Index(authCode, "#"); i >= 0 {
		codeState = authCode[i+1:]
		authCode = authCode[:i]
	}
	if codeState == "" {
		codeState = state
	}

	payload := map[string]string{
		"code":          authCode,
		"state":         codeState,
		"grant_type":    "authorization_code",
		"client_id":     claudeClientID,
		"redirect_uri":  claudeRedirectURI,
		"code_verifier": codeVerifier,
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", claudeTokenURL, strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude token exchange request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("claude token exchange failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	return parseClaudeTokenResponse(body)
}

// RefreshClaudeToken renews a Claude access token via the refresh grant (JSON body).
func RefreshClaudeToken(ctx context.Context, refreshToken string) (*ClaudeTokens, error) {
	payload := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     claudeClientID,
		"refresh_token": refreshToken,
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", claudeTokenURL, strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude token refresh request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("claude token refresh failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	t, err := parseClaudeTokenResponse(body)
	if err != nil {
		return nil, err
	}
	if t.RefreshToken == "" {
		t.RefreshToken = refreshToken
	}
	return t, nil
}

func parseClaudeTokenResponse(body []byte) (*ClaudeTokens, error) {
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("claude token response parse failed: %w", err)
	}
	if raw.AccessToken == "" {
		return nil, fmt.Errorf("claude token response missing access_token")
	}
	return &ClaudeTokens{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		ExpiresIn:    raw.ExpiresIn,
		Scope:        raw.Scope,
	}, nil
}

func claudePKCEChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
