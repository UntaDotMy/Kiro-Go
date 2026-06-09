package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// ============================================================================
// Google Gemini generateContent dialect.
//
// Request: {contents:[{role, parts:[{text}|{functionCall}|{functionResponse}]}],
//           systemInstruction, tools:[{functionDeclarations:[...]}],
//           generationConfig:{maxOutputTokens, temperature, topP}}.
// Roles: Gemini uses "user" and "model" (not "assistant").
// Response SSE: each data line is a GenerateContentResponse with
//   candidates[0].content.parts[] (text or functionCall) and a usageMetadata.
// ============================================================================

// buildGeminiBody converts a NormalizedRequest into a Gemini generateContent body.
func buildGeminiBody(nr *NormalizedRequest, upstreamModel string) ([]byte, error) {
	body := map[string]interface{}{}
	genCfg := map[string]interface{}{}

	addGen := func(maxTokens int, temp, topP float64) {
		if maxTokens > 0 {
			genCfg["maxOutputTokens"] = maxTokens
		}
		if temp != 0 {
			genCfg["temperature"] = temp
		}
		if topP != 0 {
			genCfg["topP"] = topP
		}
	}

	switch {
	case nr.OpenAI != nil:
		req := nr.OpenAI
		addGen(req.MaxTokens, req.Temperature, req.TopP)
		sys, contents := openAIToGeminiContents(req.Messages)
		if sys != "" {
			body["systemInstruction"] = map[string]interface{}{"parts": []map[string]interface{}{{"text": sys}}}
		}
		body["contents"] = contents
		if tools := openAIToolsToGemini(req.Tools); tools != nil {
			body["tools"] = tools
		}
	case nr.Claude != nil:
		req := nr.Claude
		addGen(req.MaxTokens, req.Temperature, req.TopP)
		if sys := extractClaudeSystemString(req.System); sys != "" {
			body["systemInstruction"] = map[string]interface{}{"parts": []map[string]interface{}{{"text": sys}}}
		}
		body["contents"] = claudeToGeminiContents(req.Messages)
		if tools := claudeToolsToGemini(req.Tools); tools != nil {
			body["tools"] = tools
		}
	}

	if len(genCfg) > 0 {
		body["generationConfig"] = genCfg
	}
	return json.Marshal(body)
}

func openAIToGeminiContents(msgs []OpenAIMessage) (string, []map[string]interface{}) {
	var system strings.Builder
	var contents []map[string]interface{}
	for _, m := range msgs {
		switch m.Role {
		case "system":
			if s := extractOpenAIMessageText(m.Content); s != "" {
				if system.Len() > 0 {
					system.WriteString("\n")
				}
				system.WriteString(s)
			}
		case "assistant":
			parts := []map[string]interface{}{}
			if txt := extractOpenAIMessageText(m.Content); txt != "" {
				parts = append(parts, map[string]interface{}{"text": txt})
			}
			for _, tc := range m.ToolCalls {
				var args map[string]interface{}
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
				parts = append(parts, map[string]interface{}{
					"functionCall": map[string]interface{}{"name": tc.Function.Name, "args": args},
				})
			}
			if len(parts) == 0 {
				parts = append(parts, map[string]interface{}{"text": ""})
			}
			contents = append(contents, map[string]interface{}{"role": "model", "parts": parts})
		case "tool":
			contents = append(contents, map[string]interface{}{
				"role": "user",
				"parts": []map[string]interface{}{{
					"functionResponse": map[string]interface{}{
						"name":     m.ToolCallID,
						"response": map[string]interface{}{"content": extractOpenAIMessageText(m.Content)},
					},
				}},
			})
		default: // user
			contents = append(contents, map[string]interface{}{
				"role":  "user",
				"parts": []map[string]interface{}{{"text": extractOpenAIMessageText(m.Content)}},
			})
		}
	}
	return system.String(), contents
}

