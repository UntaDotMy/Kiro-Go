package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
)

// CodeBuddy (Tencent) browser-OAuth polling flow, ported from 9router's
// src/lib/oauth/providers.js (the `codebuddy` handler) and CODEBUDDY_CONFIG.
// CodeBuddy is NOT a plain api-key provider: it issues an OAuth access token via
// a browser-approval poll. After auth, inference is ordinary OpenAI-compatible
// /v1/chat/completions with a Bearer access token — so the proxy reuses the
// generic OpenAI provider for Call/ListModels and only needs this package for the
// login + token refresh.
//
// CodeBuddy ships under two interchangeable "official endpoints" (per the CLI's
// product.json): the China gateway copilot.tencent.com and the international site
// www.codebuddy.ai. Both speak the IDENTICAL /v2/plugin/auth/* scheme and only
// differ by host, so every function here takes a host base and the two providers
// (codebuddy = CN, codebuddy-ai = international) share this one implementation.
//
// The flow:
//  1. StartCodeBuddyAuth POSTs to the state endpoint and returns { state, authUrl }.
//     The operator opens authUrl in a browser and approves.
//  2. PollCodeBuddyToken POSTs the state to the token endpoint; while pending the
//     endpoint returns code 11217, on approval code 0 with the access/refresh tokens.
//  3. RefreshCodeBuddyToken renews the access token via the refresh endpoint.
const (
	// CodeBuddyHostCN is the China gateway; CodeBuddyHostIntl is the international
	// site. These are two of the four officialEndpoints in the CLI's product.json.
	CodeBuddyHostCN   = "https://copilot.tencent.com"
	CodeBuddyHostIntl = "https://www.codebuddy.ai"

	codeBuddyStatePath   = "/v2/plugin/auth/state"
	codeBuddyTokenPath   = "/v2/plugin/auth/token"
	codeBuddyRefreshPath = "/v2/plugin/auth/token/refresh"
	codeBuddyUserAgent   = "CLI/2.63.2 CodeBuddy/2.63.2"
	codeBuddyPlatform    = "CLI"

	// codeBuddyTokenTTL is the access-token lifetime 9router assumes (mapTokens
	// hardcodes expiresIn: 86400). CodeBuddy's token response omits expires_in, so
	// we apply this default so the refresh scheduler renews proactively.
	codeBuddyTokenTTL = 86400
)

// codeBuddyHostBase normalizes a host base, defaulting to the China gateway when
// empty so existing callers/accounts that predate the host parameter keep working.
func codeBuddyHostBase(host string) string {
	host = strings.TrimRight(strings.TrimSpace(host), "/")
	if host == "" {
		return CodeBuddyHostCN
	}
	return host
}

// codeBuddyDomain returns the bare host (no scheme) for the X-Domain header.
func codeBuddyDomain(host string) string {
	base := codeBuddyHostBase(host)
	base = strings.TrimPrefix(base, "https://")
	base = strings.TrimPrefix(base, "http://")
	return base
}

// CodeBuddyAuth is a started login awaiting browser approval. State doubles as the
// device code for the poll.
type CodeBuddyAuth struct {
	State   string
	AuthURL string
}

// CodeBuddyTokens is a completed token or refresh exchange.
type CodeBuddyTokens struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresIn    int
}

// codeBuddyHeaders returns the Tencent-specific headers 9router sends on the
// state/token requests. X-No-Authorization/X-No-User-Id tell the gateway this is
// the pre-auth handshake. X-Domain tracks the chosen host (CN or international).
func codeBuddyHeaders(host string) map[string]string {
	return map[string]string{
		"Content-Type":       "application/json",
		"Accept":             "application/json",
		"User-Agent":         codeBuddyUserAgent,
		"X-Requested-With":   "XMLHttpRequest",
		"X-Domain":           codeBuddyDomain(host),
		"X-No-Authorization": "true",
		"X-No-User-Id":       "true",
		"X-Product":          "SaaS",
	}
}

func codeBuddyPost(ctx context.Context, host, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range codeBuddyHeaders(host) {
		req.Header.Set(k, v)
	}
	return httpClient().Do(req)
}

func codeBuddyGet(ctx context.Context, host, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range codeBuddyHeaders(host) {
		req.Header.Set(k, v)
	}
	return httpClient().Do(req)
}

