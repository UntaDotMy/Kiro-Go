package proxy

import (
	"context"
	"encoding/json"
	"kiro-go/config"
	"net/http"

	"github.com/google/uuid"
)

// ============================================================================
// Web-search agentic loop.
//
// This is the piece that makes Anthropic's hosted web_search actually WORK
// through Kiro (which has no server-side tools). The flow, per client request:
//
//   1. web_search is exposed to the model as a callable function tool
//      (convertClaudeTools → webSearchToolSpec) when the feature is on.
//   2. We run the conversation against Kiro. If the model emits a web_search
//      tool_use, we execute the search ourselves via Kiro's native /mcp
//      endpoint (performKiroWebSearch), feed the results back as a tool_result,
//      and run Kiro again — looping until the model answers (or a real client
//      tool is requested, or a round cap is hit).
//   3. The final answer is emitted to the client with native
//      server_tool_use + web_search_tool_result blocks spliced in, so Claude
//      Code renders real citations. Clients that ignore those blocks still get
//      the grounded text answer.
//
// ISOLATION: this path runs ONLY when web search is enabled AND the request
// actually carries a web_search tool. Every other request takes the original,
// unchanged handler path. Internally it uses buffered (non-streaming) Kiro
// rounds and replays the result as SSE when the client asked to stream — this
// keeps the delicate live-streaming + failover machinery untouched.
//
// SAFETY: if the /mcp search fails (endpoint unavailable on this tier/region,
// auth, etc.) the loop degrades gracefully — it feeds an "unavailable" note
// back to the model and lets it answer from training, so a request NEVER
// breaks because web search is missing.
// ============================================================================

// maxWebSearchRounds bounds how many search round-trips a single request can
// trigger, so a model that keeps calling web_search can't loop forever.
const maxWebSearchRounds = 4

// kiroRoundResult is what one collected (non-streamed) Kiro round produced.
type kiroRoundResult struct {
	content      string
	thinking     string
	toolUses     []KiroToolUse
	inputTokens  int
	outputTokens int
	credits      float64
	stopReason   string
}

// runKiroCollect runs a single Kiro round against the pool with full failover,
// collecting the assistant output instead of writing it to the client. Returns
// the collected result, or an error if every account attempt failed.
//
// ACCOUNTING NOTE: this runs per ROUND of the web-search agentic loop (up to
// maxWebSearchRounds times for one client request). It records ONLY the
// per-account pool stats (RecordSuccess/UpdateStats), which are legitimately
// per-upstream-call. It deliberately does NOT call recordSuccess (global
// request/token/credit counters + SQLite) or ConsumeAPIKey (per-key quota
// debit) — those would over-count a single client request by up to N×. The
// caller (handleClaudeWebSearch) aggregates across rounds and debits exactly
// once. See the loop in handleClaudeWebSearch.
func (h *Handler) runKiroCollect(model, apiKeyID string, payload *KiroPayload) (*kiroRoundResult, error) {
	out := &kiroRoundResult{}
	var realInputTokens int

	worker := func(account *config.Account) (bool, error) {
		// Reset per attempt so a failed attempt's partial output never leaks
		// into the next account's result.
		*out = kiroRoundResult{}
		realInputTokens = 0
		payload.ProfileArn = ""

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					out.thinking += text
				} else {
					out.content += text
				}
			},
			OnToolUse:  func(tu KiroToolUse) { out.toolUses = append(out.toolUses, tu) },
			OnComplete: func(inTok, outTok int) { out.inputTokens = inTok; out.outputTokens = outTok },
			OnCredits:  func(c float64) { out.credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(clampPercent(pct) * float64(h.contextWindowForModel(model)) / 100.0)
			},
			OnStopReason: func(r string) { out.stopReason = r },
		}

		if err := CallKiroAPI(account, payload, callback); err != nil {
			h.recordAttemptError(err, account.ID)
			return false, err
		}

		out.inputTokens = resolveInputTokens(out.inputTokens, realInputTokens, 0)
		// Per-ACCOUNT pool bookkeeping only — this round is a real upstream call
		// against this account, so its pool counters and cooldown reset are
		// correct here. Global counters + per-key quota are aggregated once by
		// the caller (see the accounting note above).
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, out.inputTokens+out.outputTokens, out.credits)
		h.triggerAccountRefresh(account.ID)
		// Collected, not committed to the client — return (false, nil) so the
		// dispatcher treats it as success without expecting a written response.
		return false, nil
	}

	// countGlobalFailure=false: the agentic loop (handleClaudeWebSearch /
	// handleClaudeToolSearch) records exactly one global success or failure for
	// the whole client request after all rounds finish, so a per-round terminal
	// failure here must NOT also bump the global counter (that double-counted a
	// request that succeeded one round then failed a later one).
	_, _, err := h.runWithFailoverCounted(model, apiKeyID, payload.ResolvedEffort, worker, false)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// handleClaudeWebSearch orchestrates the web-search agentic loop and writes the
