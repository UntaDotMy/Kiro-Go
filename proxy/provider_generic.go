package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
)

// genericProvider serves any data-defined backend (built-in catalog entry or a
// user config.ProviderConfig) for the OpenAI / Anthropic / Gemini dialects. One
// instance per dialect is registered as "generic:<dialect>"; ProviderForBackend
// routes a backend id to the right instance by resolving its dialect.
//
// It owns no per-provider code: the URL, headers, auth scheme, and request/
// response translation are all driven by the resolved providerSettings.
type genericProvider struct {
	dialect Dialect
}

func init() {
	RegisterProvider(&genericProvider{dialect: DialectOpenAI})
	RegisterProvider(&genericProvider{dialect: DialectAnthropic})
	RegisterProvider(&genericProvider{dialect: DialectGemini})
}

func (g *genericProvider) Name() string { return "generic:" + string(g.dialect) }

// providerSettings is the resolved, ready-to-use config for one backend id,
// merged from the built-in catalog and/or a user config.ProviderConfig plus any
// per-account override.
type providerSettings struct {
	id         string
	dialect    Dialect
	baseURL    string
	authHeader string
	headers    map[string]string
	models     []string
}

// resolveProviderSettings builds the effective settings for an account's
// backend, layering: built-in catalog -> user ProviderConfig -> per-account
// BaseURLOverride. Returns false if the backend is unknown.
func resolveProviderSettings(acct *config.Account) (providerSettings, bool) {
	backend := config.GetAccountBackend(acct)
	if acct != nil && strings.TrimSpace(acct.ProviderRef) != "" {
		backend = strings.ToLower(strings.TrimSpace(acct.ProviderRef))
	}

	var ps providerSettings
	ps.id = backend

	if bp, ok := resolveBuiltinProvider(backend); ok {
		ps.dialect = bp.Dialect
		ps.baseURL = bp.BaseURL
		ps.authHeader = bp.AuthHeader
		ps.headers = bp.Headers
	} else if pc, ok := config.GetProviderConfig(backend); ok {
		ps.dialect = Dialect(strings.ToLower(strings.TrimSpace(pc.Dialect)))
		ps.baseURL = pc.BaseURL
		ps.authHeader = pc.AuthHeader
		ps.headers = pc.Headers
		ps.models = pc.Models
	} else {
		return providerSettings{}, false
	}

	if ps.authHeader == "" {
		ps.authHeader = defaultAuthHeaderForDialect(ps.dialect)
	}
	if acct != nil && strings.TrimSpace(acct.BaseURLOverride) != "" {
		ps.baseURL = strings.TrimSpace(acct.BaseURLOverride)
	}
	return ps, true
}

// apiBase returns the provider's API base (no trailing inference path), derived
// from the configured baseURL. This accepts BOTH a full inference URL (the
// builtin catalog stores e.g. https://api.groq.com/openai/v1/chat/completions)
// AND a bare base (a custom provider where the operator pasted
// https://api.example.com/v1). Both normalize to the same base so we can derive
// the chat-completions and models endpoints from one source. For Gemini the
// "base" already IS the models-listing root (.../v1beta/models), so it's
// returned unchanged.
func (ps providerSettings) apiBase() string {
	u := strings.TrimRight(strings.TrimSpace(ps.baseURL), "/")
	switch ps.dialect {
	case DialectAnthropic:
		return strings.TrimSuffix(u, "/messages")
	case DialectGemini:
		return u
	default: // openai-compatible
		for _, suffix := range []string{"/chat/completions", "/responses", "/completions"} {
			if strings.HasSuffix(u, suffix) {
				return strings.TrimSuffix(u, suffix)
			}
		}
		return u
	}
}

// chatURL returns the POST inference endpoint. For OpenAI/Anthropic it's derived
// from the API base; Gemini encodes the model in the path so it's handled in
// buildRequest, not here.
func (ps providerSettings) chatURL() string {
	base := ps.apiBase()
	switch ps.dialect {
	case DialectAnthropic:
		return base + "/messages"
	default:
		return base + "/chat/completions"
	}
}

