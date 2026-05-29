// Package proxy is the core proxy layer for the Kiro API.
// It handles streaming API calls to the Kiro backend and parses AWS Event Stream responses.
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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Endpoint configuration (auto-fallback on quota exhaustion).
type kiroEndpoint struct {
	URL       string
	Origin    string
	AmzTarget string
	Name      string
}

// kiroEndpointsOverride, when non-nil, replaces the result of
// kiroEndpointsForRegion. Set only by tests via swapKiroEndpoints; production
// code never touches it.
var kiroEndpointsOverride []kiroEndpoint

// kiroEndpointsForRegion builds the endpoint chain for a specific AWS
// region. AWS rate-limits per (identity, action), so a 429 on one of
// these does NOT imply the others are also throttled — that is why the
// per-account loop in CallKiroAPI tries each endpoint before surfacing a
// QuotaError.
//
// Region defaults to us-east-1 if empty. Per research against jwadow
// kiro-gateway issue #58 and source, the endpoint set is region-specific:
//
//   - "https://q.<region>.amazonaws.com/generateAssistantResponse"
//     Works in us-east-1, eu-west-1, eu-central-1, and likely other Q
//     Developer regions. Default Kiro IDE target.
//
//   - "https://codewhisperer.<region>.amazonaws.com/generateAssistantResponse"
//     **Only resolves in us-east-1** — the codewhisperer.* hostname
//     returns NXDOMAIN in every other region (jwadow #58). We skip this
//     entry outside us-east-1 so we don't burn a network round-trip on
//     a guaranteed DNS failure.
//
//   - "https://runtime.<region>.kiro.dev/generateAssistantResponse"
//     Universal Kiro runtime hostname used by jwadow as a regional
//     replacement for codewhisperer.*. Works in any region where Kiro
//     is provisioned. We append this for non-us-east-1 regions so the
//     three-endpoint chain stays full-length.
//
//   - q.<region>.amazonaws.com again with X-Amz-Target=AmazonQDeveloper-
//     StreamingService.SendMessage.
//
// All three are tried per-request with auto-fallback on 429.
func kiroEndpointsForRegion(region string) []kiroEndpoint {
	if kiroEndpointsOverride != nil {
		out := make([]kiroEndpoint, len(kiroEndpointsOverride))
		copy(out, kiroEndpointsOverride)
		return out
	}
	if region == "" {
		region = "us-east-1"
	}
	endpoints := []kiroEndpoint{
		{
			URL:       fmt.Sprintf("https://q.%s.amazonaws.com/generateAssistantResponse", region),
			Origin:    "AI_EDITOR",
			AmzTarget: "",
			Name:      "Kiro IDE",
		},
	}
	if region == "us-east-1" {
		// codewhisperer.* only resolves in us-east-1.
		endpoints = append(endpoints, kiroEndpoint{
			URL:       fmt.Sprintf("https://codewhisperer.%s.amazonaws.com/generateAssistantResponse", region),
			Origin:    "AI_EDITOR",
			AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
			Name:      "CodeWhisperer",
		})
	} else {
		// Use the runtime.<region>.kiro.dev hostname as the regional
		// replacement for codewhisperer.*.
		endpoints = append(endpoints, kiroEndpoint{
			URL:       fmt.Sprintf("https://runtime.%s.kiro.dev/generateAssistantResponse", region),
			Origin:    "AI_EDITOR",
			AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
			Name:      "Kiro Runtime",
		})
	}
	endpoints = append(endpoints, kiroEndpoint{
		URL:       fmt.Sprintf("https://q.%s.amazonaws.com/generateAssistantResponse", region),
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonQDeveloperStreamingService.SendMessage",
		Name:      "AmazonQ",
	})
	return endpoints
}

// kiroRESTBaseForRegion returns the REST base URL for usage / profile
// queries in a specific region. us-east-1 uses codewhisperer.<region>;
// other regions fall back to runtime.<region>.kiro.dev because the
// codewhisperer.* hostname doesn't resolve outside us-east-1.
func kiroRESTBaseForRegion(region string) string {
	if region == "" {
		region = "us-east-1"
	}
	if region == "us-east-1" {
		return "https://codewhisperer.us-east-1.amazonaws.com"
	}
	return fmt.Sprintf("https://runtime.%s.kiro.dev", region)
}

// Streaming-call timeout knobs. We deliberately do NOT set
// http.Client.Timeout because that is a wall-clock cap covering the entire
// body read, which kills long thinking-mode streams (or any stream where
// total elapsed exceeds the cap) with a "Client.Timeout … while reading
// body" error. Instead:
//
//   - responseHeaderTimeout caps connect + headers, so a stalled handshake
//     can't hang a request indefinitely.
//   - streamIdleTimeout is enforced by idleTimeoutReader: each Read must
//     produce data within this window, otherwise the underlying request
//     context is cancelled. This kills genuinely dead connections without
//     punishing slow-but-progressing generations.
const (
	responseHeaderTimeout = 60 * time.Second
	streamIdleTimeout     = 5 * time.Minute
	restRequestTimeout    = 30 * time.Second
)

