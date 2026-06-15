package proxy

import (
	"context"
	"encoding/json"
	"io"
	"kiro-go/config"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// handleResponses POST /v1/responses (Codex CLI / OpenAI Responses API).
func (h *Handler) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		h.sendResponsesError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendResponsesError(w, 400, "invalid_request_error", "Invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		req.Model = "claude-sonnet-4.5"
	}

	// We are a stateless passthrough — we do not persist Responses to disk or
	// reconstruct history from a previous_response_id chain. Accepting the field
	// and silently dropping it would make the model lose all prior context with
	// no signal to the client. Reject it explicitly so callers know to re-send
	// the full input each turn (the OpenAI/Codex default) instead of getting
	// silently degraded answers.
	if strings.TrimSpace(req.PreviousResponseID) != "" {
		h.sendResponsesError(w, 400, "invalid_request_error",
			"previous_response_id is not supported: this server is stateless and does not store prior responses. Resend the full conversation in 'input' each turn.")
		return
	}

	if h.enforceAPIKeyLimit(w, r, req.Model) {
		return
	}
	apiKeyID := matchedAPIKeyID(r)

	// Translate to Claude shape, then run the same pipeline as /v1/messages.
	claudeReq := ResponsesToClaudeRequest(&req)

	// Pick the upstream account by the mapped model id.
	thinkingCfg := config.GetThinkingConfig()
	mappedModel, suffixThinking := ParseModelAndThinking(claudeReq.Model, thinkingCfg.Suffix)
	// Fold reasoning.effort into the thinking decision via the shared resolver
	// (minimal -> off, low/medium/high/max -> on, unset -> keep suffix default).
	effort := ""
	if req.Reasoning != nil {
		effort = req.Reasoning.Effort
	}
	thinking := resolveThinkingWithEffort(suffixThinking, effort)

	// Resolve the provider backend from the model string so a non-Kiro model
	// ("groq/...", "cx/...") selects the right account and is translated
	// correctly. Unprefixed -> "kiro" (existing behavior unchanged).
	reqBackend, upstreamModel := ParseModelBackend(req.Model)

	// poolModel: the mapped Kiro id for Kiro, the de-prefixed upstream id for a
	// non-Kiro backend (so a fetched /models catalog matches instead of shedding
	// as "no available accounts"). See the Claude handler.
	poolModel := mappedModel
	if reqBackend != "kiro" {
		poolModel = upstreamModel
	}

	// Reserve an in-flight slot via the AIMD-aware picker so Responses/Codex
	// traffic participates in the same per-account concurrency gate as the
	// Claude/OpenAI paths. Without this, Responses requests didn't increment
	// inflight and could pile onto an account on top of the reserved main-path
	// requests, defeating the saturation protection for mixed Codex+Claude load.
	// The slot is released once the synchronous stream/non-stream call below
	// returns (no failover here, so a single Acquire/Release pair is correct).
	// Scoped to the resolved backend.
	account, retryAfter, ok := h.pool.AcquireForBackendModelExcluding(reqBackend, poolModel, nil)
	if !ok {
		if retryAfter > 0 {
			setRetryAfter(w, retryAfter)
			h.sendResponsesError(w, 429, "rate_limit_exceeded", "All accounts are rate limited; retry after "+strconv.Itoa(retryAfterSeconds(retryAfter))+"s")
			return
		}
		h.sendResponsesError(w, 503, "server_error", "No available accounts")
		return
	}
	defer h.pool.Release(account.ID)
	if err := h.ensureValidToken(account); err != nil {
		h.sendResponsesError(w, 503, "server_error", safeUpstreamError("responses token refresh", err))
		return
	}

	estimatedInputTokens := estimateClaudeRequestInputTokens(claudeReq)
	if estimatedInputTokens < 1 {
		estimatedInputTokens = 1
	}

	kiroPayload := ClaudeToKiro(claudeReq, thinking)

	// Forward graded reasoning.effort natively when the resolved model supports
	// it (output_config.effort), clamped to the model's advertised levels. No-op
	// for models without effort support; the thinking on/off mapping above
	// already covers those.
	h.applyReasoningEffort(kiroPayload, effort)

	// Decide whether to surface reasoning summary back to the client. Codex
	// asks for one via reasoning.summary != "none".
	includeReasoning := true
	if req.Reasoning != nil && strings.EqualFold(strings.TrimSpace(req.Reasoning.Summary), "none") {
		includeReasoning = false
	}

	// Thread the originating request onto the context so a non-Kiro account
	// selected for this model is translated correctly (a Kiro account ignores it
	// and uses the prebuilt kiroPayload). ClientDialect is "responses".
	baseNR := &NormalizedRequest{
		Model:         upstreamModel,
		ClientDialect: DialectResponses,
		Claude:        claudeReq,
		Thinking:      thinking,
		Stream:        req.Stream,
		Effort:        effort,
	}
	respCtx := withNormalizedRequest(r.Context(), baseNR)

	// The Responses path keeps its original single-account behavior for
	// account selection above (token refresh, skeleton emission). Failover
	// across accounts is handled by the Claude/OpenAI chat endpoints, which
	// carry the bulk of Claude Code / SDK traffic; the Responses (Codex)
	// path emits a response.created skeleton up front, so a mid-flight
	// switch would require replaying that handshake. We keep it simple here.
	if req.Stream {
		h.handleResponsesStream(respCtx, w, account, kiroPayload, req.Model, thinking, includeReasoning, estimatedInputTokens, req.Reasoning, apiKeyID)
	} else {
		h.handleResponsesNonStream(respCtx, w, account, kiroPayload, req.Model, thinking, includeReasoning, estimatedInputTokens, req.Reasoning, apiKeyID)
	}
}

