package proxy

import (
	"encoding/json"
	"testing"
)

// TestToolChoiceMappingAcrossDialects pins the cross-dialect tool_choice mapping
// table. A forced/affirmative tool choice from one client dialect must survive to
// whatever upstream dialect serves the request, or "you MUST call tool X" silently
// degrades to "model decides" and tool calling breaks.
func TestToolChoiceMappingAcrossDialects(t *testing.T) {
	cases := []struct {
		name          string
		intent        toolChoiceIntent
		wantOpenAI    interface{}
		wantAnthropic map[string]interface{}
		wantGeminiNil bool // true => toGeminiToolConfig returns nil (auto default)
		wantGeminiCfg map[string]interface{}
	}{
		{
			name:          "auto",
			intent:        toolChoiceIntent{mode: toolChoiceAuto},
			wantOpenAI:    "auto",
			wantAnthropic: map[string]interface{}{"type": "auto"},
			wantGeminiNil: true,
		},
		{
			name:          "none",
			intent:        toolChoiceIntent{mode: toolChoiceNone},
			wantOpenAI:    "none",
			wantAnthropic: map[string]interface{}{"type": "none"},
			wantGeminiCfg: map[string]interface{}{"mode": "NONE"},
		},
		{
			name:          "any",
			intent:        toolChoiceIntent{mode: toolChoiceAny},
			wantOpenAI:    "required",
			wantAnthropic: map[string]interface{}{"type": "any"},
			wantGeminiCfg: map[string]interface{}{"mode": "ANY"},
		},
		{
			name:          "tool:search",
			intent:        toolChoiceIntent{mode: toolChoiceTool, name: "search"},
			wantOpenAI:    map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "search"}},
			wantAnthropic: map[string]interface{}{"type": "tool", "name": "search"},
			wantGeminiCfg: map[string]interface{}{"mode": "ANY", "allowedFunctionNames": []string{"search"}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// OpenAI emission (compare via JSON to normalize map/string shapes).
			if got := mustJSON(t, c.intent.toOpenAI()); got != mustJSON(t, c.wantOpenAI) {
				t.Errorf("toOpenAI() = %s, want %s", got, mustJSON(t, c.wantOpenAI))
			}
			// Anthropic emission.
			if got := mustJSON(t, c.intent.toAnthropic()); got != mustJSON(t, c.wantAnthropic) {
				t.Errorf("toAnthropic() = %s, want %s", got, mustJSON(t, c.wantAnthropic))
			}
			// Gemini emission.
			gcfg := c.intent.toGeminiToolConfig()
			if c.wantGeminiNil {
				if gcfg != nil {
					t.Errorf("toGeminiToolConfig() = %v, want nil (auto)", gcfg)
				}
				return
			}
			fcc, ok := gcfg["functionCallingConfig"].(map[string]interface{})
			if !ok {
				t.Fatalf("toGeminiToolConfig() missing functionCallingConfig: %v", gcfg)
			}
			if got := mustJSON(t, fcc); got != mustJSON(t, c.wantGeminiCfg) {
				t.Errorf("functionCallingConfig = %s, want %s", got, mustJSON(t, c.wantGeminiCfg))
			}
		})
	}
}

// TestParseOpenAIToolChoice covers the string and object inbound forms.
func TestParseOpenAIToolChoice(t *testing.T) {
	cases := []struct {
		in       interface{}
		wantOK   bool
		wantMode string
		wantName string
	}{
		{"auto", true, toolChoiceAuto, ""},
		{"none", true, toolChoiceNone, ""},
		{"required", true, toolChoiceAny, ""},
		{map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "foo"}}, true, toolChoiceTool, "foo"},
		{map[string]interface{}{"type": "required"}, true, toolChoiceAny, ""},
		{map[string]interface{}{"name": "bar"}, true, toolChoiceTool, "bar"}, // lenient flat form
		{nil, false, "", ""},
		{"garbage", false, "", ""},
	}
	for _, c := range cases {
		ti, ok := parseOpenAIToolChoice(c.in)
		if ok != c.wantOK || ti.mode != c.wantMode || ti.name != c.wantName {
			t.Errorf("parseOpenAIToolChoice(%v) = (%+v,%v), want mode=%q name=%q ok=%v", c.in, ti, ok, c.wantMode, c.wantName, c.wantOK)
		}
	}
}

