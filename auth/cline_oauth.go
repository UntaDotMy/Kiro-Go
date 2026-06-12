package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Cline OAuth, ported from 9router's src/lib/oauth/providers.js (the `cline`
// handler) and CLINE_CONFIG. Cline's authorize page redirects with a `code` that
// is actually a base64-encoded JSON token bundle (accessToken/refreshToken/...).
// We surface the authorize URL, the operator approves and pastes the returned code,
// and ExchangeClineCode decodes it (falling back to the server token-exchange
// endpoint if the value isn't a base64 bundle). After auth, inference is
// OpenAI-compatible at api.cline.bot, so the proxy reuses the generic OpenAI provider.
const (
	clineAuthorizeURL = "https://api.cline.bot/api/v1/auth/authorize"
	clineTokenExchURL = "https://api.cline.bot/api/v1/auth/token"
	clineRefreshURL   = "https://api.cline.bot/api/v1/auth/refresh"
	// clineRedirectURI is the value passed as callback_url/redirect_uri. Cline shows
	// the resulting code for the operator to copy (manual-paste flow).
	clineRedirectURI = "https://app.cline.bot/auth/callback"
)

// ClineTokens is a completed Cline exchange/refresh.
type ClineTokens struct {
	AccessToken  string
	RefreshToken string
	Email        string
	ExpiresIn    int
}

// BuildClineAuthURL returns the authorize URL the operator opens.
func BuildClineAuthURL() string {
	params := url.Values{}
	params.Set("client_type", "extension")
	params.Set("callback_url", clineRedirectURI)
	params.Set("redirect_uri", clineRedirectURI)
	return clineAuthorizeURL + "?" + params.Encode()
}

// ExchangeClineCode decodes the pasted code. Cline encodes the token bundle as
// base64 JSON in the code; if that fails we POST to the token-exchange endpoint.
func ExchangeClineCode(ctx context.Context, code string) (*ClineTokens, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, fmt.Errorf("empty code")
	}
	if t := decodeClineBundle(code); t != nil {
		return t, nil
	}
	// Fallback: server-side exchange.
	payload := map[string]string{
		"grant_type":   "authorization_code",
		"code":         code,
		"client_type":  "extension",
		"redirect_uri": clineRedirectURI,
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", clineTokenExchURL, strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("cline token exchange request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("cline token exchange failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	return parseClineServerResponse(body)
}

// decodeClineBundle decodes a base64 JSON token bundle. Returns nil if the value
// isn't a decodable bundle (caller falls back to the server exchange).
func decodeClineBundle(code string) *ClineTokens {
	s := code
	if pad := len(s) % 4; pad != 0 {
		s += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(s)
		if err != nil {
			return nil
		}
	}
	decoded := string(raw)
	last := strings.LastIndex(decoded, "}")
	if last < 0 {
		return nil
	}
	var d struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		Email        string `json:"email"`
		ExpiresAt    string `json:"expiresAt"`
	}
	if err := json.Unmarshal([]byte(decoded[:last+1]), &d); err != nil {
		return nil
	}
	if d.AccessToken == "" {
		return nil
	}
	return &ClineTokens{
		AccessToken:  d.AccessToken,
		RefreshToken: d.RefreshToken,
		Email:        d.Email,
		ExpiresIn:    clineExpiresInFrom(d.ExpiresAt),
	}
}

func parseClineServerResponse(body []byte) (*ClineTokens, error) {
	var d struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    string `json:"expiresAt"`
		Data         struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    string `json:"expiresAt"`
			UserInfo     struct {
				Email string `json:"email"`
			} `json:"userInfo"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("cline token response parse failed: %w", err)
	}
	at := firstNonEmptyStr(d.Data.AccessToken, d.AccessToken)
	if at == "" {
		return nil, fmt.Errorf("cline token response missing accessToken")
	}
	return &ClineTokens{
		AccessToken:  at,
		RefreshToken: firstNonEmptyStr(d.Data.RefreshToken, d.RefreshToken),
		Email:        d.Data.UserInfo.Email,
		ExpiresIn:    clineExpiresInFrom(firstNonEmptyStr(d.Data.ExpiresAt, d.ExpiresAt)),
	}, nil
}

// RefreshClineToken renews tokens via the refresh endpoint.
func RefreshClineToken(ctx context.Context, refreshToken string) (*ClineTokens, error) {
	payload := map[string]string{
		"grantType":  "refresh_token",
		"clientType": "extension",
		"refreshToken": refreshToken,
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", clineRefreshURL, strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("cline token refresh request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("cline token refresh failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	t, err := parseClineServerResponse(body)
	if err != nil {
		return nil, err
	}
	if t.RefreshToken == "" {
		t.RefreshToken = refreshToken
	}
	return t, nil
}

// clineExpiresInFrom converts an ISO-8601 expiresAt to a seconds-from-now TTL.
// Returns a 1h default when absent/unparseable.
func clineExpiresInFrom(expiresAt string) int {
	expiresAt = strings.TrimSpace(expiresAt)
	if expiresAt == "" {
		return 3600
	}
	t, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return 3600
	}
	secs := int(time.Until(t).Seconds())
	if secs < 60 {
		return 3600
	}
	return secs
}