// Global HTTP clients, swappable at runtime to apply proxy reconfiguration without restart.
var kiroHttpStore atomic.Pointer[http.Client]
var kiroRestHttpStore atomic.Pointer[http.Client]

// proxyClientCache caches http.Client instances keyed by proxy URL for per-account proxy support.
var proxyClientCache sync.Map

func init() {
	InitKiroHttpClient("")
}

// GetClientForProxy returns an http.Client configured for the given proxy URL.
// If proxyURL is empty, returns the global kiro HTTP client.
func GetClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroHttpStore.Load()
	}
	if cached, ok := proxyClientCache.Load(proxyURL); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		// No Timeout: long-running streaming responses are governed by
		// idleTimeoutReader on the body; ResponseHeaderTimeout on the
		// transport guards the handshake.
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(proxyURL, client)
	return client
}

// GetRestClientForProxy returns a rest http.Client (30s timeout) for the given proxy URL.
// If proxyURL is empty, returns the global kiro REST HTTP client.
func GetRestClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroRestHttpStore.Load()
	}
	cacheKey := "rest:" + proxyURL
	if cached, ok := proxyClientCache.Load(cacheKey); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   restRequestTimeout,
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(cacheKey, client)
	return client
}

// ResolveAccountProxyURL returns the effective proxy URL for an account.
// Falls back to global config.GetProxyURL() if the account has no per-account proxy.
func ResolveAccountProxyURL(account *config.Account) string {
	if account != nil && account.ProxyURL != "" {
		return account.ProxyURL
	}
	return config.GetProxyURL()
}

// buildKiroTransport constructs an HTTP Transport with optional outbound proxy support.
func buildKiroTransport(proxyURL string) *http.Transport {
	t := &http.Transport{
		MaxIdleConns: 200,
		// Nearly all traffic targets a single host (q.<region>.kiro.dev or
		// codewhisperer.<region>.amazonaws.com), so the per-host idle pool is
		// what actually bounds connection reuse. The Go default of 2 forced a
		// fresh TLS dial for every concurrent stream beyond the second;
		// matching it to MaxIdleConns keeps warm keep-alive connections for
		// the whole concurrent working set instead of churning handshakes.
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    false,
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			t.Proxy = http.ProxyURL(u)
			// Proxied connections cannot negotiate HTTP/2.
			t.ForceAttemptHTTP2 = false
		}
	} else {
		t.Proxy = http.ProxyFromEnvironment
	}
	return t
}

// InitKiroHttpClient initializes (or reinitializes) the HTTP clients used for Kiro API requests.
func InitKiroHttpClient(proxyURL string) {
	client := &http.Client{
		// No Timeout: streaming bodies handle their own idle timeout
		// (idleTimeoutReader). ResponseHeaderTimeout in the transport
		// keeps stuck handshakes from leaking forever.
		Transport: buildKiroTransport(proxyURL),
	}
	kiroHttpStore.Store(client)

	restClient := &http.Client{
		Timeout:   restRequestTimeout,
		Transport: buildKiroTransport(proxyURL),
	}
	kiroRestHttpStore.Store(restClient)
}

// ==================== Request Structs ====================

// KiroPayload is the top-level request body sent to the Kiro API.
type KiroPayload struct {
	ConversationState struct {
		AgentContinuationId string `json:"agentContinuationId,omitempty"`
		AgentTaskType       string `json:"agentTaskType,omitempty"`
		ChatTriggerType     string `json:"chatTriggerType"`
		ConversationID      string `json:"conversationId"`
		CurrentMessage      struct {
			UserInputMessage KiroUserInputMessage `json:"userInputMessage"`
		} `json:"currentMessage"`
		History []KiroHistoryMessage `json:"history,omitempty"`
	} `json:"conversationState"`
	ProfileArn      string           `json:"profileArn,omitempty"`
	InferenceConfig *InferenceConfig `json:"inferenceConfig,omitempty"`

	// ToolNameMap maps sanitized tool names (sent to Kiro) back to the
	// original names supplied by the client. Used to restore original names
	// in tool_use responses so the client can match them to its tool registry.
	// Not serialized to the Kiro API request body.
	ToolNameMap map[string]string `json:"-"`
}

type KiroUserInputMessage struct {
	Content                 string                   `json:"content"`
	ModelID                 string                   `json:"modelId,omitempty"`
	Origin                  string                   `json:"origin"`
	Images                  []KiroImage              `json:"images,omitempty"`
	UserInputMessageContext *UserInputMessageContext `json:"userInputMessageContext,omitempty"`
}

