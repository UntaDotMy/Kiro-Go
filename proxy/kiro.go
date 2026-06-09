// Package proxy is the core proxy layer for the Kiro API.
// It handles streaming API calls to the Kiro backend and parses AWS Event Stream responses.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/http2"
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
// Region defaults to us-east-1 if empty. Per Kiro's published firewall
// allowlist (https://kiro.dev/docs/privacy-and-security/firewalls/) the
// CURRENT "Kiro service" inference host is runtime.<region>.kiro.dev, and
// the q.<region>.amazonaws.com endpoints are documented there as LEGACY and
// "will be deprecated in a future release." We therefore LEAD with the
// runtime host and keep the legacy AWS endpoints as reactive 429/error
// fallback (NOT removed — the operator asked to keep the legacy endpoint
// reachable). The chain, in preference order:
//
//   - "https://runtime.<region>.kiro.dev/generateAssistantResponse"
//     PRIMARY. The modern Kiro runtime host; the only one the firewall doc
//     lists as the active "Kiro service". Resolves in every region Kiro is
//     provisioned in.
//
//   - "https://q.<region>.amazonaws.com/generateAssistantResponse"
//     LEGACY (kept as fallback). The previous Kiro IDE default; no
//     X-Amz-Target. Still works today, deprecation pending.
//
//   - "https://codewhisperer.<region>.amazonaws.com/generateAssistantResponse"
//     LEGACY (kept, us-east-1 ONLY). The codewhisperer.* hostname returns
//     NXDOMAIN in every other region (jwadow #58), so it's appended only
//     for us-east-1.
//
//   - q.<region>.amazonaws.com again with X-Amz-Target=AmazonQDeveloper-
//     StreamingService.SendMessage. LEGACY (kept).
//
// All are tried per-request with auto-fallback on 429.
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
		// PRIMARY: the current Kiro runtime service host.
		{
			URL:       fmt.Sprintf("https://runtime.%s.kiro.dev/generateAssistantResponse", region),
			Origin:    "AI_EDITOR",
			AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
			Name:      "Kiro Runtime",
		},
		// LEGACY (kept as fallback): the previous Kiro IDE default, no X-Amz-Target.
		{
			URL:       fmt.Sprintf("https://q.%s.amazonaws.com/generateAssistantResponse", region),
			Origin:    "AI_EDITOR",
			AmzTarget: "",
			Name:      "Kiro IDE (legacy)",
		},
	}
	if region == "us-east-1" {
		// LEGACY (kept): codewhisperer.* only resolves in us-east-1.
		endpoints = append(endpoints, kiroEndpoint{
			URL:       fmt.Sprintf("https://codewhisperer.%s.amazonaws.com/generateAssistantResponse", region),
			Origin:    "AI_EDITOR",
			AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
			Name:      "CodeWhisperer (legacy)",
		})
	}
	endpoints = append(endpoints, kiroEndpoint{
		URL:       fmt.Sprintf("https://q.%s.amazonaws.com/generateAssistantResponse", region),
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonQDeveloperStreamingService.SendMessage",
		Name:      "AmazonQ (legacy)",
	})
	return endpoints
}

