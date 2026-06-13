package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// ============================================================================
// Generic-provider translation layer.
//
// The generic provider normalizes through OpenAI Chat Completions as the
// intermediate wire format for the "openai" dialect, builds an Anthropic
// Messages body for the "anthropic" dialect, and a Gemini generateContent body
// for "gemini". The RESPONSE side parses each dialect's SSE stream and drives the
// shared KiroStreamCallback, so all existing response renderers (Claude / OpenAI
// / Responses) work unchanged regardless of which provider served the request.
//
// Ported in spirit from 9router's translator/request|response/* modules, adapted
// to Kiro-Go's existing ClaudeRequest / OpenAIRequest types.
// ============================================================================

// openAIChatBody is the JSON body sent to an OpenAI-compatible /chat/completions
// endpoint. We marshal a map rather than a fixed struct so we can pass through
// only the fields a generic provider expects and omit Kiro-specific ones.
type openAIChatBody struct {
	Model       string                   `json:"model"`
	Messages    []map[string]interface{} `json:"messages"`
	MaxTokens   int                      `json:"max_tokens,omitempty"`
	Temperature float64                  `json:"temperature,omitempty"`
	TopP        float64                  `json:"top_p,omitempty"`
	Stream      bool                     `json:"stream"`
	Tools       []map[string]interface{} `json:"tools,omitempty"`
	ToolChoice  interface{}              `json:"tool_choice,omitempty"`
}

// buildOpenAIChatBody converts a NormalizedRequest into an OpenAI chat
// completions request body for the given upstream model id. Exactly one of
// nr.Claude / nr.OpenAI is set. When the client already spoke OpenAI, this is a
// near-passthrough; when it spoke Claude, we down-convert messages/tools.
func buildOpenAIChatBody(nr *NormalizedRequest, upstreamModel string, stream bool) ([]byte, error) {
	body := openAIChatBody{Model: upstreamModel, Stream: stream}

	switch {
	case nr.OpenAI != nil:
		req := nr.OpenAI
		body.MaxTokens = req.MaxTokens
		body.Temperature = req.Temperature
		body.TopP = req.TopP
		body.Messages = openAIMessagesToMaps(req.Messages)
		body.Tools = openAIToolsToMaps(req.Tools)
		// Carry the tool-selection intent through unchanged (it's already an
		// OpenAI-shaped value); normalize only to drop unknown forms.
		if len(body.Tools) > 0 {
			if ti, ok := parseOpenAIToolChoice(req.ToolChoice); ok {
				body.ToolChoice = ti.toOpenAI()
			}
		}
	case nr.Claude != nil:
		req := nr.Claude
		body.MaxTokens = req.MaxTokens
		body.Temperature = req.Temperature
		body.TopP = req.TopP
		body.Messages = claudeToOpenAIMessages(req)
		body.Tools = claudeToolsToOpenAIMaps(req.Tools)
		if len(body.Tools) > 0 {
			if ti, ok := parseClaudeToolChoice(req.ToolChoice); ok {
				body.ToolChoice = ti.toOpenAI()
			}
		}
	}

	return json.Marshal(body)
}

// openAIMessagesToMaps re-serializes inbound OpenAI messages as generic maps,
// preserving role/content/tool_calls/tool_call_id. Marshaling through the
// existing struct then back to a map keeps the exact wire shape.
func openAIMessagesToMaps(msgs []OpenAIMessage) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(msgs))
	for _, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			continue
		}
		var mm map[string]interface{}
		if json.Unmarshal(b, &mm) == nil {
			out = append(out, mm)
		}
	}
	return out
}

func openAIToolsToMaps(tools []OpenAITool) []map[string]interface{} {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		b, err := json.Marshal(t)
		if err != nil {
			continue
		}
		var mm map[string]interface{}
		if json.Unmarshal(b, &mm) == nil {
			out = append(out, mm)
		}
	}
	return out
}