// handleResponsesNonStream blocks until upstream is done, then returns one
// JSON Responses payload.
func (h *Handler) handleResponsesNonStream(ctx context.Context, w http.ResponseWriter, account *config.Account, payload *KiroPayload, model string, thinking, includeReasoning bool, estimatedInputTokens int, reasoningCfg *ResponsesReason, apiKeyID string) {
	var content, reasoning string
	var toolUses []KiroToolUse
	var inputTokens, outputTokens int
	var credits float64
	var realInputTokens int
	var upstreamStopReason string

	callback := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				reasoning += text
			} else {
				content += text
			}
		},
		OnToolUse:  func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
		OnComplete: func(in, out int) { inputTokens, outputTokens = in, out },
		OnCredits:  func(c float64) { credits = c },
		OnContextUsage: func(pct float64) {
			realInputTokens = int(clampPercent(pct) * float64(h.contextWindowForModel(model)) / 100.0)
		},
		OnStopReason: func(r string) { upstreamStopReason = r },
	}

	if err := h.callProviderForKiro(ctx, account, payload, model, thinking, callback); err != nil {
		h.handleUpstreamError(err, account.ID, model, apiKeyID, payload.ResolvedEffort)
		h.sendResponsesError(w, 500, "server_error", safeUpstreamError("responses upstream call", err))
		return
	}

	finalContent, extracted := extractThinkingFromContent(content)
	if thinking && reasoning == "" && extracted != "" {
		reasoning = extracted
	}
	if !thinking {
		reasoning = ""
	}

	if realInputTokens > 0 {
		inputTokens = realInputTokens
	} else if inputTokens <= 0 {
		inputTokens = estimatedInputTokens
	}
	outputTokens = estimateClaudeOutputTokens(finalContent, reasoning, toolUses)

	// Pool counters before recordSuccess so the realtime dashboard push
	// reflects this request's per-account credits/tokens (see handler.go).
	h.pool.RecordSuccess(account.ID)
	h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
	h.recordSuccess(model, apiKeyID, payload.ResolvedEffort, inputTokens, outputTokens, credits, 0)
	h.triggerAccountRefresh(account.ID)
	if apiKeyID != "" {
		_, _ = config.ConsumeAPIKey(apiKeyID, inputTokens+outputTokens, credits, model)
	}

	resp := BuildResponsesNonStream(model, finalContent, reasoning, toolUses, inputTokens, outputTokens, includeReasoning, 0, reasoningCfg, upstreamStopReason)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(resp)
}