type UserInputMessageContext struct {
	Tools       []KiroToolWrapper `json:"tools,omitempty"`
	ToolResults []KiroToolResult  `json:"toolResults,omitempty"`
}

type KiroToolWrapper struct {
	ToolSpecification struct {
		Name        string      `json:"name"`
		Description string      `json:"description"`
		InputSchema InputSchema `json:"inputSchema"`
	} `json:"toolSpecification"`
}

type InputSchema struct {
	JSON interface{} `json:"json"`
}

type KiroToolResult struct {
	ToolUseID string              `json:"toolUseId"`
	Content   []KiroResultContent `json:"content"`
	Status    string              `json:"status"`
}

type KiroResultContent struct {
	Text string `json:"text"`
}

type KiroImage struct {
	Format string `json:"format"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}

type KiroHistoryMessage struct {
	UserInputMessage         *KiroUserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *KiroAssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type KiroAssistantResponseMessage struct {
	Content  string        `json:"content"`
	ToolUses []KiroToolUse `json:"toolUses,omitempty"`
}

type KiroToolUse struct {
	ToolUseID string                 `json:"toolUseId"`
	Name      string                 `json:"name"`
	Input     map[string]interface{} `json:"input"`
}

type InferenceConfig struct {
	MaxTokens   int     `json:"maxTokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"topP,omitempty"`
}

// ==================== Stream Callbacks ====================

// KiroStreamCallback stream response callbacks
type KiroStreamCallback struct {
	OnText         func(text string, isThinking bool)
	OnToolUse      func(toolUse KiroToolUse)
	OnComplete     func(inputTokens, outputTokens int)
	OnError        func(err error)
	OnCredits      func(credits float64)
	OnContextUsage func(percentage float64)
	// OnStopReason surfaces a canonical stop reason ("end_turn", "max_tokens",
	// "stop_sequence", "tool_use", "pause_turn", "refusal") detected from the
	// upstream event stream — either from explicit messageStopEvent /
	// finishReason fields or from exception events such as
	// ContentLengthExceededException. Optional; callers that don't supply this
	// fall back to a heuristic in the response builder (tool_use vs end_turn).
	OnStopReason func(reason string)
}

// ==================== API Call ====================

// maxEndpointChain bounds the worst-case number of endpoints CallKiroAPI
// will sequentially attempt for a single request. With one configured
// region this is 3 (Kiro IDE / CodeWhisperer / AmazonQ); a multi-region
// failover chain (KiroAPIRegions) multiplies that by region count. We
// cap at 12 so a misconfigured 5-region list can't produce a 15-attempt
// per-request stall — under responseHeaderTimeout=60s, 12 sequential
// stuck handshakes is already 12 minutes worst-case, which is the
// outer limit of "client will tolerate" for a single API call.
const maxEndpointChain = 12

// getSortedEndpoints returns endpoints ordered by user preference, with
// optional fallback. When config.KiroAPIRegions is non-empty, the chain
// expands across all listed regions in order: every endpoint in region[0]
// is tried before any endpoint in region[1]. With auto-fallback off, only
// the preferred endpoint in the primary region is used. The total chain
// is capped at maxEndpointChain to prevent a misconfigured multi-region
// list from producing per-request timeouts that exceed reasonable client
// tolerance.
func getSortedEndpoints(preferred string) []kiroEndpoint {
	fallback := config.GetEndpointFallback()
	regions := config.GetKiroAPIRegions()
	if len(regions) == 0 {
		regions = []string{config.GetKiroAPIRegion()}
	}

	var all []kiroEndpoint
	for _, region := range regions {
		regional := kiroEndpointsForRegion(region)
		all = append(all, sortRegionalEndpoints(regional, preferred, fallback)...)
		if !fallback {
			break
		}
		if len(all) >= maxEndpointChain {
			break
		}
	}
	if len(all) > maxEndpointChain {
		all = all[:maxEndpointChain]
	}
	return all
}

// sortRegionalEndpoints orders one region's endpoints by the preferred
// service target, then any others. Returns just the preferred entry when
// fallback is disabled.
func sortRegionalEndpoints(endpoints []kiroEndpoint, preferred string, fallback bool) []kiroEndpoint {
	var primary int
	switch preferred {
	case "kiro":
		primary = 0
	case "codewhisperer":
		// Position 1 is CodeWhisperer-style (or the regional Kiro Runtime
		// replacement) regardless of region.
		primary = 1
	case "amazonq":
		primary = 2
	default:
		// "auto" — try in declared order.
		out := make([]kiroEndpoint, len(endpoints))
		copy(out, endpoints)
		return out
	}
	if primary >= len(endpoints) {
		primary = 0
	}
	if !fallback {
		return []kiroEndpoint{endpoints[primary]}
	}
	result := []kiroEndpoint{endpoints[primary]}
	for i, ep := range endpoints {
		if i != primary {
			result = append(result, ep)
		}
	}
	return result
}

