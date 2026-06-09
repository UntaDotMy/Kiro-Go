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
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// codexProvider serves ChatGPT/Codex accounts by calling the ChatGPT backend
// Responses API (https://chatgpt.com/backend-api/codex/responses) with the
// account's OAuth bearer token + chatgpt-account-id header and a synthesized
// Codex-CLI fingerprint. The upstream stream is the OpenAI Responses SSE format,
// which we parse into the shared KiroStreamCallback so all client renderers work.
//
// Ported from codex-lb (app/core/clients/proxy.py stream_codex_responses +
// _build_upstream_headers) and 9router (open-sse/executors/codex.js).
type codexProvider struct{}

func init() {
	RegisterProvider(codexProvider{})
}

func (codexProvider) Name() string { return "codex" }

const (
	codexUpstreamURL = "https://chatgpt.com/backend-api/codex/responses"
	codexUsageURL    = "https://chatgpt.com/backend-api/wham/usage"
	// codexUserAgent mirrors the Codex CLI fingerprint (originator codex_cli_rs).
	// Kept in sync with 9router's PROVIDERS.codex headers.
	codexUserAgent = "codex_cli_rs/0.136.0"
)

// RefreshToken renews the account's OpenAI access token. It uses the age-based
// refresh model: codex-lb refreshes by token age, but Kiro-Go's ensureValidToken
// gate is exp-based, so the handler sets ExpiresAt from ExpiresIn when it
// persists. Here we just perform the refresh and return the new set + the
// re-derived identity in Extra.
func (codexProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	if strings.TrimSpace(acct.RefreshToken) == "" {
		// No refresh token (e.g. a pasted access_token import) — nothing to do.
		// Report a far-future expiry so the exp-based gate doesn't spin trying to
		// refresh a credential we can't refresh.
		return TokenSet{AccessToken: acct.AccessToken, ExpiresAt: 0}, nil
	}
	t, err := auth.RefreshCodexToken(acct.RefreshToken)
	if err != nil {
		return TokenSet{}, err
	}
	extra := map[string]string{}
	if t.IDToken != "" {
		extra["idToken"] = t.IDToken
	}
	if t.AccountID != "" {
		extra["chatgptAccountId"] = t.AccountID
	}
	if t.PlanType != "" {
		extra["planType"] = t.PlanType
	}
	var expiresAt int64
	if t.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + int64(t.ExpiresIn)
	}
	return TokenSet{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		ExpiresAt:    expiresAt,
		Extra:        extra,
	}, nil
}

// ListModels returns the Codex model catalog filtered (loosely) by plan. We keep
// a small static set the ChatGPT Codex backend accepts; an empty per-account list
// would also work (the pool treats it as "serves anything"), but a concrete list
// lets the dashboard show real ids and lets model routing filter sensibly.
func (codexProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	ids := []string{"gpt-5-codex", "gpt-5", "gpt-5-mini", "o4-mini", "codex-mini-latest"}
	out := make([]ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, ModelInfo{ModelId: id})
	}
	return out, nil
}

// Call builds a Responses request from nr (originating Claude or OpenAI request),
// POSTs to the Codex backend, and streams the Responses SSE into cb.
func (codexProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	if ctx == nil {
		ctx = context.Background()
	}
	upstreamModel := strings.TrimSpace(nr.Model)
	if upstreamModel == "" {
		upstreamModel = "gpt-5-codex"
	}

	body, err := buildCodexResponsesBody(nr, upstreamModel)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", codexUpstreamURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	applyCodexHeaders(req, acct)

	resp, err := GetClientForProxy(ResolveAccountProxyURL(acct)).Do(req)
	if err != nil {
		return classifyStreamError(err)
	}

	if resp.StatusCode == 429 {
		retryAfter := parseRetryAfter(resp.Header)
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		logger.Infof("[codex] throttled (429, retry-after=%s) acct=%s", retryAfter, acct.ID)
		return &QuotaError{Endpoints: []string{"codex"}, RetryAfter: retryAfter}
	}
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		// A 401 here usually means the access token lapsed — return it as a plain
		// (non-quota) error so the failover/refresh path can react.
		return fmt.Errorf("HTTP %d from codex: %s", resp.StatusCode, string(errBody))
	}

	streamErr := func() error {
		defer resp.Body.Close()
		rdr := newIdleTimeoutReader(resp.Body, streamIdleTimeout, func() {})
		return parseCodexResponsesSSE(rdr, cb)
	}()
	return classifyStreamError(streamErr)
}