// handleResponsesStream emits Codex-shaped SSE events as content arrives.
//
// Event sequence (text-only happy path):
//
//	response.created
//	response.in_progress
//	response.output_item.added           (item.type=message)
//	response.content_part.added          (part.type=output_text)
//	response.output_text.delta * N
//	response.output_text.done
//	response.content_part.done
//	response.output_item.done
//	response.completed
//
// Reasoning, when present, is emitted as a separate output_item BEFORE the
// message item, with reasoning_summary_text deltas. Function calls are emitted
// as their own output_items with function_call_arguments deltas.
func (h *Handler) handleResponsesStream(ctx context.Context, w http.ResponseWriter, account *config.Account, payload *KiroPayload, model string, thinking, includeReasoning bool, estimatedInputTokens int, reasoningCfg *ResponsesReason, apiKeyID string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendResponsesError(w, 500, "server_error", "Streaming not supported")
		return
	}

	respID := "resp_" + uuid.New().String()
	now := time.Now().Unix()

	// Emit the initial pair right away so Codex knows we're working.
	skeleton := map[string]interface{}{
		"id":                  respID,
		"object":              "response",
		"created_at":          now,
		"status":              "in_progress",
		"model":               canonicalAnthropicModelID(model),
		"output":              []interface{}{},
		"parallel_tool_calls": true,
		"reasoning":           reasoningCfg,
		"tools":               []interface{}{},
	}
	h.sendResponsesEvent(w, flusher, "response.created", map[string]interface{}{
		"type":     "response.created",
		"response": skeleton,
	})
	h.sendResponsesEvent(w, flusher, "response.in_progress", map[string]interface{}{
		"type":     "response.in_progress",
		"response": skeleton,
	})

	// Streaming state.
	type fnCallState struct {
		ID        string
		CallID    string
		Name      string
		ArgsBuf   string
		ItemIndex int
		Started   bool
	}

	var (
		seq              int // event sequence number
		outputIndex      int // index of the next output item
		messageStarted   bool
		messageItemID    = "msg_" + uuid.New().String()
		messageIndex     int
		messageBuf       strings.Builder
		reasoningBuf     strings.Builder
		reasoningStarted bool
		reasoningItemID  = "rs_" + uuid.New().String()
		reasoningIndex   int
		toolCalls        = map[string]*fnCallState{}
		toolOrder        []string

		inputTokens, outputTokens int
		credits                   float64
		realInputTokens           int
		upstreamStopReason        string
	)

	nextSeq := func() int { seq++; return seq }

	emitReasoningStart := func() {
		if reasoningStarted || !includeReasoning {
			return
		}
		reasoningStarted = true
		reasoningIndex = outputIndex
		outputIndex++
		item := map[string]interface{}{
			"type":    "reasoning",
			"id":      reasoningItemID,
			"summary": []interface{}{},
		}
		h.sendResponsesEvent(w, flusher, "response.output_item.added", map[string]interface{}{
			"type":            "response.output_item.added",
			"sequence_number": nextSeq(),
			"output_index":    reasoningIndex,
			"item":            item,
		})
		h.sendResponsesEvent(w, flusher, "response.reasoning_summary_part.added", map[string]interface{}{
			"type":            "response.reasoning_summary_part.added",
			"sequence_number": nextSeq(),
			"item_id":         reasoningItemID,
			"output_index":    reasoningIndex,
			"summary_index":   0,
			"part":            map[string]interface{}{"type": "summary_text", "text": ""},
		})
	}

	emitReasoningDone := func() {
		if !reasoningStarted {
			return
		}
		txt := reasoningBuf.String()
		h.sendResponsesEvent(w, flusher, "response.reasoning_summary_text.done", map[string]interface{}{
			"type":            "response.reasoning_summary_text.done",
			"sequence_number": nextSeq(),
			"item_id":         reasoningItemID,
			"output_index":    reasoningIndex,
			"summary_index":   0,
			"text":            txt,
		})
		h.sendResponsesEvent(w, flusher, "response.reasoning_summary_part.done", map[string]interface{}{
			"type":            "response.reasoning_summary_part.done",
			"sequence_number": nextSeq(),
			"item_id":         reasoningItemID,
			"output_index":    reasoningIndex,
			"summary_index":   0,
			"part":            map[string]interface{}{"type": "summary_text", "text": txt},
		})
		h.sendResponsesEvent(w, flusher, "response.output_item.done", map[string]interface{}{
			"type":            "response.output_item.done",
			"sequence_number": nextSeq(),
			"output_index":    reasoningIndex,
			"item": map[string]interface{}{
				"type":    "reasoning",
				"id":      reasoningItemID,
				"status":  "completed",
				"summary": []interface{}{map[string]interface{}{"type": "summary_text", "text": txt}},
			},
		})
	}

	emitMessageStart := func() {
		if messageStarted {
			return
		}
		messageStarted = true
		messageIndex = outputIndex
		outputIndex++
		item := map[string]interface{}{
			"type":    "message",
			"id":      messageItemID,
			"status":  "in_progress",
			"role":    "assistant",
			"content": []interface{}{},
		}
		h.sendResponsesEvent(w, flusher, "response.output_item.added", map[string]interface{}{
			"type":            "response.output_item.added",
			"sequence_number": nextSeq(),
			"output_index":    messageIndex,
			"item":            item,
		})
		h.sendResponsesEvent(w, flusher, "response.content_part.added", map[string]interface{}{
			"type":            "response.content_part.added",
			"sequence_number": nextSeq(),
			"item_id":         messageItemID,
			"output_index":    messageIndex,
			"content_index":   0,
			"part": map[string]interface{}{
				"type":        "output_text",
				"text":        "",
				"annotations": []interface{}{},
			},
		})
	}

	emitMessageDelta := func(delta string) {
		emitMessageStart()
		messageBuf.WriteString(delta)
		h.sendResponsesEvent(w, flusher, "response.output_text.delta", map[string]interface{}{
			"type":            "response.output_text.delta",
			"sequence_number": nextSeq(),
			"item_id":         messageItemID,
			"output_index":    messageIndex,
			"content_index":   0,
			"delta":           delta,
		})
	}

	emitMessageDone := func() {
		if !messageStarted {
			return
		}
		txt := messageBuf.String()
		h.sendResponsesEvent(w, flusher, "response.output_text.done", map[string]interface{}{
			"type":            "response.output_text.done",
			"sequence_number": nextSeq(),
			"item_id":         messageItemID,
			"output_index":    messageIndex,
			"content_index":   0,
			"text":            txt,
		})
		h.sendResponsesEvent(w, flusher, "response.content_part.done", map[string]interface{}{
			"type":            "response.content_part.done",
			"sequence_number": nextSeq(),
			"item_id":         messageItemID,
			"output_index":    messageIndex,
			"content_index":   0,
			"part": map[string]interface{}{
				"type":        "output_text",
				"text":        txt,
				"annotations": []interface{}{},
			},
		})
		h.sendResponsesEvent(w, flusher, "response.output_item.done", map[string]interface{}{
			"type":            "response.output_item.done",
			"sequence_number": nextSeq(),
			"output_index":    messageIndex,
			"item": map[string]interface{}{
				"type":   "message",
				"id":     messageItemID,
				"status": "completed",
				"role":   "assistant",
				"content": []interface{}{map[string]interface{}{
					"type":        "output_text",
					"text":        txt,
					"annotations": []interface{}{},
				}},
			},
		})
	}

	emitReasoningDelta := func(delta string) {
		if !includeReasoning || delta == "" {
			return
		}
		emitReasoningStart()
		reasoningBuf.WriteString(delta)
		h.sendResponsesEvent(w, flusher, "response.reasoning_summary_text.delta", map[string]interface{}{
			"type":            "response.reasoning_summary_text.delta",
			"sequence_number": nextSeq(),
			"item_id":         reasoningItemID,
			"output_index":    reasoningIndex,
			"summary_index":   0,
			"delta":           delta,
		})
	}

	// Route ALL upstream text through the shared thinkingTextProcessor so inline
	// <thinking>...</thinking> tags emitted as ordinary assistantResponseEvent
	// text are parsed into reasoning deltas instead of leaking verbatim into
	// output_text.delta — matching the Claude /v1/messages and OpenAI chat stream
	// paths. The emitter maps the processor's thinking-state to Responses events:
	//   state 0 -> assistant message text (closing any open reasoning item first)
	//   state 1/2/3 -> reasoning_summary_text deltas
	// The reasoning item is closed lazily when ordinary text follows or at
	// Finalize(), preserving the existing output_index ordering.
	processor := newThinkingProcessor(thinking, func(text string, thinkingState int) {
		if thinkingState == 0 {
			if text == "" {
				return
			}
			if reasoningStarted && !messageStarted {
				emitReasoningDone()
			}
			emitMessageDelta(text)
			return
		}
		// thinkingState 1/2/3: thinking content -> reasoning deltas.
		emitReasoningDelta(text)
	}, allowReasoningSource, allowTagSource)

	callback := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			processor.Process(text, isThinking)
		},
		OnToolUse: func(tu KiroToolUse) {
			// Flush any buffered thinking/text (and close an open thinking block)
			// before tool output so a partial <thinking> tag can't straddle the
			// tool-call boundary.
			processor.Finalize()
			// Close text + reasoning before emitting tool calls.
			if reasoningStarted && !messageStarted {
				emitReasoningDone()
			}
			if messageStarted {
				emitMessageDone()
				messageStarted = false
			}

			st := &fnCallState{
				ID:        "fc_" + uuid.New().String(),
				CallID:    tu.ToolUseID,
				Name:      tu.Name,
				ItemIndex: outputIndex,
				Started:   true,
			}
			outputIndex++
			toolCalls[tu.ToolUseID] = st
			toolOrder = append(toolOrder, tu.ToolUseID)
			argsStr, _ := json.Marshal(tu.Input)
			st.ArgsBuf = string(argsStr)

			itemSkeleton := map[string]interface{}{
				"type":      "function_call",
				"id":        st.ID,
				"status":    "in_progress",
				"call_id":   st.CallID,
				"name":      st.Name,
				"arguments": "",
			}
			h.sendResponsesEvent(w, flusher, "response.output_item.added", map[string]interface{}{
				"type":            "response.output_item.added",
				"sequence_number": nextSeq(),
				"output_index":    st.ItemIndex,
				"item":            itemSkeleton,
			})
			h.sendResponsesEvent(w, flusher, "response.function_call_arguments.delta", map[string]interface{}{
				"type":            "response.function_call_arguments.delta",
				"sequence_number": nextSeq(),
				"item_id":         st.ID,
				"output_index":    st.ItemIndex,
				"delta":           st.ArgsBuf,
			})
			h.sendResponsesEvent(w, flusher, "response.function_call_arguments.done", map[string]interface{}{
				"type":            "response.function_call_arguments.done",
				"sequence_number": nextSeq(),
				"item_id":         st.ID,
				"output_index":    st.ItemIndex,
				"arguments":       st.ArgsBuf,
			})
			h.sendResponsesEvent(w, flusher, "response.output_item.done", map[string]interface{}{
				"type":            "response.output_item.done",
				"sequence_number": nextSeq(),
				"output_index":    st.ItemIndex,
				"item": map[string]interface{}{
					"type":      "function_call",
					"id":        st.ID,
					"status":    "completed",
					"call_id":   st.CallID,
					"name":      st.Name,
					"arguments": st.ArgsBuf,
				},
			})
		},
		OnComplete: func(in, out int) { inputTokens, outputTokens = in, out },
		OnCredits:  func(c float64) { credits = c },
		OnContextUsage: func(pct float64) {
			realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
		},
		OnStopReason: func(r string) { upstreamStopReason = r },
	}

	if err := h.callProviderForKiro(ctx, account, payload, model, thinking, callback); err != nil {
		// Flush any buffered thinking/text before surfacing the error.
		processor.Finalize()
		h.handleUpstreamError(err, account.ID, model, apiKeyID, payload.ResolvedEffort)
		// Emit failure events Codex understands.
		h.sendResponsesEvent(w, flusher, "response.failed", map[string]interface{}{
			"type":            "response.failed",
			"sequence_number": nextSeq(),
			"response": map[string]interface{}{
				"id":     respID,
				"status": "failed",
				"error":  map[string]interface{}{"type": "server_error", "message": safeStreamErrorMessage(err)},
			},
		})
		return
	}

	// Flush the thinking processor so any held tail (a partial tag hedge) is
	// emitted before we close the items.
	processor.Finalize()

	// Close any open reasoning / message items.
	if reasoningStarted && reasoningBuf.Len() > 0 && !messageStarted {
		emitReasoningDone()
	}
	if messageStarted {
		emitMessageDone()
	}

	// Final usage + completion.
	if realInputTokens > 0 {
		inputTokens = realInputTokens
	} else if inputTokens <= 0 {
		inputTokens = estimatedInputTokens
	}
	outputTokens = estimateClaudeOutputTokens(messageBuf.String(), reasoningBuf.String(), nil)

	// Pool counters before recordSuccess so the realtime dashboard push
	// reflects this request's per-account credits/tokens (see handler.go).
	h.pool.RecordSuccess(account.ID)
	h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
	h.recordSuccess(model, apiKeyID, payload.ResolvedEffort, inputTokens, outputTokens, credits, 0)
	h.triggerAccountRefresh(account.ID)
	if apiKeyID != "" {
		_, _ = config.ConsumeAPIKey(apiKeyID, inputTokens+outputTokens, credits, model)
	}

	finalOutputs := []interface{}{}
	if reasoningStarted {
		finalOutputs = append(finalOutputs, map[string]interface{}{
			"type":    "reasoning",
			"id":      reasoningItemID,
			"status":  "completed",
			"summary": []interface{}{map[string]interface{}{"type": "summary_text", "text": reasoningBuf.String()}},
		})
	}
	if messageBuf.Len() > 0 {
		finalOutputs = append(finalOutputs, map[string]interface{}{
			"type":   "message",
			"id":     messageItemID,
			"status": "completed",
			"role":   "assistant",
			"content": []interface{}{map[string]interface{}{
				"type":        "output_text",
				"text":        messageBuf.String(),
				"annotations": []interface{}{},
			}},
		})
	}
	for _, id := range toolOrder {
		st := toolCalls[id]
		finalOutputs = append(finalOutputs, map[string]interface{}{
			"type":      "function_call",
			"id":        st.ID,
			"status":    "completed",
			"call_id":   st.CallID,
			"name":      st.Name,
			"arguments": st.ArgsBuf,
		})
	}

	usage := map[string]interface{}{
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
		"total_tokens":  inputTokens + outputTokens,
	}
	if reasoningStarted {
		usage["output_tokens_details"] = map[string]interface{}{
			"reasoning_tokens": estimateApproxTokens(reasoningBuf.String()),
		}
	}

	completedStatus := "completed"
	completedResponse := map[string]interface{}{
		"id":                  respID,
		"object":              "response",
		"created_at":          now,
		"status":              completedStatus,
		"model":               canonicalAnthropicModelID(model),
		"output":              finalOutputs,
		"usage":               usage,
		"parallel_tool_calls": true,
		"reasoning":           reasoningCfg,
	}
	if upstreamStopReason == "max_tokens" {
		completedResponse["status"] = "incomplete"
		completedResponse["incomplete_details"] = map[string]string{"reason": "max_output_tokens"}
	}

	h.sendResponsesEvent(w, flusher, "response.completed", map[string]interface{}{
		"type":            "response.completed",
		"sequence_number": nextSeq(),
		"response":        completedResponse,
	})
}

// sendResponsesEvent writes a single SSE event with the Codex Responses event
// envelope: "event: <name>\ndata: <json>\n\n".
func (h *Handler) sendResponsesEvent(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	body, err := json.Marshal(data)
	if err != nil {
		return
	}
	// Single Write of a pre-built frame — same fast path as sendSSE; avoids
	// fmt.Fprintf format-parse cost on the streaming hot path.
	frame := make([]byte, 0, len(event)+len(body)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, event...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, body...)
	frame = append(frame, "\n\n"...)
	w.Write(frame)
	flusher.Flush()
}

// sendResponsesError writes a JSON error in Responses-style.
func (h *Handler) sendResponsesError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	})
}
