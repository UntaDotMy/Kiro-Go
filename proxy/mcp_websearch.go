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
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// Native Kiro web search via the upstream MCP endpoint.
//
// Kiro's AWS Q backend exposes a server-side `web_search` tool over a JSON-RPC
// "Model Context Protocol" endpoint at https://q.<region>.amazonaws.com/mcp,
// reachable with the SAME bearer token used for normal generateAssistantResponse
// calls. Anthropic's hosted web_search tool (type "web_search_20250305") and
// Claude Code's WebSearch cannot be forwarded to the plain chat endpoint —
// CodeWhisperer has no concept of server-side tools — so we EMULATE them here:
// we call this MCP endpoint ourselves and reshape the result into the native
// Anthropic web_search_tool_result form the client expects.
//
// This mirrors how jwadow/kiro-gateway and aliom-v/KiroGate implement web
// search. There is NO external search provider and NO API key: the search runs
// on Amazon's side and is billed against the same Kiro account.
//
// IMPORTANT — availability is not guaranteed. The /mcp endpoint is opaque and
// may not be enabled on every account tier or region. Every caller MUST treat
// a non-nil error as "web search unavailable" and fall back to the prior
// behavior (drop the tool cleanly). This feature is OFF by default
// (config WebSearchEnabled) so a stable deployment is unaffected unless the
// operator opts in.
// ============================================================================

// mcpRequestTimeout bounds a single /mcp round-trip. Web search is synchronous
// from the model's perspective, so we cap it tighter than a streaming chat.
const mcpRequestTimeout = 25 * time.Second

// maxWebSearchResults caps how many results we surface to the model/client.
// Anthropic's hosted tool defaults to 5; we match that to keep token cost and
// latency predictable.
const maxWebSearchResults = 5

// WebSearchResult is one normalized search hit, provider-agnostic.
type WebSearchResult struct {
	Title         string `json:"title"`
	URL           string `json:"url"`
	Snippet       string `json:"snippet"`
	PublishedDate string `json:"publishedDate,omitempty"`
}

