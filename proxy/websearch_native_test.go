package proxy

import (
	"encoding/json"
	"testing"
)

// TestNativeWebSearchKind verifies provider-native web-search classification.
// This is the heart of the "native-first, Kiro fallback" design: a DashScope /
// Gemini / Anthropic provider must be recognized as native so the proxy never
// needs a Kiro account to search for it.
func TestNativeWebSearchKind(t *testing.T) {
	cases := []struct {
		name    string
		dialect Dialect
		id      string
		baseURL string
		want    string
	}{
		{"dashscope by base url intl", DialectOpenAI, "mycustom", "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", nativeSearchDashScope},
		{"dashscope by base url cn", DialectOpenAI, "mycustom", "https://dashscope.aliyuncs.com/compatible-mode/v1", nativeSearchDashScope},
		{"qwen by id", DialectOpenAI, "qwen", "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions", nativeSearchDashScope},
		{"alicode by id", DialectOpenAI, "alicode", "https://example.com/v1", nativeSearchDashScope},
		{"plain openai is not native", DialectOpenAI, "openai", "https://api.openai.com/v1/chat/completions", ""},
		{"groq is not native", DialectOpenAI, "groq", "https://api.groq.com/openai/v1/chat/completions", ""},
		{"gemini is native", DialectGemini, "gemini", "https://generativelanguage.googleapis.com/v1beta/models", nativeSearchGemini},
		{"real anthropic is native", DialectAnthropic, "anthropic", "https://api.anthropic.com/v1/messages", nativeSearchAnthropic},
		{"anthropic-compatible is NOT native", DialectAnthropic, "glm", "https://open.bigmodel.cn/api/anthropic/v1/messages", ""},
		{"kiro dialect is not native", DialectKiro, "kiro", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nativeWebSearchKind(c.dialect, c.id, c.baseURL)
			if got != c.want {
				t.Fatalf("nativeWebSearchKind(%s,%s,%s) = %q, want %q", c.dialect, c.id, c.baseURL, got, c.want)
			}
		})
	}
}

// TestInjectNativeWebSearchDashScope verifies the OpenAI-compatible DashScope
// injection sets enable_search WITHOUT clobbering existing fields.
func TestInjectNativeWebSearchDashScope(t *testing.T) {
	body := []byte(`{"model":"qwen-plus","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	out := injectNativeWebSearch(nativeSearchDashScope, body)

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["enable_search"] != true {
		t.Fatalf("enable_search not set: %v", m["enable_search"])
	}
	if m["model"] != "qwen-plus" {
		t.Fatalf("model clobbered: %v", m["model"])
	}
	if m["stream"] != true {
		t.Fatalf("stream clobbered: %v", m["stream"])
	}
}

// TestInjectNativeWebSearchGemini verifies google_search is appended alongside
// any existing functionDeclarations rather than replacing them.
func TestInjectNativeWebSearchGemini(t *testing.T) {
	body := []byte(`{"contents":[],"tools":[{"functionDeclarations":[{"name":"read_file"}]}]}`)
	out := injectNativeWebSearch(nativeSearchGemini, body)

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tools, ok := m["tools"].([]interface{})
	if !ok || len(tools) != 2 {
		t.Fatalf("expected 2 tool entries (functionDeclarations + google_search), got %v", m["tools"])
	}
	// The appended entry must carry google_search.
	last, _ := tools[1].(map[string]interface{})
	if _, ok := last["google_search"]; !ok {
		t.Fatalf("google_search entry missing: %v", tools[1])
	}
	// The original functionDeclarations must survive.
	first, _ := tools[0].(map[string]interface{})
	if _, ok := first["functionDeclarations"]; !ok {
		t.Fatalf("functionDeclarations dropped: %v", tools[0])
	}
}

// TestInjectNativeWebSearchGeminiNoExistingTools verifies google_search is added
// even when the body has no tools array yet.
func TestInjectNativeWebSearchGeminiNoExistingTools(t *testing.T) {
	body := []byte(`{"contents":[]}`)
	out := injectNativeWebSearch(nativeSearchGemini, body)
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tools, ok := m["tools"].([]interface{})
	if !ok || len(tools) != 1 {
		t.Fatalf("expected 1 tool entry, got %v", m["tools"])
	}
}

// TestInjectNativeWebSearchAnthropic verifies the hosted web_search tool is
// re-added (the dialect translator drops it as a server tool).
func TestInjectNativeWebSearchAnthropic(t *testing.T) {
	body := []byte(`{"model":"claude-x","tools":[{"name":"read_file","input_schema":{}}]}`)
	out := injectNativeWebSearch(nativeSearchAnthropic, body)
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tools, _ := m["tools"].([]interface{})
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools (user tool + hosted web_search), got %v", m["tools"])
	}
	last, _ := tools[1].(map[string]interface{})
	if last["type"] != "web_search_20250305" || last["name"] != webSearchToolName {
		t.Fatalf("hosted web_search tool malformed: %v", last)
	}
}

// TestInjectNativeWebSearchNoopOnEmptyKind verifies an empty kind leaves the body
// byte-identical (the "no native capability" path must not mutate the request).
func TestInjectNativeWebSearchNoopOnEmptyKind(t *testing.T) {
	body := []byte(`{"model":"x","enable_search":false}`)
	out := injectNativeWebSearch("", body)
	if string(out) != string(body) {
		t.Fatalf("empty kind mutated body: %s", out)
	}
}

// TestInjectNativeWebSearchNoopOnMalformedBody verifies malformed JSON is
// returned unchanged rather than dropped — a bad injection must never break the
// upstream call.
func TestInjectNativeWebSearchNoopOnMalformedBody(t *testing.T) {
	body := []byte(`not json`)
	out := injectNativeWebSearch(nativeSearchDashScope, body)
	if string(out) != string(body) {
		t.Fatalf("malformed body was altered: %s", out)
	}
}

// TestNrHasWebSearch verifies web_search detection across both client dialects.
func TestNrHasWebSearch(t *testing.T) {
	claude := &NormalizedRequest{Claude: &ClaudeRequest{Tools: []ClaudeTool{{Type: "web_search_20250305", Name: "web_search"}}}}
	if !nrHasWebSearch(claude) {
		t.Fatal("claude web_search not detected")
	}
	claudeByName := &NormalizedRequest{Claude: &ClaudeRequest{Tools: []ClaudeTool{{Name: "web_search"}}}}
	if !nrHasWebSearch(claudeByName) {
		t.Fatal("claude web_search by name not detected")
	}
	none := &NormalizedRequest{Claude: &ClaudeRequest{Tools: []ClaudeTool{{Name: "read_file"}}}}
	if nrHasWebSearch(none) {
		t.Fatal("false positive on non-search tool")
	}

	oa := &NormalizedRequest{OpenAI: &OpenAIRequest{}}
	oa.OpenAI.Tools = []OpenAITool{{Type: "function"}}
	oa.OpenAI.Tools[0].Function.Name = "web_search"
	if !nrHasWebSearch(oa) {
		t.Fatal("openai web_search not detected")
	}
}
