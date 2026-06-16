package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// ============================================================================
// Anthropic Messages dialect (for backends like anthropic, glm, kimi, minimax).
//
// For a Claude client this is a near-passthrough (swap model + auth only). For an
// OpenAI client we up-convert messages/tools into the Anthropic Messages shape.
// The RESPONSE side parses Anthropic's SSE event stream into KiroStreamCallback.
// ============================================================================

// buildAnthropicBody constructs an Anthropic /v1/messages request body.
func buildAnthropicBody(nr *NormalizedRequest, upstreamModel string, stream bool) ([]byte, error) {
	body := map[string]interface{}{
		"model":  upstreamModel,
		"stream": stream,
	}

	switch {
	case nr.Claude != nil:
		req := nr.Claude
		maxTokens := req.MaxTokens
		if maxTokens <= 0 {
			maxTokens = 4096
		}
		body["max_tokens"] = maxTokens
		if req.Temperature != 0 {
			body["temperature"] = req.Temperature
		}
		if req.TopP != 0 {
			body["top_p"] = req.TopP
		}
		if sys := extractClaudeSystemString(req.System); sys != "" {
			body["system"] = sys
		}
		// Messages pass through as-is (already Anthropic shape).
		body["messages"] = req.Messages
		if tools := claudeToolsPassthrough(req.Tools); len(tools) > 0 {
			body["tools"] = tools
			if ti, ok := parseClaudeToolChoice(req.ToolChoice); ok {
				body["tool_choice"] = ti.toAnthropic()
			}
		}
	case nr.OpenAI != nil:
		req := nr.OpenAI
		maxTokens := req.MaxTokens
		if maxTokens <= 0 {
			maxTokens = 4096
		}
		body["max_tokens"] = maxTokens
		if req.Temperature != 0 {
			body["temperature"] = req.Temperature
		}
		sys, msgs := openAIToAnthropicMessages(req.Messages)
		if sys != "" {
			body["system"] = sys
		}
		body["messages"] = msgs
		if tools := openAIToolsToAnthropic(req.Tools); len(tools) > 0 {
			body["tools"] = tools
			if ti, ok := parseOpenAIToolChoice(req.ToolChoice); ok {
				body["tool_choice"] = ti.toAnthropic()
			}
		}
	}

	return json.Marshal(body)
}

// claudeToolsPassthrough forwards user-defined Claude tools (dropping hosted
// server tools) in the Anthropic tool shape {name, description, input_schema}.
func claudeToolsPassthrough(tools []ClaudeTool) []map[string]interface{} {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		if strings.TrimSpace(t.Type) != "" {
			continue
		}
		out = append(out, map[string]interface{}{
			"name":         t.Name,
			"description":  t.Description,
			"input_schema": t.InputSchema,
		})
	}
	return out
}

// openAIToAnthropicMessages converts OpenAI chat messages into an Anthropic
// system string + messages array. Assistant tool_calls become tool_use blocks;
// tool-role messages become user tool_result blocks.
func openAIToAnthropicMessages(msgs []OpenAIMessage) (string, []map[string]interface{}) {
	var system strings.Builder
	var out []map[string]interface{}

	for _, m := range msgs {
		switch m.Role {
		case "system":
			if s := extractOpenAIMessageText(m.Content); s != "" {
				if system.Len() > 0 {
					system.WriteString("\n")
				}
				system.WriteString(s)
			}
		case "tool":
			out = append(out, map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{{
					"type":        "tool_result",
					"tool_use_id": m.ToolCallID,
					"content":     extractOpenAIMessageText(m.Content),
				}},
			})
		case "assistant":
			var blocks []map[string]interface{}
			if txt := extractOpenAIMessageText(m.Content); txt != "" {
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": txt})
			}
			for _, tc := range m.ToolCalls {
				var input map[string]interface{}
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
				if input == nil {
					input = map[string]interface{}{}
				}
				blocks = append(blocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": input,
				})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": ""})
			}
			out = append(out, map[string]interface{}{"role": "assistant", "content": blocks})
		default: // user
			imgs := extractOpenAIImages(m.Content)
			if len(imgs) > 0 {
				// Multimodal user turn: text block (if any) + one image block per
				// image, in the Anthropic content-array shape.
				blocks := make([]map[string]interface{}, 0, len(imgs)+1)
				if txt := extractOpenAIMessageText(m.Content); txt != "" {
					blocks = append(blocks, map[string]interface{}{"type": "text", "text": txt})
				}
				for _, img := range imgs {
					blocks = append(blocks, kiroImageToAnthropicBlock(img))
				}
				out = append(out, map[string]interface{}{"role": "user", "content": blocks})
			} else {
				out = append(out, map[string]interface{}{
					"role":    "user",
					"content": extractOpenAIMessageText(m.Content),
				})
			}
		}
	}
	return system.String(), out
}

