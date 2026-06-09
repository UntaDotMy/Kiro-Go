package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// --- Model resolution -------------------------------------------------------

func TestParseModelBackend(t *testing.T) {
	cases := []struct {
		in           string
		wantBackend  string
		wantUpstream string
	}{
		// Unprefixed -> kiro, model unchanged (existing behavior).
		{"claude-opus-4-7", "kiro", "claude-opus-4-7"},
		{"gpt-4o", "kiro", "gpt-4o"},
		{"auto", "kiro", "auto"},
		// Provider id prefix.
		{"groq/llama-3.3-70b", "groq", "llama-3.3-70b"},
		{"cerebras/qwen-3-coder", "cerebras", "qwen-3-coder"},
		{"openai/gpt-4o", "openai", "gpt-4o"},
		{"anthropic/claude-sonnet-4.5", "anthropic", "claude-sonnet-4.5"},
		{"gemini/gemini-2.5-flash", "gemini", "gemini-2.5-flash"},
		// Provider ALIAS prefix.
		{"or/anthropic/claude-sonnet-4.5", "openrouter", "anthropic/claude-sonnet-4.5"},
		{"ds/deepseek-chat", "deepseek", "deepseek-chat"},
		{"cx/gpt-5-codex", "codex", "gpt-5-codex"},
		{"kr/claude-opus-4-7", "kiro", "claude-opus-4-7"},
		// Unknown prefix -> kiro, whole string preserved.
		{"unknownvendor/some-model", "kiro", "unknownvendor/some-model"},
		// Edge cases.
		{"", "kiro", ""},
		{"groq/", "kiro", "groq/"}, // trailing slash, no model -> not a route
	}
	for _, c := range cases {
		gotB, gotM := ParseModelBackend(c.in)
		if gotB != c.wantBackend || gotM != c.wantUpstream {
			t.Errorf("ParseModelBackend(%q) = (%q,%q), want (%q,%q)", c.in, gotB, gotM, c.wantBackend, c.wantUpstream)
		}
	}
}

func TestDialectForBackend(t *testing.T) {
	cases := map[string]Dialect{
		"kiro":      DialectKiro,
		"":          DialectKiro,
		"codex":     DialectCodex,
		"groq":      DialectOpenAI,
		"cerebras":  DialectOpenAI,
		"anthropic": DialectAnthropic,
		"glm":       DialectAnthropic,
		"gemini":    DialectGemini,
		"nope":      "",
	}
	for backend, want := range cases {
		if got := dialectFor(backend); got != want {
			t.Errorf("dialectFor(%q) = %q, want %q", backend, got, want)
		}
	}
}

// --- Claude -> OpenAI request ----------------------------------------------

func TestBuildOpenAIChatBodyFromClaude(t *testing.T) {
	nr := &NormalizedRequest{
		Model: "llama-3.3-70b",
		Claude: &ClaudeRequest{
			Model:     "groq/llama-3.3-70b",
			MaxTokens: 1024,
			System:    "You are helpful.",
			Messages: []ClaudeMessage{
				{Role: "user", Content: "hello"},
			},
			Tools: []ClaudeTool{
				{Name: "get_weather", Description: "Get weather", InputSchema: map[string]interface{}{"type": "object"}},
				{Type: "web_search_20250305", Name: "web_search"}, // server tool -> dropped
			},
		},
	}
	raw, err := buildOpenAIChatBody(nr, "llama-3.3-70b", true)
	if err != nil {
		t.Fatalf("buildOpenAIChatBody: %v", err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["model"] != "llama-3.3-70b" {
		t.Errorf("model = %v, want llama-3.3-70b", body["model"])
	}
	if body["stream"] != true {
		t.Errorf("stream should be true")
	}
	msgs, _ := body["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expected system+user messages, got %d: %v", len(msgs), msgs)
	}
	first := msgs[0].(map[string]interface{})
	if first["role"] != "system" || first["content"] != "You are helpful." {
		t.Errorf("first message should be the system prompt, got %v", first)
	}
	tools, _ := body["tools"].([]interface{})
	if len(tools) != 1 {
		t.Fatalf("server tool should be dropped, expected 1 tool, got %d", len(tools))
	}
}

func TestClaudeAssistantToolUseToOpenAI(t *testing.T) {
	content := []interface{}{
		map[string]interface{}{"type": "text", "text": "let me check"},
		map[string]interface{}{"type": "tool_use", "id": "tu_1", "name": "search", "input": map[string]interface{}{"q": "go"}},
	}
	msg := claudeAssistantMessageToOpenAI(content)
	if msg["role"] != "assistant" {
		t.Fatalf("role = %v", msg["role"])
	}
	tcs, ok := msg["tool_calls"].([]map[string]interface{})
	if !ok || len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %v", msg["tool_calls"])
	}
	fn := tcs[0]["function"].(map[string]interface{})
	if fn["name"] != "search" {
		t.Errorf("tool name = %v", fn["name"])
	}
	if !strings.Contains(fn["arguments"].(string), "\"q\"") {
		t.Errorf("arguments should carry the input JSON, got %v", fn["arguments"])
	}
}

// --- OpenAI SSE -> callback -------------------------------------------------