// final response. Precondition: req carries a web_search tool and the feature
// is enabled (verified by the caller).
func (h *Handler) handleClaudeWebSearch(w http.ResponseWriter, req *ClaudeRequest, model, apiKeyID string, thinking bool) {
	// Working copy of the conversation we grow across search rounds.
	messages := make([]ClaudeMessage, len(req.Messages))
	copy(messages, req.Messages)

	// Accumulated native citation blocks (server_tool_use + web_search_tool_result)
	// to splice into the final assistant message.
	var citationBlocks []map[string]interface{}

	var totalInput, totalOutput int
	var totalCredits float64
	var finalContent, finalThinking, finalStop string
	var clientToolUses []KiroToolUse
	roundsRun := 0

	for round := 0; round < maxWebSearchRounds; round++ {
		// Re-check the per-key limit before every round. Round 0 was already
		// gated by enforceAPIKeyLimit upstream, but rounds 1..N-1 are extra
		// upstream calls the model triggered by asking to search again — without
		// this a single request could spend up to maxWebSearchRounds× the key's
		// periodic/lifetime quota before any 429. We stop the loop (rather than
		// erroring) once a limit is crossed, so the user still gets whatever the
		// model produced so far; the debit for what already ran is applied below.
		if round > 0 && apiKeyID != "" {
			if rejected, _ := config.CheckAPIKeyLimit(apiKeyID, model); rejected {
				break
			}
		}

		roundReq := *req
		roundReq.Messages = messages
		payload := ClaudeToKiro(&roundReq, thinking)
		// Forward native reasoning effort (output_config.effort) per round, the
		// same as the non-web-search Claude path, so an effort level set by the
		// client isn't silently dropped on web-search requests.
		h.applyReasoningEffort(payload, claudeRequestEffort(req))

		res, err := h.runKiroCollect(model, apiKeyID, payload)
		if err != nil {
			// If at least one round already succeeded, surface what we have
			// rather than discarding it; the partial usage is still debited
			// below. Only a round-0 failure (nothing produced) is a hard error.
			if roundsRun == 0 {
				// runKiroCollect suppresses the global failure count (this loop
				// owns once-per-request accounting), so record the single failure
				// here for a total round-0 failure.
				h.recordFailure(model, apiKeyID, payload.ResolvedEffort)
				h.sendClaudeError(w, 502, "api_error", safeUpstreamError("web-search upstream call", err))
				return
			}
			break
		}
		roundsRun++
		totalInput += res.inputTokens
		totalOutput += res.outputTokens
		totalCredits += res.credits
		finalContent = res.content
		finalThinking = res.thinking
		finalStop = res.stopReason

		// Split the model's tool calls into web_search vs. real client tools.
		var searchCalls, otherCalls []KiroToolUse
		for _, tu := range res.toolUses {
			if tu.Name == webSearchToolName {
				searchCalls = append(searchCalls, tu)
			} else {
				otherCalls = append(otherCalls, tu)
			}
		}

		// No web search requested this round → we're done. Return the answer
		// (and any real client tool calls, which the client will execute).
		if len(searchCalls) == 0 {
			clientToolUses = otherCalls
			break
		}

		// Execute each requested search and feed results back to the model.
		// Append the assistant turn (carrying the tool_use) then a user turn
		// (carrying the tool_result) so the next round is grounded.
		assistantBlocks := buildAssistantToolUseBlocks(res.content, searchCalls)
		messages = append(messages, ClaudeMessage{Role: "assistant", Content: assistantBlocks})

		var resultBlocks []interface{}
		for _, call := range searchCalls {
			query := extractWebSearchQuery(call.Input)
			results, searchErr := performKiroWebSearch(context.Background(), h.firstUsableAccount(), query)
			logWebSearch(query, len(results), searchErr)

			feedback := formatWebSearchForModel(query, results)
			if searchErr != nil {
				feedback = "Web search is currently unavailable. Answer from your existing knowledge and note that you could not search."
			} else {
				// Record native citation blocks for the final response.
				citationBlocks = append(citationBlocks, buildWebSearchResultBlocks(call.ToolUseID, query, results)...)
			}
			resultBlocks = append(resultBlocks, map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": call.ToolUseID,
				"content":     feedback,
			})
		}
		messages = append(messages, ClaudeMessage{Role: "user", Content: resultBlocks})
		// loop to next round
	}

	// Account for the WHOLE request exactly ONCE, with the summed totals across
	// every round. runKiroCollect deliberately skips global counters + per-key
	// debit (see its accounting note) so we don't inflate them N× for a single
	// client request. Only record if at least one round actually ran.
	if roundsRun > 0 {
		// The web-search loop runs on the Claude /v1/messages path, which carries
		// no graded reasoning effort (effort is expressed via thinking, not a
		// level), so the effort bucket is empty — matching the non-web-search
		// Claude path in handleClaudeStream.
		h.recordSuccess(model, apiKeyID, "", totalInput, totalOutput, totalCredits)
		if apiKeyID != "" {
			_, _ = config.ConsumeAPIKey(apiKeyID, totalInput+totalOutput, totalCredits, model)
		}
	}

	h.writeClaudeWebSearchResponse(w, req.Stream, model, finalContent, finalThinking,
		citationBlocks, clientToolUses, totalInput, totalOutput, finalStop, thinking)
}