// kiroRESTBaseForRegion returns the REST base URL for the account-MANAGEMENT
// calls (getUsageLimits / GetUserInfo / ListAvailableModels /
// ListAvailableProfiles). These are AWS CodeWhisperer/Q API actions served by
// the amazonaws.com hosts — NOT by runtime.<region>.kiro.dev, which only serves
// the inference action (generateAssistantResponse). us-east-1 therefore uses
// codewhisperer.us-east-1.amazonaws.com; other regions use
// runtime.<region>.kiro.dev only because codewhisperer.* returns NXDOMAIN
// outside us-east-1 (best-effort there).
//
// NOTE: this intentionally does NOT follow the streaming chain onto
// runtime.us-east-1.kiro.dev. Pointing these REST paths at the runtime host
// breaks account refresh (the host 4xxs the management paths), and unlike the
// streaming chain the REST callers have no fallback. The firewall allowlist's
// "Kiro service" = runtime host applies to inference traffic, which IS routed
// there (see kiroEndpointsForRegion); the management API is a separate surface.
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

	// HTTP/2 active health-check knobs for the upstream connection to AWS.
	//
	// Root cause of "context deadline exceeded … while reading body" mid-stream
	// disconnects: Go's HTTP/2 transport never probes an idle connection on its
	// own. During a long thinking-mode gap (or any quiet stretch) a middlebox —
	// AWS ALB/NLB idle reaper, NAT rebind, outbound-proxy drop, wifi handoff —
	// can silently drop the keep-alive connection without a FIN/RST ever
	// reaching us. The blocked resp.Body.Read() then hangs until
	// streamIdleTimeout (5m) cancels the request, surfacing the cancellation as
	// a client-visible API error in the middle of a turn.
	//
	// With ReadIdleTimeout set, the transport sends an HTTP/2 PING after this
	// much silence on a connection. That does double duty: the PING traffic
	// keeps middleboxes from treating the connection as idle (prevention), and a
	// missing PING ACK within PingTimeout tears the connection down with a
	// concrete error instead of 5 minutes (fast detection). A healthy-but-slow
	// stream is unaffected — PINGs are answered at the transport layer regardless
	// of application-level token generation, reads keep returning data, and
	// idleTimeoutReader's 5m budget is never approached.
	//
	// Budget tuning (raised from 15s/15s): a PING ACK isn't only delayed by a
	// dead connection — an alive-but-busy intermediary (AWS ALB/NLB, NAT, a
	// TLS-terminating proxy) can delay the ACK round-trip during a legitimate
	// quiet stretch of a long generation. With a tight 15s ReadIdleTimeout +
	// 15s PingTimeout, that delay tripped closeForLostPing and aborted the
	// in-flight request with "http2: client connection lost" — a false-positive
	// teardown surfaced to the user as an API error. We now ping only after 30s
	// of genuine silence and give the ACK 20s of headroom (gRPC's own keepalive
	// ACK-timeout default), so a slow-but-alive upstream is no longer killed.
	// The combined 50s detection budget is still far under streamIdleTimeout (5m)
	// — enforced by TestEnableHTTP2PingsAppliesTimeouts — so a genuinely dead
	// connection is still caught in well under a minute, not five.
	h2ReadIdleTimeout = 30 * time.Second
	h2PingTimeout     = 20 * time.Second

	// tcpKeepAliveInterval is the OS-level TCP keep-alive probe interval on the
	// upstream dialer. It is the dead-connection detection floor for BOTH
	// transports, and the only active liveness probing on the proxied path
	// (where HTTP/2 PINGs are unavailable). Kept in the same ballpark as the h2
	// ping budget so detection latency is comparable across both paths.
	tcpKeepAliveInterval = 15 * time.Second

	// streamHeartbeatInterval is how often the downstream streaming handlers
	// emit an SSE keepalive (an Anthropic `ping` event) to the client while the
	// upstream is silent. The real Anthropic API sends pings during long
	// operations for exactly this reason; without them a healthy-but-quiet
	// generation (a multi-minute thinking pause, a slow tool gap) looks like a
	// dead connection to the client's own idle timer and gets aborted.
	streamHeartbeatInterval = 15 * time.Second
)

// Global HTTP clients, swappable at runtime to apply proxy reconfiguration without restart.
var kiroHttpStore atomic.Pointer[http.Client]
var kiroRestHttpStore atomic.Pointer[http.Client]