func TestParseOpenAISSE(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"foo","arguments":"{\"a\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`data: [DONE]`,
	}, "\n")

	var text strings.Builder
	var tools []KiroToolUse
	var inTok, outTok int
	var stop string
	cb := &KiroStreamCallback{
		OnText:       func(t string, thinking bool) { text.WriteString(t) },
		OnToolUse:    func(tu KiroToolUse) { tools = append(tools, tu) },
		OnComplete:   func(in, out int) { inTok, outTok = in, out },
		OnStopReason: func(r string) { stop = r },
	}
	if err := parseOpenAISSE(strings.NewReader(stream), cb); err != nil {
		t.Fatalf("parseOpenAISSE: %v", err)
	}
	if text.String() != "Hello" {
		t.Errorf("text = %q, want Hello", text.String())
	}
	if len(tools) != 1 || tools[0].Name != "foo" {
		t.Fatalf("expected 1 tool 'foo', got %v", tools)
	}
	if got := tools[0].Input["a"]; got != float64(1) {
		t.Errorf("tool arg a = %v, want 1", got)
	}
	if inTok != 10 || outTok != 5 {
		t.Errorf("tokens = %d/%d, want 10/5", inTok, outTok)
	}
	if stop != "tool_use" {
		t.Errorf("stop = %q, want tool_use", stop)
	}
}

// --- Anthropic SSE -> callback ----------------------------------------------

func TestParseAnthropicSSE(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":7}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_9","name":"calc"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"x\":2}"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`,
		`data: {"type":"message_stop"}`,
	}, "\n")

	var text strings.Builder
	var tools []KiroToolUse
	var inTok, outTok int
	var stop string
	cb := &KiroStreamCallback{
		OnText:       func(t string, thinking bool) { text.WriteString(t) },
		OnToolUse:    func(tu KiroToolUse) { tools = append(tools, tu) },
		OnComplete:   func(in, out int) { inTok, outTok = in, out },
		OnStopReason: func(r string) { stop = r },
	}
	if err := parseAnthropicSSE(strings.NewReader(stream), cb); err != nil {
		t.Fatalf("parseAnthropicSSE: %v", err)
	}
	if text.String() != "Hi" {
		t.Errorf("text = %q", text.String())
	}
	if len(tools) != 1 || tools[0].Name != "calc" || tools[0].Input["x"] != float64(2) {
		t.Fatalf("tool parse wrong: %v", tools)
	}
	if inTok != 7 || outTok != 4 {
		t.Errorf("tokens = %d/%d, want 7/4", inTok, outTok)
	}
	if stop != "tool_use" {
		t.Errorf("stop = %q", stop)
	}
}

// --- Gemini SSE -> callback -------------------------------------------------

func TestParseGeminiSSE(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"Hello"}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"id":3}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":6}}`,
	}, "\n")

	var text strings.Builder
	var tools []KiroToolUse
	var inTok, outTok int
	var stop string
	cb := &KiroStreamCallback{
		OnText:       func(t string, thinking bool) { text.WriteString(t) },
		OnToolUse:    func(tu KiroToolUse) { tools = append(tools, tu) },
		OnComplete:   func(in, out int) { inTok, outTok = in, out },
		OnStopReason: func(r string) { stop = r },
	}
	if err := parseGeminiSSE(strings.NewReader(stream), cb); err != nil {
		t.Fatalf("parseGeminiSSE: %v", err)
	}
	if text.String() != "Hello" {
		t.Errorf("text = %q", text.String())
	}
	if len(tools) != 1 || tools[0].Name != "lookup" || tools[0].Input["id"] != float64(3) {
		t.Fatalf("tool parse wrong: %v", tools)
	}
	if inTok != 12 || outTok != 6 {
		t.Errorf("tokens = %d/%d, want 12/6", inTok, outTok)
	}
	if stop != "end_turn" {
		t.Errorf("stop = %q", stop)
	}
}

// --- Build bodies for Anthropic / Gemini from an OpenAI client --------------

func TestBuildAnthropicBodyFromOpenAI(t *testing.T) {
	nr := &NormalizedRequest{
		Model: "claude-sonnet-4.5",
		OpenAI: &OpenAIRequest{
			Model:     "anthropic/claude-sonnet-4.5",
			MaxTokens: 0, // should default
			Messages: []OpenAIMessage{
				{Role: "system", Content: "sys"},
				{Role: "user", Content: "hi"},
			},
		},
	}
	raw, err := buildAnthropicBody(nr, "claude-sonnet-4.5", true)
	if err != nil {
		t.Fatalf("buildAnthropicBody: %v", err)
	}
	var body map[string]interface{}
	json.Unmarshal(raw, &body)
	if body["system"] != "sys" {
		t.Errorf("system = %v", body["system"])
	}
	if body["max_tokens"] == nil || body["max_tokens"].(float64) <= 0 {
		t.Errorf("max_tokens should default to a positive value, got %v", body["max_tokens"])
	}
	msgs := body["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 non-system message, got %d", len(msgs))
	}
}

func TestBuildGeminiBodyRolesMapModel(t *testing.T) {
	nr := &NormalizedRequest{
		Model: "gemini-2.5-flash",
		OpenAI: &OpenAIRequest{
			Messages: []OpenAIMessage{
				{Role: "user", Content: "q"},
				{Role: "assistant", Content: "a"},
			},
		},
	}
	raw, err := buildGeminiBody(nr, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("buildGeminiBody: %v", err)
	}
	var body map[string]interface{}
	json.Unmarshal(raw, &body)
	contents := body["contents"].([]interface{})
	if len(contents) != 2 {
		t.Fatalf("expected 2 contents, got %d", len(contents))
	}
	// assistant must map to "model" role.
	second := contents[1].(map[string]interface{})
	if second["role"] != "model" {
		t.Errorf("assistant should map to gemini role 'model', got %v", second["role"])
	}
}
