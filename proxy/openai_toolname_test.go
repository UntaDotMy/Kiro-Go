package proxy

import "testing"

// Story s6: convertOpenAITools must apply the SAME sanitizeToolName +
// shortenToolName pipeline as convertClaudeTools and return a restore map, so
// underscored OpenAI tool names don't reach Kiro verbatim (400 risk) and any
// altered name can be restored to the client's original in the response.

func TestConvertOpenAIToolsSanitizesNames(t *testing.T) {
	tools := []OpenAITool{
		{Type: "function"},
		{Type: "function"},
	}
	tools[0].Function.Name = "get_weather"
	tools[0].Function.Description = "weather"
	tools[1].Function.Name = "list-files"
	tools[1].Function.Description = "fs"

	specs, nameMap := convertOpenAITools(tools)
	if len(specs) != 2 {
		t.Fatalf("expected 2 tool specs, got %d", len(specs))
	}

	// Names sent to Kiro must be pure camelCase (no underscores/dashes).
	for _, s := range specs {
		name := s.ToolSpecification.Name
		for _, r := range name {
			if r == '_' || r == '-' {
				t.Fatalf("sanitized tool name %q still contains a separator", name)
			}
		}
	}

	// The sanitized names match what the Claude path would produce.
	if specs[0].ToolSpecification.Name != "getWeather" {
		t.Errorf("get_weather should sanitize to getWeather, got %q", specs[0].ToolSpecification.Name)
	}
	if specs[1].ToolSpecification.Name != "listFiles" {
		t.Errorf("list-files should sanitize to listFiles, got %q", specs[1].ToolSpecification.Name)
	}

	// The restore map maps sanitized -> original so the response can hand the
	// client back the name it sent.
	if nameMap["getWeather"] != "get_weather" {
		t.Errorf("restore map should map getWeather -> get_weather, got %q", nameMap["getWeather"])
	}
	if nameMap["listFiles"] != "list-files" {
		t.Errorf("restore map should map listFiles -> list-files, got %q", nameMap["listFiles"])
	}
}

// TestConvertOpenAIToolsMatchesClaudePath verifies the two converters produce
// identical sanitized names + restore entries for the same tool name — the
// consistency the story requires.
func TestConvertOpenAIToolsMatchesClaudePath(t *testing.T) {
	const raw = "mcp__server__do_thing"

	oaTools := []OpenAITool{{Type: "function"}}
	oaTools[0].Function.Name = raw
	oaTools[0].Function.Description = "d"
	oaSpecs, oaMap := convertOpenAITools(oaTools)

	clTools := []ClaudeTool{{Name: raw, Description: "d", InputSchema: map[string]interface{}{"type": "object"}}}
	clSpecs, clMap := convertClaudeTools(clTools)

	if len(oaSpecs) != 1 || len(clSpecs) != 1 {
		t.Fatalf("expected 1 spec each, got oa=%d cl=%d", len(oaSpecs), len(clSpecs))
	}
	if oaSpecs[0].ToolSpecification.Name != clSpecs[0].ToolSpecification.Name {
		t.Fatalf("OpenAI and Claude paths sanitized %q differently: oa=%q cl=%q",
			raw, oaSpecs[0].ToolSpecification.Name, clSpecs[0].ToolSpecification.Name)
	}
	san := oaSpecs[0].ToolSpecification.Name
	if oaMap[san] != clMap[san] {
		t.Fatalf("restore entries differ for %q: oa=%q cl=%q", san, oaMap[san], clMap[san])
	}
}

// TestConvertOpenAIToolsCleanNameNoRestoreEntry: a name that needs no change has
// no restore-map entry (matching the Claude path's "only map when altered").
func TestConvertOpenAIToolsCleanNameNoRestoreEntry(t *testing.T) {
	tools := []OpenAITool{{Type: "function"}}
	tools[0].Function.Name = "search" // already valid camelCase, short
	tools[0].Function.Description = "d"
	_, nameMap := convertOpenAITools(tools)
	if _, ok := nameMap["search"]; ok {
		t.Errorf("clean name should not get a restore-map entry, map=%v", nameMap)
	}
}

// TestOpenAIToKiroPlumbsToolNameMap verifies the restore map reaches the payload
// so OnToolUse can restore names (the kiro.go wrapper keys on payload.ToolNameMap).
func TestOpenAIToKiroPlumbsToolNameMap(t *testing.T) {
	req := &OpenAIRequest{
		Model: "gpt-4o",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "hi"},
		},
		Tools: []OpenAITool{{Type: "function"}},
	}
	req.Tools[0].Function.Name = "get_weather"
	req.Tools[0].Function.Description = "w"

	payload := OpenAIToKiro(req, false)
	if payload.ToolNameMap["getWeather"] != "get_weather" {
		t.Fatalf("payload.ToolNameMap must restore getWeather -> get_weather, got %v", payload.ToolNameMap)
	}
}