// claudeToOpenAIMessages converts a Claude Messages request into OpenAI chat
// messages: the system prompt becomes a leading system message; user/assistant
// text and tool_use/tool_result blocks map to OpenAI's tool-call protocol.
func claudeToOpenAIMessages(req *ClaudeRequest) []map[string]interface{} {
	var out []map[string]interface{}

	if sys := extractClaudeSystemString(req.System); sys != "" {
		out = append(out, map[string]interface{}{"role": "system", "content": sys})
	}

	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		switch role {
		case "user":
			out = append(out, claudeUserMessageToOpenAI(msg.Content)...)
		case "assistant":
			out = append(out, claudeAssistantMessageToOpenAI(msg.Content))
		default:
			if s, ok := msg.Content.(string); ok && s != "" {
				out = append(out, map[string]interface{}{"role": role, "content": s})
			}
		}
	}
	return out
}

// claudeUserMessageToOpenAI maps a Claude user message. A user message carrying
// tool_result blocks becomes one or more OpenAI "tool" role messages; plain text
// becomes a single "user" message. Image blocks are forwarded as OpenAI
// image_url content parts so vision input survives the cross-dialect hop (the
// user message then carries a multimodal content array instead of a string).
func claudeUserMessageToOpenAI(content interface{}) []map[string]interface{} {
	if s, ok := content.(string); ok {
		return []map[string]interface{}{{"role": "user", "content": s}}
	}
	blocks, ok := content.([]interface{})
	if !ok {
		return []map[string]interface{}{{"role": "user", "content": ""}}
	}

	var out []map[string]interface{}
	var text strings.Builder
	images := extractClaudeImages(content)
	for _, b := range blocks {
		block, ok := b.(map[string]interface{})
		if !ok {
			continue
		}
		switch block["type"] {
		case "text", "input_text":
			if t, ok := block["text"].(string); ok {
				text.WriteString(t)
			}
		case "tool_result":
			toolUseID, _ := block["tool_use_id"].(string)
			out = append(out, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": toolUseID,
				"content":      extractToolResultContent(block["content"]),
			})
		}
	}
	if len(images) > 0 {
		// Multimodal user message: text part (if any) + one image_url part per
		// image. OpenAI-compatible vision endpoints accept this content array.
		parts := make([]map[string]interface{}, 0, len(images)+1)
		if text.Len() > 0 {
			parts = append(parts, map[string]interface{}{"type": "text", "text": text.String()})
		}
		for _, img := range images {
			parts = append(parts, kiroImageToOpenAIPart(img))
		}
		out = append(out, map[string]interface{}{"role": "user", "content": parts})
	} else if text.Len() > 0 {
		out = append(out, map[string]interface{}{"role": "user", "content": text.String()})
	}
	if len(out) == 0 {
		out = append(out, map[string]interface{}{"role": "user", "content": ""})
	}
	return out
}

// claudeAssistantMessageToOpenAI maps a Claude assistant message, converting
// tool_use blocks into OpenAI tool_calls and concatenating text.
func claudeAssistantMessageToOpenAI(content interface{}) map[string]interface{} {
	msg := map[string]interface{}{"role": "assistant"}
	if s, ok := content.(string); ok {
		msg["content"] = s
		return msg
	}
	blocks, ok := content.([]interface{})
	if !ok {
		msg["content"] = ""
		return msg
	}
	var text strings.Builder
	var toolCalls []map[string]interface{}
	for _, b := range blocks {
		block, ok := b.(map[string]interface{})
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			if t, ok := block["text"].(string); ok {
				text.WriteString(t)
			}
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			args := "{}"
			if raw, err := json.Marshal(block["input"]); err == nil {
				args = string(raw)
			}
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   id,
				"type": "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": args,
				},
			})
		}
	}
	msg["content"] = text.String()
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return msg
}

// claudeToolsToOpenAIMaps converts Claude tool definitions into OpenAI function
// tools. Server tools (web_search, etc., which carry a non-empty Type) are
// dropped — a generic provider can't service them.
func claudeToolsToOpenAIMaps(tools []ClaudeTool) []map[string]interface{} {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		if strings.TrimSpace(t.Type) != "" {
			continue // hosted server tool — skip
		}
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.InputSchema,
			},
		})
	}
	return out
}

