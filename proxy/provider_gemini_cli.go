package proxy

import (
	"bufio"
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

// geminiCLIProvider serves Gemini CLI accounts authenticated via the Google Cloud
// Code Assist OAuth flow (see auth/gemini_cli_oauth.go). Cloud Code Assist wraps the
// standard Gemini generateContent body in an envelope:
//
//	POST https://cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse
//	{ "model": "<model>", "project": "<projectId>", "request": { <generateContent body> } }
//
// and each SSE chunk wraps the GenerateContentResponse under a "response" key. This
// provider reuses buildGeminiBody for the inner body and reuses parseGeminiSSE by
// stripping the wrapper via an io.Pipe transform, so all the existing Gemini
// tool/text/usage handling applies unchanged. The GCP project id is stored in
// Account.ExtraHeaders["x-goog-project"] at login (a convenient per-account slot).
type geminiCLIProvider struct{}

func init() {
	RegisterProvider(geminiCLIProvider{})
}

const geminiCLIStreamURL = "https://cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse"

func (geminiCLIProvider) Name() string { return "gemini-cli" }

func (geminiCLIProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if acct.RefreshToken == "" {
		return TokenSet{AccessToken: acct.AccessToken, ExpiresAt: acct.ExpiresAt}, nil
	}
	t, err := auth.RefreshGoogleToken(ctx, acct.RefreshToken)
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

// ListModels returns the static Gemini model catalog (Cloud Code Assist has no
// public /models listing). Source: gemini-cli supported models.
func (geminiCLIProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	ids := []string{"gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.0-flash", "gemini-2.0-flash-thinking-exp"}
	out := make([]ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, ModelInfo{ModelId: id})
	}
	return out, nil
}

func (geminiCLIProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
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
	envelope := map[string]interface{}{
		"model":   model,
		"request": innerObj,
	}
	if proj := strings.TrimSpace(acct.ExtraHeaders["x-goog-project"]); proj != "" {
		envelope["project"] = proj
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", geminiCLIStreamURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(acct.AccessToken))
	req.Header.Set("User-Agent", "google-api-nodejs-client/9.15.1")

	resp, err := GetClientForProxy(ResolveAccountProxyURL(acct)).Do(req)
	if err != nil {
		return classifyStreamError(err)
	}
	if resp.StatusCode == 429 {
		retryAfter := parseRetryAfter(resp.Header)
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return &QuotaError{Endpoints: []string{"gemini-cli"}, RetryAfter: retryAfter}
	}
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return fmt.Errorf("HTTP %d from gemini-cli: %s", resp.StatusCode, string(errBody))
	}

	streamErr := func() error {
		defer resp.Body.Close()
		r := newIdleTimeoutReader(resp.Body, streamIdleTimeout, func() {})
		return parseGeminiCLISSE(r, cb)
	}()
	return classifyStreamError(streamErr)
}

// parseGeminiCLISSE strips the Cloud Code Assist "response" wrapper from each SSE
// data line and feeds the inner GenerateContentResponse to parseGeminiSSE via an
// io.Pipe, so all existing Gemini stream handling is reused verbatim.
func parseGeminiCLISSE(r io.Reader, cb *KiroStreamCallback) error {
	pr, pw := io.Pipe()
	go func() {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		var werr error
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			var wrap struct {
				Response json.RawMessage `json:"response"`
			}
			inner := data
			if err := json.Unmarshal([]byte(data), &wrap); err == nil && len(wrap.Response) > 0 {
				inner = string(wrap.Response)
			}
			if _, werr = pw.Write([]byte("data: " + inner + "\n\n")); werr != nil {
				break
			}
		}
		if werr == nil {
			werr = scanner.Err()
		}
		pw.CloseWithError(werr)
	}()
	return parseGeminiSSE(pr, cb)
}