// modelsURL returns the GET model-listing endpoint. OpenAI/Anthropic expose
// {base}/models; Gemini's base IS the models endpoint.
func (ps providerSettings) modelsURL() string {
	base := ps.apiBase()
	if ps.dialect == DialectGemini {
		return base
	}
	return base + "/models"
}

// RefreshToken is a no-op for api-key providers: there is nothing to renew, so we
// report the static key as a never-expiring credential. (OAuth-based generic
// providers are not currently supported; those get bespoke implementations.)
func (g *genericProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	return TokenSet{AccessToken: acct.AccessToken, ExpiresAt: 0}, nil
}

// ListModels fetches the provider's live model catalog from {base}/models
// (OpenAI/Anthropic) or the Gemini models endpoint, using the account's key.
// This is what makes "add your own OpenAI-compatible endpoint and it gets the
// models" work. Falls back to the configured static list (then empty) if the
// fetch fails or the provider has no models endpoint — an empty per-account
// list makes the pool treat the account as "serves anything", so routing still
// works and the upstream validates the model id at call time.
func (g *genericProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	ps, ok := resolveProviderSettings(acct)
	if !ok {
		return nil, fmt.Errorf("unknown provider for account %s", acct.ID)
	}

	if live, err := g.fetchModels(context.Background(), ps, acct); err == nil && len(live) > 0 {
		return live, nil
	} else if err != nil {
		logger.Warnf("[%s] live model fetch failed for %s (%s); using static/empty catalog: %v", ps.id, acct.ID, ps.modelsURL(), err)
	}

	// Fallback: configured static list (custom providers can pin one).
	out := make([]ModelInfo, 0, len(ps.models))
	for _, id := range ps.models {
		out = append(out, ModelInfo{ModelId: id})
	}
	return out, nil
}

// FetchModelsForAccount fetches the live model list for a provider account,
// returning just the ids. Exposed for the admin add/validate flow so the
// dashboard can show what models an endpoint offers immediately on add.
func (g *genericProvider) FetchModelsForAccount(ctx context.Context, acct *config.Account) ([]string, error) {
	ps, ok := resolveProviderSettings(acct)
	if !ok {
		return nil, fmt.Errorf("unknown provider for account %s", acct.ID)
	}
	models, err := g.fetchModels(ctx, ps, acct)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ModelId)
	}
	return ids, nil
}

// fetchModels does the GET {modelsURL} call and parses the response. It accepts
// the common OpenAI/Anthropic/Gemini shapes: {"data":[{"id":...}]},
// {"models":[{"id"|"name":...}]}, {"results":[...]}, or a bare array. Returns an
// error on transport/non-200/parse failure so the caller can decide whether to
// fall back.
func (g *genericProvider) fetchModels(ctx context.Context, ps providerSettings, acct *config.Account) ([]ModelInfo, error) {
	url := ps.modelsURL()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range ps.headers {
		req.Header.Set(k, v)
	}
	g.applyAuthHeader(req, ps, acct)

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(acct)).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return parseModelsListResponse(body), nil
}