// CallKiroAPI calls the Kiro streaming API, trying each configured endpoint with automatic fallback.
func CallKiroAPI(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	// Wrap OnToolUse to restore original tool names for the client.
	if callback != nil && callback.OnToolUse != nil && len(payload.ToolNameMap) > 0 {
		originalOnToolUse := callback.OnToolUse
		nameMap := payload.ToolNameMap
		wrapped := *callback
		wrapped.OnToolUse = func(tu KiroToolUse) {
			if original, ok := nameMap[tu.Name]; ok {
				tu.Name = original
			}
			originalOnToolUse(tu)
		}
		callback = &wrapped
	}

	if payload != nil && strings.TrimSpace(payload.ProfileArn) == "" {
		if profileArn, err := ResolveProfileArn(account); err == nil {
			payload.ProfileArn = profileArn
		} else {
			accountEmail := "<nil>"
			if account != nil {
				accountEmail = account.Email
			}
			logger.Warnf("[ProfileArn] Failed to resolve profile ARN for %s: %v", accountEmail, err)
		}
	}

	// Build endpoint list ordered by configuration.
	endpoints := getSortedEndpoints(config.GetPreferredEndpoint())

	// All currently configured endpoints share Origin="AI_EDITOR", so the
	// marshaled bytes are identical per iteration. We re-marshal inside the
	// loop only when an endpoint's Origin differs from what we last serialized.
	originPtr := &payload.ConversationState.CurrentMessage.UserInputMessage.Origin
	var reqBody []byte

	var lastErr error
	// AWS rate-limits per (identity, action) — Kiro IDE / CodeWhisperer /
	// AmazonQ are three different actions, so a 429 on one does NOT imply the
	// others are throttled. We try every endpoint before reporting quota
	// exhaustion to the pool.
	type throttleHit struct {
		name       string
		retryAfter time.Duration
	}
	var throttled []throttleHit
	for _, ep := range endpoints {
		// Marshal lazily: first iteration always re-marshals (originPtr starts
		// as whatever the caller supplied, typically ""), subsequent iterations
		// only re-marshal if Origin actually changes.
		if ep.Origin != *originPtr || reqBody == nil {
			*originPtr = ep.Origin
			body, err := json.Marshal(payload)
			if err != nil {
				lastErr = err
				continue
			}
			reqBody = body
			// Debug dump only after a fresh marshal, gated on level so
			// production INFO/WARN runs avoid the string conversion.
			if logger.GetLevel() <= logger.LevelDebug {
				logger.Debugf("[KiroAPI] Request payload: %s", string(reqBody))
			}
		}

		req, err := http.NewRequest("POST", ep.URL, bytes.NewReader(reqBody))
		if err != nil {
			lastErr = err
			continue
		}

		// Each endpoint attempt gets its own cancellable context. Cancel is
		// invoked either from idleTimeoutReader (no body activity for
		// streamIdleTimeout) or from the deferred cleanup at end of attempt.
		// Without this, a stuck stream would either hang indefinitely (since
		// we removed Client.Timeout) or, with the old wall-clock cap, drop
		// a still-progressing slow stream.
		reqCtx, reqCancel := context.WithCancel(context.Background())
		req = req.WithContext(reqCtx)

		host := ""
		if parsedURL, parseErr := url.Parse(ep.URL); parseErr == nil {
			host = parsedURL.Host
		}
		headerValues := buildStreamingHeaderValues(account, host)

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "*/*")
		if ep.AmzTarget != "" {
			req.Header.Set("X-Amz-Target", ep.AmzTarget)
		}
		applyKiroBaseHeaders(req, account, headerValues)
		req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
		req.Header.Set("x-amzn-codewhisperer-optout", "true")
		req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
		req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())

		resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
		if err != nil {
			reqCancel()
			lastErr = err
			logger.Warnf("[KiroAPI] Endpoint %s failed: %v", ep.Name, err)
			continue
		}

		if resp.StatusCode == 429 {
			retryAfter := parseRetryAfter(resp.Header)
			// Drain the body before Close so the underlying connection can be
			// reused from the keep-alive pool for the next endpoint attempt.
			// Cap at 64KiB — AWS throttling envelopes are <1KB; the limit
			// guards chain-failover latency against a misbehaving upstream.
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			reqCancel()
			// Per-endpoint throttle is logged at INFO so the operator can see
			// the chain progress without WARN-level noise. We only WARN once
			// at the end if EVERY endpoint refused.
			logger.Infof("[KiroAPI] Endpoint %s throttled (429, retry-after=%s) — trying next endpoint", ep.Name, retryAfter)
			throttled = append(throttled, throttleHit{name: ep.Name, retryAfter: retryAfter})
			lastErr = &QuotaError{Endpoints: []string{ep.Name}, RetryAfter: retryAfter}
			continue
		}

		if resp.StatusCode != 200 {
			// Cap the error envelope at 64KiB. AWS error JSON is well under
			// this; the limit guards us against a misbehaving upstream that
			// might return a multi-MB HTML error page.
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			reqCancel()
			lastErr = fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, ep.Name, string(errBody))
			// Authentication errors and payment errors are not retried across endpoints.
			if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 402 {
				return lastErr
			}
			logger.Warnf("[KiroAPI] Endpoint %s error: %v", ep.Name, lastErr)
			continue
		}

		// Wrap body in idleTimeoutReader so a stalled stream cancels the
		// request context, but a slow-but-progressing stream is allowed to
		// run indefinitely. parseEventStream reads frame-by-frame so any
		// real progress resets the timer.
		body := newIdleTimeoutReader(resp.Body, streamIdleTimeout, reqCancel)
		err = parseEventStream(body, callback)
		resp.Body.Close()
		reqCancel()
		return err
	}

	// Every endpoint returned 429: surface a single QuotaError carrying the
	// SHORTEST upstream Retry-After we saw, so the pool's cooldown reflects
	// the most optimistic recovery hint.
	if len(throttled) > 0 && len(throttled) == len(endpoints) {
		names := make([]string, 0, len(throttled))
		minRetry := time.Duration(0)
		for _, t := range throttled {
			names = append(names, t.name)
			if t.retryAfter > 0 && (minRetry == 0 || t.retryAfter < minRetry) {
				minRetry = t.retryAfter
			}
		}
		logger.Warnf("[KiroAPI] Account throttled on all endpoints (%s, retry-after=%s)", strings.Join(names, "+"), minRetry)
		return &QuotaError{Endpoints: names, RetryAfter: minRetry}
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("all endpoints failed")
}