// StartCodeBuddyAuth requests a login state + browser auth URL on the given host
// (CodeBuddyHostCN or CodeBuddyHostIntl; empty defaults to CN).
func StartCodeBuddyAuth(ctx context.Context, host string) (*CodeBuddyAuth, error) {
	base := codeBuddyHostBase(host)
	url := base + codeBuddyStatePath + "?platform=" + codeBuddyPlatform
	resp, err := codeBuddyPost(ctx, host, url, []byte("{}"))
	if err != nil {
		return nil, fmt.Errorf("codebuddy state request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("codebuddy state failed: HTTP %d %s", resp.StatusCode, string(raw))
	}
	var d struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("codebuddy state parse failed: %w", err)
	}
	if d.Code != 0 || d.Data.State == "" || d.Data.AuthURL == "" {
		msg := d.Msg
		if msg == "" {
			msg = "missing state/authUrl"
		}
		return nil, fmt.Errorf("codebuddy state error: %s", msg)
	}
	return &CodeBuddyAuth{State: d.Data.State, AuthURL: d.Data.AuthURL}, nil
}

// PollCodeBuddyToken performs ONE token poll against the given host. Returns
// "pending" (keep polling), "ok" (tokens captured), or "" with err on terminal
// failure. Code 11217 = pending, code 0 = success. A transport error is reported
// as "pending" so the caller retries.
//
// The official CodeBuddy CLI polls with GET /v2/plugin/auth/token?state=<state>
// (not a POST body). The China gateway tolerates POST, but the international host
// www.codebuddy.ai only returns the tokens for the GET form — so we match the CLI
// exactly and use GET for both hosts.
func PollCodeBuddyToken(ctx context.Context, host, state string) (status string, tokens *CodeBuddyTokens, err error) {
	base := codeBuddyHostBase(host)
	url := base + codeBuddyTokenPath + "?state=" + neturl.QueryEscape(state)
	resp, err := codeBuddyGet(ctx, host, url)
	if err != nil {
		return "pending", nil, nil // transient — caller retries
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "pending", nil, nil // gateway hiccup — keep polling
	}
	var d struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			TokenType    string `json:"tokenType"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return "", nil, fmt.Errorf("codebuddy token parse failed: %w", err)
	}
	switch d.Code {
	case 0:
		if d.Data.AccessToken == "" {
			return "", nil, fmt.Errorf("codebuddy token response missing accessToken")
		}
		tt := d.Data.TokenType
		if tt == "" {
			tt = "Bearer"
		}
		return "ok", &CodeBuddyTokens{
			AccessToken:  d.Data.AccessToken,
			RefreshToken: d.Data.RefreshToken,
			TokenType:    tt,
			ExpiresIn:    codeBuddyTokenTTL,
		}, nil
	case 11217:
		return "pending", nil, nil
	default:
		msg := d.Msg
		if msg == "" {
			msg = "unknown_error"
		}
		return "", nil, fmt.Errorf("codebuddy token error: %s", msg)
	}
}

// RefreshCodeBuddyToken renews an access token via the refresh endpoint on the
// given host. If the response omits a new refresh token, the supplied one is
// preserved by the caller.
//
// Matching the official CLI: the refresh is a POST with an EMPTY body and the
// refresh token carried in the X-Refresh-Token header (not a JSON body). The CN
// gateway is lenient, but the international host expects the header form.
func RefreshCodeBuddyToken(ctx context.Context, host, refreshToken string) (*CodeBuddyTokens, error) {
	base := codeBuddyHostBase(host)
	req, err := http.NewRequestWithContext(ctx, "POST", base+codeBuddyRefreshPath, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("codebuddy token refresh request failed: %w", err)
	}
	for k, v := range codeBuddyHeaders(host) {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Refresh-Token", refreshToken)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("codebuddy token refresh request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("codebuddy token refresh failed: HTTP %d %s", resp.StatusCode, string(raw))
	}
	var d struct {
		Code int `json:"code"`
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			TokenType    string `json:"tokenType"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("codebuddy token refresh parse failed: %w", err)
	}
	if d.Code != 0 || d.Data.AccessToken == "" {
		return nil, fmt.Errorf("codebuddy token refresh returned no access token (code %d)", d.Code)
	}
	tt := d.Data.TokenType
	if tt == "" {
		tt = "Bearer"
	}
	return &CodeBuddyTokens{
		AccessToken:  d.Data.AccessToken,
		RefreshToken: d.Data.RefreshToken,
		TokenType:    tt,
		ExpiresIn:    codeBuddyTokenTTL,
	}, nil
}