func openAIToolsToAnthropic(tools []OpenAITool) []map[string]interface{} {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]interface{}{
			"name":         t.Function.Name,
			"description":  t.Function.Description,
			"input_schema": t.Function.Parameters,
		})
	}
	return out
}

// parseAnthropicSSE reads an Anthropic Messages SSE stream and drives cb. It
// tracks content blocks: text_delta -> OnText, thinking_delta -> OnText(thinking),
// and tool_use blocks (content_block_start + input_json_delta fragments) ->
// OnToolUse on content_block_stop. message_delta carries the stop_reason + output
// token usage.
func parseAnthropicSSE(r io.Reader, cb *KiroStreamCallback) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	type blockState struct {
		typ      string
		toolID   string
		toolName string
		jsonBuf  strings.Builder
	}
	blocks := map[int]*blockState{}
	var inputTokens, outputTokens, cacheRead, cacheCreation int

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var ev map[string]interface{}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		switch ev["type"] {
		case "message_start":
			if msg, ok := ev["message"].(map[string]interface{}); ok {
				if usage, ok := msg["usage"].(map[string]interface{}); ok {
					inputTokens = intFromAny(usage["input_tokens"])
					// Real upstream prompt-cache counts (Anthropic-compatible hosts).
					// Passed through verbatim — never a local estimate.
					if v := intFromAny(usage["cache_read_input_tokens"]); v > 0 {
						cacheRead = v
					}
					if v := intFromAny(usage["cache_creation_input_tokens"]); v > 0 {
						cacheCreation = v
					}
				}
			}
		case "content_block_start":
			idx := intFromAny(ev["index"])
			bs := &blockState{}
			if cbk, ok := ev["content_block"].(map[string]interface{}); ok {
				bs.typ, _ = cbk["type"].(string)
				bs.toolID, _ = cbk["id"].(string)
				bs.toolName, _ = cbk["name"].(string)
			}
			blocks[idx] = bs
		case "content_block_delta":
			idx := intFromAny(ev["index"])
			bs := blocks[idx]
			delta, _ := ev["delta"].(map[string]interface{})
			if delta == nil {
				continue
			}
			switch delta["type"] {
			case "text_delta":
				if t, ok := delta["text"].(string); ok && cb.OnText != nil {
					cb.OnText(t, false)
				}
			case "thinking_delta":
				if t, ok := delta["thinking"].(string); ok && cb.OnText != nil {
					cb.OnText(t, true)
				}
			case "input_json_delta":
				if bs != nil {
					if pj, ok := delta["partial_json"].(string); ok {
						bs.jsonBuf.WriteString(pj)
					}
				}
			}
		case "content_block_stop":
			idx := intFromAny(ev["index"])
			bs := blocks[idx]
			if bs != nil && bs.typ == "tool_use" && cb.OnToolUse != nil {
				var input map[string]interface{}
				js := strings.TrimSpace(bs.jsonBuf.String())
				if js == "" {
					js = "{}"
				}
				_ = json.Unmarshal([]byte(js), &input)
				if input == nil {
					input = map[string]interface{}{}
				}
				cb.OnToolUse(KiroToolUse{ToolUseID: bs.toolID, Name: bs.toolName, Input: input})
			}
			delete(blocks, idx)
		case "message_delta":
			if delta, ok := ev["delta"].(map[string]interface{}); ok {
				if sr, ok := delta["stop_reason"].(string); ok && sr != "" && cb.OnStopReason != nil {
					cb.OnStopReason(sr)
				}
			}
			if usage, ok := ev["usage"].(map[string]interface{}); ok {
				outputTokens = intFromAny(usage["output_tokens"])
				// Anthropic also re-reports cache counts on message_delta usage on
				// some hosts; keep the latest non-zero values.
				if v := intFromAny(usage["cache_read_input_tokens"]); v > 0 {
					cacheRead = v
				}
				if v := intFromAny(usage["cache_creation_input_tokens"]); v > 0 {
					cacheCreation = v
				}
			}
		case "message_stop":
			// terminal
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if inputTokens > 0 || outputTokens > 0 || cacheRead > 0 || cacheCreation > 0 {
		// Anthropic reports no reasoning split; output_tokens stands alone.
		fireUpstreamUsage(cb, UpstreamUsage{
			InputTokens:         inputTokens,
			OutputTokens:        outputTokens,
			CacheReadTokens:     cacheRead,
			CacheCreationTokens: cacheCreation,
			HasRealCounts:       true,
		})
	}
	return nil
}

func intFromAny(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}