// QuotaError signals that the upstream returned 429 for this account. The
// pool should move the account into cooldown and the handler should either
// retry on a different account or surface 429 + Retry-After to the client.
//
// Callers discriminate via errors.As(err, &qe) so wrapping with fmt.Errorf
// "%w" remains safe. len(Endpoints) > 1 means the entire endpoint chain was
// exhausted; len == 1 means a single-endpoint failure (which today is unusual
// since the chain always tries every endpoint, but keeps the API symmetric).
type QuotaError struct {
	Endpoints  []string
	RetryAfter time.Duration // 0 if upstream did not send Retry-After
}

func (e *QuotaError) Error() string {
	joined := strings.Join(e.Endpoints, "+")
	if e.RetryAfter > 0 {
		return fmt.Sprintf("quota exhausted on %s (retry after %s)", joined, e.RetryAfter)
	}
	return fmt.Sprintf("quota exhausted on %s", joined)
}

// parseRetryAfter reads the Retry-After header in either delta-seconds or
// HTTP-date format (RFC 7231 §7.1.3). AWS Q Developer historically sends
// delta-seconds via API Gateway-style fronting, but we accept either to be
// safe. Returns 0 if absent or unparseable.
func parseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		// Fall back to AWS-style x-amz-retry-after which is in milliseconds.
		v = strings.TrimSpace(h.Get("x-amz-retry-after"))
		if v == "" {
			return 0
		}
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// ==================== Event Stream Parsing ====================

// eventStreamMsgPool reuses message buffers across SSE frames. Each Kiro
// streaming response can deliver hundreds of small frames; allocating a
// fresh []byte per frame was a dominant GC source. The pool stores pointers
// to byte slices (the slice header itself is a value type, so we pool the
// pointer to avoid the well-known sync.Pool slice-pinning footgun). The
// returned slice is grown to the required size via append on cold-path and
// reused otherwise.
var eventStreamMsgPool = sync.Pool{
	New: func() interface{} {
		// 4 KiB covers the typical text/reasoning frame. Larger frames grow
		// the slice; smaller ones reuse the existing capacity.
		b := make([]byte, 0, 4096)
		return &b
	},
}

