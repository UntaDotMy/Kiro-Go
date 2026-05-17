package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
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

	body, err := io.ReadAll(r.Body)
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

	if h.enforceAPIKeyLimit(w, r, req.Model) {
		return
	}
	apiKeyID := matchedAPIKeyID(r)

	// Translate to Claude shape, then run the same pipeline as /v1/messages.
	claudeReq := ResponsesToClaudeRequest(&req)

	// Pick the upstream account by the mapped model id.
	thinkingCfg := config.GetThinkingConfig()
	mappedModel, suffixThinking := ParseModelAndThinking(claudeReq.Model, thinkingCfg.Suffix)
	thinking := suffixThinking || (req.Reasoning != nil && req.Reasoning.Effort != "" && !strings.EqualFold(req.Reasoning.Effort, "minimal"))

	account := h.pool.GetNextForModel(mappedModel)
	if account == nil {
		h.sendResponsesError(w, 503, "server_error", "No available accounts")
		return
	}
	if err := h.ensureValidToken(account); err != nil {
		h.sendResponsesError(w, 503, "server_error", "Token refresh failed: "+err.Error())
		return
	}

	estimatedInputTokens := estimateClaudeRequestInputTokens(claudeReq)
	if estimatedInputTokens < 1 {
		estimatedInputTokens = 1
	}

	kiroPayload := ClaudeToKiro(claudeReq, thinking)

	// Decide whether to surface reasoning summary back to the client. Codex
	// asks for one via reasoning.summary != "none".
	includeReasoning := true
	if req.Reasoning != nil && strings.EqualFold(strings.TrimSpace(req.Reasoning.Summary), "none") {
		includeReasoning = false
	}

	if req.Stream {
		h.handleResponsesStream(w, account, kiroPayload, req.Model, thinking, includeReasoning, estimatedInputTokens, req.Reasoning, apiKeyID)
	} else {
		h.handleResponsesNonStream(w, account, kiroPayload, req.Model, thinking, includeReasoning, estimatedInputTokens, req.Reasoning, apiKeyID)
	}
}

// handleResponsesNonStream blocks until upstream is done, then returns one
// JSON Responses payload.
func (h *Handler) handleResponsesNonStream(w http.ResponseWriter, account *config.Account, payload *KiroPayload, model string, thinking, includeReasoning bool, estimatedInputTokens int, reasoningCfg *ResponsesReason, apiKeyID string) {
	var content, reasoning string
	var toolUses []KiroToolUse
	var inputTokens, outputTokens int
	var credits float64
	var realInputTokens int

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
		OnError:    func(err error) { h.pool.RecordError(account.ID, strings.Contains(err.Error(), "429")) },
		OnCredits:  func(c float64) { credits = c },
		OnContextUsage: func(pct float64) {
			realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
		},
	}

	if err := CallKiroAPI(account, payload, callback); err != nil {
		h.recordFailure(model, apiKeyID)
		h.pool.RecordError(account.ID, strings.Contains(err.Error(), "429"))
		h.checkOverageError(err, account.ID)
		h.sendResponsesError(w, 500, "server_error", err.Error())
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

	h.recordSuccess(model, apiKeyID, inputTokens, outputTokens, credits)
	h.pool.RecordSuccess(account.ID)
	h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
	h.triggerAccountRefresh(account.ID)
	recordModelUsage(model, inputTokens+outputTokens, credits)
	if apiKeyID != "" { _, _ = config.ConsumeAPIKey(apiKeyID, inputTokens+outputTokens, credits, model) }

	resp := BuildResponsesNonStream(model, finalContent, reasoning, toolUses, inputTokens, outputTokens, includeReasoning, 0, reasoningCfg)

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
func (h *Handler) handleResponsesStream(w http.ResponseWriter, account *config.Account, payload *KiroPayload, model string, thinking, includeReasoning bool, estimatedInputTokens int, reasoningCfg *ResponsesReason, apiKeyID string) {
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
		"model":               model,
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
		seq             int    // event sequence number
		outputIndex     int    // index of the next output item
		messageStarted  bool
		messageItemID   = "msg_" + uuid.New().String()
		messageIndex    int
		messageBuf      strings.Builder
		reasoningBuf    strings.Builder
		reasoningStarted bool
		reasoningItemID = "rs_" + uuid.New().String()
		reasoningIndex  int
		toolCalls       = map[string]*fnCallState{}
		toolOrder       []string

		inputTokens, outputTokens int
		credits                   float64
		realInputTokens           int
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
			"type":          "response.output_item.added",
			"sequence_number": nextSeq(),
			"output_index":  reasoningIndex,
			"item":          item,
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

	callback := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				if !includeReasoning {
					return
				}
				emitReasoningStart()
				reasoningBuf.WriteString(text)
				h.sendResponsesEvent(w, flusher, "response.reasoning_summary_text.delta", map[string]interface{}{
					"type":            "response.reasoning_summary_text.delta",
					"sequence_number": nextSeq(),
					"item_id":         reasoningItemID,
					"output_index":    reasoningIndex,
					"summary_index":   0,
					"delta":           text,
				})
				return
			}
			// Once real assistant text starts, close any open reasoning item first
			// so the message item has the next output_index slot.
			if reasoningStarted && !messageStarted {
				emitReasoningDone()
			}
			emitMessageDelta(text)
		},
		OnToolUse: func(tu KiroToolUse) {
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
		OnError:    func(err error) { h.pool.RecordError(account.ID, strings.Contains(err.Error(), "429")) },
		OnCredits:  func(c float64) { credits = c },
		OnContextUsage: func(pct float64) {
			realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
		},
	}

	if err := CallKiroAPI(account, payload, callback); err != nil {
		h.recordFailure(model, apiKeyID)
		h.pool.RecordError(account.ID, strings.Contains(err.Error(), "429"))
		h.checkOverageError(err, account.ID)
		// Emit failure events Codex understands.
		h.sendResponsesEvent(w, flusher, "response.failed", map[string]interface{}{
			"type":            "response.failed",
			"sequence_number": nextSeq(),
			"response": map[string]interface{}{
				"id":     respID,
				"status": "failed",
				"error":  map[string]interface{}{"type": "server_error", "message": err.Error()},
			},
		})
		return
	}

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

	h.recordSuccess(model, apiKeyID, inputTokens, outputTokens, credits)
	h.pool.RecordSuccess(account.ID)
	h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
	h.triggerAccountRefresh(account.ID)
	recordModelUsage(model, inputTokens+outputTokens, credits)
	if apiKeyID != "" { _, _ = config.ConsumeAPIKey(apiKeyID, inputTokens+outputTokens, credits, model) }

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

	h.sendResponsesEvent(w, flusher, "response.completed", map[string]interface{}{
		"type":            "response.completed",
		"sequence_number": nextSeq(),
		"response": map[string]interface{}{
			"id":                  respID,
			"object":              "response",
			"created_at":          now,
			"status":              "completed",
			"model":               model,
			"output":              finalOutputs,
			"usage":               usage,
			"parallel_tool_calls": true,
			"reasoning":           reasoningCfg,
		},
	})
}

// sendResponsesEvent writes a single SSE event with the Codex Responses event
// envelope: "event: <name>\ndata: <json>\n\n".
func (h *Handler) sendResponsesEvent(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	body, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body)
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
