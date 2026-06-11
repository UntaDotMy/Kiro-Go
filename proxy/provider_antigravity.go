package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"net/http"
	"strings"
	"time"
)

// antigravityProvider serves Antigravity accounts authenticated via the Google
// Cloud Code Assist OAuth flow (see auth/antigravity_oauth.go). It reuses the same
// Cloud Code Assist envelope + SSE unwrapping as gemini-cli (buildGeminiBody +
// parseGeminiCLISSE) but targets the daily-cloudcode endpoint and refreshes with
// antigravity's own OAuth client.
type antigravityProvider struct{}

func init() {
	RegisterProvider(antigravityProvider{})
}

const antigravityStreamURL = "https://daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse"

func (antigravityProvider) Name() string { return "antigravity" }

func (antigravityProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if acct.RefreshToken == "" {
		return TokenSet{AccessToken: acct.AccessToken, ExpiresAt: acct.ExpiresAt}, nil
	}
	t, err := auth.RefreshAntigravityToken(ctx, acct.RefreshToken)
	if err != nil {
		return TokenSet{}, err
	}
	ts := TokenSet{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken}
	if ts.RefreshToken == "" {
		ts.RefreshToken = acct.RefreshToken
	}
	if t.ExpiresIn > 0 {
		ts.ExpiresAt = time.Now().Unix() + int64(t.ExpiresIn)
	}
	return ts, nil
}

func (antigravityProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	ids := []string{"gemini-2.5-pro", "gemini-2.5-flash", "claude-sonnet-4-5", "gpt-5"}
	out := make([]ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, ModelInfo{ModelId: id})
	}
	return out, nil
}

func (antigravityProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	if ctx == nil {
		ctx = context.Background()
	}
	model := strings.TrimSpace(nr.Model)
	inner, err := buildGeminiBody(nr, model)
	if err != nil {
		return err
	}
	var innerObj map[string]interface{}
	if err := json.Unmarshal(inner, &innerObj); err != nil {
		return err
	}
	envelope := map[string]interface{}{"model": model, "request": innerObj}
	if proj := strings.TrimSpace(acct.ExtraHeaders["x-goog-project"]); proj != "" {
		envelope["project"] = proj
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", antigravityStreamURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(acct.AccessToken))
	req.Header.Set("User-Agent", "antigravity/1.107.0")
	req.Header.Set("x-request-source", "local")

	resp, err := GetClientForProxy(ResolveAccountProxyURL(acct)).Do(req)
	if err != nil {
		return classifyStreamError(err)
	}
	if resp.StatusCode == 429 {
		retryAfter := parseRetryAfter(resp.Header)
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return &QuotaError{Endpoints: []string{"antigravity"}, RetryAfter: retryAfter}
	}
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return fmt.Errorf("HTTP %d from antigravity: %s", resp.StatusCode, string(errBody))
	}

	streamErr := func() error {
		defer resp.Body.Close()
		r := newIdleTimeoutReader(resp.Body, streamIdleTimeout, func() {})
		return parseGeminiCLISSE(r, cb)
	}()
	return classifyStreamError(streamErr)
}
