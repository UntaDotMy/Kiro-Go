package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
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
			if ti, ok := parseOpenAIToolChoice(req.ToolChoice); ok {
				if tc := ti.toGeminiToolConfig(); tc != nil {
					body["toolConfig"] = tc
				}
			}
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
			if ti, ok := parseClaudeToolChoice(req.ToolChoice); ok {
				if tc := ti.toGeminiToolConfig(); tc != nil {
					body["toolConfig"] = tc
				}
			}
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
	// Gemini's functionResponse.name must be the FUNCTION name, but an OpenAI
	// tool-role message only carries the tool_call_id. Map id -> name from the
	// preceding assistant tool_calls so the response resolves to the right name.
	idToName := map[string]string{}
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
				if tc.ID != "" && tc.Function.Name != "" {
					idToName[tc.ID] = tc.Function.Name
				}
				var args map[string]interface{}
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
				if args == nil {
					args = map[string]interface{}{}
				}
				parts = append(parts, map[string]interface{}{
					"functionCall": map[string]interface{}{"name": tc.Function.Name, "args": args},
				})
			}
			if len(parts) == 0 {
				parts = append(parts, map[string]interface{}{"text": ""})
			}
			contents = append(contents, map[string]interface{}{"role": "model", "parts": parts})
		case "tool":
			name := idToName[m.ToolCallID]
			if name == "" {
				name = m.ToolCallID // fallback: better than empty
			}
			contents = append(contents, map[string]interface{}{
				"role": "user",
				"parts": []map[string]interface{}{{
					"functionResponse": map[string]interface{}{
						"name":     name,
						"response": geminiFunctionResponsePayload(extractOpenAIMessageText(m.Content)),
					},
				}},
			})
		default: // user
			parts := []map[string]interface{}{}
			if txt := extractOpenAIMessageText(m.Content); txt != "" {
				parts = append(parts, map[string]interface{}{"text": txt})
			}
			for _, img := range extractOpenAIImages(m.Content) {
				parts = append(parts, kiroImageToGeminiPart(img))
			}
			if len(parts) == 0 {
				parts = append(parts, map[string]interface{}{"text": ""})
			}
			contents = append(contents, map[string]interface{}{
				"role":  "user",
				"parts": parts,
			})
		}
	}
	return system.String(), contents
}

// claudeToGeminiContents converts Claude messages into Gemini contents,
// preserving the full tool-calling protocol: assistant tool_use blocks become
// functionCall parts and user tool_result blocks become functionResponse parts.
// (The earlier version dropped both, so any multi-turn tool conversation routed
// to a Gemini provider lost its entire tool history and the model re-asked or
// hallucinated.) Gemini's functionResponse.name must be the FUNCTION name, so we
// map tool_use_id -> name from the assistant tool_use blocks as we walk forward.
func claudeToGeminiContents(msgs []ClaudeMessage) []map[string]interface{} {
	var contents []map[string]interface{}
	idToName := map[string]string{}

	for _, m := range msgs {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}

		// Plain string content: a single text part.
		if s, ok := m.Content.(string); ok {
			contents = append(contents, map[string]interface{}{
				"role":  role,
				"parts": []map[string]interface{}{{"text": s}},
			})
			continue
		}

		blocks, ok := m.Content.([]interface{})
		if !ok {
			contents = append(contents, map[string]interface{}{
				"role":  role,
				"parts": []map[string]interface{}{{"text": ""}},
			})
			continue
		}

		var parts []map[string]interface{}
		for _, b := range blocks {
			blk, ok := b.(map[string]interface{})
			if !ok {
				continue
			}
			switch blk["type"] {
			case "text":
				if t, ok := blk["text"].(string); ok && t != "" {
					parts = append(parts, map[string]interface{}{"text": t})
				}
			case "image", "image_url", "input_image":
				// Forward vision input as a Gemini inlineData part instead of
				// dropping it.
				if img := extractImageFromClaudeBlock(blk); img != nil {
					parts = append(parts, kiroImageToGeminiPart(*img))
				}
			case "tool_use":
				id, _ := blk["id"].(string)
				name, _ := blk["name"].(string)
				if id != "" && name != "" {
					idToName[id] = name
				}
				args, _ := blk["input"].(map[string]interface{})
				if args == nil {
					args = map[string]interface{}{}
				}
				parts = append(parts, map[string]interface{}{
					"functionCall": map[string]interface{}{"name": name, "args": args},
				})
			case "tool_result":
				useID, _ := blk["tool_use_id"].(string)
				name := idToName[useID]
				if name == "" {
					name = useID // fallback
				}
				parts = append(parts, map[string]interface{}{
					"functionResponse": map[string]interface{}{
						"name":     name,
						"response": geminiFunctionResponsePayload(extractToolResultText(blk["content"])),
					},
				})
			}
		}
		if len(parts) == 0 {
			parts = append(parts, map[string]interface{}{"text": ""})
		}
		contents = append(contents, map[string]interface{}{"role": role, "parts": parts})
	}
	return contents
}

