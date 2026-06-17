// Package proxy: OpenAI Responses API support (Codex CLI / agentic clients).
//
// The Responses API differs from /v1/chat/completions:
//   - Request uses "input" (string or array of items) and "instructions" instead
//     of "messages".
//   - Tools are flat (no "function" wrapper) and live at top level.
//   - Response is an "output" array containing message / reasoning /
//     function_call items, not "choices".
//   - Streaming uses event-typed SSE: response.created,
//     response.output_text.delta, response.function_call_arguments.delta,
//     response.completed, etc.
//
// We translate Responses requests into our internal Claude-shaped request and
// reuse ClaudeToKiro for the upstream call, then emit Responses-shaped output
// (or Codex SSE events) on the way back.
package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ResponsesRequest is the inbound /v1/responses request body.
type ResponsesRequest struct {
	Model              string            `json:"model"`
	Input              interface{}       `json:"input"`
	Instructions       string            `json:"instructions,omitempty"`
	Tools              []ResponsesTool   `json:"tools,omitempty"`
	ToolChoice         interface{}       `json:"tool_choice,omitempty"`
	Stream             bool              `json:"stream,omitempty"`
	Temperature        *float64          `json:"temperature,omitempty"`
	TopP               *float64          `json:"top_p,omitempty"`
	MaxOutputTokens    int               `json:"max_output_tokens,omitempty"`
	Reasoning          *ResponsesReason  `json:"reasoning,omitempty"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
	ParallelToolCalls  *bool             `json:"parallel_tool_calls,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	Truncation         string            `json:"truncation,omitempty"`
	TextFormat         interface{}       `json:"text,omitempty"`
	PromptCacheKey     string            `json:"prompt_cache_key,omitempty"`
}

// ResponsesReason controls reasoning behavior. effort: low|medium|high|minimal.
// summary: auto|concise|detailed|none.
type ResponsesReason struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// ResponsesTool is a flat function tool (no "function" wrapper).
type ResponsesTool struct {
	Type        string                 `json:"type"`
	Name        string                 `json:"name,omitempty"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
	// Codex CLI also sends a "strict" flag we accept but ignore.
	Strict *bool `json:"strict,omitempty"`
}

// ResponsesResponse is the non-streaming /v1/responses response body.
type ResponsesResponse struct {
	ID                string                      `json:"id"`
	Object            string                      `json:"object"`
	CreatedAt         int64                       `json:"created_at"`
	Status            string                      `json:"status"`
	Model             string                      `json:"model"`
	Output            []ResponsesOutput           `json:"output"`
	Usage             ResponsesUsage              `json:"usage"`
	Metadata          map[string]string           `json:"metadata,omitempty"`
	Reasoning         *ResponsesReason            `json:"reasoning,omitempty"`
	ParallelTC        bool                        `json:"parallel_tool_calls"`
	Tools             []ResponsesTool             `json:"tools,omitempty"`
	IncompleteDetails *ResponsesIncompleteDetails `json:"incomplete_details,omitempty"`
}

// ResponsesIncompleteDetails carries the reason a response did not run to
// completion. Reason is one of: "max_output_tokens", "content_filter".
type ResponsesIncompleteDetails struct {
	Reason string `json:"reason"`
}

// ResponsesOutput is one item in the response.output array. Type is one of:
// "message", "reasoning", "function_call".
type ResponsesOutput struct {
	Type    string                 `json:"type"`
	ID      string                 `json:"id,omitempty"`
	Status  string                 `json:"status,omitempty"`
	Role    string                 `json:"role,omitempty"`
	Content []ResponsesContentPart `json:"content,omitempty"`
	Summary []ResponsesSummaryPart `json:"summary,omitempty"`
	CallID  string                 `json:"call_id,omitempty"`
	Name    string                 `json:"name,omitempty"`
	Args    string                 `json:"arguments,omitempty"`
}

// ResponsesContentPart is a piece of a message output item.
type ResponsesContentPart struct {
	Type        string        `json:"type"` // output_text | refusal
	Text        string        `json:"text,omitempty"`
	Annotations []interface{} `json:"annotations"`
	Refusal     string        `json:"refusal,omitempty"`
}

// ResponsesSummaryPart is a piece of a reasoning output item.
type ResponsesSummaryPart struct {
	Type string `json:"type"` // summary_text
	Text string `json:"text"`
}

// ResponsesUsage tracks token usage in Responses API format.
type ResponsesUsage struct {
	InputTokens         int                     `json:"input_tokens"`
	OutputTokens        int                     `json:"output_tokens"`
	TotalTokens         int                     `json:"total_tokens"`
	InputTokensDetails  *ResponsesInputDetails  `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *ResponsesOutputDetails `json:"output_tokens_details,omitempty"`
}

type ResponsesInputDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type ResponsesOutputDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ResponsesToClaudeRequest converts a Responses-shaped request into the
// internal Claude-shaped request so we can reuse ClaudeToKiro for the upstream
// call. The returned request preserves the original model id.
func ResponsesToClaudeRequest(req *ResponsesRequest) *ClaudeRequest {
	out := &ClaudeRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxOutputTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}

	// instructions => system prompt
	if strings.TrimSpace(req.Instructions) != "" {
		out.System = req.Instructions
	}

	// tools => Claude tool definitions (so ClaudeToKiro produces the right Kiro shape).
	for _, t := range req.Tools {
		if !strings.EqualFold(t.Type, "function") && t.Type != "" {
			continue
		}
		out.Tools = append(out.Tools, ClaudeTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}

	// input => Claude messages
	out.Messages = responsesInputToClaudeMessages(req.Input)
	if len(out.Messages) == 0 {
		out.Messages = []ClaudeMessage{{Role: "user", Content: minimalFallbackUserContent}}
	}

	return out
}

// responsesInputToClaudeMessages flattens Responses "input" (string OR array of
// items) into Claude messages.
func responsesInputToClaudeMessages(input interface{}) []ClaudeMessage {
	if input == nil {
		return nil
	}

	if s, ok := input.(string); ok {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		return []ClaudeMessage{{Role: "user", Content: s}}
	}

	items, ok := input.([]interface{})
	if !ok {
		return nil
	}

	var msgs []ClaudeMessage
	for _, raw := range items {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}

		// item type override (function_call_output, function_call, reasoning, etc.).
		itemType, _ := item["type"].(string)
		switch itemType {
		case "function_call_output":
			callID, _ := item["call_id"].(string)
			output := item["output"]
			outText := ""
			switch v := output.(type) {
			case string:
				outText = v
			default:
				if b, err := json.Marshal(v); err == nil {
					outText = string(b)
				}
			}
			msgs = append(msgs, ClaudeMessage{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": callID,
						"content":     outText,
					},
				},
			})
			continue
		case "function_call":
			callID, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			argsStr, _ := item["arguments"].(string)
			var argsObj map[string]interface{}
			if argsStr != "" {
				_ = json.Unmarshal([]byte(argsStr), &argsObj)
			}
			if argsObj == nil {
				argsObj = map[string]interface{}{}
			}
			msgs = append(msgs, ClaudeMessage{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{
						"type":  "tool_use",
						"id":    callID,
						"name":  name,
						"input": argsObj,
					},
				},
			})
			continue
		case "reasoning":
			// We do not preserve prior assistant reasoning across calls; drop.
			continue
		}

		role, _ := item["role"].(string)
		if role == "" {
			role = "user"
		}
		// Claude only accepts user / assistant. Treat developer/system as user
		// text (Responses convention puts them inline in the input array).
		if role == "developer" || role == "system" {
			text := extractResponsesItemText(item["content"])
			if strings.TrimSpace(text) != "" {
				msgs = append(msgs, ClaudeMessage{Role: "user", Content: text})
			}
			continue
		}
		converted := convertResponsesContentForClaude(item["content"])
		msgs = append(msgs, ClaudeMessage{Role: role, Content: converted})
	}
	return msgs
}

