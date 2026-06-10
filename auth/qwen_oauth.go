package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// Qwen (Alibaba) OAuth 2.0 Device Authorization flow, ported byte-for-byte from
// the open-source qwen-code CLI (github.com/QwenLM/qwen-code,
// packages/core/src/qwen/qwenOAuth2.ts). Qwen is NOT a plain api-key provider:
// it issues short-lived OAuth access tokens via a device flow and returns a
// per-account `resource_url` telling the client which OpenAI-compatible host to
// call. After auth, inference is ordinary OpenAI /chat/completions with a Bearer
// access token — so the proxy reuses the generic OpenAI provider for Call/
// ListModels and only needs this package for the device login + token refresh.
//
// The flow:
//  1. StartQwenDeviceAuth posts a device-code request (PKCE S256) and returns the
//     user_code + verification URL for the operator to approve in a browser.
//  2. PollQwenToken polls the token endpoint; while pending the endpoint returns
//     400 authorization_pending (or 429 slow_down); on approval it returns the
//     access/refresh tokens + resource_url.
//  3. RefreshQwenToken renews the access token via grant_type=refresh_token and
//     may return an updated resource_url.
const (
	qwenOAuthBase          = "https://chat.qwen.ai"
	qwenDeviceCodeEndpoint = qwenOAuthBase + "/api/v1/oauth2/device/code"
	qwenTokenEndpoint      = qwenOAuthBase + "/api/v1/oauth2/token"
	qwenClientID           = "f0304373b74a44d2b584a3fb70ca9e56"
	qwenScope              = "openid profile email model.completion"
	qwenGrantTypeDevice    = "urn:ietf:params:oauth:grant-type:device_code"

	// qwenDefaultBaseURL is the fallback OpenAI-compatible base used when a token
	// response omits resource_url. Matches qwen-code's DEFAULT_DASHSCOPE_BASE_URL.
	qwenDefaultBaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
)

// QwenDeviceAuth is a started device-authorization request awaiting user approval.
type QwenDeviceAuth struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               int    // seconds until the device code expires
	Interval                int    // poll interval seconds (>=1)
	CodeVerifier            string // PKCE verifier kept for the token poll
}

// QwenTokens is a completed device-token or refresh exchange.
type QwenTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int    // seconds; qwen access tokens are short-lived (~1h)
	ResourceURL  string // host (maybe scheme/-/v1) selecting the OpenAI-compat base
}

// qwenCodeVerifier returns a base64url(32-byte) PKCE verifier (matches
// qwen-code's crypto.randomBytes(32).toString('base64url')).
func qwenCodeVerifier() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// qwenCodeChallenge returns base64url(SHA256(verifier)) — PKCE S256.
func qwenCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// StartQwenDeviceAuth posts a device-code request with PKCE and returns the
// device/user codes + verification URL for the operator to approve.
func StartQwenDeviceAuth(ctx context.Context) (*QwenDeviceAuth, error) {
	verifier := qwenCodeVerifier()
	challenge := qwenCodeChallenge(verifier)

	form := url.Values{}
	form.Set("client_id", qwenClientID)
	form.Set("scope", qwenScope)
	form.Set("code_challenge", challenge)
	form.Set("code_challenge_method", "S256")

	req, err := http.NewRequestWithContext(ctx, "POST", qwenDeviceCodeEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// qwen-code sends an x-request-id on the device-code request specifically.
	req.Header.Set("x-request-id", uuid.New().String())

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("qwen device-code request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("qwen device-code failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	var d struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("qwen device-code parse failed: %w", err)
	}
	if d.DeviceCode == "" {
		return nil, fmt.Errorf("qwen device-code response missing device_code")
	}
	interval := d.Interval
	if interval < 1 {
		interval = 2 // qwen-code defaults to a 2s poll when none is supplied
	}
	return &QwenDeviceAuth{
		DeviceCode:              d.DeviceCode,
		UserCode:                d.UserCode,
		VerificationURI:         d.VerificationURI,
		VerificationURIComplete: d.VerificationURIComplete,
		ExpiresIn:               d.ExpiresIn,
		Interval:                interval,
		CodeVerifier:            verifier,
	}, nil
}

// PollQwenToken performs ONE token poll. Returns status "pending" (keep polling),
// "ok" (tokens captured), or "" with err on terminal failure. A transport error
// is reported as "pending" so the caller simply retries.
func PollQwenToken(ctx context.Context, deviceCode, codeVerifier string) (status string, tokens *QwenTokens, err error) {
	form := url.Values{}
	form.Set("grant_type", qwenGrantTypeDevice)
	form.Set("client_id", qwenClientID)
	form.Set("device_code", deviceCode)
	form.Set("code_verifier", codeVerifier)

	req, err := http.NewRequestWithContext(ctx, "POST", qwenTokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return "pending", nil, nil // transient — caller retries
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == 200 {
		t, perr := parseQwenTokenResponse(body)
		if perr != nil {
			return "", nil, perr
		}
		return "ok", t, nil
	}
	// Pending / slow-down signals: 400 authorization_pending, 429 slow_down.
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	if (resp.StatusCode == 400 && e.Error == "authorization_pending") ||
		(resp.StatusCode == 429 && e.Error == "slow_down") {
		return "pending", nil, nil
	}
	return "", nil, fmt.Errorf("qwen token poll failed: HTTP %d %s", resp.StatusCode, string(body))
}

// RefreshQwenToken renews a qwen access token via the refresh grant. If the
// response omits refresh_token, the supplied one is preserved.
func RefreshQwenToken(ctx context.Context, refreshToken string) (*QwenTokens, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", qwenClientID)

	req, err := http.NewRequestWithContext(ctx, "POST", qwenTokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("qwen token refresh request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("qwen token refresh failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	t, err := parseQwenTokenResponse(body)
	if err != nil {
		return nil, err
	}
	if t.RefreshToken == "" {
		t.RefreshToken = refreshToken
	}
	return t, nil
}

// parseQwenTokenResponse decodes a qwen token response. The base-URL field is
// `resource_url` (qwen-code also tolerates `endpoint`; we read both).
func parseQwenTokenResponse(body []byte) (*QwenTokens, error) {
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		ResourceURL  string `json:"resource_url"`
		Endpoint     string `json:"endpoint"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("qwen token response parse failed: %w", err)
	}
	if raw.AccessToken == "" {
		return nil, fmt.Errorf("qwen token response missing access_token")
	}
	res := raw.ResourceURL
	if res == "" {
		res = raw.Endpoint
	}
	return &QwenTokens{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		ExpiresIn:    raw.ExpiresIn,
		ResourceURL:  res,
	}, nil
}

// QwenBaseURLFromResource normalizes a token-response resource_url into the
// OpenAI-compatible API BASE (ending in /v1, NOT /chat/completions). Empty ->
// the DashScope default. Mirrors qwen-code's getCurrentEndpoint(): prepend
// https:// when no scheme, append /v1 when absent. The generic provider derives
// /chat/completions and /models from this base.
func QwenBaseURLFromResource(resourceURL string) string {
	base := strings.TrimSpace(resourceURL)
	if base == "" {
		return qwenDefaultBaseURL
	}
	if !strings.HasPrefix(strings.ToLower(base), "http") {
		base = "https://" + base
	}
	base = strings.TrimRight(base, "/")
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	return base
}
