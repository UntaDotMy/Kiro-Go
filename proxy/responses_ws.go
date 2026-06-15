package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// responsesWsUpgrader accepts WebSocket upgrades on /v1/responses for Codex
// CLI's experimental "responses_websockets" / "responses_websockets_v2"
// transport. Codex sends one request frame and expects the same SSE event
// shape we already emit in HTTP streaming, just delivered as text messages.
//
// Origin check is permissive because Codex doesn't set Origin and the
// admin-side API key (when configured) is the actual auth boundary. If you
// expose the proxy on the public internet, set up a reverse proxy that
// restricts WS origins. The default CheckOrigin enforces same-origin for
// browser clients (Origin header present must match Host) but allows
// non-browser clients like Codex CLI (no Origin header). Set the
// KIRO_WS_ALLOW_ANY_ORIGIN env var to "1" to revert to the permissive
// behaviour of older releases.
var responsesWsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     checkResponsesWsOrigin,
}

// checkResponsesWsOrigin enforces same-origin for browser-initiated WS
// upgrades while allowing CLI clients that don't set an Origin header.
func checkResponsesWsOrigin(r *http.Request) bool {
	if os.Getenv("KIRO_WS_ALLOW_ANY_ORIGIN") == "1" {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Non-browser client (Codex CLI doesn't set Origin). API-key auth is
		// the boundary for these.
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	// Match Origin host[:port] to Host. The browser sets Host to the proxy's
	// own address, so a same-page client succeeds and a cross-origin tab
	// fails.
	return strings.EqualFold(u.Host, r.Host)
}

// isWebSocketUpgrade returns true when the request includes the WebSocket
// upgrade headers, regardless of casing.
func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	connection := r.Header.Get("Connection")
	for _, part := range strings.Split(connection, ",") {
		if strings.EqualFold(strings.TrimSpace(part), "upgrade") {
			return true
		}
	}
	return false
}

// handleResponsesWebSocket upgrades an HTTP request to a WebSocket and runs
// the same Responses streaming pipeline as the SSE path, except every "event:
// <name>\ndata: <json>\n\n" is delivered as a single WebSocket text message
// containing the JSON envelope { "event": "<name>", "data": <json> }. Codex
// CLI's WS transport understands this format.
func (h *Handler) handleResponsesWebSocket(w http.ResponseWriter, r *http.Request) {
	// Validate API key from query string OR Authorization header (clients
	// don't always set headers on WS upgrade in browsers; Codex CLI does).
	if !h.validateApiKey(r) {
		w.WriteHeader(401)
		return
	}

	conn, err := responsesWsUpgrader.Upgrade(w, r, nil)
	if err == nil {
		conn.SetReadLimit(maxRequestBodyBytes)
	}
	if err != nil {
		logger.Warnf("[ResponsesWS] upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	// Read the initial request frame. Codex sends one JSON message containing
	// the full ResponsesRequest body.
	_, payload, err := conn.ReadMessage()
	if err != nil {
		logger.Warnf("[ResponsesWS] read initial frame: %v", err)
		return
	}

	var req ResponsesRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		_ = conn.WriteJSON(map[string]interface{}{
			"event": "error",
			"data": map[string]interface{}{
				"type":    "invalid_request_error",
				"message": "Invalid JSON: " + err.Error(),
			},
		})
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		req.Model = "claude-sonnet-4.5"
	}

	// Stateless passthrough: previous_response_id is unsupported (no server-side
	// store). Reject it explicitly instead of silently dropping context.
	if strings.TrimSpace(req.PreviousResponseID) != "" {
		_ = conn.WriteJSON(map[string]interface{}{
			"event": "error",
			"data": map[string]interface{}{
				"type":    "invalid_request_error",
				"message": "previous_response_id is not supported: this server is stateless and does not store prior responses. Resend the full conversation in 'input' each turn.",
			},
		})
		return
	}

	apiKeyID := matchedAPIKeyID(r)
	if apiKeyID != "" {
		if rejected, reason := config.CheckAPIKeyLimit(apiKeyID, req.Model); rejected {
			_ = conn.WriteJSON(map[string]interface{}{
				"event": "error",
				"data":  map[string]interface{}{"type": "rate_limit_error", "message": "API key limit reached: " + reason},
			})
			return
		}
	}

	claudeReq := ResponsesToClaudeRequest(&req)
	thinkingCfg := config.GetThinkingConfig()
	mappedModel, suffixThinking := ParseModelAndThinking(claudeReq.Model, thinkingCfg.Suffix)
	// Fold reasoning.effort into the thinking decision via the shared resolver
	// (minimal -> off, low/medium/high/max -> on, unset -> keep suffix default).
	effort := ""
	if req.Reasoning != nil {
		effort = req.Reasoning.Effort
	}
	thinking := resolveThinkingWithEffort(suffixThinking, effort)

	// Resolve the request's provider backend from the model string so a
	// non-Kiro model (e.g. "groq/..." or "cx/...") on the WS path selects the
	// right account and is translated correctly. Unprefixed -> "kiro", which
	// keeps the existing Kiro-only behavior byte-identical.
	reqBackend, upstreamModel := ParseModelBackend(req.Model)

	// poolModel: mapped Kiro id for Kiro, de-prefixed upstream id for a non-Kiro
	// backend (so a fetched /models catalog matches). See the Claude handler.
	poolModel := mappedModel
	if reqBackend != "kiro" {
		poolModel = upstreamModel
	}

	// Reserve an in-flight slot via the AIMD-aware picker so the Codex WS path
	// participates in the same per-account concurrency gate as every other path
	// (it previously used the non-reserving picker and bypassed the gate). No
	// failover on this path, so a single Acquire/Release pair is correct; the
	// slot is released when the handler returns. Scoped to the resolved backend.
	account, retryAfter, ok := h.pool.AcquireForBackendModelExcluding(reqBackend, poolModel, nil)
	if !ok {
		errMsg := "No available accounts"
		errType := "server_error"
		if retryAfter > 0 {
			errMsg = "All accounts are rate limited; retry after " + strconv.Itoa(retryAfterSeconds(retryAfter)) + "s"
			errType = "rate_limit_exceeded"
		}
		_ = conn.WriteJSON(map[string]interface{}{
			"event": "error",
			"data":  map[string]interface{}{"type": errType, "message": errMsg},
		})
		return
	}
	defer h.pool.Release(account.ID)
	if err := h.ensureValidToken(account); err != nil {
		_ = conn.WriteJSON(map[string]interface{}{
			"event": "error",
			"data":  map[string]interface{}{"type": "server_error", "message": safeUpstreamError("responses-ws token refresh", err)},
		})
		return
	}

	estimatedInputTokens := estimateClaudeRequestInputTokens(claudeReq)
	if estimatedInputTokens < 1 {
		estimatedInputTokens = 1
	}
	kiroPayload := ClaudeToKiro(claudeReq, thinking)

	// Forward graded reasoning.effort natively when the resolved model supports
	// it; no-op otherwise (thinking on/off already applied above).
	h.applyReasoningEffort(kiroPayload, effort)

	includeReasoning := true
	if req.Reasoning != nil && strings.EqualFold(strings.TrimSpace(req.Reasoning.Summary), "none") {
		includeReasoning = false
	}

	// Stream events over the WS connection by adapting the same logic the
	// SSE streaming handler uses. We reuse a minimal state machine that
	// emits the Codex envelope shape; this is intentionally a stripped-down
	// mirror of handleResponsesStream so future changes there don't have to
	// be replicated here field-for-field.
	respID := "resp_" + uuid.New().String()
	send := func(event string, data interface{}) {
		_ = conn.WriteJSON(map[string]interface{}{"event": event, "data": data})
	}
	send("response.created", map[string]interface{}{
		"type": "response.created",
		"response": map[string]interface{}{
			"id": respID, "object": "response", "status": "in_progress", "model": canonicalAnthropicModelID(req.Model),
		},
	})

	var (
		seq                int
		messageBuf         strings.Builder
		reasoningBuf       strings.Builder
		inputTokens        int
		outputTokens       int
		credits            float64
		upstreamStopReason string
	)
	nextSeq := func() int { seq++; return seq }

	// Route all upstream text through the shared thinkingTextProcessor so inline
	// <thinking>...</thinking> emitted as ordinary assistant text is parsed into
	// reasoning deltas instead of leaking into output_text.delta — matching the
	// SSE Responses path and the Claude/OpenAI stream paths.
	processor := newThinkingProcessor(thinking, func(text string, thinkingState int) {
		if thinkingState == 0 {
			if text == "" {
				return
			}
			messageBuf.WriteString(text)
			send("response.output_text.delta", map[string]interface{}{
				"type": "response.output_text.delta", "sequence_number": nextSeq(), "delta": text,
			})
			return
		}
		// thinkingState 1/2/3: thinking content -> reasoning deltas.
		if !includeReasoning || text == "" {
			return
		}
		reasoningBuf.WriteString(text)
		send("response.reasoning_summary_text.delta", map[string]interface{}{
			"type": "response.reasoning_summary_text.delta", "sequence_number": nextSeq(), "delta": text,
		})
	}, allowReasoningSource, allowTagSource)

	callback := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			processor.Process(text, isThinking)
		},
		OnToolUse: func(tu KiroToolUse) {
			processor.Finalize()
			argsStr, _ := json.Marshal(tu.Input)
			send("response.function_call_arguments.done", map[string]interface{}{
				"type": "response.function_call_arguments.done", "sequence_number": nextSeq(),
				"call_id": tu.ToolUseID, "name": tu.Name, "arguments": string(argsStr),
			})
		},
		OnComplete:   func(in, out int) { inputTokens, outputTokens = in, out },
		OnCredits:    func(c float64) { credits = c },
		OnStopReason: func(r string) { upstreamStopReason = r },
	}

	// Thread the originating request onto the context so a non-Kiro account
	// selected for this model is translated correctly (a Kiro account ignores it
	// and uses the prebuilt kiroPayload). ClientDialect is "responses" since the
	// inbound request funnels through ResponsesToClaudeRequest.
	baseNR := &NormalizedRequest{
		Model:         upstreamModel,
		ClientDialect: DialectResponses,
		Claude:        claudeReq,
		Thinking:      thinking,
		Stream:        true,
		Effort:        effort,
	}
	wsCtx := withNormalizedRequest(r.Context(), baseNR)

	if err := h.callProviderForKiro(wsCtx, account, kiroPayload, mappedModel, thinking, callback); err != nil {
		processor.Finalize()
		h.handleUpstreamError(err, account.ID, req.Model, apiKeyID, kiroPayload.ResolvedEffort)
		send("response.failed", map[string]interface{}{
			"type":            "response.failed",
			"sequence_number": nextSeq(),
			"response":        map[string]interface{}{"id": respID, "status": "failed", "error": map[string]interface{}{"type": "server_error", "message": safeStreamErrorMessage(err)}},
		})
		return
	}
	// Flush any buffered thinking/text before computing final usage + closing.
	processor.Finalize()

	if inputTokens <= 0 {
		inputTokens = estimatedInputTokens
	}
	if outputTokens <= 0 {
		outputTokens = estimateClaudeOutputTokens(messageBuf.String(), reasoningBuf.String(), nil)
	}
	// Pool counters before recordSuccess so the realtime dashboard push
	// reflects this request's per-account credits/tokens (see handler.go).
	h.pool.RecordSuccess(account.ID)
	h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
	h.recordSuccess(req.Model, apiKeyID, kiroPayload.ResolvedEffort, inputTokens, outputTokens, credits, 0)
	h.triggerAccountRefresh(account.ID)
	if apiKeyID != "" {
		_, _ = config.ConsumeAPIKey(apiKeyID, inputTokens+outputTokens, credits, req.Model)
	}

	completedStatus := "completed"
	completedPayload := map[string]interface{}{
		"id": respID, "status": completedStatus, "model": canonicalAnthropicModelID(req.Model),
		"usage": map[string]interface{}{"input_tokens": inputTokens, "output_tokens": outputTokens, "total_tokens": inputTokens + outputTokens},
	}
	if upstreamStopReason == "max_tokens" {
		completedPayload["status"] = "incomplete"
		completedPayload["incomplete_details"] = map[string]string{"reason": "max_output_tokens"}
	}

	send("response.completed", map[string]interface{}{
		"type":            "response.completed",
		"sequence_number": nextSeq(),
		"response":        completedPayload,
	})
}