// extractResponsesItemText pulls plain text out of a content field that may be
// a string or an array of {type, text} parts.
func extractResponsesItemText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var parts []string
		for _, p := range v {
			pm, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			if t, ok := pm["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

// convertResponsesContentForClaude maps Responses content (string or part array
// with input_text/input_image/output_text) to Claude content (string or
// content-block array).
func convertResponsesContentForClaude(content interface{}) interface{} {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	parts, ok := content.([]interface{})
	if !ok {
		return ""
	}

	out := make([]interface{}, 0, len(parts))
	for _, p := range parts {
		pm, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		ptype, _ := pm["type"].(string)
		switch ptype {
		case "input_text", "output_text", "text":
			if t, ok := pm["text"].(string); ok && t != "" {
				out = append(out, map[string]interface{}{"type": "text", "text": t})
			}
		case "input_image", "image":
			if url, ok := pm["image_url"]; ok {
				out = append(out, map[string]interface{}{
					"type":      "image",
					"image_url": url,
				})
			} else if src, ok := pm["source"]; ok {
				out = append(out, map[string]interface{}{
					"type":   "image",
					"source": src,
				})
			}
		case "refusal":
			if t, ok := pm["refusal"].(string); ok {
				out = append(out, map[string]interface{}{"type": "text", "text": t})
			}
		}
	}
	if len(out) == 0 {
		return ""
	}
	return out
}

// BuildResponsesNonStream assembles a non-streaming Responses payload from the
// upstream Kiro completion data.
func BuildResponsesNonStream(model, content, reasoning string, toolUses []KiroToolUse, inputTokens, outputTokens int, includeReasoning bool, cachedInputTokens int, reasoningCfg *ResponsesReason, upstreamStopReason string) *ResponsesResponse {
	now := time.Now().Unix()

	output := make([]ResponsesOutput, 0, 1+len(toolUses))

	if includeReasoning && strings.TrimSpace(reasoning) != "" {
		output = append(output, ResponsesOutput{
			Type:    "reasoning",
			ID:      "rs_" + uuid.New().String(),
			Status:  "completed",
			Summary: []ResponsesSummaryPart{{Type: "summary_text", Text: reasoning}},
		})
	}

	if strings.TrimSpace(content) != "" {
		output = append(output, ResponsesOutput{
			Type:   "message",
			ID:     "msg_" + uuid.New().String(),
			Status: "completed",
			Role:   "assistant",
			Content: []ResponsesContentPart{{
				Type:        "output_text",
				Text:        content,
				Annotations: []interface{}{},
			}},
		})
	}

	for _, tu := range toolUses {
		argsStr, _ := json.Marshal(tu.Input)
		output = append(output, ResponsesOutput{
			Type:   "function_call",
			ID:     "fc_" + uuid.New().String(),
			Status: "completed",
			CallID: tu.ToolUseID,
			Name:   tu.Name,
			Args:   string(argsStr),
		})
	}

	status := "completed"
	var incomplete *ResponsesIncompleteDetails
	if upstreamStopReason == "max_tokens" {
		status = "incomplete"
		incomplete = &ResponsesIncompleteDetails{Reason: "max_output_tokens"}
	}

	resp := &ResponsesResponse{
		ID:                "resp_" + uuid.New().String(),
		Object:            "response",
		CreatedAt:         now,
		Status:            status,
		Model:             canonicalAnthropicModelID(model),
		Output:            output,
		ParallelTC:        true,
		Reasoning:         reasoningCfg,
		IncompleteDetails: incomplete,
		Usage: ResponsesUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			TotalTokens:  inputTokens + outputTokens,
		},
	}
	if cachedInputTokens > 0 {
		resp.Usage.InputTokensDetails = &ResponsesInputDetails{CachedTokens: cachedInputTokens}
	}
	if includeReasoning && strings.TrimSpace(reasoning) != "" {
		resp.Usage.OutputTokensDetails = &ResponsesOutputDetails{ReasoningTokens: estimateApproxTokens(reasoning)}
	}
	return resp
}

// FormatResponsesError shapes a Responses-style error response body.
func FormatResponsesError(status int, code, message string) map[string]interface{} {
	return map[string]interface{}{
		"error": map[string]interface{}{
			"type":    code,
			"message": fmt.Sprintf("%d %s", status, message),
		},
	}
}