// parseEventStream decodes an AWS binary Event Stream response body.
func parseEventStream(body io.Reader, callback *KiroStreamCallback) error {
	// Read directly without bufio to avoid buffering latency in streaming responses.
	var inputTokens, outputTokens int
	var totalCredits float64
	var currentToolUse *toolUseState
	var lastAssistantContent string
	var lastReasoningContent string
	stopReasonEmitted := false

	emitStopReason := func(reason string) {
		if reason == "" || stopReasonEmitted {
			return
		}
		stopReasonEmitted = true
		if callback != nil && callback.OnStopReason != nil {
			callback.OnStopReason(reason)
		}
	}

	// Stack-allocated prelude buffer reused for every frame. The 12-byte
	// header structure (total_len + headers_len + crc) is fixed-size so we
	// don't need a heap alloc per frame.
	var prelude [12]byte

	// Borrow a single message buffer from the pool for the lifetime of this
	// stream. Returned on function exit. We grow it as needed inside the
	// loop and reset its length each iteration.
	msgBufPtr := eventStreamMsgPool.Get().(*[]byte)
	defer eventStreamMsgPool.Put(msgBufPtr)

	for {
		// Prelude: 12 bytes (total_len + headers_len + crc)
		_, err := io.ReadFull(body, prelude[:])
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		totalLength := int(prelude[0])<<24 | int(prelude[1])<<16 | int(prelude[2])<<8 | int(prelude[3])
		headersLength := int(prelude[4])<<24 | int(prelude[5])<<16 | int(prelude[6])<<8 | int(prelude[7])

		if totalLength < 16 {
			continue
		}

		// Read the remaining message bytes into the pooled scratch buffer.
		// We grow capacity only when the frame exceeds what we already have
		// — the typical case is reuse with zero allocation.
		remaining := totalLength - 12
		buf := *msgBufPtr
		if cap(buf) < remaining {
			buf = make([]byte, remaining)
		} else {
			buf = buf[:remaining]
		}
		*msgBufPtr = buf
		msgBuf := buf
		_, err = io.ReadFull(body, msgBuf)
		if err != nil {
			return err
		}

		if headersLength > len(msgBuf)-4 {
			continue
		}

		eventType, exceptionType, messageType := extractEventHeaders(msgBuf[0:headersLength])
		payloadBytes := msgBuf[headersLength : len(msgBuf)-4]
		if len(payloadBytes) == 0 {
			continue
		}

		// Exception frames carry a JSON body like {"message":"..."} but the
		// crucial signal is the :exception-type header. Map known truncation
		// exceptions to canonical Anthropic stop_reason values so downstream
		// clients (Claude Code, OpenAI SDKs) see "max_tokens" / "length"
		// instead of a clean "end_turn".
		if strings.EqualFold(messageType, "exception") || exceptionType != "" {
			lower := strings.ToLower(exceptionType)
			switch {
			case strings.Contains(lower, "contentlengthexceeded"),
				strings.Contains(lower, "content_length_exceeds"),
				strings.Contains(lower, "maxtokens"),
				strings.Contains(lower, "max_tokens"):
				logger.Debugf("[KiroAPI] Upstream truncation exception: %s — %s", exceptionType, string(payloadBytes))
				emitStopReason("max_tokens")
				// Don't fall through to normal event dispatch.
				continue
			default:
				if exceptionType != "" {
					logger.Warnf("[KiroAPI] Upstream exception event %q: %s", exceptionType, string(payloadBytes))
				}
			}
		}

		var event map[string]interface{}
		if err := json.Unmarshal(payloadBytes, &event); err != nil {
			continue
		}

		inputTokens, outputTokens = updateTokensFromEvent(event, inputTokens, outputTokens)

		// Best-effort: an explicit finishReason / stopReason field anywhere in
		// the payload signals the upstream's intended termination state. Map
		// it to canonical Anthropic stop_reason values.
		if reason := extractFinishReason(event); reason != "" {
			emitStopReason(reason)
		}

		// Dispatch by event type.
		switch eventType {
		case "assistantResponseEvent":
			if content, ok := event["content"].(string); ok && content != "" {
				normalized := normalizeChunk(content, &lastAssistantContent)
				if normalized != "" {
					callback.OnText(normalized, false)
				}
			}
		case "reasoningContentEvent":
			if text, ok := event["text"].(string); ok && text != "" {
				normalized := normalizeChunk(text, &lastReasoningContent)
				if normalized != "" {
					callback.OnText(normalized, true)
				}
			}
		case "toolUseEvent":
			currentToolUse = handleToolUseEvent(event, currentToolUse, callback)
		case "meteringEvent":
			if usage, ok := event["usage"].(float64); ok {
				totalCredits += usage
			}
		case "contextUsageEvent":
			if pct, ok := event["contextUsagePercentage"].(float64); ok {
				if callback.OnContextUsage != nil {
					callback.OnContextUsage(pct)
				}
			}
		case "messageStopEvent", "messageStop":
			// Bedrock-style messageStopEvent carries the canonical stop reason.
			if reason, ok := event["stopReason"].(string); ok && reason != "" {
				emitStopReason(canonicalizeStopReason(reason))
			} else if reason, ok := event["finishReason"].(string); ok && reason != "" {
				emitStopReason(canonicalizeStopReason(reason))
			}
		}
	}

	if callback.OnCredits != nil && totalCredits > 0 {
		callback.OnCredits(totalCredits)
	}

	callback.OnComplete(inputTokens, outputTokens)
	return nil
}

// extractFinishReason looks for a finishReason / stopReason / stop_reason field
// anywhere in an event payload and returns the canonical Anthropic stop reason.
// Returns empty string when no recognized signal is present.
func extractFinishReason(event map[string]interface{}) string {
	candidates := []string{}
	collectStopReasons(event, &candidates)
	for _, raw := range candidates {
		if normalized := canonicalizeStopReason(raw); normalized != "" {
			return normalized
		}
	}
	return ""
}

