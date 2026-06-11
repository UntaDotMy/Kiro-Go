package proxy

import "strings"

// ============================================================================
// Tool-choice translation across dialects.
//
// Each client dialect expresses "which tool may/must the model call" differently;
// a generic-provider request must carry that intent to whatever upstream dialect
// serves it, or a client that says "you MUST call tool X now" silently degrades to
// "model decides" and tool calling breaks (the model answers in prose instead of
// invoking the tool). This module normalizes both inbound shapes into one intent
// and emits the right shape per upstream dialect.
//
//	intent      OpenAI tool_choice            Anthropic tool_choice        Gemini functionCallingConfig
//	----------  ---------------------------   --------------------------   ----------------------------
//	auto        "auto"                        {type:"auto"}                {mode:"AUTO"}
//	none        "none"                        {type:"none"}                {mode:"NONE"}
//	any         "required"                    {type:"any"}                 {mode:"ANY"}
//	tool:X      {type:"function",             {type:"tool",name:"X"}       {mode:"ANY",
//	             function:{name:"X"}}                                       allowedFunctionNames:["X"]}
//
// Ported in spirit from 9router's translator tool_choice mapping.
// ============================================================================

// toolChoiceMode enumerates the canonical intents.
const (
	toolChoiceAuto = "auto" // model decides whether to call a tool
	toolChoiceNone = "none" // model must not call any tool
	toolChoiceAny  = "any"  // model must call SOME tool
	toolChoiceTool = "tool" // model must call the named tool
)

// toolChoiceIntent is the dialect-neutral tool-selection intent.
type toolChoiceIntent struct {
	mode string // auto | none | any | tool
	name string // set only when mode == tool
}

// parseOpenAIToolChoice normalizes an OpenAI tool_choice value. Accepts the
// string forms ("auto"/"none"/"required") and the object form
// {"type":"function","function":{"name":"X"}}. Returns false when absent/unknown
// so the caller omits tool_choice entirely (upstream default = auto).
func parseOpenAIToolChoice(v interface{}) (toolChoiceIntent, bool) {
	switch tc := v.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(tc)) {
		case "auto":
			return toolChoiceIntent{mode: toolChoiceAuto}, true
		case "none":
			return toolChoiceIntent{mode: toolChoiceNone}, true
		case "required", "any":
			return toolChoiceIntent{mode: toolChoiceAny}, true
		}
	case map[string]interface{}:
		// {"type":"function","function":{"name":"X"}} — or a few lenient variants.
		if name := openAIToolChoiceName(tc); name != "" {
			return toolChoiceIntent{mode: toolChoiceTool, name: name}, true
		}
		// {"type":"auto"|"none"|"required"} object variant.
		if t, ok := tc["type"].(string); ok {
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "auto":
				return toolChoiceIntent{mode: toolChoiceAuto}, true
			case "none":
				return toolChoiceIntent{mode: toolChoiceNone}, true
			case "required", "any":
				return toolChoiceIntent{mode: toolChoiceAny}, true
			}
		}
	}
	return toolChoiceIntent{}, false
}

// openAIToolChoiceName extracts the function name from an OpenAI object
// tool_choice, tolerating both {"function":{"name":"X"}} and a flat {"name":"X"}.
func openAIToolChoiceName(tc map[string]interface{}) string {
	if fn, ok := tc["function"].(map[string]interface{}); ok {
		if n, ok := fn["name"].(string); ok && strings.TrimSpace(n) != "" {
			return strings.TrimSpace(n)
		}
	}
	if n, ok := tc["name"].(string); ok && strings.TrimSpace(n) != "" {
		return strings.TrimSpace(n)
	}
	return ""
}

// parseClaudeToolChoice normalizes an Anthropic tool_choice value. Accepts the
// object forms {"type":"auto"|"any"|"none"|"tool","name":"X"}. Anthropic has no
// string form. Returns false when absent/unknown.
func parseClaudeToolChoice(v interface{}) (toolChoiceIntent, bool) {
	tc, ok := v.(map[string]interface{})
	if !ok {
		return toolChoiceIntent{}, false
	}
	t, _ := tc["type"].(string)
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "auto":
		return toolChoiceIntent{mode: toolChoiceAuto}, true
	case "any":
		return toolChoiceIntent{mode: toolChoiceAny}, true
	case "none":
		return toolChoiceIntent{mode: toolChoiceNone}, true
	case "tool":
		if n, ok := tc["name"].(string); ok && strings.TrimSpace(n) != "" {
			return toolChoiceIntent{mode: toolChoiceTool, name: strings.TrimSpace(n)}, true
		}
		// "tool" with no name is malformed; treat as "any" (must call something).
		return toolChoiceIntent{mode: toolChoiceAny}, true
	}
	return toolChoiceIntent{}, false
}

// toOpenAI emits an OpenAI tool_choice value for this intent.
func (ti toolChoiceIntent) toOpenAI() interface{} {
	switch ti.mode {
	case toolChoiceNone:
		return "none"
	case toolChoiceAny:
		return "required"
	case toolChoiceTool:
		return map[string]interface{}{
			"type":     "function",
			"function": map[string]interface{}{"name": ti.name},
		}
	default: // auto
		return "auto"
	}
}

// toAnthropic emits an Anthropic tool_choice object for this intent.
func (ti toolChoiceIntent) toAnthropic() interface{} {
	switch ti.mode {
	case toolChoiceNone:
		return map[string]interface{}{"type": "none"}
	case toolChoiceAny:
		return map[string]interface{}{"type": "any"}
	case toolChoiceTool:
		return map[string]interface{}{"type": "tool", "name": ti.name}
	default: // auto
		return map[string]interface{}{"type": "auto"}
	}
}

// toGeminiToolConfig emits a Gemini tool_config object
// ({"functionCallingConfig":{"mode":...,"allowedFunctionNames":[...]}}) for this
// intent, or nil for plain "auto" (Gemini's default — omit to keep the body lean).
func (ti toolChoiceIntent) toGeminiToolConfig() map[string]interface{} {
	cfg := map[string]interface{}{}
	switch ti.mode {
	case toolChoiceNone:
		cfg["mode"] = "NONE"
	case toolChoiceAny:
		cfg["mode"] = "ANY"
	case toolChoiceTool:
		cfg["mode"] = "ANY"
		cfg["allowedFunctionNames"] = []string{ti.name}
	default: // auto
		return nil
	}
	return map[string]interface{}{"functionCallingConfig": cfg}
}
