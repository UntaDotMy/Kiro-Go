package proxy

// ============================================================================
// Image forwarding for the generic (non-Kiro) dialects.
//
// Vision INPUT used to be silently dropped on every generic backend: the
// Claude/OpenAI -> {OpenAI,Anthropic,Gemini} converters extracted only text,
// tool_use and tool_result, so a multimodal request routed to OpenAI / Anthropic
// / Gemini / OpenRouter / Groq etc. lost its images with no error. Only the Kiro
// path forwarded images.
//
// These helpers reuse the existing KiroImage extractors (extractImageFromClaudeBlock
// / extractImageFromOpenAIPart, which already do data-URL parsing, base64
// validation and format normalization) and re-emit each image in the target
// dialect's native content shape so the four cross-dialect paths keep multimodal
// parity with the Kiro path. The two same-dialect paths (Claude->Anthropic,
// OpenAI->OpenAI) already forward images verbatim and are unchanged.
// ============================================================================

// kiroImageToOpenAIPart renders a KiroImage as an OpenAI Chat Completions
// image_url content part carrying a data URL.
func kiroImageToOpenAIPart(img KiroImage) map[string]interface{} {
	return map[string]interface{}{
		"type": "image_url",
		"image_url": map[string]interface{}{
			"url": "data:image/" + img.Format + ";base64," + img.Source.Bytes,
		},
	}
}

// kiroImageToAnthropicBlock renders a KiroImage as an Anthropic Messages image
// block with a base64 source.
func kiroImageToAnthropicBlock(img KiroImage) map[string]interface{} {
	return map[string]interface{}{
		"type": "image",
		"source": map[string]interface{}{
			"type":       "base64",
			"media_type": "image/" + img.Format,
			"data":       img.Source.Bytes,
		},
	}
}

// kiroImageToGeminiPart renders a KiroImage as a Gemini generateContent
// inlineData part.
func kiroImageToGeminiPart(img KiroImage) map[string]interface{} {
	return map[string]interface{}{
		"inlineData": map[string]interface{}{
			"mimeType": "image/" + img.Format,
			"data":     img.Source.Bytes,
		},
	}
}

// extractClaudeImages pulls every image block out of a Claude message's content
// (the []interface{} block form). String content carries no images.
func extractClaudeImages(content interface{}) []KiroImage {
	blocks, ok := content.([]interface{})
	if !ok {
		return nil
	}
	var imgs []KiroImage
	for _, b := range blocks {
		block, ok := b.(map[string]interface{})
		if !ok {
			continue
		}
		switch block["type"] {
		case "image", "image_url", "input_image":
			if img := extractImageFromClaudeBlock(block); img != nil {
				imgs = append(imgs, *img)
			}
		}
	}
	return imgs
}

// extractOpenAIImages pulls every image out of an OpenAI message's content (the
// multimodal []interface{} part form). String content carries no images.
func extractOpenAIImages(content interface{}) []KiroImage {
	parts, ok := content.([]interface{})
	if !ok {
		return nil
	}
	var imgs []KiroImage
	for _, p := range parts {
		part, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		if img := extractImageFromOpenAIPart(part); img != nil {
			imgs = append(imgs, *img)
		}
	}
	return imgs
}
