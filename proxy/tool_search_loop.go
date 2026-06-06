package proxy

import (
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
)

// ============================================================================
// Tool-search agentic loop.
//
// This is the piece that makes Anthropic's Tool Search actually WORK through
// Kiro (which has no server-side tools and no defer_loading concept). It is a
// near-sibling of the web-search loop (websearch_loop.go) and deliberately
// reuses runKiroCollect, buildAssistantToolUseBlocks, and
// writeClaudeWebSearchResponse — the response shaping (native blocks spliced
// ahead of the text answer, streamed or buffered) is identical.
//
// Flow, per client request that carries a tool_search server tool + deferred
// tools:
//
//   1. We split tools into eager (sent to the model), deferred (withheld), and
//      the search mode. Tools the model already discovered in earlier turns are
//      pre-expanded. A single synthetic `tool_search` function tool is exposed.
//   2. We run the conversation against Kiro. If the model calls tool_search, we
//      run the regex/BM25 match ourselves over the deferred tools, ADD the
//      matched tool schemas to the visible set, feed a short result back as a
//      tool_result, and run Kiro again — looping until the model answers (or
//      calls a real tool, or the round cap is hit).
//   3. The final answer is emitted with native server_tool_use +
//      tool_search_tool_result blocks spliced in, so Claude Code renders real
//      tool-search results. Clients that ignore those blocks still get the
//      grounded text answer plus any real tool calls.
//
// ISOLATION: runs ONLY when tool search is enabled AND the request carries a
// tool_search tool AND there is at least one deferred tool. Every other request
// takes the original handler path untouched. Like the web-search loop it uses
// buffered Kiro rounds replayed as SSE, keeping the live-streaming + failover
// machinery untouched.
// ============================================================================

// maxToolSearchRounds bounds how many search round-trips a single request can
// trigger, so a model that keeps searching can't loop forever.
const maxToolSearchRounds = 5

// requestHasDeferredTools reports whether the request is a genuine tool-search
// request worth routing to this loop: it must carry a tool_search server tool
// AND at least one defer_loading tool. A tool_search tool with no deferred tools
// is a no-op (nothing to withhold), so we let it fall through to the normal path.
func requestHasToolSearch(tools []ClaudeTool) bool {
	if detectToolSearchMode(tools) == toolSearchModeNone {
		return false
	}
	for _, t := range tools {
		if t.DeferLoading && !isAnthropicServerTool(t) {
			return true
		}
	}
	return false
}

