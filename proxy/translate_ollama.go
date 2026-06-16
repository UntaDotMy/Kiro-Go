package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// ============================================================================
// Ollama dialect translation (/api/chat, NDJSON stream).
//
// Ollama's chat endpoint takes a messages array shaped like OpenAI's but in its
// own envelope: {model, messages, stream, tools?, options?}. The response is
// newline-delimited JSON (NDJSON), one object per line, each carrying a
// message.content delta and a final object with done:true + token counts. Ported
// from 9router's open-sse Ollama executor.
// ============================================================================

// buildOllamaChatBody converts a NormalizedRequest into an Ollama /api/chat body.
// We reuse the OpenAI message/tool conversion (Ollama accepts the same message
// shape) and re-wrap into Ollama's envelope.
func buildOllamaChatBody(nr *NormalizedRequest, upstreamModel string) ([]byte, error) {
	var messages []map[string]interface{}
	var tools []map[string]interface{}
	options := map[string]interface{}{}

	switch {
	case nr.OpenAI != nil:
		req := nr.OpenAI
		messages = openAIMessagesToMaps(req.Messages)
		tools = openAIToolsToMaps(req.Tools)
		if req.Temperature != 0 {
			options["temperature"] = req.Temperature
		}
		if req.TopP != 0 {
			options["top_p"] = req.TopP
		}
		if req.MaxTokens != 0 {
			options["num_predict"] = req.MaxTokens
		}
	case nr.Claude != nil:
		req := nr.Claude
		messages = claudeToOpenAIMessages(req)
		tools = claudeToolsToOpenAIMaps(req.Tools)
		if req.Temperature != 0 {
			options["temperature"] = req.Temperature
		}
		if req.TopP != 0 {
			options["top_p"] = req.TopP
		}
		if req.MaxTokens != 0 {
			options["num_predict"] = req.MaxTokens
		}
	}

	body := map[string]interface{}{
		"model":    upstreamModel,
		"messages": messages,
		"stream":   true,
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if len(options) > 0 {
		body["options"] = options
	}
	return json.Marshal(body)
}

// ollamaStreamChunk is one NDJSON object from /api/chat.
type ollamaStreamChunk struct {
	Message struct {
		Content   string `json:"content"`
		Thinking  string `json:"thinking"`
		ToolCalls []struct {
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
}

// parseOllamaStream reads an Ollama NDJSON chat stream and drives cb. Each line is
// a complete JSON object; tool calls arrive whole (Ollama does not fragment
// arguments the way OpenAI does), so they're emitted immediately.
func parseOllamaStream(r io.Reader, cb *KiroStreamCallback) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var stopReason string
	toolIdx := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var chunk ollamaStreamChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue // skip malformed lines defensively
		}
		if chunk.Message.Thinking != "" && cb.OnText != nil {
			cb.OnText(chunk.Message.Thinking, true)
		}
		if chunk.Message.Content != "" && cb.OnText != nil {
			cb.OnText(chunk.Message.Content, false)
		}
		for _, tc := range chunk.Message.ToolCalls {
			if tc.Function.Name == "" {
				continue
			}
			var input map[string]interface{}
			if len(tc.Function.Arguments) > 0 {
				_ = json.Unmarshal(tc.Function.Arguments, &input)
			}
			if input == nil {
				input = map[string]interface{}{}
			}
			if cb.OnToolUse != nil {
				cb.OnToolUse(KiroToolUse{ToolUseID: ollamaToolID(toolIdx), Name: tc.Function.Name, Input: input})
			}
			toolIdx++
			stopReason = "tool_use"
		}
		if chunk.Done {
			if stopReason == "" {
				stopReason = mapOllamaDoneReason(chunk.DoneReason)
			}
			fireUpstreamUsage(cb, UpstreamUsage{
				InputTokens:   chunk.PromptEvalCount,
				OutputTokens:  chunk.EvalCount,
				HasRealCounts: true,
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if stopReason != "" && cb.OnStopReason != nil {
		cb.OnStopReason(stopReason)
	}
	return nil
}

func mapOllamaDoneReason(r string) string {
	switch r {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

// ollamaToolID synthesizes a stable tool-call id (Ollama doesn't supply one).
func ollamaToolID(idx int) string {
	return "ollama_tool_" + itoa(idx)
}