// timingEnabled gates env-gated per-request latency decomposition logging
// (KIRO_TIMING=1). Read once at startup; the per-request timing state itself is
// kept in LOCAL variables (never package-level) so concurrent requests can't
// race on it. Off by default = zero hot-path cost.
var timingEnabled = os.Getenv("KIRO_TIMING") == "1"

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
		// With per-account region pinning (resolveAccountRegion), every
		// request from one account targets the SAME host (q.<region> or
		// the regional REST host), so the per-host idle pool is what
		// bounds keep-alive reuse. A generous per-host pool keeps warm
		// connections for the whole concurrent working set
		// (≈ accounts-sharing-a-region × aimdMaxLimit) instead of forcing
		// a fresh TLS dial per stream. NOTE: this is also why the earlier
		// per-request endpoint rotation hurt latency — it alternated the
		// destination host every request, so keep-alive reuse never
		// happened and each call paid a fresh handshake. Pinning fixed
		// that. The Go default of 2 would reintroduce the churn.
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    false,
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: responseHeaderTimeout,
		// OS-level TCP keep-alive on the dialer. This is the floor of
		// dead-connection detection and, crucially, the ONLY active probing on
		// the proxied path where HTTP/2 PINGs are unavailable (h2 is disabled
		// for forward proxies below). The kernel sends keep-alive probes on an
		// otherwise-silent socket; a middlebox that has dropped the connection
		// answers with a RST, surfacing a concrete read error instead of a
		// multi-minute hang. On the direct path this layers under the h2 PINGs.
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: tcpKeepAliveInterval,
		}).DialContext,
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

	// Enable HTTP/2 active PING health-checks on the direct (non-proxied)
	// transport. ForceAttemptHTTP2 lets net/http build an INTERNAL http2
	// transport we can't otherwise reach, so its ReadIdleTimeout defaults to 0
	// (disabled) and a silently-dropped connection is never detected until the
	// 5-minute idleTimeoutReader fires. http2.ConfigureTransports surfaces that
	// internal transport so we can turn on PINGs. See the h2*Timeout constants
	// for the full rationale. Proxied connections speak HTTP/1.1 (h2 disabled
	// above) and rely on the DialContext TCP keep-alive instead, so we skip the
	// h2 wiring there.
	if t.ForceAttemptHTTP2 {
		if _, err := enableHTTP2Pings(t); err != nil {
			logger.Warnf("[KiroAPI] HTTP/2 ping health-check setup failed (continuing without active keepalive): %v", err)
		}
	}
	return t
}