// geminiFunctionResponsePayload wraps a tool-result string in the object Gemini's
// functionResponse.response field expects. Gemini requires a JSON object here; if
// the result text is itself a JSON object we forward it as-is, otherwise we wrap
// it under a "content" key so a bare string/array is still valid.
func geminiFunctionResponsePayload(text string) map[string]interface{} {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") {
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(trimmed), &obj); err == nil && obj != nil {
			return obj
		}
	}
	return map[string]interface{}{"content": text}
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
			// Gemini rejects unsupported JSON-Schema keywords; sanitize so the whole
			// request doesn't 400 on a stock OpenAI/Claude-Code tool schema.
			"parameters": geminiParametersOrEmpty(t.Function.Parameters),
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
			// See openAIToolsToGemini: Gemini's schema validator is strict.
			"parameters": geminiParametersOrEmpty(t.InputSchema),
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

	var inputTokens, outputTokens, cachedTokens, thoughtTokens int
	var stopReason string
	toolSeq := 0
	// Native grounding (web search) accumulators. groundingMetadata is documented
	// to arrive on the final chunk (like usageMetadata), but placement can vary,
	// so we capture the LAST non-empty occurrence across the whole stream rather
	// than assuming a fixed chunk — correct regardless of where Gemini emits it.
	var groundingResults []WebSearchResult
	var groundingQuery string

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
				// Native Google Search grounding. groundingChunks[].web.{uri,title}
				// are the cited sources; webSearchQueries[] are the queries Gemini
				// ran. Parsed exactly as gemini-cli's web-search tool reads them.
				GroundingMetadata *struct {
					GroundingChunks []struct {
						Web *struct {
							URI   string `json:"uri"`
							Title string `json:"title"`
						} `json:"web"`
					} `json:"groundingChunks"`
					WebSearchQueries []string `json:"webSearchQueries"`
				} `json:"groundingMetadata"`
			} `json:"candidates"`
			UsageMetadata *struct {
				PromptTokenCount        int `json:"promptTokenCount"`
				CandidatesTokenCount    int `json:"candidatesTokenCount"`
				CachedContentTokenCount int `json:"cachedContentTokenCount"`
				ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
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
					// Gemini function calls carry no id, so we synthesize a UNIQUE one
					// per call. Including the sequence number is essential: two parallel
					// calls to the SAME function would otherwise collide on
					// "call_<name>", and the client could not match each tool_result
					// back to its tool_use.
					cb.OnToolUse(KiroToolUse{
						ToolUseID: fmt.Sprintf("call_%s_%d", part.FunctionCall.Name, toolSeq),
						Name:      part.FunctionCall.Name,
						Input:     input,
					})
				}
			}
			if cand.FinishReason != "" {
				stopReason = mapGeminiFinishReason(cand.FinishReason)
			}
			// Capture grounding sources from this candidate. Keep the LAST chunk
			// that actually carries chunks (later chunks supersede earlier partials).
			if gm := cand.GroundingMetadata; gm != nil && len(gm.GroundingChunks) > 0 {
				results := make([]WebSearchResult, 0, len(gm.GroundingChunks))
				for _, ch := range gm.GroundingChunks {
					if ch.Web == nil || strings.TrimSpace(ch.Web.URI) == "" {
						continue
					}
					results = append(results, WebSearchResult{
						Title: strings.TrimSpace(ch.Web.Title),
						URL:   strings.TrimSpace(ch.Web.URI),
					})
				}
				if len(results) > 0 {
					groundingResults = results
					if len(gm.WebSearchQueries) > 0 {
						groundingQuery = strings.Join(gm.WebSearchQueries, "; ")
					}
				}
			}
		}
		if resp.UsageMetadata != nil {
			inputTokens = resp.UsageMetadata.PromptTokenCount
			outputTokens = resp.UsageMetadata.CandidatesTokenCount
			thoughtTokens = resp.UsageMetadata.ThoughtsTokenCount
			if resp.UsageMetadata.CachedContentTokenCount > 0 {
				cachedTokens = resp.UsageMetadata.CachedContentTokenCount
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if inputTokens > 0 || outputTokens > 0 || cachedTokens > 0 || thoughtTokens > 0 {
		// candidatesTokenCount EXCLUDES thoughts, so fold thoughts into Output to
		// honor the contract (Output includes reasoning) and surface thoughts as
		// the reasoning subset. cachedContentTokenCount is the real cache hit.
		fireUpstreamUsage(cb, UpstreamUsage{
			InputTokens:     inputTokens,
			OutputTokens:    outputTokens + thoughtTokens,
			CacheReadTokens: cachedTokens,
			ReasoningTokens: thoughtTokens,
			HasRealCounts:   true,
		})
	}
	if len(groundingResults) > 0 && cb.OnWebSearchResults != nil {
		cb.OnWebSearchResults(groundingQuery, groundingResults)
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
