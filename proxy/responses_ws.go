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
	if err == nil { conn.SetReadLimit(maxRequestBodyBytes) }
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

	apiKeyID := matchedAPIKeyID(r)
	if apiKeyID != "" {
		if rejected, reason := config.CheckAPIKeyLimit(apiKeyID, req.Model); rejected {
			_ = conn.WriteJSON(map[string]interface{}{
				"event": "error",
				"data": map[string]interface{}{"type": "rate_limit_error", "message": "API key limit reached: " + reason},
			})
			return
		}
	}

	claudeReq := ResponsesToClaudeRequest(&req)
	thinkingCfg := config.GetThinkingConfig()
	mappedModel, suffixThinking := ParseModelAndThinking(claudeReq.Model, thinkingCfg.Suffix)
	thinking := suffixThinking || (req.Reasoning != nil && req.Reasoning.Effort != "" && !strings.EqualFold(req.Reasoning.Effort, "minimal"))

	account, retryAfter, ok := h.pool.GetNextForModelInGroup(mappedModel, apiKeyGroup(r))
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
	if err := h.ensureValidToken(account); err != nil {
		_ = conn.WriteJSON(map[string]interface{}{
			"event": "error",
			"data":  map[string]interface{}{"type": "server_error", "message": "Token refresh failed: " + err.Error()},
		})
		return
	}

	estimatedInputTokens := estimateClaudeRequestInputTokens(claudeReq)
	if estimatedInputTokens < 1 {
		estimatedInputTokens = 1
	}
	kiroPayload := ClaudeToKiro(claudeReq, thinking)

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

	callback := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				if !includeReasoning {
					return
				}
				reasoningBuf.WriteString(text)
				send("response.reasoning_summary_text.delta", map[string]interface{}{
					"type": "response.reasoning_summary_text.delta", "sequence_number": nextSeq(), "delta": text,
				})
				return
			}
			messageBuf.WriteString(text)
			send("response.output_text.delta", map[string]interface{}{
				"type": "response.output_text.delta", "sequence_number": nextSeq(), "delta": text,
			})
		},
		OnToolUse: func(tu KiroToolUse) {
			argsStr, _ := json.Marshal(tu.Input)
			send("response.function_call_arguments.done", map[string]interface{}{
				"type": "response.function_call_arguments.done", "sequence_number": nextSeq(),
				"call_id": tu.ToolUseID, "name": tu.Name, "arguments": string(argsStr),
			})
		},
		OnComplete:   func(in, out int) { inputTokens, outputTokens = in, out },
		OnCredits:    func(c float64) { credits = c },
		OnError:      func(err error) { h.recordPoolError(account.ID, err) },
		OnStopReason: func(r string) { upstreamStopReason = r },
	}

	if err := CallKiroAPI(account, kiroPayload, callback); err != nil {
		h.handleUpstreamError(err, account.ID, req.Model, apiKeyID)
		send("response.failed", map[string]interface{}{
			"type":            "response.failed",
			"sequence_number": nextSeq(),
			"response":        map[string]interface{}{"id": respID, "status": "failed", "error": map[string]interface{}{"type": "server_error", "message": err.Error()}},
		})
		return
	}

	if inputTokens <= 0 {
		inputTokens = estimatedInputTokens
	}
	if outputTokens <= 0 {
		outputTokens = estimateClaudeOutputTokens(messageBuf.String(), reasoningBuf.String(), nil)
	}
	h.recordSuccess(req.Model, apiKeyID, inputTokens, outputTokens, credits)
	h.pool.RecordSuccess(account.ID)
	h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
	h.triggerAccountRefresh(account.ID)
	if apiKeyID != "" { _, _ = config.ConsumeAPIKey(apiKeyID, inputTokens+outputTokens, credits, req.Model) }

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