// buildCodexResponsesBody constructs the Codex Responses request body from the
// originating request. We force store=false, stream=true, and include the
// encrypted reasoning content (matching Codex CLI). Input is built from the
// originating Claude/OpenAI messages.
func buildCodexResponsesBody(nr *NormalizedRequest, upstreamModel string) ([]byte, error) {
	instructions, input := codexInputFromRequest(nr)

	body := map[string]interface{}{
		"model":        upstreamModel,
		"instructions": instructions,
		"input":        input,
		"stream":       true,
		"store":        false,
		"include":      []string{"reasoning.encrypted_content"},
	}
	// Reasoning effort: map from the resolved effort if present.
	if eff := normalizeCodexEffort(nr.Effort); eff != "" {
		body["reasoning"] = map[string]interface{}{"effort": eff, "summary": "auto"}
	}
	// Tools, if any, in the flat Responses tool shape.
	if tools := codexToolsFromRequest(nr); len(tools) > 0 {
		body["tools"] = tools
	}
	return json.Marshal(body)
}

// codexInputFromRequest builds the Responses (instructions, input[]) pair from
// the originating request. Claude/OpenAI system prompt -> instructions; messages
// -> input items in the Responses message shape.
func codexInputFromRequest(nr *NormalizedRequest) (string, []map[string]interface{}) {
	var instructions string
	var input []map[string]interface{}

	addText := func(role, text string) {
		if text == "" {
			return
		}
		partType := "input_text"
		if role == "assistant" {
			partType = "output_text"
		}
		input = append(input, map[string]interface{}{
			"type":    "message",
			"role":    role,
			"content": []map[string]interface{}{{"type": partType, "text": text}},
		})
	}

	switch {
	case nr.Claude != nil:
		instructions = extractClaudeSystemString(nr.Claude.System)
		for _, m := range nr.Claude.Messages {
			role := m.Role
			text := claudeMessageText(m.Content)
			addText(role, text)
		}
	case nr.OpenAI != nil:
		var sys strings.Builder
		for _, m := range nr.OpenAI.Messages {
			if m.Role == "system" {
				if s := extractOpenAIMessageText(m.Content); s != "" {
					if sys.Len() > 0 {
						sys.WriteString("\n")
					}
					sys.WriteString(s)
				}
				continue
			}
			addText(m.Role, extractOpenAIMessageText(m.Content))
		}
		instructions = sys.String()
	}
	if len(input) == 0 {
		// The Responses API requires a non-empty input; seed a minimal user turn.
		addText("user", " ")
	}
	return instructions, input
}

// claudeMessageText flattens a Claude message's content to plain text (text +
// tool_result text). Tool-use round-tripping through Codex is best-effort; the
// common interactive case (text turns) is exact.
func claudeMessageText(content interface{}) string {
	if s, ok := content.(string); ok {
		return s
	}
	blocks, ok := content.([]interface{})
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		block, ok := b.(map[string]interface{})
		if !ok {
			continue
		}
		switch block["type"] {
		case "text", "input_text":
			if t, ok := block["text"].(string); ok {
				sb.WriteString(t)
			}
		case "tool_result":
			sb.WriteString(extractToolResultContent(block["content"]))
		}
	}
	return sb.String()
}

// codexToolsFromRequest converts tools from the originating request into the flat
// Responses tool shape ({type:"function", name, description, parameters}).
func codexToolsFromRequest(nr *NormalizedRequest) []map[string]interface{} {
	var out []map[string]interface{}
	switch {
	case nr.Claude != nil:
		for _, t := range nr.Claude.Tools {
			if strings.TrimSpace(t.Type) != "" {
				continue
			}
			out = append(out, map[string]interface{}{
				"type": "function", "name": t.Name, "description": t.Description, "parameters": t.InputSchema,
			})
		}
	case nr.OpenAI != nil:
		for _, t := range nr.OpenAI.Tools {
			out = append(out, map[string]interface{}{
				"type": "function", "name": t.Function.Name, "description": t.Function.Description, "parameters": t.Function.Parameters,
			})
		}
	}
	return out
}

// normalizeCodexEffort maps a resolved effort string to a Codex reasoning effort.
// "minimal"/"" -> "" (omit); low/medium/high pass through; xhigh/max -> high.
func normalizeCodexEffort(eff string) string {
	switch strings.ToLower(strings.TrimSpace(eff)) {
	case "low":
		return "low"
	case "medium":
		return "medium"
	case "high", "xhigh", "max":
		return "high"
	default:
		return ""
	}
}

// applyCodexHeaders sets the bearer token, chatgpt-account-id, and the Codex CLI
// fingerprint headers. Stdlib TLS first (per the plan); uTLS only if the WAF
// rejects this.
func applyCodexHeaders(req *http.Request, acct *config.Account) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(acct.AccessToken))
	if acct.CodexAccountID != "" {
		req.Header.Set("chatgpt-account-id", acct.CodexAccountID)
	}
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("originator", codexOriginatorHeader)
	req.Header.Set("User-Agent", codexUserAgent)
	req.Header.Set("session_id", uuid.New().String())
}