// handleClaudeToolSearch orchestrates the tool-search agentic loop and writes
// the final response. Precondition: requestHasToolSearch(req.Tools) is true and
// the feature is enabled (verified by the caller).
func (h *Handler) handleClaudeToolSearch(w http.ResponseWriter, req *ClaudeRequest, model, apiKeyID string, thinking bool) {
	eager, deferred, mode := partitionToolSearchTools(req.Tools)

	// Pre-expand any deferred tool already discovered earlier in the conversation
	// so a multi-turn session doesn't force the model to re-search for a tool it
	// already used. These move into the visible set with defer_loading cleared.
	discovered := discoveredToolNamesInHistory(req.Messages)
	preExpanded := expandDeferredByName(deferred, discovered)

	// visibleTools is the tool set the upstream model sees this round. It starts
	// as: eager tools + the synthetic search tool + any pre-expanded discoveries.
	// Each successful search appends the matched deferred tools.
	visibleTools := make([]ClaudeTool, 0, len(eager)+len(preExpanded)+1)
	visibleTools = append(visibleTools, eager...)
	visibleTools = append(visibleTools, preExpanded...)
	visibleTools = append(visibleTools, toolSearchFnSpec())

	// Track which deferred tools are already visible so a repeated search for the
	// same capability doesn't add duplicate specs.
	visibleByName := make(map[string]bool, len(visibleTools))
	for _, t := range visibleTools {
		visibleByName[t.Name] = true
	}
	deferredByName := make(map[string]ClaudeTool, len(deferred))
	for _, t := range deferred {
		deferredByName[t.Name] = t
	}

	// Working copy of the conversation we grow across search rounds.
	messages := make([]ClaudeMessage, len(req.Messages))
	copy(messages, req.Messages)

	// Accumulated native tool-search blocks (server_tool_use +
	// tool_search_tool_result) to splice into the final assistant message.
	var searchBlocks []map[string]interface{}

	var totalInput, totalOutput int
	var totalCredits float64
	var finalContent, finalThinking, finalStop string
	var clientToolUses []KiroToolUse
	roundsRun := 0

	for round := 0; round < maxToolSearchRounds; round++ {
		// Re-check the per-key limit before every extra round, mirroring the
		// web-search loop: rounds 1..N-1 are additional upstream calls the model
		// triggered by searching again. Stop (don't error) once a limit is
		// crossed so the user keeps whatever the model already produced.
		if round > 0 && apiKeyID != "" {
			if rejected, _ := config.CheckAPIKeyLimit(apiKeyID, model); rejected {
				break
			}
		}

		roundReq := *req
		roundReq.Messages = messages
		roundReq.Tools = visibleTools
		// ToolChoice from the original request is preserved; the model is free to
		// search or answer.
		payload := ClaudeToKiro(&roundReq, thinking)
		// Forward native reasoning effort (output_config.effort) per round, the
		// same as the normal Claude path, so an effort level set by the client
		// isn't silently dropped on tool-search requests.
		h.applyReasoningEffort(payload, claudeRequestEffort(req))

		res, err := h.runKiroCollect(model, apiKeyID, payload)
		if err != nil {
			if roundsRun == 0 {
				// runKiroCollect suppresses the global failure count (this loop
				// owns once-per-request accounting), so record the single failure
				// here for a total round-0 failure.
				h.recordFailure(model, apiKeyID, payload.ResolvedEffort)
				h.sendClaudeError(w, 502, "api_error", safeUpstreamError("tool-search upstream call", err))
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

		// Split the model's tool calls into tool_search vs. everything else.
		var searchCalls, otherCalls []KiroToolUse
		for _, tu := range res.toolUses {
			if tu.Name == toolSearchFnName {
				searchCalls = append(searchCalls, tu)
			} else {
				otherCalls = append(otherCalls, tu)
			}
		}

		// No search requested → we're done. Return the answer plus any real
		// client tool calls (the client executes those).
		if len(searchCalls) == 0 {
			clientToolUses = otherCalls
			break
		}

		// Execute each requested search, expand matches into the visible set, and
		// feed a compact result back to the model. Append the assistant turn
		// (carrying the tool_use) then a user turn (carrying the tool_result).
		assistantBlocks := buildAssistantToolUseBlocks(res.content, searchCalls)
		messages = append(messages, ClaudeMessage{Role: "assistant", Content: assistantBlocks})

		var resultBlocks []interface{}
		for _, call := range searchCalls {
			query := extractToolSearchQuery(call.Input)
			matched := searchDeferredTools(mode, query, deferred)
			logger.Infof("[ToolSearch] query (%d chars) matched %d/%d deferred tool(s)",
				len(query), len(matched), len(deferred))

			// Add newly-matched tools to the visible set for subsequent rounds.
			for _, mt := range matched {
				if !visibleByName[mt.Name] {
					ct := deferredByName[mt.Name]
					ct.DeferLoading = false
					visibleTools = append(visibleTools, ct)
					visibleByName[mt.Name] = true
				}
			}

			// Record native tool-search blocks for the final client response.
			searchBlocks = append(searchBlocks, buildToolSearchResultBlocks(mode, call.ToolUseID, query, matched)...)

			resultBlocks = append(resultBlocks, map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": call.ToolUseID,
				"content":     formatToolSearchForModel(query, matched),
			})
		}
		messages = append(messages, ClaudeMessage{Role: "user", Content: resultBlocks})
		// loop to next round with the enlarged visible tool set
	}

	// Account for the whole request exactly ONCE with summed totals, matching the
	// web-search loop (runKiroCollect deliberately skips global counters + per-key
	// debit so a single client request isn't inflated N×).
	if roundsRun > 0 {
		h.recordSuccess(model, apiKeyID, "", totalInput, totalOutput, totalCredits)
		if apiKeyID != "" {
			_, _ = config.ConsumeAPIKey(apiKeyID, totalInput+totalOutput, totalCredits, model)
		}
	}

	// Reuse the web-search response writer: the splice shape (native blocks ahead
	// of the text answer, streamed or buffered) is identical.
	h.writeClaudeWebSearchResponse(w, req.Stream, model, finalContent, finalThinking,
		searchBlocks, clientToolUses, totalInput, totalOutput, finalStop, thinking)
}