// parseModelsListResponse extracts model ids from the common provider shapes.
// Ported from 9router's parseOpenAIStyleModels / parseGeminiCliModels.
func parseModelsListResponse(body []byte) []ModelInfo {
	// Try the object-with-array shapes first, then a bare array.
	var obj struct {
		Data    []map[string]interface{} `json:"data"`
		Models  []map[string]interface{} `json:"models"`
		Results []map[string]interface{} `json:"results"`
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(body, &obj); err == nil {
		switch {
		case len(obj.Data) > 0:
			arr = obj.Data
		case len(obj.Models) > 0:
			arr = obj.Models
		case len(obj.Results) > 0:
			arr = obj.Results
		}
	}
	if arr == nil {
		_ = json.Unmarshal(body, &arr) // bare array fallback
	}

	seen := map[string]bool{}
	out := make([]ModelInfo, 0, len(arr))
	for _, m := range arr {
		id := firstStringField(m, "id", "model", "name", "slug")
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		mi := ModelInfo{ModelId: id}
		mi.ModelName = firstStringField(m, "name", "display_name", "displayName")
		out = append(out, mi)
	}
	return out
}

func firstStringField(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Call performs one upstream attempt: build the dialect-specific URL/headers/body
// from nr, POST, and stream the response into cb. Mirrors CallKiroAPIContext's
// error contract: a 429 returns *QuotaError (retryable, drives cooldown), other
// non-200s return a plain error, and a transport/stream error is classified for
// the failover dispatcher.
func (g *genericProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ps, ok := resolveProviderSettings(acct)
	if !ok {
		return fmt.Errorf("unknown provider for account %s", acct.ID)
	}

	// Build the upstream model id (prefix already stripped by ParseModelBackend
	// upstream; nr.Model is the upstream id for a non-Kiro account).
	upstreamModel := strings.TrimSpace(nr.Model)

	url, body, err := g.buildRequest(ps, nr, upstreamModel)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	g.applyHeaders(req, ps, acct)

	resp, err := GetClientForProxy(ResolveAccountProxyURL(acct)).Do(req)
	if err != nil {
		return classifyStreamError(err)
	}

	if resp.StatusCode == 429 {
		retryAfter := parseRetryAfter(resp.Header)
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		logger.Infof("[%s] throttled (429, retry-after=%s)", ps.id, retryAfter)
		return &QuotaError{Endpoints: []string{ps.id}, RetryAfter: retryAfter}
	}
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, ps.id, string(errBody))
	}

	streamErr := func() error {
		defer resp.Body.Close()
		body := newIdleTimeoutReader(resp.Body, streamIdleTimeout, func() {})
		return g.parseStream(ps, body, cb)
	}()
	return classifyStreamError(streamErr)
}

// buildRequest produces the upstream URL and JSON body for the provider's
// dialect.
func (g *genericProvider) buildRequest(ps providerSettings, nr *NormalizedRequest, upstreamModel string) (string, []byte, error) {
	switch ps.dialect {
	case DialectOpenAI:
		body, err := buildOpenAIChatBody(nr, upstreamModel, true)
		return ps.chatURL(), body, err
	case DialectAnthropic:
		body, err := buildAnthropicBody(nr, upstreamModel, true)
		return ps.chatURL(), body, err
	case DialectGemini:
		// Gemini encodes the model + streaming mode in the URL path, off the
		// models-listing base.
		url := strings.TrimRight(ps.apiBase(), "/") + "/" + upstreamModel + ":streamGenerateContent?alt=sse"
		body, err := buildGeminiBody(nr, upstreamModel)
		return url, body, err
	default:
		return "", nil, fmt.Errorf("unsupported dialect %q", ps.dialect)
	}
}

// parseStream dispatches the SSE parser for the provider's dialect.
func (g *genericProvider) parseStream(ps providerSettings, r io.Reader, cb *KiroStreamCallback) error {
	switch ps.dialect {
	case DialectOpenAI:
		return parseOpenAISSE(r, cb)
	case DialectAnthropic:
		return parseAnthropicSSE(r, cb)
	case DialectGemini:
		return parseGeminiSSE(r, cb)
	default:
		return fmt.Errorf("unsupported dialect %q", ps.dialect)
	}
}

// applyHeaders sets Content-Type, Accept, the dialect's auth header, and any
// static provider headers for an inference request.
func (g *genericProvider) applyHeaders(req *http.Request, ps providerSettings, acct *config.Account) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range ps.headers {
		req.Header.Set(k, v)
	}
	g.applyAuthHeader(req, ps, acct)
}

// applyAuthHeader sets only the credential header per the dialect's auth scheme.
// Shared by the inference path (applyHeaders) and the model-listing fetch so
// both authenticate identically.
func (g *genericProvider) applyAuthHeader(req *http.Request, ps providerSettings, acct *config.Account) {
	key := strings.TrimSpace(acct.APIKey)
	if key == "" {
		key = strings.TrimSpace(acct.AccessToken)
	}
	switch strings.ToLower(ps.authHeader) {
	case "x-api-key":
		req.Header.Set("x-api-key", key)
	case "x-goog-api-key":
		req.Header.Set("x-goog-api-key", key)
	case "key":
		req.Header.Set("key", key)
	default: // bearer
		req.Header.Set("Authorization", "Bearer "+key)
	}
}
