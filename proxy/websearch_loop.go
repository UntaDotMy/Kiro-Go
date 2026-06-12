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
// the collected result, or an error if every account attempt failed. This is the
// Kiro-only variant used by the tool-search loop; the web-search loop uses the
// backend-aware runProviderCollect below.
//
// ACCOUNTING NOTE: this runs per ROUND of the agentic loop (up to maxRounds
// times for one client request). It records ONLY the per-account pool stats
// (RecordSuccess/UpdateStats), which are legitimately per-upstream-call. It
// deliberately does NOT call recordSuccess (global request/token/credit counters
// + SQLite) or ConsumeAPIKey (per-key quota debit) — those would over-count a
// single client request by up to N×. The caller aggregates across rounds and
// debits exactly once.
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

// runProviderCollect runs ONE agentic round against the request's OWN backend
// (Kiro or any generic/non-Kiro provider) with full backend-scoped failover,
// collecting the assistant output instead of writing it to the client. This is
// what makes the web-search loop work for non-Kiro providers: a dashscope/qwen
// account generates the turn, while the search side-call still goes through a
// Kiro account's MCP endpoint (handled by the caller).
//
// backend is the resolved provider id ("kiro" or e.g. "qwen"/"dashscope"); the
// pool selection is scoped to accounts on that backend. poolModel is the id the
// pool's per-account filter matches against (the de-prefixed upstream id for a
// non-Kiro backend, the request model for Kiro). roundReq is the Claude request
// for THIS round (already grown with prior search results). payload is the
// prebuilt Kiro payload — used verbatim by the Kiro provider and ignored by a
// generic provider, which translates roundReq itself.
//
// Accounting matches runKiroCollect: per-account pool stats only; the caller
// owns the once-per-request global + per-key accounting.
func (h *Handler) runProviderCollect(backend, poolModel, model, upstreamModel, apiKeyID string, roundReq *ClaudeRequest, payload *KiroPayload, thinking bool, effort string) (*kiroRoundResult, error) {
	out := &kiroRoundResult{}
	var realInputTokens int

	baseNR := &NormalizedRequest{
		Model:         upstreamModel,
		ClientDialect: DialectClaude,
		Claude:        roundReq,
		Thinking:      thinking,
		Stream:        false,
		Effort:        effort,
	}

	worker := func(account *config.Account) (bool, error) {
		*out = kiroRoundResult{}
		realInputTokens = 0
		if payload != nil {
			payload.ProfileArn = ""
		}

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

		// Stash the per-round NormalizedRequest so callProviderForKiro hands the
		// generic provider a real request to translate. A fresh copy per attempt
		// keeps concurrent failover attempts from racing on nr.Kiro.
		nr := *baseNR
		ctx := withNormalizedRequest(context.Background(), &nr)
		if err := h.callProviderForKiro(ctx, account, payload, model, thinking, callback); err != nil {
			h.recordAttemptError(err, account.ID)
			return false, err
		}

		out.inputTokens = resolveInputTokens(out.inputTokens, realInputTokens, 0)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, out.inputTokens+out.outputTokens, out.credits)
		h.triggerAccountRefresh(account.ID)
		return false, nil
	}

	_, _, err := h.runWithFailoverCountedBackend(backend, poolModel, apiKeyID, effort, worker, false)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// handleClaudeWebSearch orchestrates the web-search agentic EMULATION loop and
// writes the final response. Precondition (verified by the caller): the feature
// is enabled, req carries a web_search tool, the request's backend has NO native
// web search, and a usable Kiro account exists for the MCP search side-call.
//
// Generation runs against the request's OWN backend (Kiro or any generic/non-Kiro
// provider) via runProviderCollect, so e.g. a Groq/OpenAI model that lacks native
// search still gets grounded answers. The actual search always executes through a
// Kiro account's MCP endpoint (firstUsableKiroAccount), which is the only search
// source this emulation has.
//
//	model         — public model id as requested (echoed back to the client)
//	upstreamModel — de-prefixed id sent upstream (== model for a Kiro request)
//	backend       — resolved provider id ("kiro" or e.g. "groq")
func (h *Handler) handleClaudeWebSearch(w http.ResponseWriter, req *ClaudeRequest, model, upstreamModel, backend, apiKeyID string, thinking bool) {
	// poolModel is the id the pool's per-account filter matches against: the
	// de-prefixed upstream id for a non-Kiro backend, the request model for Kiro.
	poolModel := model
	if backend != "" && backend != "kiro" {
		poolModel = upstreamModel
	}
	effort := claudeRequestEffort(req)

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
		// Prebuild the Kiro payload so a Kiro-backed request is byte-identical to
		// the original path; a generic provider ignores it and translates roundReq.
		payload := ClaudeToKiro(&roundReq, thinking)
		h.applyReasoningEffort(payload, effort)

		res, err := h.runProviderCollect(backend, poolModel, model, upstreamModel, apiKeyID, &roundReq, payload, thinking, effort)
		if err != nil {
			// If at least one round already succeeded, surface what we have
			// rather than discarding it; the partial usage is still debited
			// below. Only a round-0 failure (nothing produced) is a hard error.
			if roundsRun == 0 {
				// runProviderCollect suppresses the global failure count (this loop
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

		// Resolve the search backend once per round. Default is Kiro's MCP search
		// (Kiro account required); when an external provider is configured
		// (config.WebSearchProvider), searches go there instead and NO Kiro account
		// is needed — so a pure DashScope/Gemini deployment can still emulate
		// hosted web_search.
		searchAccount := h.firstUsableKiroAccount()
		extProvider := config.GetWebSearchProvider()
		extKey := config.GetWebSearchAPIKey()

		var resultBlocks []interface{}
		for _, call := range searchCalls {
			query := extractWebSearchQuery(call.Input)
			var results []WebSearchResult
			var searchErr error
			if extProvider != "" && extProvider != "kiro" {
				results, searchErr = performExternalWebSearch(context.Background(), extProvider, extKey, query)
			} else {
				results, searchErr = performKiroWebSearch(context.Background(), searchAccount, query)
			}
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
	// every round. runProviderCollect deliberately skips global counters + per-key
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
