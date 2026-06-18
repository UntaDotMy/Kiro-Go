package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
)

// codeBuddyCNProvider serves codebuddy-cn accounts authenticated via ck_ API keys
// fetched from reseller servers (see auth/codebuddy_cn_seller.go). After the key
// is stored in Account.APIKey, inference is an OpenAI-compatible endpoint at
// https://copilot.tencent.com/v2/chat/completions with a Bearer token and
// minimal headers (the reseller strips IDE framing headers to avoid 429
// queue-limiting — this is deliberate, not an omission).
//
// Unlike the OAuth-based codebuddy/codebuddy-ai providers, this is an api-key
// provider with no token refresh. It owns its own inference path because the CN
// endpoint requires stream_options.include_usage and X-Request-ID that the
// generic provider does not add.
type codeBuddyCNProvider struct{}

var codeBuddyCNInference = &genericProvider{dialect: DialectOpenAI}

func init() {
	RegisterProvider(codeBuddyCNProvider{})
}

func (p codeBuddyCNProvider) Name() string { return "codebuddy-cn" } // RefreshToken is a no-op for the api-key provider: the ck_ key lives in
// Account.APIKey and never expires.
func (p codeBuddyCNProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	return TokenSet{AccessToken: acct.AccessToken, ExpiresAt: 0}, nil
}

// ListModels delegates to the shared generic OpenAI provider. Tencent's CN
// gateway has no working /models endpoint, so the catalog's advisory list is the
// fallback.
func (p codeBuddyCNProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	return codeBuddyCNInference.ListModels(acct)
}

// Call implements the CN-specific inference path: forced stream, stream_options,
// X-Request-ID, and minimal headers (no X-Domain/X-IDE-*/X-Product — the
// reseller strips them to avoid 429 queue-limiting).
func (p codeBuddyCNProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	if ctx == nil {
		ctx = context.Background()
	}

	upstreamModel := strings.TrimSpace(nr.Model)
	body, err := buildOpenAIChatBody(nr, upstreamModel, true)
	if err != nil {
		return err
	}

	// Force stream + include_usage — the CN gateway requires both.
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return err
	}
	m["stream"] = true
	m["stream_options"] = map[string]interface{}{"include_usage": true}
	body, err = json.Marshal(m)
	if err != nil {
		return err
	}

	// Neutralize Claude Code harness identity tokens (same tencentReplacer as
	// codebuddy/codebuddy-ai) so the content-moderating gateway doesn't reject
	// competitor brand tokens.
	if config.GetFilterClaudeCode() {
		body = neutralizeProviderBody(body, "codebuddy-cn")
	}

	// Diagnostic capture: when CODEBUDDY_CN_DUMP is set to a directory, write the
	// FINAL outbound body (post-neutralization) there so the exact payload reaching
	// the moderating gateway can be inspected. Off unless the env var is set.
	dumpOutboundBody("codebuddy-cn", body)

	url := "https://copilot.tencent.com/v2/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}

	// Minimal headers — match the reseller's anti-429 design.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	key := strings.TrimSpace(acct.APIKey)
	if key == "" {
		key = strings.TrimSpace(acct.AccessToken)
	}
	req.Header.Set("Authorization", "Bearer "+key)

	// Random request id (16 bytes hex) — standard for the CN gateway.
	rid := make([]byte, 16)
	if _, err := rand.Read(rid); err == nil {
		req.Header.Set("X-Request-ID", hex.EncodeToString(rid))
	}

	resp, err := GetClientForProxy(ResolveAccountProxyURL(acct)).Do(req)
	if err != nil {
		return classifyStreamError(err)
	}

	if resp.StatusCode == 429 {
		retryAfter := parseRetryAfter(resp.Header)
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		logger.Infof("[codebuddy-cn] throttled (429, retry-after=%s)", retryAfter)
		return &QuotaError{Endpoints: []string{"codebuddy-cn"}, RetryAfter: retryAfter}
	}
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		dumpUpstreamResponse("codebuddy-cn", errBody)
		return fmt.Errorf("HTTP %d from codebuddy-cn: %s", resp.StatusCode, string(errBody))
	}

	streamErr := func() error {
		defer resp.Body.Close()
		body := newIdleTimeoutReader(resp.Body, streamIdleTimeout, func() {})
		var reader io.Reader = body
		// When debug capture is on, tee the raw upstream SSE into a buffer and
		// write it after the stream ends, so the exact tool_call shape the gateway
		// emits can be inspected (the most likely cause of the cross-client
		// tool-use failure). Bounded so a long stream can't exhaust memory.
		var captured *bytes.Buffer
		if debugCaptureDir() != "" {
			captured = &bytes.Buffer{}
			reader = io.TeeReader(body, &limitedWriter{w: captured, n: 2 << 20})
		}
		err := parseOpenAISSE(reader, cb)
		if captured != nil {
			dumpUpstreamResponse("codebuddy-cn", captured.Bytes())
		}
		return err
	}()
	return classifyStreamError(streamErr)
}

// limitedWriter caps how many bytes are buffered for a debug capture so a long
// stream cannot exhaust memory; writes past the cap are silently dropped.
type limitedWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		take := len(p)
		if take > l.n {
			take = l.n
		}
		l.w.Write(p[:take])
		l.n -= take
	}
	return len(p), nil
}