func collectStopReasons(v interface{}, out *[]string) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			lk := strings.ToLower(k)
			if lk == "finishreason" || lk == "finish_reason" ||
				lk == "stopreason" || lk == "stop_reason" {
				if s, ok := child.(string); ok && s != "" {
					*out = append(*out, s)
				}
			}
			collectStopReasons(child, out)
		}
	case []interface{}:
		for _, child := range t {
			collectStopReasons(child, out)
		}
	}
}

// canonicalizeStopReason maps an upstream finish/stop reason to the canonical
// Anthropic Messages API stop_reason vocabulary. Returns "" when the input is
// not recognized so the caller can fall back to its heuristic.
func canonicalizeStopReason(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "end_turn", "endturn", "stop", "complete", "completed", "finish", "finished":
		return "end_turn"
	case "max_tokens", "maxtokens", "length", "token_limit", "tokenlimit",
		"content_length_exceeds", "contentlengthexceeded", "model_length",
		"max_output_tokens":
		return "max_tokens"
	case "stop_sequence", "stopsequence":
		return "stop_sequence"
	case "tool_use", "tooluse", "tool_calls", "toolcalls":
		return "tool_use"
	case "pause_turn", "pauseturn":
		return "pause_turn"
	case "refusal", "refused":
		return "refusal"
	}
	return ""
}

func updateTokensFromEvent(event map[string]interface{}, currentInputTokens, currentOutputTokens int) (int, int) {
	candidates := []map[string]interface{}{event}
	collectUsageMaps(event, &candidates)

	inputTokens := currentInputTokens
	outputTokens := currentOutputTokens

	for _, usage := range candidates {
		if usage == nil {
			continue
		}

		if v, ok := readTokenNumber(usage,
			"outputTokens", "completionTokens", "totalOutputTokens",
			"output_tokens", "completion_tokens", "total_output_tokens",
		); ok {
			outputTokens = v
		}

		if v, ok := readTokenNumber(usage,
			"inputTokens", "promptTokens", "totalInputTokens",
			"input_tokens", "prompt_tokens", "total_input_tokens",
		); ok {
			inputTokens = v
			continue
		}

		uncached, _ := readTokenNumber(usage, "uncachedInputTokens", "uncached_input_tokens")
		cacheRead, _ := readTokenNumber(usage, "cacheReadInputTokens", "cache_read_input_tokens")
		cacheWrite, _ := readTokenNumber(usage, "cacheWriteInputTokens", "cache_write_input_tokens", "cacheCreationInputTokens", "cache_creation_input_tokens")
		if uncached+cacheRead+cacheWrite > 0 {
			inputTokens = uncached + cacheRead + cacheWrite
			continue
		}

		total, ok := readTokenNumber(usage, "totalTokens", "total_tokens")
		if ok && total > 0 {
			candidateOutput := outputTokens
			if v, vok := readTokenNumber(usage,
				"outputTokens", "completionTokens", "totalOutputTokens",
				"output_tokens", "completion_tokens", "total_output_tokens",
			); vok {
				candidateOutput = v
			}
			if total-candidateOutput > 0 {
				inputTokens = total - candidateOutput
			}
		}
	}

	return inputTokens, outputTokens
}

// getContextWindowSize returns the context window size (in tokens) for a model.
func getContextWindowSize(model string) int {
	m := strings.ToLower(model)
	// sonnet-4.6, opus-4.6, opus-4.7 all have 1M context windows
	if strings.Contains(m, "4.6") || strings.Contains(m, "4-6") ||
		strings.Contains(m, "4.7") || strings.Contains(m, "4-7") {
		return 1_000_000
	}
	return 200_000
}

func collectUsageMaps(v interface{}, out *[]map[string]interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			lk := strings.ToLower(k)
			if lk == "usage" || lk == "tokenusage" || lk == "token_usage" {
				if m, ok := child.(map[string]interface{}); ok {
					*out = append(*out, m)
				}
			}
			collectUsageMaps(child, out)
		}
	case []interface{}:
		for _, child := range t {
			collectUsageMaps(child, out)
		}
	}
}

