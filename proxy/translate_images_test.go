package proxy

import (
	"encoding/json"
	"testing"
)

// Story s1: images must be forwarded (not silently dropped) to the generic
// OpenAI / Anthropic / Gemini dialects. A tiny valid base64 PNG payload.
const s1TestPNGB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+P+/HgAFhAJ/wlseKgAAAABJRU5ErkJggg=="

func s1ClaudeImageDataURL() string { return "data:image/png;base64," + s1TestPNGB64 }

// findImagePartOpenAI walks an OpenAI content value (string or []part) and
// reports whether it contains an image_url part with a base64 data URL.
func findImagePartOpenAI(content interface{}) bool {
	parts, ok := content.([]interface{})
	if !ok {
		return false
	}
	for _, p := range parts {
		part, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		if part["type"] != "image_url" {
			continue
		}
		iu, ok := part["image_url"].(map[string]interface{})
		if !ok {
			continue
		}
		if url, ok := iu["url"].(string); ok && len(url) > 0 {
			return true
		}
	}
	return false
}

// TestClaudeToOpenAIForwardsImage: a Claude user message with an image block
// must produce an OpenAI user message carrying an image_url content part.
func TestClaudeToOpenAIForwardsImage(t *testing.T) {
	nr := &NormalizedRequest{
		Model: "openai/gpt-4o",
		Claude: &ClaudeRequest{
			MaxTokens: 100,
			Messages: []ClaudeMessage{{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "what is this?"},
					map[string]interface{}{
						"type": "image",
						"source": map[string]interface{}{
							"type":       "base64",
							"media_type": "image/png",
							"data":       s1TestPNGB64,
						},
					},
				},
			}},
		},
	}
	raw, err := buildOpenAIChatBody(nr, "gpt-4o", true)
	if err != nil {
		t.Fatalf("buildOpenAIChatBody: %v", err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := body["messages"].([]interface{})
	var found bool
	for _, m := range msgs {
		mm := m.(map[string]interface{})
		if mm["role"] == "user" && findImagePartOpenAI(mm["content"]) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an OpenAI user message with an image_url part; body=%s", raw)
	}
}

// TestOpenAIToAnthropicForwardsImage: an OpenAI multimodal user message must
// produce an Anthropic image block.
func TestOpenAIToAnthropicForwardsImage(t *testing.T) {
	nr := &NormalizedRequest{
		Model: "anthropic/claude-sonnet-4.5",
		OpenAI: &OpenAIRequest{
			Messages: []OpenAIMessage{{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "describe"},
					map[string]interface{}{
						"type":      "image_url",
						"image_url": map[string]interface{}{"url": s1ClaudeImageDataURL()},
					},
				},
			}},
		},
	}
	raw, err := buildAnthropicBody(nr, "claude-sonnet-4.5", true)
	if err != nil {
		t.Fatalf("buildAnthropicBody: %v", err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := body["messages"].([]interface{})
	var found bool
	for _, m := range msgs {
		mm := m.(map[string]interface{})
		blocks, ok := mm["content"].([]interface{})
		if !ok {
			continue
		}
		for _, b := range blocks {
			blk := b.(map[string]interface{})
			if blk["type"] == "image" {
				src, _ := blk["source"].(map[string]interface{})
				if src != nil && src["data"] == s1TestPNGB64 {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("expected an Anthropic image block with base64 data; body=%s", raw)
	}
}

// TestClaudeToGeminiForwardsImage: a Claude image block must become a Gemini
// inlineData part.
func TestClaudeToGeminiForwardsImage(t *testing.T) {
	nr := &NormalizedRequest{
		Model: "gemini/gemini-2.5-flash",
		Claude: &ClaudeRequest{
			MaxTokens: 100,
			Messages: []ClaudeMessage{{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "caption"},
					map[string]interface{}{
						"type": "image",
						"source": map[string]interface{}{
							"type":       "base64",
							"media_type": "image/png",
							"data":       s1TestPNGB64,
						},
					},
				},
			}},
		},
	}
	raw, err := buildGeminiBody(nr, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("buildGeminiBody: %v", err)
	}
	if !geminiBodyHasInlineImage(t, raw) {
		t.Fatalf("expected a Gemini inlineData image part; body=%s", raw)
	}
}

// TestOpenAIToGeminiForwardsImage: an OpenAI multimodal user message must become
// a Gemini inlineData part.
func TestOpenAIToGeminiForwardsImage(t *testing.T) {
	nr := &NormalizedRequest{
		Model: "gemini/gemini-2.5-flash",
		OpenAI: &OpenAIRequest{
			Messages: []OpenAIMessage{{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "caption"},
					map[string]interface{}{
						"type":      "image_url",
						"image_url": map[string]interface{}{"url": s1ClaudeImageDataURL()},
					},
				},
			}},
		},
	}
	raw, err := buildGeminiBody(nr, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("buildGeminiBody: %v", err)
	}
	if !geminiBodyHasInlineImage(t, raw) {
		t.Fatalf("expected a Gemini inlineData image part; body=%s", raw)
	}
}

func geminiBodyHasInlineImage(t *testing.T, raw []byte) bool {
	t.Helper()
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	contents, _ := body["contents"].([]interface{})
	for _, c := range contents {
		cm := c.(map[string]interface{})
		parts, _ := cm["parts"].([]interface{})
		for _, p := range parts {
			pm := p.(map[string]interface{})
			if inline, ok := pm["inlineData"].(map[string]interface{}); ok {
				if inline["data"] == s1TestPNGB64 {
					return true
				}
			}
		}
	}
	return false
}