// extractClaudeSystemString flattens a Claude `system` field (string or
// []{type,text}) into a single string.
func extractClaudeSystemString(system interface{}) string {
	if system == nil {
		return ""
	}
	if s, ok := system.(string); ok {
		return s
	}
	if blocks, ok := system.([]interface{}); ok {
		var sb strings.Builder
		for _, b := range blocks {
			if block, ok := b.(map[string]interface{}); ok {
				if t, ok := block["text"].(string); ok {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// ============================================================================
// OpenAI SSE -> KiroStreamCallback
// ============================================================================

// openAIStreamChunk is the minimal shape of an OpenAI chat.completion.chunk we
// need to drive the callback.
type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			Reasoning string `json:"reasoning_content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		// Real upstream prompt-cache hit count. OpenAI / DashScope-compatible
		// report it under prompt_tokens_details.cached_tokens; DeepSeek uses the
		// flat prompt_cache_hit_tokens. Either is the genuine cached prefix — we
		// pass it through verbatim (never a local estimate) via OnCacheUsage.
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
	} `json:"usage"`
}

// parseOpenAISSE reads an OpenAI-compatible SSE stream and drives cb. Tool-call
// argument fragments are accumulated by index and flushed as a single OnToolUse
// when the stream ends (or finish_reason=tool_calls), matching how the Kiro path
// emits whole tool_use blocks. Returns an error only on a transport read failure
// (the caller maps that to the retry/cooldown path).
func parseOpenAISSE(r io.Reader, cb *KiroStreamCallback) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	type toolAcc struct {
		id, name string
		args     bytes.Buffer
	}
	tools := map[int]*toolAcc{}
	var order []int
	var stopReason string

	flushTools := func() {
		for _, idx := range order {
			ta := tools[idx]
			if ta == nil || ta.name == "" {
				continue
			}
			var input map[string]interface{}
			argStr := strings.TrimSpace(ta.args.String())
			if argStr == "" {
				argStr = "{}"
			}
			_ = json.Unmarshal([]byte(argStr), &input)
			if input == nil {
				input = map[string]interface{}{}
			}
			if cb.OnToolUse != nil {
				cb.OnToolUse(KiroToolUse{ToolUseID: ta.id, Name: ta.name, Input: input})
			}
		}
		tools = map[int]*toolAcc{}
		order = nil
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // skip malformed lines defensively
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Reasoning != "" && cb.OnText != nil {
				cb.OnText(ch.Delta.Reasoning, true)
			}
			if ch.Delta.Content != "" && cb.OnText != nil {
				cb.OnText(ch.Delta.Content, false)
			}
			for _, tc := range ch.Delta.ToolCalls {
				ta := tools[tc.Index]
				if ta == nil {
					ta = &toolAcc{}
					tools[tc.Index] = ta
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					ta.id = tc.ID
				}
				if tc.Function.Name != "" {
					ta.name = tc.Function.Name
				}
				ta.args.WriteString(tc.Function.Arguments)
			}
			if ch.FinishReason != "" {
				stopReason = mapOpenAIFinishReason(ch.FinishReason)
			}
		}
		if chunk.Usage != nil && cb.OnComplete != nil {
			cb.OnComplete(chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens)
		}
		if chunk.Usage != nil && cb.OnCacheUsage != nil {
			// Pass through the REAL upstream cached-prefix count (never estimated).
			// prompt_tokens_details.cached_tokens is the OpenAI/DashScope-compatible
			// field; prompt_cache_hit_tokens is DeepSeek's flat equivalent.
			cached := 0
			if d := chunk.Usage.PromptTokensDetails; d != nil {
				cached = d.CachedTokens
			}
			if cached == 0 && chunk.Usage.PromptCacheHitTokens > 0 {
				cached = chunk.Usage.PromptCacheHitTokens
			}
			if cached > 0 {
				cb.OnCacheUsage(cached, 0) // read-only providers report no cache-creation
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	flushTools()
	if stopReason != "" && cb.OnStopReason != nil {
		cb.OnStopReason(stopReason)
	}
	return nil
}

// mapOpenAIFinishReason maps an OpenAI finish_reason to the canonical stop reason
// the Kiro response builders expect.
func mapOpenAIFinishReason(fr string) string {
	switch fr {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}