// normalizeChunk reconciles a freshly received upstream text chunk with the
// snapshot of what we have already emitted. Kiro's event stream sometimes
// replays cumulative content (e.g. successive chunks "abc" then "abcde"), and
// older snapshots can arrive out of order on retries. We strip those exact
// overlaps but never guess at partial-suffix duplication: a coincidental match
// between prev's tail and chunk's head can just as easily be legitimate text
// across a chunk boundary, and dropping it produced visible truncations like
// "sleep" -> "slep" or "lets begin" -> "letsbegin".
func normalizeChunk(chunk string, previous *string) string {
	if chunk == "" {
		return ""
	}

	prev := *previous
	if prev == "" {
		*previous = chunk
		return chunk
	}

	// Exact replay of the most recent snapshot — drop.
	if chunk == prev {
		return ""
	}

	// Cumulative replay: the new chunk extends prev. Emit only the new tail.
	if strings.HasPrefix(chunk, prev) {
		delta := chunk[len(prev):]
		*previous = chunk
		return delta
	}

	// Rewind: an older, shorter snapshot arrived after a longer one. Ignore it
	// and keep the longer snapshot so future cumulative comparisons stay sound.
	if strings.HasPrefix(prev, chunk) {
		return ""
	}

	// Otherwise treat the chunk as a pure delta. Earlier revisions tried to
	// detect partial overlaps via HasSuffix(prev, chunk[:i]) and trim them,
	// but that heuristic mis-deletes legitimate characters whenever prev's
	// trailing rune happens to match chunk's leading rune.
	*previous = chunk
	return chunk
}

func readTokenNumber(m map[string]interface{}, keys ...string) (int, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int(n), true
		case int:
			return n, true
		case int64:
			return int(n), true
		case json.Number:
			if parsed, err := n.Int64(); err == nil {
				return int(parsed), true
			}
		case string:
			if parsed, err := strconv.Atoi(n); err == nil {
				return parsed, true
			}
			if parsed, err := strconv.ParseFloat(n, 64); err == nil {
				return int(parsed), true
			}
		}
	}
	return 0, false
}

// ==================== Tool Use Handling ====================

type toolUseState struct {
	ToolUseID   string
	Name        string
	InputBuffer strings.Builder
}

func handleToolUseEvent(event map[string]interface{}, current *toolUseState, callback *KiroStreamCallback) *toolUseState {
	toolUseID, _ := event["toolUseId"].(string)
	name, _ := event["name"].(string)
	isStop, _ := event["stop"].(bool)

	if toolUseID != "" && name != "" {
		if current == nil {
			current = &toolUseState{ToolUseID: toolUseID, Name: name}
		} else if current.ToolUseID != toolUseID {
			finishToolUse(current, callback)
			current = &toolUseState{ToolUseID: toolUseID, Name: name}
		}
	}

	if current != nil {
		if input, ok := event["input"].(string); ok {
			current.InputBuffer.WriteString(input)
		} else if inputObj, ok := event["input"].(map[string]interface{}); ok {
			data, _ := json.Marshal(inputObj)
			current.InputBuffer.Reset()
			current.InputBuffer.Write(data)
		}
	}

	if isStop && current != nil {
		finishToolUse(current, callback)
		return nil
	}

	return current
}

func finishToolUse(state *toolUseState, callback *KiroStreamCallback) {
	var input map[string]interface{}
	if state.InputBuffer.Len() > 0 {
		json.Unmarshal([]byte(state.InputBuffer.String()), &input)
	}
	if input == nil {
		input = make(map[string]interface{})
	}
	callback.OnToolUse(KiroToolUse{
		ToolUseID: state.ToolUseID,
		Name:      state.Name,
		Input:     input,
	})
}

// extractEventType extracts the event type string from AWS Event Stream message headers.
func extractEventType(headers []byte) string {
	eventType, _, _ := extractEventHeaders(headers)
	return eventType
}

// extractEventHeaders extracts the AWS Event Stream framing identity headers
// (:event-type, :exception-type, :message-type). Either of the first two is
// usually present; :message-type tells us whether this frame is a normal
// "event" or a fatal "exception" we must surface to the client.
func extractEventHeaders(headers []byte) (eventType, exceptionType, messageType string) {
	offset := 0
	for offset < len(headers) {
		if offset >= len(headers) {
			break
		}
		nameLen := int(headers[offset])
		offset++
		if offset+nameLen > len(headers) {
			break
		}
		name := string(headers[offset : offset+nameLen])
		offset += nameLen
		if offset >= len(headers) {
			break
		}
		valueType := headers[offset]
		offset++

		if valueType == 7 { // String
			if offset+2 > len(headers) {
				break
			}
			valueLen := int(headers[offset])<<8 | int(headers[offset+1])
			offset += 2
			if offset+valueLen > len(headers) {
				break
			}
			value := string(headers[offset : offset+valueLen])
			offset += valueLen
			switch name {
			case ":event-type":
				eventType = value
			case ":exception-type":
				exceptionType = value
			case ":message-type":
				messageType = value
			}
			continue
		}

		// Skip other value types by their fixed byte widths.
		skipSizes := map[byte]int{0: 0, 1: 0, 2: 1, 3: 2, 4: 4, 5: 8, 8: 8, 9: 16}
		if valueType == 6 {
			if offset+2 > len(headers) {
				break
			}
			l := int(headers[offset])<<8 | int(headers[offset+1])
			offset += 2 + l
		} else if skip, ok := skipSizes[valueType]; ok {
			offset += skip
		} else {
			break
		}
	}
	return eventType, exceptionType, messageType
}
