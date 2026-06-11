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

// Kimi Coding (Moonshot) OAuth 2.0 Device Authorization flow, ported from 9router's
// src/lib/oauth/providers.js (the `kimi-coding` handler) and KIMI_CODING_CONFIG.
// After auth, Kimi Coding is an Anthropic-compatible /v1/messages endpoint reached
// with a Bearer access token, so the proxy reuses the generic Anthropic provider
// for inference and only needs this package for the device login + token refresh.
const (
	kimiCodingDeviceCodeURL = "https://auth.kimi.com/api/oauth/device_authorization"
	kimiCodingTokenURL      = "https://auth.kimi.com/api/oauth/token"
	kimiCodingClientID      = "17e5f671-d194-4dfb-9706-5516cb48c098"
	kimiCodingGrantDevice   = "urn:ietf:params:oauth:grant-type:device_code"
)

// KimiCodingDeviceAuth is a started device-authorization request.
type KimiCodingDeviceAuth struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               int
	Interval                int
}

// KimiCodingTokens is a completed device-token or refresh exchange.
type KimiCodingTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
}

// StartKimiCodingDeviceAuth posts a device-code request and returns the user code
// + verification URL for the operator to approve.
func StartKimiCodingDeviceAuth(ctx context.Context) (*KimiCodingDeviceAuth, error) {
	form := url.Values{}
	form.Set("client_id", kimiCodingClientID)

	req, err := http.NewRequestWithContext(ctx, "POST", kimiCodingDeviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("kimi-coding device-code request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("kimi-coding device-code failed: HTTP %d %s", resp.StatusCode, string(body))
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
		return nil, fmt.Errorf("kimi-coding device-code parse failed: %w", err)
	}
	if d.DeviceCode == "" {
		return nil, fmt.Errorf("kimi-coding device-code response missing device_code")
	}
	interval := d.Interval
	if interval < 1 {
		interval = 5
	}
	return &KimiCodingDeviceAuth{
		DeviceCode:              d.DeviceCode,
		UserCode:                d.UserCode,
		VerificationURI:         d.VerificationURI,
		VerificationURIComplete: d.VerificationURIComplete,
		ExpiresIn:               d.ExpiresIn,
		Interval:                interval,
	}, nil
}

// PollKimiCodingToken performs ONE token poll. Returns "pending"/"ok"/"" with err.
func PollKimiCodingToken(ctx context.Context, deviceCode string) (status string, tokens *KimiCodingTokens, err error) {
	form := url.Values{}
	form.Set("grant_type", kimiCodingGrantDevice)
	form.Set("client_id", kimiCodingClientID)
	form.Set("device_code", deviceCode)

	req, err := http.NewRequestWithContext(ctx, "POST", kimiCodingTokenURL, strings.NewReader(form.Encode()))
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
	if resp.StatusCode == 200 {
		t, perr := parseKimiCodingTokenResponse(body)
		if perr != nil {
			return "", nil, perr
		}
		return "ok", t, nil
	}
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	if (resp.StatusCode == 400 && e.Error == "authorization_pending") ||
		(resp.StatusCode == 429 && e.Error == "slow_down") {
		return "pending", nil, nil
	}
	return "", nil, fmt.Errorf("kimi-coding token poll failed: HTTP %d %s", resp.StatusCode, string(body))
}

// RefreshKimiCodingToken renews an access token via the refresh grant.
func RefreshKimiCodingToken(ctx context.Context, refreshToken string) (*KimiCodingTokens, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", kimiCodingClientID)

	req, err := http.NewRequestWithContext(ctx, "POST", kimiCodingTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("kimi-coding token refresh request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("kimi-coding token refresh failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	t, err := parseKimiCodingTokenResponse(body)
	if err != nil {
		return nil, err
	}
	if t.RefreshToken == "" {
		t.RefreshToken = refreshToken
	}
	return t, nil
}

func parseKimiCodingTokenResponse(body []byte) (*KimiCodingTokens, error) {
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("kimi-coding token response parse failed: %w", err)
	}
	if raw.AccessToken == "" {
		return nil, fmt.Errorf("kimi-coding token response missing access_token")
	}
	return &KimiCodingTokens{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		ExpiresIn:    raw.ExpiresIn,
	}, nil
}
