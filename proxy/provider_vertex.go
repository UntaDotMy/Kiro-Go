package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"net/http"
	"strings"
)

// vertexProvider serves Google Vertex AI accounts authenticated by a Service
// Account JSON key (see auth/vertex_auth.go). The SA JSON is stored in
// Account.APIKey; the region in Account.Region (default us-central1); the project
// id is read from the SA JSON. Inference uses the Gemini generateContent body
// (reused via buildGeminiBody) posted to the regional Vertex endpoint:
//
//	POST https://{region}-aiplatform.googleapis.com/v1/projects/{project}/locations/{region}/publishers/google/models/{model}:streamGenerateContent?alt=sse
//
// authenticated with a Bearer access token minted from the SA. The response is the
// standard Gemini SSE stream, so parseGeminiSSE is reused verbatim.
type vertexProvider struct{}

func init() {
	RegisterProvider(vertexProvider{})
}

func (vertexProvider) Name() string { return "vertex" }

func vertexRegion(acct *config.Account) string {
	r := strings.TrimSpace(acct.Region)
	if r == "" {
		return "us-central1"
	}
	return r
}

// RefreshToken mints/refreshes the SA-derived access token and stores it in
// AccessToken with the real expiry, so the pool's exp-based gate renews on schedule.
// The SA JSON in APIKey is the durable credential.
func (vertexProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	saJSON := strings.TrimSpace(acct.APIKey)
	if saJSON == "" {
		// No SA JSON — degrade gracefully (an access token may have been set manually).
		return TokenSet{AccessToken: acct.AccessToken, ExpiresAt: acct.ExpiresAt}, nil
	}
	tok, expiresAt, err := auth.VertexAccessToken(ctx, saJSON)
	if err != nil {
		return TokenSet{}, err
	}
	return TokenSet{AccessToken: tok, ExpiresAt: expiresAt.Unix()}, nil
}

func (vertexProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	ids := []string{"gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.0-flash", "gemini-1.5-pro", "gemini-1.5-flash"}
	out := make([]ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, ModelInfo{ModelId: id})
	}
	return out, nil
}

func (vertexProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	if ctx == nil {
		ctx = context.Background()
	}
	model := strings.TrimSpace(nr.Model)

	// Resolve the project id from the SA JSON.
	sa, err := auth.ParseVertexServiceAccount(strings.TrimSpace(acct.APIKey))
	if err != nil {
		return fmt.Errorf("vertex: %w", err)
	}
	region := vertexRegion(acct)

	// Mint/reuse the access token (cached in auth).
	token, _, err := auth.VertexAccessToken(ctx, acct.APIKey)
	if err != nil {
		return err
	}

	body, err := buildGeminiBody(nr, model)
	if err != nil {
		return err
	}

	url := fmt.Sprintf(
		"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:streamGenerateContent?alt=sse",
		region, sa.ProjectID, region, model,
	)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := GetClientForProxy(ResolveAccountProxyURL(acct)).Do(req)
	if err != nil {
		return classifyStreamError(err)
	}
	if resp.StatusCode == 429 {
		retryAfter := parseRetryAfter(resp.Header)
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return &QuotaError{Endpoints: []string{"vertex"}, RetryAfter: retryAfter}
	}
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return fmt.Errorf("HTTP %d from vertex: %s", resp.StatusCode, string(errBody))
	}

	streamErr := func() error {
		defer resp.Body.Close()
		r := newIdleTimeoutReader(resp.Body, streamIdleTimeout, func() {})
		return parseGeminiSSE(r, cb)
	}()
	return classifyStreamError(streamErr)
}