// TestParseClaudeToolChoice covers the Anthropic object forms.
func TestParseClaudeToolChoice(t *testing.T) {
	cases := []struct {
		in       interface{}
		wantOK   bool
		wantMode string
		wantName string
	}{
		{map[string]interface{}{"type": "auto"}, true, toolChoiceAuto, ""},
		{map[string]interface{}{"type": "any"}, true, toolChoiceAny, ""},
		{map[string]interface{}{"type": "none"}, true, toolChoiceNone, ""},
		{map[string]interface{}{"type": "tool", "name": "calc"}, true, toolChoiceTool, "calc"},
		{map[string]interface{}{"type": "tool"}, true, toolChoiceAny, ""}, // no name -> any
		{"auto", false, "", ""}, // Anthropic has no string form
		{nil, false, "", ""},
	}
	for _, c := range cases {
		ti, ok := parseClaudeToolChoice(c.in)
		if ok != c.wantOK || ti.mode != c.wantMode || ti.name != c.wantName {
			t.Errorf("parseClaudeToolChoice(%v) = (%+v,%v), want mode=%q name=%q ok=%v", c.in, ti, ok, c.wantMode, c.wantName, c.wantOK)
		}
	}
}

// TestBuildBodiesCarryToolChoice verifies the body builders actually emit
// tool_choice / toolConfig when the request carries one alongside tools.
func TestBuildBodiesCarryToolChoice(t *testing.T) {
	// Claude client forcing a specific tool, routed to an OpenAI upstream.
	claudeNR := &NormalizedRequest{
		Model: "llama-3.3-70b",
		Claude: &ClaudeRequest{
			MaxTokens:  256,
			Messages:   []ClaudeMessage{{Role: "user", Content: "weather?"}},
			Tools:      []ClaudeTool{{Name: "get_weather", Description: "d", InputSchema: map[string]interface{}{"type": "object"}}},
			ToolChoice: map[string]interface{}{"type": "tool", "name": "get_weather"},
		},
	}
	raw, err := buildOpenAIChatBody(claudeNR, "llama-3.3-70b", true)
	if err != nil {
		t.Fatalf("buildOpenAIChatBody: %v", err)
	}
	var oa map[string]interface{}
	json.Unmarshal(raw, &oa)
	tc, ok := oa["tool_choice"].(map[string]interface{})
	if !ok {
		t.Fatalf("OpenAI body missing tool_choice: %v", oa["tool_choice"])
	}
	fn, _ := tc["function"].(map[string]interface{})
	if tc["type"] != "function" || fn["name"] != "get_weather" {
		t.Errorf("OpenAI tool_choice = %v, want forced get_weather", tc)
	}

	// Same Claude request routed to a Gemini upstream -> toolConfig ANY + allowed.
	rawG, err := buildGeminiBody(claudeNR, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("buildGeminiBody: %v", err)
	}
	var ge map[string]interface{}
	json.Unmarshal(rawG, &ge)
	tcfg, ok := ge["toolConfig"].(map[string]interface{})
	if !ok {
		t.Fatalf("Gemini body missing toolConfig: %v", ge["toolConfig"])
	}
	fcc, _ := tcfg["functionCallingConfig"].(map[string]interface{})
	if fcc["mode"] != "ANY" {
		t.Errorf("Gemini functionCallingConfig.mode = %v, want ANY", fcc["mode"])
	}

	// OpenAI client with "required", routed to an Anthropic upstream -> {type:any}.
	openaiNR := &NormalizedRequest{
		Model: "claude-sonnet-4.5",
		OpenAI: &OpenAIRequest{
			MaxTokens:  256,
			Messages:   []OpenAIMessage{{Role: "user", Content: "hi"}},
			ToolChoice: "required",
		},
	}
	openaiNR.OpenAI.Tools = []OpenAITool{{Type: "function"}}
	openaiNR.OpenAI.Tools[0].Function.Name = "do_thing"
	openaiNR.OpenAI.Tools[0].Function.Parameters = map[string]interface{}{"type": "object"}
	rawA, err := buildAnthropicBody(openaiNR, "claude-sonnet-4.5", true)
	if err != nil {
		t.Fatalf("buildAnthropicBody: %v", err)
	}
	var an map[string]interface{}
	json.Unmarshal(rawA, &an)
	atc, ok := an["tool_choice"].(map[string]interface{})
	if !ok || atc["type"] != "any" {
		t.Errorf("Anthropic tool_choice = %v, want {type:any}", an["tool_choice"])
	}
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