func claudeToGeminiContents(msgs []ClaudeMessage) []map[string]interface{} {
	var contents []map[string]interface{}
	for _, m := range msgs {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		text := ""
		if s, ok := m.Content.(string); ok {
			text = s
		} else if blocks, ok := m.Content.([]interface{}); ok {
			var sb strings.Builder
			for _, b := range blocks {
				if blk, ok := b.(map[string]interface{}); ok {
					if t, ok := blk["text"].(string); ok {
						sb.WriteString(t)
					}
				}
			}
			text = sb.String()
		}
		contents = append(contents, map[string]interface{}{
			"role":  role,
			"parts": []map[string]interface{}{{"text": text}},
		})
	}
	return contents
}

func openAIToolsToGemini(tools []OpenAITool) []map[string]interface{} {
	if len(tools) == 0 {
		return nil
	}
	decls := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		decls = append(decls, map[string]interface{}{
			"name":        t.Function.Name,
			"description": t.Function.Description,
			"parameters":  t.Function.Parameters,
		})
	}
	return []map[string]interface{}{{"functionDeclarations": decls}}
}

func claudeToolsToGemini(tools []ClaudeTool) []map[string]interface{} {
	if len(tools) == 0 {
		return nil
	}
	decls := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		if strings.TrimSpace(t.Type) != "" {
			continue
		}
		decls = append(decls, map[string]interface{}{
			"name":        t.Name,
			"description": t.Description,
			"parameters":  t.InputSchema,
		})
	}
	if len(decls) == 0 {
		return nil
	}
	return []map[string]interface{}{{"functionDeclarations": decls}}
}

// parseGeminiSSE reads a Gemini streamGenerateContent SSE stream (alt=sse) and
// drives cb. Each data line is a GenerateContentResponse; parts carry text or a
// functionCall (emitted whole, so OnToolUse fires immediately). usageMetadata on
// the final chunk supplies token counts; finishReason maps to the stop reason.
func parseGeminiSSE(r io.Reader, cb *KiroStreamCallback) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var inputTokens, outputTokens int
	var stopReason string
	toolSeq := 0

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var resp struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text         string `json:"text"`
						FunctionCall *struct {
							Name string                 `json:"name"`
							Args map[string]interface{} `json:"args"`
						} `json:"functionCall"`
					} `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			UsageMetadata *struct {
				PromptTokenCount     int `json:"promptTokenCount"`
				CandidatesTokenCount int `json:"candidatesTokenCount"`
			} `json:"usageMetadata"`
		}
		if err := json.Unmarshal([]byte(data), &resp); err != nil {
			continue
		}
		for _, cand := range resp.Candidates {
			for _, part := range cand.Content.Parts {
				if part.Text != "" && cb.OnText != nil {
					cb.OnText(part.Text, false)
				}
				if part.FunctionCall != nil && cb.OnToolUse != nil {
					input := part.FunctionCall.Args
					if input == nil {
						input = map[string]interface{}{}
					}
					toolSeq++
					cb.OnToolUse(KiroToolUse{
						ToolUseID: "call_" + part.FunctionCall.Name,
						Name:      part.FunctionCall.Name,
						Input:     input,
					})
				}
			}
			if cand.FinishReason != "" {
				stopReason = mapGeminiFinishReason(cand.FinishReason)
			}
		}
		if resp.UsageMetadata != nil {
			inputTokens = resp.UsageMetadata.PromptTokenCount
			outputTokens = resp.UsageMetadata.CandidatesTokenCount
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if cb.OnComplete != nil && (inputTokens > 0 || outputTokens > 0) {
		cb.OnComplete(inputTokens, outputTokens)
	}
	if stopReason != "" && cb.OnStopReason != nil {
		cb.OnStopReason(stopReason)
	}
	return nil
}

func mapGeminiFinishReason(fr string) string {
	switch strings.ToUpper(fr) {
	case "STOP":
		return "end_turn"
	case "MAX_TOKENS":
		return "max_tokens"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
		return "refusal"
	default:
		return "end_turn"
	}
}