const codexOriginatorHeader = "codex_cli_rs"

// parseCodexResponsesSSE parses the OpenAI Responses SSE stream from the Codex
// backend into cb. It handles output_text deltas, reasoning summary deltas
// (as thinking text), function_call items (emitted whole on .done), the final
// usage on response.completed, and response.failed errors.
func parseCodexResponsesSSE(r io.Reader, cb *KiroStreamCallback) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	// Accumulate function_call arguments by item id across *.delta events.
	type fnAcc struct {
		callID, name string
		args         strings.Builder
	}
	fns := map[string]*fnAcc{}
	var stopReason string

	var curEvent string
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "event:") {
			curEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			continue
		}
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev map[string]interface{}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		// Prefer the explicit "type" field on the payload; fall back to the SSE
		// event: line.
		typ, _ := ev["type"].(string)
		if typ == "" {
			typ = curEvent
		}

		switch typ {
		case "response.output_text.delta":
			if d, ok := ev["delta"].(string); ok && cb.OnText != nil {
				cb.OnText(d, false)
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if d, ok := ev["delta"].(string); ok && cb.OnText != nil {
				cb.OnText(d, true)
			}
		case "response.output_item.added":
			if item, ok := ev["item"].(map[string]interface{}); ok {
				if item["type"] == "function_call" {
					id, _ := item["id"].(string)
					callID, _ := item["call_id"].(string)
					name, _ := item["name"].(string)
					fa := &fnAcc{callID: callID, name: name}
					if args, ok := item["arguments"].(string); ok {
						fa.args.WriteString(args)
					}
					fns[id] = fa
				}
			}
		case "response.function_call_arguments.delta":
			id, _ := ev["item_id"].(string)
			if fa := fns[id]; fa != nil {
				if d, ok := ev["delta"].(string); ok {
					fa.args.WriteString(d)
				}
			}
		case "response.output_item.done":
			if item, ok := ev["item"].(map[string]interface{}); ok && item["type"] == "function_call" {
				id, _ := item["id"].(string)
				fa := fns[id]
				name, _ := item["name"].(string)
				callID, _ := item["call_id"].(string)
				argStr := ""
				if fa != nil {
					argStr = fa.args.String()
					if name == "" {
						name = fa.name
					}
					if callID == "" {
						callID = fa.callID
					}
				}
				if s, ok := item["arguments"].(string); ok && s != "" {
					argStr = s // the done event carries the full arguments
				}
				emitCodexToolUse(cb, callID, name, argStr)
				delete(fns, id)
				stopReason = "tool_use"
			}
		case "response.completed":
			if respObj, ok := ev["response"].(map[string]interface{}); ok {
				if usage, ok := respObj["usage"].(map[string]interface{}); ok && cb.OnComplete != nil {
					in := intFromAny(usage["input_tokens"])
					out := intFromAny(usage["output_tokens"])
					cb.OnComplete(in, out)
				}
			}
			if stopReason == "" {
				stopReason = "end_turn"
			}
		case "response.failed", "error":
			msg := "codex upstream error"
			if respObj, ok := ev["response"].(map[string]interface{}); ok {
				if e, ok := respObj["error"].(map[string]interface{}); ok {
					if m, ok := e["message"].(string); ok {
						msg = m
					}
				}
			}
			// A capacity/quota refusal must surface as a QuotaError so the pool
			// cools this account and failover tries the next Codex account, rather
			// than immediately retrying a saturated one. Other failures are plain
			// errors (terminal for the request unless retryable upstream).
			lower := strings.ToLower(msg)
			if strings.Contains(lower, "usage_limit") || strings.Contains(lower, "rate_limit") ||
				strings.Contains(lower, "capacity") || strings.Contains(lower, "quota") {
				return &QuotaError{Endpoints: []string{"codex"}}
			}
			return fmt.Errorf("%s", msg)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if stopReason != "" && cb.OnStopReason != nil {
		cb.OnStopReason(stopReason)
	}
	return nil
}

func emitCodexToolUse(cb *KiroStreamCallback, callID, name, argStr string) {
	if cb.OnToolUse == nil || name == "" {
		return
	}
	var input map[string]interface{}
	argStr = strings.TrimSpace(argStr)
	if argStr == "" {
		argStr = "{}"
	}
	_ = json.Unmarshal([]byte(argStr), &input)
	if input == nil {
		input = map[string]interface{}{}
	}
	if callID == "" {
		callID = "call_" + uuid.New().String()
	}
	cb.OnToolUse(KiroToolUse{ToolUseID: callID, Name: name, Input: input})
}