// buildAssistantToolUseBlocks reconstructs the assistant turn that requested
// the searches, as the raw content-block array ClaudeToKiro consumes.
func buildAssistantToolUseBlocks(content string, toolUses []KiroToolUse) []interface{} {
	blocks := make([]interface{}, 0, len(toolUses)+1)
	if content != "" {
		blocks = append(blocks, map[string]interface{}{"type": "text", "text": content})
	}
	for _, tu := range toolUses {
		blocks = append(blocks, map[string]interface{}{
			"type":  "tool_use",
			"id":    tu.ToolUseID,
			"name":  tu.Name,
			"input": tu.Input,
		})
	}
	return blocks
}

// firstUsableAccount returns an account with a token for executing the MCP
// search side-call. It prefers the pool's strategy-aware pick (so it honors
// weights and skips accounts currently in cooldown), and only falls back to a
// linear scan of enabled accounts if the pool has nothing eligible. The main
// generation rounds still go through full pool failover via runKiroCollect.
func (h *Handler) firstUsableAccount() *config.Account {
	// Pool-first: respects cooldown/backoff and weighting. Empty model = "any
	// eligible account", which is correct for a model-agnostic MCP side-call.
	if acc, _, ok := h.pool.GetNextForModel(""); ok && acc != nil && acc.AccessToken != "" {
		a := *acc
		_ = h.ensureValidToken(&a)
		return &a
	}
	// Fallback: a cooled-down pool shouldn't make web search impossible, so
	// scan for any enabled account with a token.
	for _, a := range config.GetAccounts() {
		if a.Enabled && a.AccessToken != "" {
			acc := a
			_ = h.ensureValidToken(&acc)
			return &acc
		}
	}
	return nil
}

// writeClaudeWebSearchResponse emits the final assistant message, splicing the
// accumulated web-search citation blocks ahead of the text answer. Handles both
// streaming (replayed as SSE) and non-streaming clients.
func (h *Handler) writeClaudeWebSearchResponse(w http.ResponseWriter, stream bool, model, content, thinkingContent string,
	citationBlocks []map[string]interface{}, clientToolUses []KiroToolUse, inputTokens, outputTokens int, stopReason string, thinking bool) {

	msgID := "msg_" + uuid.New().String()
	canonicalModel := canonicalAnthropicModelID(model)

	// Assemble the content-block array: thinking, citations, text, then any
	// real client tool calls. Raw maps so we can inline the native web-search
	// block shapes alongside typed blocks.
	blocks := make([]map[string]interface{}, 0, len(citationBlocks)+len(clientToolUses)+2)
	if thinking && thinkingContent != "" {
		blocks = append(blocks, map[string]interface{}{"type": "thinking", "thinking": thinkingContent})
	}
	blocks = append(blocks, citationBlocks...)
	if content != "" {
		blocks = append(blocks, map[string]interface{}{"type": "text", "text": content})
	}
	for _, tu := range clientToolUses {
		blocks = append(blocks, map[string]interface{}{
			"type": "tool_use", "id": tu.ToolUseID, "name": tu.Name, "input": tu.Input,
		})
	}

	resolvedStop := resolveAnthropicStopReason(stopReason, len(clientToolUses) > 0)
	usage := map[string]interface{}{"input_tokens": inputTokens, "output_tokens": outputTokens}

	if !stream {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": msgID, "type": "message", "role": "assistant",
			"content": blocks, "model": canonicalModel,
			"stop_reason": resolvedStop, "stop_sequence": nil, "usage": usage,
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, 500, "api_error", "Streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	h.sendSSE(w, flusher, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": msgID, "type": "message", "role": "assistant",
			"content": []interface{}{}, "model": canonicalModel,
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]interface{}{"input_tokens": inputTokens, "output_tokens": 0},
		},
	})

	// Replay each assembled block as content_block_start/delta/stop. For the
	// structured web-search/tool_use blocks we send the whole block in
	// content_block_start (clients read it there); text blocks stream a delta.
	for i, blk := range blocks {
		switch blk["type"] {
		case "text":
			h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type": "content_block_start", "index": i,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": i,
				"delta": map[string]interface{}{"type": "text_delta", "text": blk["text"]},
			})
		case "thinking":
			h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type": "content_block_start", "index": i,
				"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
			})
			h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": i,
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": blk["thinking"]},
			})
		default:
			// server_tool_use, web_search_tool_result, tool_use — send whole.
			h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type": "content_block_start", "index": i, "content_block": blk,
			})
		}
		h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
			"type": "content_block_stop", "index": i,
		})
	}

	h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": resolvedStop},
		"usage": usage,
	})
	h.sendSSE(w, flusher, "message_stop", map[string]interface{}{"type": "message_stop"})
}