// enableHTTP2Pings turns on active HTTP/2 PING health-checking for an
// http.Transport that will negotiate h2. It must be called exactly once per
// transport before first use (http2.ConfigureTransports errors if h2 is
// already configured). Returns the configured *http2.Transport (for tests to
// assert the timeouts) and any setup error. Split out from buildKiroTransport
// so the ping wiring is unit-testable in isolation.
func enableHTTP2Pings(t *http.Transport) (*http2.Transport, error) {
	h2, err := http2.ConfigureTransports(t)
	if err != nil {
		return nil, err
	}
	if h2 != nil {
		h2.ReadIdleTimeout = h2ReadIdleTimeout
		h2.PingTimeout = h2PingTimeout
	}
	return h2, nil
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

	// AdditionalModelRequestFields is the Bedrock-style passthrough the Kiro
	// CLI 2.5 uses to carry per-model knobs that aren't first-class request
	// fields — most notably reasoning effort:
	//
	//   {"output_config": {"effort": "high"}}
	//
	// It is a TOP-LEVEL sibling of conversationState / profileArn in
	// GenerateAssistantResponseInput (verified against kiro-cli 2.5.0's own
	// request serializer and a live generateAssistantResponse body capture).
	// Each model advertises which keys/values it accepts via its
	// ModelInfo.AdditionalModelRequestFieldsSchema; sending a value a model
	// doesn't declare yields HTTP 400 ("model does not support additional
	// fields"), so callers MUST gate on the per-model schema before populating
	// this. Empty map -> omitted entirely (safe default).
	AdditionalModelRequestFields map[string]interface{} `json:"additionalModelRequestFields,omitempty"`

	// ToolNameMap maps sanitized tool names (sent to Kiro) back to the
	// original names supplied by the client. Used to restore original names
	// in tool_use responses so the client can match them to its tool registry.
	// Not serialized to the Kiro API request body.
	ToolNameMap map[string]string `json:"-"`

	// ResolvedEffort is the reasoning-effort level actually forwarded upstream
	// for this request ("" when none — Claude path, unsupported model, or unset
	// request). Set by Handler.applyReasoningEffort and read back at
	// recordSuccess time so per-effort analytics can be attributed without
	// threading the level through every handler signature. Not serialized to
	// the Kiro API request body.
	ResolvedEffort string `json:"-"`
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

// resolveAccountRegion returns the ONE stable AWS region an account talks
// to, every request, for the life of that account. This is the key to
// clean cross-account load spreading WITHOUT making any single OAuth
// identity look anomalous: each identity is pinned to one region and never
// hops, while different accounts deterministically land on different
// regions so the pool's traffic is spread across regional rate buckets.
//
// Resolution precedence:
//
//  1. An explicit per-account override (account.KiroRegion, if you ever
//     add one) — not used today; the account.Region field holds the OIDC
//     login region, which is NOT always a valid streaming-API region
//     (e.g. us-east-2 IdC accounts: runtime.us-east-2.kiro.dev may not
//     resolve), so we deliberately do NOT route by it.
//  2. If the operator configured KIRO_API_REGIONS, pick one
//     deterministically by hashing the account ID: crc32(id) % N. Stable
//     per account (same account → same region every time), and spreads
//     accounts evenly across the configured regions.
//  3. Otherwise the single global region (GetKiroAPIRegion, default
//     us-east-1).
//
// A nil account or empty ID falls back to the global region so callers
// that don't have an account (REST cold-start paths) keep working.
func resolveAccountRegion(account *config.Account) string {
	regions := config.GetKiroAPIRegions()
	if len(regions) == 0 || account == nil || account.ID == "" {
		return config.GetKiroAPIRegion()
	}
	idx := crc32.ChecksumIEEE([]byte(account.ID)) % uint32(len(regions))
	return regions[idx]
}

// getSortedEndpoints returns the endpoint chain for a single account,
// pinned to that account's stable region (resolveAccountRegion). The
// chain covers ONLY that one region's service actions in preference
// order; a healthy request uses the primary action, and the others are
// reactive 429-only fallback WITHIN the same region. One identity never
// crosses regions — cross-region spreading happens across ACCOUNTS, not
// within an account. The chain is capped at maxEndpointChain.
func getSortedEndpoints(preferred string, account *config.Account) []kiroEndpoint {
	fallback := config.GetEndpointFallback()
	region := resolveAccountRegion(account)

	regional := kiroEndpointsForRegion(region)
	all := sortRegionalEndpoints(regional, preferred, fallback)
	if len(all) > maxEndpointChain {
		all = all[:maxEndpointChain]
	}
	return all
}

// sortRegionalEndpoints orders one region's endpoints by the preferred
// service target, then any others. Returns just the preferred entry when
// fallback is disabled. The primary is resolved by MATCHING the endpoint's
// identity (not a hardcoded index), so it stays correct regardless of the
// chain order:
//
//   - "kiro"          → the modern Kiro Runtime host (runtime.<region>.kiro.dev).
//   - "codewhisperer" → the CodeWhisperer endpoint when present (us-east-1),
//     else the runtime host (it carries the same
//     GenerateAssistantResponse target outside us-east-1).
//   - "amazonq"       → the AmazonQ SendMessage endpoint.
//   - "auto" / other  → declared order (runtime first), all reactive 429-only.
func sortRegionalEndpoints(endpoints []kiroEndpoint, preferred string, fallback bool) []kiroEndpoint {
	if len(endpoints) == 0 {
		return endpoints
	}

	// findByName returns the first index whose Name contains sub, or -1.
	findByName := func(sub string) int {
		for i, ep := range endpoints {
			if strings.Contains(ep.Name, sub) {
				return i
			}
		}
		return -1
	}

	primary := -1
	switch preferred {
	case "kiro":
		primary = findByName("Kiro Runtime")
	case "codewhisperer":
		if i := findByName("CodeWhisperer"); i >= 0 {
			primary = i
		} else {
			primary = findByName("Kiro Runtime") // regional equivalent
		}
	case "amazonq":
		primary = findByName("AmazonQ")
	default:
		// "auto" — try in declared order. A healthy request always executes
		// endpoint[0] (the Kiro Runtime host, now the primary per the firewall
		// allowlist); the legacy endpoints stay REACTIVE 429-only fallback. This
		// keeps a single, consistent service action per request rather than
		// rotating the X-Amz-Target every call.
		out := make([]kiroEndpoint, len(endpoints))
		copy(out, endpoints)
		return out
	}
	if primary < 0 || primary >= len(endpoints) {
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

// CallKiroAPI calls the Kiro streaming API with a background context. Retained
// for callers without a request context (admin warmup/probe, agentic-loop
// buffered rounds, tests). Request handlers should prefer CallKiroAPIContext so
// a client disconnect cancels the upstream call.
func CallKiroAPI(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	return CallKiroAPIContext(context.Background(), account, payload, callback)
}

// CallKiroAPIContext calls the Kiro streaming API, trying each configured
// endpoint with automatic fallback. The supplied ctx is the parent of each
// per-endpoint attempt context, so cancelling it (e.g. the client disconnected)
// aborts the in-flight upstream call instead of letting it run to completion and
// burn account credits + hold an AIMD in-flight slot for nothing.
func CallKiroAPIContext(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if ctx == nil {
		ctx = context.Background()
	}
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

	// Per-request latency-decomposition state (KIRO_TIMING). LOCAL to this
	// invocation so concurrent requests never race on it. callStart anchors both
	// the TTFT measurement and headersMs so genGapMs = firstTokenMs - headersMs is
	// internally consistent. The endpoint loop writes these; the recordTTFT
	// closure reads them when the first token lands.
	callStart := time.Now()
	var timingHeadersMs float64
	var timingEpTried int
	var timing429 int
	var timingEpName string

	// Measure TIME-TO-FIRST-TOKEN for latency-aware "smart laning" selection.
	// We record the elapsed time from request dispatch to the FIRST content
	// callback (text or tool_use), per account, into the pool's TTFT EWMA. TTFT
	// is the load-bearing latency signal (it reflects the account's queueing /
	// warmth), unlike total completion time which is dominated by output length.
	// The wrap fires the measurement exactly once per call via sync.Once, and
	// only on a real account id, so it never double-counts across the multi-
	// endpoint retry loop. Skipped for a nil account (tests / probes).
	if account != nil && account.ID != "" && callback != nil {
		acctID := account.ID
		start := callStart
		var ttftOnce sync.Once
		recordTTFT := func() {
			ttftOnce.Do(func() {
				// Sub-millisecond resolution: time.Duration.Milliseconds() truncates
				// to an integer, so a first token arriving in <1ms would yield 0 and
				// be discarded by RecordTTFT's `ms <= 0` guard — losing the
				// measurement for fast responses (and making every local-stub test
				// record nothing). Divide by time.Millisecond to keep the fraction.
				ms := float64(time.Since(start)) / float64(time.Millisecond)
				pool.GetPool().RecordTTFT(acctID, ms)
				// Env-gated latency decomposition. genGapMs = firstToken - headers,
				// i.e. how long the UPSTREAM model took to produce the first token
				// AFTER the connection was established — the part no proxy change can
				// shrink except by routing to a faster identity. A large headersMs
				// instead points at connect/throttle at the HTTP layer; endpointsTried
				// > 1 / throttled > 0 means the endpoint chain burned round-trips
				// before landing a 200.
				if timingEnabled {
					logger.Infof("[Timing] acct=%s firstTokenMs=%.0f headersMs=%.0f genGapMs=%.0f endpointsTried=%d throttled=%d endpoint=%s",
						acctID, ms, timingHeadersMs, ms-timingHeadersMs, timingEpTried, timing429, timingEpName)
				}
			})
		}
		wrapped := *callback
		if origText := callback.OnText; origText != nil {
			wrapped.OnText = func(text string, isThinking bool) {
				recordTTFT()
				origText(text, isThinking)
			}
		} else {
			wrapped.OnText = func(string, bool) { recordTTFT() }
		}
		if origTool := callback.OnToolUse; origTool != nil {
			wrapped.OnToolUse = func(tu KiroToolUse) {
				recordTTFT()
				origTool(tu)
			}
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

	// Build the endpoint list for THIS account, pinned to its stable
	// region (see resolveAccountRegion). One identity never crosses
	// regions; load spreads across accounts.
	endpoints := getSortedEndpoints(config.GetPreferredEndpoint(), account)

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
		if timingEnabled {
			timingEpTried++
			timingEpName = ep.Name
		}
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
			// At DEBUG, log only a non-sensitive summary by default. The full
			// payload carries the entire conversation history + profileArn, so
			// dumping it on every request silently persists user conversations to
			// whatever sink DEBUG is wired to (file, stdout shipper, cloud logs)
			// for as long as the level stays on. The raw dump is available behind
			// an explicit opt-in (KIRO_DUMP_PAYLOADS=1) for deliberate debugging.
			if logger.GetLevel() <= logger.LevelDebug {
				if os.Getenv("KIRO_DUMP_PAYLOADS") == "1" {
					logger.Debugf("[KiroAPI] Request payload: %s", string(reqBody))
				} else {
					logger.Debugf("[KiroAPI] Request payload: %d bytes (set KIRO_DUMP_PAYLOADS=1 to log full body)", len(reqBody))
				}
			}
		}

		req, err := http.NewRequest("POST", ep.URL, bytes.NewReader(reqBody))
		if err != nil {
			lastErr = err
			continue
		}

		// Each endpoint attempt gets its own cancellable context derived from the
		// caller's ctx. Cancel fires from idleTimeoutReader (no body activity for
		// streamIdleTimeout), from the deferred cleanup at end of attempt, OR when
		// the parent ctx is cancelled (client disconnect) — so a stuck or
		// abandoned stream no longer runs to completion burning credits and an
		// AIMD slot. Without this, a slow-but-progressing stream still runs
		// indefinitely (the idle reader only trips on a stall), which is intended.
		reqCtx, reqCancel := context.WithCancel(ctx)
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
			// Classify HTTP/2 RST_STREAM / GOAWAY at the transport layer
			// so the dispatcher's retryable check (and the post-commit
			// SSE error event, when the chain finally gives up) sees
			// *ErrUpstreamStreamReset rather than the raw *url.Error
			// wrapping a *http2.StreamError.
			lastErr = classifyStreamError(err)
			logger.Warnf("[KiroAPI] Endpoint %s failed: %v", ep.Name, lastErr)
			continue
		}

		if resp.StatusCode == 429 {
			retryAfter := parseRetryAfter(resp.Header)
			if timingEnabled {
				timing429++
			}
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
		// real progress resets the timer. The Close + cancel are deferred inside
		// a closure so they run even if a callback panics mid-parse (callbacks
		// write to the client ResponseWriter and json.Marshal) — otherwise a
		// panic would unwind past the inline cleanup and leak the connection and
		// the context's supervisor goroutine.
		if timingEnabled {
			// client.Do() returns once response HEADERS are received, so "now" is
			// the time-to-first-byte. firstTokenMs - this = pure upstream generation
			// latency (the model thinking before its first token), which routing
			// can only fix by choosing a faster identity.
			timingHeadersMs = float64(time.Since(callStart)) / float64(time.Millisecond)
		}
		streamErr := func() error {
			defer resp.Body.Close()
			defer reqCancel()
			body := newIdleTimeoutReader(resp.Body, streamIdleTimeout, reqCancel)
			return parseEventStream(body, callback)
		}()
		return classifyStreamError(streamErr)
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

// maxEventStreamFrameBytes caps a single AWS event-stream frame. The 4-byte
// total-length prefix is attacker/transport-controlled (a malformed or MITMed
// upstream frame could claim ~2GiB), and we allocate `remaining` bytes for it —
// so without a cap one bad frame can OOM the process. Real Kiro frames are a
// few KiB (text/reasoning deltas) up to low-MiB at most; 16 MiB is comfortably
// above any legitimate frame while bounding the worst-case allocation.
const maxEventStreamFrameBytes = 16 << 20 // 16 MiB

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
			// Clean EOF at a frame boundary is how the upstream signals normal
			// end-of-stream: CodeWhisperer / Amazon Q generateAssistantResponse
			// has NO application-level terminal event (no messageStop /
			// stopReason). A drop that lands exactly on a frame boundary is
			// therefore indistinguishable from a complete response even to AWS's
			// own client, so we treat a clean EOF as success. A drop MID-FRAME is
			// caught below: io.ReadFull returns io.ErrUnexpectedEOF, which
			// classifyStreamError flags as a retryable stream reset.
			break
		}
		if err != nil {
			return classifyStreamError(err)
		}

		totalLength := int(prelude[0])<<24 | int(prelude[1])<<16 | int(prelude[2])<<8 | int(prelude[3])
		headersLength := int(prelude[4])<<24 | int(prelude[5])<<16 | int(prelude[6])<<8 | int(prelude[7])

		if totalLength < 16 {
			continue
		}
		// Reject an implausibly large frame before allocating for it. The
		// length prefix is upstream/transport-controlled, so a malformed or
		// tampered frame claiming ~2GiB would otherwise trigger a giant
		// make([]byte, remaining) and OOM the process.
		if totalLength > maxEventStreamFrameBytes {
			return fmt.Errorf("event stream frame too large: %d bytes (max %d)", totalLength, maxEventStreamFrameBytes)
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
			return classifyStreamError(err)
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
			// content is an INCREMENTAL delta fragment, NOT a cumulative
			// snapshot: CodeWhisperer / Amazon Q generateAssistantResponse emits
			// each piece once and the full message is their verbatim
			// concatenation (confirmed against AWS's own Q CLI, which does a
			// plain push_str, and every reference proxy, which does
			// `result.content += event.content`). We therefore forward each
			// fragment AS-IS. An earlier "normalizeChunk" deduper assumed a
			// cumulative stream and dropped exact-equal or prefix-overlapping
			// fragments — which silently corrupted legitimate output ("i like
			// you" -> "ilike you", "water" -> "wate", a doubled word -> one).
			// The binary frame parser already delivers each frame exactly once,
			// so there is nothing to dedup.
			if content, ok := event["content"].(string); ok && content != "" {
				if callback.OnText != nil {
					callback.OnText(content, false)
				}
			}
		case "reasoningContentEvent":
			if text, ok := event["text"].(string); ok && text != "" {
				if callback.OnText != nil {
					callback.OnText(text, true)
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

	// Clean EOF reached (every error path inside the loop returns, so getting
	// here means the body ended normally). Finalize any tool use that was still
	// being assembled: it never received its stop:true frame. This is the AWS
	// "UnexpectedToolUseEos" shape — "the stream can unexpectedly end while
	// waiting for an exceptionally complex tool use [because] some proxy server
	// dropped the idle connection." Previously the pending tool use was SILENTLY
	// DISCARDED here: the loop just fell through to OnComplete, the handler saw
	// no tool call, and the request resolved to a fabricated end_turn — so a
	// client like Claude Code emitted its "Let me look at the project..."
	// preamble and then ended the turn instead of running the tool.
	//
	// Decide by the completeness of the accumulated input, which is correct
	// whether stop:true is a guaranteed terminator or merely best-effort:
	//   - empty input (argless tool) or input that parses as valid complete
	//     JSON  -> the call finished and only the stop frame was lost in transit;
	//     flush it so the tool call is delivered rather than dropped.
	//   - non-empty input that is NOT valid JSON -> genuinely truncated
	//     mid-arguments; we can't fabricate the rest, so surface a retryable
	//     stream-reset (classifyStreamError maps io.ErrUnexpectedEOF to one) so
	//     the dispatcher fails over to a peer pre-commit, or the post-commit path
	//     emits a real error frame — never a clean end_turn.
	if currentToolUse != nil {
		if buf := currentToolUse.InputBuffer.String(); buf != "" && !json.Valid([]byte(buf)) {
			return classifyStreamError(io.ErrUnexpectedEOF)
		}
		finishToolUse(currentToolUse, callback)
		currentToolUse = nil
	}

	if callback.OnCredits != nil && totalCredits > 0 {
		callback.OnCredits(totalCredits)
	}

	if callback.OnComplete != nil {
		callback.OnComplete(inputTokens, outputTokens)
	}
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
	if isLargeContextModel(model) {
		return 1_000_000
	}
	return 200_000
}

// isLargeContextModel reports whether a Claude model has the 1M-token context
// window. Claude Opus/Sonnet/Haiku >= 4.6 (and any major >= 5) are 1M; earlier
// versions are 200K.
//
// We version-PARSE rather than substring-match a fixed "4.6"/"4.7" list so new
// minors (4.8, 4.9, 4.10) and majors (5.x) are classified correctly without a
// code change. The previous fixed-list form under-reported 4.8+ as 200K — a ~5x
// under-count of input tokens — even though our model router resolves those ids.
// A substring fallback covers non-standard identifiers that don't parse cleanly.
func isLargeContextModel(model string) bool {
	m := strings.ToLower(model)
	if major, minor, ok := parseClaudeVersion(m); ok {
		if major > 4 {
			return true
		}
		if major == 4 && minor >= 6 {
			return true
		}
		return false
	}
	// Fallback for ids that don't match the family-version shape.
	for _, tag := range []string{"4.6", "4-6", "4.7", "4-7", "4.8", "4-8", "4.9", "4-9"} {
		if strings.Contains(m, tag) {
			return true
		}
	}
	return false
}

// parseClaudeVersion extracts the major.minor version from a
// "claude-<family>-<major>.<minor>" (dot or dash) id without a regexp
// dependency. Returns ok=false if the id doesn't match that shape.
func parseClaudeVersion(m string) (major, minor int, ok bool) {
	const prefix = "claude-"
	if !strings.HasPrefix(m, prefix) {
		return 0, 0, false
	}
	rest := m[len(prefix):]
	for _, fam := range []string{"opus", "sonnet", "haiku"} {
		famPrefix := fam + "-"
		if !strings.HasPrefix(rest, famPrefix) {
			continue
		}
		ver := rest[len(famPrefix):]
		// Locate the major/minor separator ('.' or '-').
		sep := -1
		for i := 0; i < len(ver); i++ {
			if ver[i] == '.' || ver[i] == '-' {
				sep = i
				break
			}
		}
		if sep < 1 {
			return 0, 0, false
		}
		majStr := ver[:sep]
		// Minor is the run of digits after the separator (stop at next non-digit).
		rem := ver[sep+1:]
		end := 0
		for end < len(rem) && rem[end] >= '0' && rem[end] <= '9' {
			end++
		}
		if end == 0 {
			return 0, 0, false
		}
		minStr := rem[:end]
		maj, errMaj := strconv.Atoi(majStr)
		min, errMin := strconv.Atoi(minStr)
		if errMaj != nil || errMin != nil {
			return 0, 0, false
		}
		return maj, min, true
	}
	return 0, 0, false
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
	if callback.OnToolUse == nil {
		return
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
