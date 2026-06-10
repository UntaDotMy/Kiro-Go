package proxy

import (
	"strings"
	"testing"
)

// TestClaudeToGeminiContentsPreservesToolCalls is the regression guard for the
// bug where claudeToGeminiContents flattened every message to text only, DROPPING
// assistant tool_use blocks and user tool_result blocks. That silently broke
// multi-turn tool calling through any Gemini provider: the model lost its entire
// tool history and re-asked or hallucinated. This pins that tool_use ->
// functionCall and tool_result -> functionResponse survive, and that the
// functionResponse carries the FUNCTION name (Gemini requires it), not the id.
func TestClaudeToGeminiContentsPreservesToolCalls(t *testing.T) {
	msgs := []ClaudeMessage{
		{Role: "user", Content: "what's the weather in Paris?"},
		{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "text", "text": "let me check"},
			map[string]interface{}{"type": "tool_use", "id": "toolu_abc", "name": "get_weather", "input": map[string]interface{}{"city": "Paris"}},
		}},
		{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "tool_result", "tool_use_id": "toolu_abc", "content": "18C sunny"},
		}},
	}

	contents := claudeToGeminiContents(msgs)
	if len(contents) != 3 {
		t.Fatalf("expected 3 contents, got %d: %v", len(contents), contents)
	}

	// [1] assistant -> role "model" with a functionCall part for get_weather.
	model := contents[1]
	if model["role"] != "model" {
		t.Errorf("assistant role = %v, want model", model["role"])
	}
	mparts := model["parts"].([]map[string]interface{})
	var foundCall bool
	for _, p := range mparts {
		if fc, ok := p["functionCall"].(map[string]interface{}); ok {
			foundCall = true
			if fc["name"] != "get_weather" {
				t.Errorf("functionCall.name = %v, want get_weather", fc["name"])
			}
			args, _ := fc["args"].(map[string]interface{})
			if args["city"] != "Paris" {
				t.Errorf("functionCall.args.city = %v, want Paris", args["city"])
			}
		}
	}
	if !foundCall {
		t.Fatalf("assistant tool_use was DROPPED — no functionCall part: %v", mparts)
	}

	// [2] tool_result -> role "user" with a functionResponse whose name is the
	// FUNCTION name resolved from the tool_use_id (NOT the id itself).
	tr := contents[2]
	tparts := tr["parts"].([]map[string]interface{})
	fr, ok := tparts[0]["functionResponse"].(map[string]interface{})
	if !ok {
		t.Fatalf("tool_result was DROPPED — no functionResponse part: %v", tparts)
	}
	if fr["name"] != "get_weather" {
		t.Errorf("functionResponse.name = %v, want get_weather (resolved from id)", fr["name"])
	}
	resp, _ := fr["response"].(map[string]interface{})
	if resp["content"] != "18C sunny" {
		t.Errorf("functionResponse.response.content = %v, want '18C sunny'", resp["content"])
	}
}

// TestOpenAIToGeminiFunctionResponseName guards that an OpenAI tool-role message
// maps to a Gemini functionResponse using the FUNCTION name (looked up from the
// preceding assistant tool_calls by id), not the raw tool_call_id. Gemini rejects
// a functionResponse whose name doesn't match a prior functionCall name.
func TestOpenAIToGeminiFunctionResponseName(t *testing.T) {
	msgs := []OpenAIMessage{
		{Role: "user", Content: "search go docs"},
		{Role: "assistant", ToolCalls: []ToolCall{mkToolCall("call_1", "search", `{"q":"go"}`)}},
		{Role: "tool", ToolCallID: "call_1", Content: "results..."},
	}
	_, contents := openAIToGeminiContents(msgs)
	last := contents[len(contents)-1]
	parts := last["parts"].([]map[string]interface{})
	fr := parts[0]["functionResponse"].(map[string]interface{})
	if fr["name"] != "search" {
		t.Errorf("functionResponse.name = %v, want 'search' (resolved from call_1), not the id", fr["name"])
	}
}

// TestGeminiParallelToolCallIDsUnique guards that two parallel Gemini function
// calls to the SAME function get DISTINCT synthesized ids, so the client can match
// each tool_result back to its tool_use. Before the fix both collided on
// "call_<name>".
func TestGeminiParallelToolCallIDsUnique(t *testing.T) {
	stream := `data: {"candidates":[{"content":{"parts":[` +
		`{"functionCall":{"name":"lookup","args":{"id":1}}},` +
		`{"functionCall":{"name":"lookup","args":{"id":2}}}` +
		`]},"finishReason":"STOP"}]}` + "\n"

	var tools []KiroToolUse
	cb := &KiroStreamCallback{
		OnToolUse: func(tu KiroToolUse) { tools = append(tools, tu) },
	}
	if err := parseGeminiSSE(strings.NewReader(stream), cb); err != nil {
		t.Fatalf("parseGeminiSSE: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(tools))
	}
	if tools[0].ToolUseID == tools[1].ToolUseID {
		t.Errorf("parallel calls to same function collided on id %q — client can't match results", tools[0].ToolUseID)
	}
}

// TestGeminiFunctionResponsePayloadJSON verifies a JSON-object tool result is
// forwarded as-is (not double-wrapped), while a bare string is wrapped under
// "content".
func TestGeminiFunctionResponsePayloadJSON(t *testing.T) {
	obj := geminiFunctionResponsePayload(`{"temp":18,"unit":"C"}`)
	if obj["temp"] != float64(18) || obj["unit"] != "C" {
		t.Errorf("JSON object result should pass through, got %v", obj)
	}
	str := geminiFunctionResponsePayload("just text")
	if str["content"] != "just text" {
		t.Errorf("bare string should wrap under content, got %v", str)
	}
}

// mkToolCall builds an OpenAI ToolCall (the Function fields are nested + unexported
// shape, so a helper keeps the tests readable).
func mkToolCall(id, name, args string) ToolCall {
	var tc ToolCall
	tc.ID = id
	tc.Type = "function"
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}