// UnmarshalJSON decodes a single search hit leniently. Kiro's /mcp web_search
// payload is loosely typed: in practice `publishedDate` arrives sometimes as a
// string ("2026-01-01") and sometimes as a NUMBER (a year like 2026, or an
// epoch timestamp). Go's default decoder rejects the whole payload on that one
// type mismatch with "cannot unmarshal number into Go struct field
// ...publishedDate of type string" — which the agentic loop then reports to the
// model as "web search is currently unavailable", so a perfectly good search
// looks broken. We decode the scalar fields through json.RawMessage and coerce
// each to a string so a string-or-number value never kills the parse.
func (r *WebSearchResult) UnmarshalJSON(data []byte) error {
	var aux struct {
		Title         json.RawMessage `json:"title"`
		URL           json.RawMessage `json:"url"`
		Snippet       json.RawMessage `json:"snippet"`
		PublishedDate json.RawMessage `json:"publishedDate"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	r.Title = jsonScalarToString(aux.Title)
	r.URL = jsonScalarToString(aux.URL)
	r.Snippet = jsonScalarToString(aux.Snippet)
	r.PublishedDate = jsonScalarToString(aux.PublishedDate)
	return nil
}

// jsonScalarToString coerces a JSON scalar (string, number, bool, or null) to a
// string. Strings are unquoted/unescaped; integral numbers render without a
// decimal point or scientific notation (so an epoch like 1733000000 stays
// "1733000000", not "1.733e+09"); null/empty becomes "". Non-scalar inputs
// (object/array) fall back to the raw literal so we never panic.
func jsonScalarToString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return ""
	}
	var num json.Number
	if err := json.Unmarshal(raw, &num); err == nil {
		if i, err := num.Int64(); err == nil {
			return strconv.FormatInt(i, 10)
		}
		return num.String()
	}
	return strings.Trim(string(raw), `"`)
}

// mcpQHostForRegion returns the JSON-RPC MCP base for a region. Unlike the REST
// base (which uses runtime.<region>.kiro.dev outside us-east-1), the MCP tool
// endpoint is served from the q.<region>.amazonaws.com streaming host in every
// region.
func mcpQHostForRegion(region string) string {
	if region == "" {
		region = "us-east-1"
	}
	return fmt.Sprintf("https://q.%s.amazonaws.com", region)
}

// jsonRPCRequest is the minimal JSON-RPC 2.0 envelope for an MCP tools/call.
type jsonRPCRequest struct {
	ID      string                 `json:"id"`
	JSONRPC string                 `json:"jsonrpc"`
	Method  string                 `json:"method"`
	Params  map[string]interface{} `json:"params"`
}

// jsonRPCResponse is the MCP tools/call reply. The actual search payload is a
// JSON STRING nested in result.content[].text (requires a second parse).
type jsonRPCResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Result  *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// performKiroWebSearch executes a single web_search against Kiro's native MCP
// endpoint for the given account and returns normalized results. A non-nil
// error means the caller should fall back to dropping the web_search tool.
func performKiroWebSearch(ctx context.Context, account *config.Account, query string) ([]WebSearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("web search: empty query")
	}
	if account == nil || account.AccessToken == "" {
		return nil, fmt.Errorf("web search: no authenticated account")
	}

	region := config.GetKiroAPIRegion()
	endpoint := mcpQHostForRegion(region) + "/mcp"

	rpc := jsonRPCRequest{
		ID:      uuid.New().String(),
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params: map[string]interface{}{
			"name": "web_search",
			"arguments": map[string]interface{}{
				"query": query,
			},
		},
	}
	body, err := json.Marshal(rpc)
	if err != nil {
		return nil, fmt.Errorf("web search: marshal request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, mcpRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("web search: build request: %w", err)
	}

	// Reuse the standard Kiro header set (bearer token, UA, host), then set the
	// MCP-specific bits. We send optout=false to match the verified-working
	// reference implementations for the MCP path.
	host := ""
	if req.URL != nil {
		host = req.URL.Host
	}
	applyKiroBaseHeaders(req, account, buildStreamingHeaderValues(account, host))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-amzn-codewhisperer-optout", "false")

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return nil, fmt.Errorf("web search: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("web search: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("web search: read response: %w", err)
	}

	return parseMCPWebSearchResponse(raw)
}

// parseMCPWebSearchResponse decodes the JSON-RPC envelope and the nested
// JSON-string search payload into normalized results. Split out from the HTTP
// call so it is unit-testable without a live endpoint.
func parseMCPWebSearchResponse(raw []byte) ([]WebSearchResult, error) {
	var rpcResp jsonRPCResponse
	if err := json.Unmarshal(raw, &rpcResp); err != nil {
		return nil, fmt.Errorf("web search: decode JSON-RPC: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("web search: MCP error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if rpcResp.Result == nil || len(rpcResp.Result.Content) == 0 {
		return nil, fmt.Errorf("web search: empty MCP result")
	}
	if rpcResp.Result.IsError {
		return nil, fmt.Errorf("web search: MCP reported tool error: %s", rpcResp.Result.Content[0].Text)
	}

	// The first text part carries a JSON string of the actual results.
	payloadText := ""
	for _, c := range rpcResp.Result.Content {
		if c.Text != "" {
			payloadText = c.Text
			break
		}
	}
	if payloadText == "" {
		return nil, fmt.Errorf("web search: no text content in MCP result")
	}

	var inner struct {
		Results []WebSearchResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(payloadText), &inner); err != nil {
		// Some variants may return a bare array rather than {"results":[...]}.
		var bare []WebSearchResult
		if err2 := json.Unmarshal([]byte(payloadText), &bare); err2 != nil {
			return nil, fmt.Errorf("web search: decode results payload: %w", err)
		}
		inner.Results = bare
	}

	if len(inner.Results) > maxWebSearchResults {
		inner.Results = inner.Results[:maxWebSearchResults]
	}
	if len(inner.Results) == 0 {
		// A successful search that found nothing is not an error — return an
		// empty slice so the caller emits an empty result block.
		return []WebSearchResult{}, nil
	}
	return inner.Results, nil
}

// logWebSearch emits a single INFO line for observability without leaking the
// full query at higher log levels.
func logWebSearch(query string, n int, err error) {
	if err != nil {
		logger.Warnf("[WebSearch] query failed (%d chars): %v", len(query), err)
		return
	}
	logger.Infof("[WebSearch] returned %d result(s) for query (%d chars)", n, len(query))
}
