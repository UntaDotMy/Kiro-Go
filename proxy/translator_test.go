package proxy

import (
	"strings"
	"testing"
)

func TestExtractOpenAIMessageTextStructured(t *testing.T) {
	content := []interface{}{
		map[string]interface{}{"type": "text", "text": "alpha"},
		map[string]interface{}{"type": "input_text", "text": "beta"},
	}

	if got := extractOpenAIMessageText(content); got != "alphabeta" {
		t.Fatalf("expected concatenated structured text, got %q", got)
	}

	nested := map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": "nested"}},
	}
	if got := extractOpenAIMessageText(nested); got != "nested" {
		t.Fatalf("expected nested content extraction, got %q", got)
	}
}

func TestOpenAIToKiroPreservesStructuredAssistantAndToolContent(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Tools: []OpenAITool{{
			Type: "function",
			Function: struct {
				Name        string      `json:"name"`
				Description string      `json:"description"`
				Parameters  interface{} `json:"parameters"`
			}{
				Name:        "lookup",
				Description: "lookup",
				Parameters:  map[string]interface{}{"type": "object"},
			},
		}},
		Messages: []OpenAIMessage{
			{
				Role: "system",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "system-a"},
					map[string]interface{}{"type": "text", "text": "system-b"},
				},
			},
			{Role: "user", Content: "first-question"},
			{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "assistant-structured"},
				},
				ToolCalls: []ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "lookup", Arguments: "{}"},
				}},
			},
			{
				Role:       "tool",
				ToolCallID: "call_1",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "tool-result-structured"},
				},
			},
		},
	}

	payload := OpenAIToKiro(req, false)

	if len(payload.ConversationState.History) != 2 {
		t.Fatalf("expected 2 history items, got %d", len(payload.ConversationState.History))
	}

	firstHistoryUser := payload.ConversationState.History[0].UserInputMessage
	if firstHistoryUser == nil {
		t.Fatalf("expected first history item to be user message")
	}
	if !strings.Contains(firstHistoryUser.Content, "system-a") ||
		!strings.Contains(firstHistoryUser.Content, "system-b") ||
		!strings.Contains(firstHistoryUser.Content, "first-question") {
		t.Fatalf("expected merged system+user content, got %q", firstHistoryUser.Content)
	}

	historyAssistant := payload.ConversationState.History[1].AssistantResponseMessage
	if historyAssistant == nil {
		t.Fatalf("expected second history item to be assistant message")
	}
	if historyAssistant.Content != "assistant-structured" {
		t.Fatalf("expected assistant structured content to be preserved, got %q", historyAssistant.Content)
	}

	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	// Structured tool results live in UserInputMessageContext.ToolResults, NOT
	// in the prose content. Older revisions also synthesized a "Tool results:"
	// prose envelope inside cur.Content; that has been removed because the
	// upstream model fingerprinted it as a fake harness signal.
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("expected one tool result in current context")
	}
	gotToolText := cur.UserInputMessageContext.ToolResults[0].Content[0].Text
	if gotToolText != "tool-result-structured" {
		t.Fatalf("expected structured tool result text, got %q", gotToolText)
	}
	if strings.Contains(cur.Content, "Tool results:") {
		t.Fatalf("did not expect prose tool-results envelope in cur.Content, got %q", cur.Content)
	}
}

func TestOpenAIToKiroAssistantMapContentInHistory(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "u1"},
			{Role: "assistant", Content: map[string]interface{}{"type": "text", "text": "assistant-map"}},
			{Role: "user", Content: "u2"},
		},
	}

	payload := OpenAIToKiro(req, false)

	if len(payload.ConversationState.History) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(payload.ConversationState.History))
	}
	assistant := payload.ConversationState.History[1].AssistantResponseMessage
	if assistant == nil {
		t.Fatalf("expected second history entry to be assistant")
	}
	if assistant.Content != "assistant-map" {
		t.Fatalf("expected assistant map content preserved, got %q", assistant.Content)
	}
}

func TestOpenAIToKiroAssistantToolCallsDoNotInjectPlaceholder(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Tools: []OpenAITool{{
			Type: "function",
			Function: struct {
				Name        string      `json:"name"`
				Description string      `json:"description"`
				Parameters  interface{} `json:"parameters"`
			}{
				Name:        "get_weather",
				Description: "weather",
				Parameters:  map[string]interface{}{"type": "object"},
			},
		}},
		Messages: []OpenAIMessage{
			{Role: "user", Content: "find weather"},
			{
				Role:    "assistant",
				Content: nil,
				ToolCalls: []ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "get_weather", Arguments: "{}"},
				}},
			},
			{Role: "tool", ToolCallID: "call_1", Content: "sunny"},
			{Role: "user", Content: "continue"},
		},
	}

	payload := OpenAIToKiro(req, false)
	if len(payload.ConversationState.History) < 2 {
		t.Fatalf("expected history with assistant tool call")
	}
	assistant := payload.ConversationState.History[1].AssistantResponseMessage
	if assistant == nil {
		t.Fatalf("expected assistant history entry")
	}
	if assistant.Content != "" {
		t.Fatalf("expected empty assistant content for tool-call-only turn, got %q", assistant.Content)
	}
	if len(assistant.ToolUses) != 1 || assistant.ToolUses[0].ToolUseID != "call_1" {
		t.Fatalf("expected paired tool_use to survive normalization, got %#v", assistant.ToolUses)
	}
}

func TestOpenAIConversationIDStableFromAnchor(t *testing.T) {
	baseMessages := []OpenAIMessage{
		{Role: "system", Content: "You are helpful"},
		{Role: "user", Content: "Build calculator"},
		{Role: "assistant", Content: "Sure"},
		{Role: "user", Content: "Continue"},
	}

	reqA := &OpenAIRequest{Model: "claude-sonnet-4.5", Messages: baseMessages}
	reqB := &OpenAIRequest{Model: "claude-sonnet-4.5", Messages: append(baseMessages, OpenAIMessage{Role: "assistant", Content: "Next step"})}

	payloadA := OpenAIToKiro(reqA, false)
	payloadB := OpenAIToKiro(reqB, false)

	if payloadA.ConversationState.ConversationID == "" || payloadB.ConversationState.ConversationID == "" {
		t.Fatalf("expected non-empty conversation IDs")
	}
	if payloadA.ConversationState.ConversationID != payloadB.ConversationState.ConversationID {
		t.Fatalf("expected stable conversation ID across turns, got %q vs %q", payloadA.ConversationState.ConversationID, payloadB.ConversationState.ConversationID)
	}
}

func TestClaudeConversationIDStableFromAnchor(t *testing.T) {
	reqA := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: "sys",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
		},
	}
	reqB := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: "sys",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "ok"},
			{Role: "user", Content: "next"},
		},
	}

	payloadA := ClaudeToKiro(reqA, false)
	payloadB := ClaudeToKiro(reqB, false)

	if payloadA.ConversationState.ConversationID == "" || payloadB.ConversationState.ConversationID == "" {
		t.Fatalf("expected non-empty conversation IDs")
	}
	if payloadA.ConversationState.ConversationID != payloadB.ConversationState.ConversationID {
		t.Fatalf("expected stable conversation ID across turns, got %q vs %q", payloadA.ConversationState.ConversationID, payloadB.ConversationState.ConversationID)
	}
}

func TestOpenAIConversationIDRandomForSyntheticAnchor(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Messages: []OpenAIMessage{
			{Role: "assistant", Content: "prefill"},
		},
	}

	payloadA := OpenAIToKiro(req, false)
	payloadB := OpenAIToKiro(req, false)

	if payloadA.ConversationState.ConversationID == payloadB.ConversationState.ConversationID {
		t.Fatalf("expected synthetic anchor to generate non-deterministic conversation IDs")
	}
}

func TestClaudeToKiroDropsLeadingAssistantHistory(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		Messages: []ClaudeMessage{
			{Role: "assistant", Content: "prefill"},
			{Role: "user", Content: "real user message"},
		},
	}

	payload := ClaudeToKiro(req, false)

	if len(payload.ConversationState.History) != 0 {
		t.Fatalf("expected leading assistant-only history to be dropped, got %d entries", len(payload.ConversationState.History))
	}

	if strings.Contains(payload.ConversationState.CurrentMessage.UserInputMessage.Content, "Begin conversation") {
		t.Fatalf("unexpected synthetic Begin conversation injection in current content: %q", payload.ConversationState.CurrentMessage.UserInputMessage.Content)
	}
}

func TestKiroToClaudeResponseCanEmitEmptyThinkingBlock(t *testing.T) {
	resp := KiroToClaudeResponse("final answer", "", true, nil, 10, 20, "claude-sonnet-4.6", "")

	if len(resp.Content) != 2 {
		t.Fatalf("expected empty thinking block plus text block, got %d blocks", len(resp.Content))
	}
	if resp.Content[0].Type != "thinking" {
		t.Fatalf("expected first block to be thinking, got %#v", resp.Content[0])
	}
	if resp.Content[0].Thinking != "" {
		t.Fatalf("expected omitted thinking block to have empty content, got %#v", resp.Content[0].Thinking)
	}
	if resp.Content[1].Type != "text" || resp.Content[1].Text != "final answer" {
		t.Fatalf("expected text block to be preserved, got %#v", resp.Content[1])
	}
}

func TestToolResultsContinuationIncludesInstructionPrefix(t *testing.T) {
	t.Skip("retired: tool results no longer travel as a 'Tool results:' prose envelope; see TestOpenAIToolResultsTravelInStructuredField")
}

// TestOpenAIToolResultsTravelInStructuredField verifies that when the OpenAI
// path receives a tool result as the final turn, it is forwarded via
// UserInputMessageContext.ToolResults rather than synthesized into a
// "Tool results:" prose envelope inside userInputMessage.Content. The prose
// envelope was an older convention the upstream model fingerprinted as a
// fake harness signal.
func TestOpenAIToolResultsTravelInStructuredField(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Tools: []OpenAITool{{
			Type: "function",
			Function: struct {
				Name        string      `json:"name"`
				Description string      `json:"description"`
				Parameters  interface{} `json:"parameters"`
			}{
				Name:        "fetch",
				Description: "fetch data",
				Parameters:  map[string]interface{}{"type": "object"},
			},
		}},
		Messages: []OpenAIMessage{
			{Role: "user", Content: "find data"},
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "fetch", Arguments: "{}"},
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: "result-1"},
		},
	}

	payload := OpenAIToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage

	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("expected one tool result in current context, got %#v", cur.UserInputMessageContext)
	}
	if got := cur.UserInputMessageContext.ToolResults[0].Content[0].Text; got != "result-1" {
		t.Fatalf("expected structured tool result text 'result-1', got %q", got)
	}
	if strings.Contains(cur.Content, "Tool results:") {
		t.Fatalf("did not expect 'Tool results:' prose envelope in cur.Content, got %q", cur.Content)
	}
	if strings.Contains(cur.Content, "result-1") {
		t.Fatalf("did not expect tool-result text duplicated in cur.Content, got %q", cur.Content)
	}
}

// claudeToolWithSchema builds a one-tool slice for tests that need a declared
// tool catalog so historical tool blocks survive normalization.
func claudeToolWithSchema(name string) []ClaudeTool {
	return []ClaudeTool{{
		Name:        name,
		Description: name + " description",
		InputSchema: map[string]interface{}{"type": "object"},
	}}
}

// TestExtractToolResultContentNeverEmpty asserts the workaround for the
// upstream Kiro / CodeWhisperer 400 "Improperly formed request" — we never
// emit an empty toolResult text, even when the source content is structurally
// non-text.
func TestExtractToolResultContentNeverEmpty(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
	}{
		{"nil", nil},
		{"empty string", ""},
		{"empty array", []interface{}{}},
		{"image-only block", []interface{}{
			map[string]interface{}{"type": "image", "source": map[string]interface{}{"data": "deadbeef"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractToolResultContent(tc.in)
			if got == "" {
				t.Fatalf("extractToolResultContent(%v) returned empty string", tc.in)
			}
		})
	}
}

// TestExtractToolResultContentPreservesText confirms that ordinary text
// content survives extraction unchanged (no placeholder wrapping).
func TestExtractToolResultContentPreservesText(t *testing.T) {
	got := extractToolResultContent("ok")
	if got != "ok" {
		t.Fatalf("expected 'ok', got %q", got)
	}
	got = extractToolResultContent([]interface{}{
		map[string]interface{}{"type": "text", "text": "hello"},
	})
	if got != "hello" {
		t.Fatalf("expected 'hello', got %q", got)
	}
}

// TestClaudeToKiroMapsIsErrorToErrorStatus verifies that Anthropic's is_error
// flag on a tool_result block is forwarded as KiroToolResult.Status="error",
// not the previously hardcoded "success".
func TestClaudeToKiroMapsIsErrorToErrorStatus(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		Tools: claudeToolWithSchema("fetch"),
		Messages: []ClaudeMessage{
			{Role: "user", Content: "do it"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "tu_1", "name": "fetch", "input": map[string]interface{}{}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{
					"type":        "tool_result",
					"tool_use_id": "tu_1",
					"content":     "boom",
					"is_error":    true,
				},
			}},
		},
	}

	payload := ClaudeToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("expected one tool result")
	}
	if got := cur.UserInputMessageContext.ToolResults[0].Status; got != "error" {
		t.Fatalf("expected Status=error, got %q", got)
	}
}

// TestClaudeToKiroDropsOrphanToolResults verifies that when a user turn
// references a toolUseId that no assistant turn ever emitted, the orphan
// toolResult is stripped instead of forwarded (which would 400 upstream).
func TestClaudeToKiroDropsOrphanToolResults(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		Tools: claudeToolWithSchema("fetch"),
		Messages: []ClaudeMessage{
			{Role: "user", Content: "first turn"},
			{Role: "assistant", Content: "no tools used here"},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{
					"type":        "tool_result",
					"tool_use_id": "tu_orphan",
					"content":     "stale",
				},
			}},
		},
	}

	payload := ClaudeToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) > 0 {
		t.Fatalf("expected orphan toolResults to be stripped, got %#v", cur.UserInputMessageContext.ToolResults)
	}
}

// TestClaudeToKiroDropsOrphanToolUsesInHistory verifies the symmetric case:
// an assistant turn in history emits a tool_use but the next user turn
// never returns a matching tool_result. The orphan tool_use is stripped.
func TestClaudeToKiroDropsOrphanToolUsesInHistory(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		Tools: claudeToolWithSchema("fetch"),
		Messages: []ClaudeMessage{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "thinking..."},
				map[string]interface{}{"type": "tool_use", "id": "tu_orphan", "name": "fetch", "input": map[string]interface{}{}},
			}},
			{Role: "user", Content: "ignore that, here's a fresh question"},
		},
	}

	payload := ClaudeToKiro(req, false)
	for i, h := range payload.ConversationState.History {
		if h.AssistantResponseMessage == nil {
			continue
		}
		if len(h.AssistantResponseMessage.ToolUses) > 0 {
			t.Fatalf("history[%d] still carries orphan toolUses: %#v", i, h.AssistantResponseMessage.ToolUses)
		}
	}
}

// TestClaudeToKiroStripsToolBlocksWhenNoToolsDeclared verifies that when the
// request declares no tools, all historical tool_use / tool_result blocks
// are scrubbed from history. CodeWhisperer validates tool history against
// the declared tool catalog and 400s on a mismatch.
func TestClaudeToKiroStripsToolBlocksWhenNoToolsDeclared(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		// No Tools field set.
		Messages: []ClaudeMessage{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "ok"},
				map[string]interface{}{"type": "tool_use", "id": "tu_1", "name": "fetch", "input": map[string]interface{}{}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "tu_1", "content": "result"},
				map[string]interface{}{"type": "text", "text": "and continue"},
			}},
		},
	}

	payload := ClaudeToKiro(req, false)

	for i, h := range payload.ConversationState.History {
		if h.AssistantResponseMessage != nil && len(h.AssistantResponseMessage.ToolUses) > 0 {
			t.Fatalf("history[%d] assistant still carries toolUses with no tools declared: %#v", i, h.AssistantResponseMessage.ToolUses)
		}
		if h.UserInputMessage != nil && h.UserInputMessage.UserInputMessageContext != nil &&
			len(h.UserInputMessage.UserInputMessageContext.ToolResults) > 0 {
			t.Fatalf("history[%d] user still carries toolResults with no tools declared: %#v", i, h.UserInputMessage.UserInputMessageContext.ToolResults)
		}
	}

	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) > 0 {
		t.Fatalf("expected current-turn toolResults to be stripped, got %#v", cur.UserInputMessageContext.ToolResults)
	}
}

// TestClaudeToKiroDropsAnthropicServerTools verifies that Anthropic-hosted
// "server tools" — web_search, code_execution, computer, text_editor, bash —
// are stripped from the catalog before forwarding upstream.
//
// These tools arrive as `{"type": "web_search_20250305", "name": "web_search",
// "max_uses": 5}` with no description and no input_schema. CodeWhisperer /
// Kiro / AmazonQ have no concept of server-executed tools and reject the
// resulting spec with HTTP 400 "Improperly formed request" because the tool
// description fails minLength=1 validation. Stripping them here also prevents
// the upstream model from attempting to call a tool the proxy can't fulfil.
func TestClaudeToKiroDropsAnthropicServerTools(t *testing.T) {
	req := &ClaudeRequest{
		Model:     "claude-sonnet-4.5",
		MaxTokens: 1024,
		Tools: []ClaudeTool{
			{Type: "web_search_20250305", Name: "web_search"},
			{Type: "code_execution_20250522", Name: "code_execution"},
			{Type: "computer_20250124", Name: "computer"},
			{Type: "text_editor_20241022", Name: "str_replace_editor"},
			{Type: "bash_20241022", Name: "bash"},
			// Custom user tool — keep this one.
			{
				Name:        "lookup_user",
				Description: "Lookup a user by id",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"id": map[string]interface{}{"type": "string"},
					},
				},
			},
		},
		Messages: []ClaudeMessage{
			{Role: "user", Content: "what's the weather"},
		},
	}

	payload := ClaudeToKiro(req, false)
	if payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext == nil {
		t.Fatalf("expected UserInputMessageContext to carry the tool catalog")
	}
	tools := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools
	if len(tools) != 1 {
		t.Fatalf("expected exactly 1 tool forwarded (server tools stripped), got %d: %#v", len(tools), tools)
	}
	if tools[0].ToolSpecification.Name != "lookupUser" {
		t.Fatalf("expected the user-defined tool to survive, got name=%q", tools[0].ToolSpecification.Name)
	}
}

// TestClaudeToKiroBackstopsEmptyToolDescription guards against a future
// Anthropic server-tool variant whose `type` we don't yet recognize: even if
// it slips past the type filter, an empty description must be replaced with a
// placeholder so the upstream tool-spec validator (minLength=1) doesn't 400.
func TestClaudeToKiroBackstopsEmptyToolDescription(t *testing.T) {
	req := &ClaudeRequest{
		Model:     "claude-sonnet-4.5",
		MaxTokens: 1024,
		Tools: []ClaudeTool{
			{Name: "mystery_tool"}, // no Type, no Description, no InputSchema
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
	}

	payload := ClaudeToKiro(req, false)
	tools := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools
	if len(tools) != 1 {
		t.Fatalf("expected mystery_tool forwarded, got %d tools", len(tools))
	}
	if tools[0].ToolSpecification.Description == "" {
		t.Fatalf("expected non-empty description backstop, got empty string")
	}
}

// TestClaudeToKiroCapsMaxTokens verifies that an overlarge max_tokens budget
// is clamped to the documented Kiro upstream cap (32000) instead of being
// forwarded verbatim and triggering a 400 from CodeWhisperer.
func TestClaudeToKiroCapsMaxTokens(t *testing.T) {
	req := &ClaudeRequest{
		Model:     "claude-sonnet-4.5",
		MaxTokens: 64000,
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hi"},
		},
	}

	payload := ClaudeToKiro(req, false)
	if payload.InferenceConfig == nil {
		t.Fatalf("expected InferenceConfig to be set")
	}
	if payload.InferenceConfig.MaxTokens != kiroUpstreamMaxTokens {
		t.Fatalf("expected MaxTokens clamped to %d, got %d", kiroUpstreamMaxTokens, payload.InferenceConfig.MaxTokens)
	}
}

// TestClaudeToKiroPreservesUnderCapMaxTokens verifies that when the client
// requests a budget below the upstream cap, the value is forwarded unchanged.
func TestClaudeToKiroPreservesUnderCapMaxTokens(t *testing.T) {
	req := &ClaudeRequest{
		Model:     "claude-sonnet-4.5",
		MaxTokens: 4096,
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hi"},
		},
	}

	payload := ClaudeToKiro(req, false)
	if payload.InferenceConfig == nil || payload.InferenceConfig.MaxTokens != 4096 {
		t.Fatalf("expected MaxTokens to remain 4096, got %#v", payload.InferenceConfig)
	}
}

// TestOpenAIToKiroEmptyToolResultBecomesPlaceholder verifies the OpenAI path
// substitutes the placeholder when a tool message arrives with empty content,
// preventing the upstream 400 "Improperly formed request" caused by empty
// toolResult text fields.
func TestOpenAIToKiroEmptyToolResultBecomesPlaceholder(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-sonnet-4.5",
		Tools: []OpenAITool{{
			Type: "function",
			Function: struct {
				Name        string      `json:"name"`
				Description string      `json:"description"`
				Parameters  interface{} `json:"parameters"`
			}{
				Name:        "noop",
				Description: "noop",
				Parameters:  map[string]interface{}{"type": "object"},
			},
		}},
		Messages: []OpenAIMessage{
			{Role: "user", Content: "do it"},
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID:   "call_e",
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "noop", Arguments: "{}"},
			}}},
			{Role: "tool", ToolCallID: "call_e", Content: ""},
		},
	}

	payload := OpenAIToKiro(req, false)
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("expected one tool result, got %#v", cur.UserInputMessageContext)
	}
	got := cur.UserInputMessageContext.ToolResults[0].Content[0].Text
	if got == "" {
		t.Fatalf("expected non-empty text fallback, got empty string")
	}
}

// TestCanonicalAnthropicModelID verifies the dotted-to-dashed transform that
// makes Claude Code's "/model" panel resolve correctly. The proxy uses
// dotted forms internally for Kiro upstream routing, but every response
// surface must echo the dashed form so Claude Code stops falling back to the
// "Opus 4 has been updated to the latest" banner.
func TestCanonicalAnthropicModelID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"claude-opus-4.7", "claude-opus-4-7"},
		{"claude-opus-4.6", "claude-opus-4-6"},
		{"claude-sonnet-4.6", "claude-sonnet-4-6"},
		{"claude-haiku-4.5", "claude-haiku-4-5"},
		{"claude-opus-4-7", "claude-opus-4-7"},          // already dashed — idempotent
		{"claude-sonnet-4-5-20251101", "claude-sonnet-4-5-20251101"}, // dated — preserved
		{"", ""},
	}
	for _, c := range cases {
		if got := canonicalAnthropicModelID(c.in); got != c.want {
			t.Fatalf("canonicalAnthropicModelID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCapForModelReturnsUpstreamCap confirms the per-model cap helper still
// returns the universal 32000 limit today; the function exists so future
// per-model overrides land in one place.
func TestCapForModelReturnsUpstreamCap(t *testing.T) {
	for _, m := range []string{
		"claude-opus-4.7",
		"claude-opus-4.6",
		"claude-sonnet-4.6",
		"claude-haiku-4.5",
		"",
		"unknown-model",
	} {
		if got := capForModel(m); got != kiroUpstreamMaxTokens {
			t.Fatalf("capForModel(%q) = %d, want %d", m, got, kiroUpstreamMaxTokens)
		}
	}
}

// TestKiroToClaudeResponsePropagatesUpstreamStopReason verifies the upstream
// truncation signal (max_tokens, surfaced from a ContentLengthExceededException
// or messageStopEvent.stopReason) overrides the local heuristic so Claude Code
// sees stop_reason: max_tokens instead of a clean end_turn.
func TestKiroToClaudeResponsePropagatesUpstreamStopReason(t *testing.T) {
	resp := KiroToClaudeResponse("partial", "", false, nil, 10, 32000, "claude-opus-4.7", "max_tokens")
	if resp.StopReason != "max_tokens" {
		t.Fatalf("expected stop_reason=max_tokens, got %q", resp.StopReason)
	}

	// When upstream surfaced no signal, fall back to tool_use heuristic.
	withTool := KiroToClaudeResponse("", "", false, []KiroToolUse{{ToolUseID: "tu_1", Name: "fs", Input: map[string]interface{}{}}}, 5, 6, "claude-opus-4.7", "")
	if withTool.StopReason != "tool_use" {
		t.Fatalf("expected stop_reason=tool_use, got %q", withTool.StopReason)
	}

	// No upstream signal, no tool use → end_turn.
	plain := KiroToClaudeResponse("ok", "", false, nil, 1, 1, "claude-opus-4.7", "")
	if plain.StopReason != "end_turn" {
		t.Fatalf("expected stop_reason=end_turn, got %q", plain.StopReason)
	}
}

// TestKiroToOpenAIResponseMapsMaxTokensToLength verifies the upstream
// max_tokens stop reason maps to OpenAI finish_reason: length.
func TestKiroToOpenAIResponseMapsMaxTokensToLength(t *testing.T) {
	resp := KiroToOpenAIResponseWithReasoning("partial", "", nil, 10, 32000, "claude-opus-4.7", "reasoning_content", "max_tokens")
	choices, ok := resp["choices"].([]map[string]interface{})
	if !ok || len(choices) != 1 {
		t.Fatalf("expected 1 choice, got %#v", resp["choices"])
	}
	if got := choices[0]["finish_reason"]; got != "length" {
		t.Fatalf("expected finish_reason=length, got %v", got)
	}
}

// TestKiroResponseModelNormalizedToDashForm verifies that response builders
// echo the canonical dashed model id even when given the dotted form.
func TestKiroResponseModelNormalizedToDashForm(t *testing.T) {
	c := KiroToClaudeResponse("hi", "", false, nil, 1, 1, "claude-opus-4.7", "")
	if c.Model != "claude-opus-4-7" {
		t.Fatalf("expected dashed model id in Claude response, got %q", c.Model)
	}

	o := KiroToOpenAIResponse("hi", nil, 1, 1, "claude-sonnet-4.6", "")
	if o.Model != "claude-sonnet-4-6" {
		t.Fatalf("expected dashed model id in OpenAI response, got %q", o.Model)
	}

	r := KiroToOpenAIResponseWithReasoning("hi", "", nil, 1, 1, "claude-haiku-4.5", "reasoning_content", "")
	if got := r["model"]; got != "claude-haiku-4-5" {
		t.Fatalf("expected dashed model id in OpenAI reasoning response, got %v", got)
	}
}

// TestCanonicalizeStopReason verifies the upstream-to-Anthropic stop reason
// mapping covers the variants seen across Bedrock messageStopEvent stopReason
// values, ContentLengthExceededException exception types, and the OpenAI
// "length" finish reason.
func TestCanonicalizeStopReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":               "end_turn",
		"stop":                   "end_turn",
		"COMPLETE":               "end_turn",
		"max_tokens":             "max_tokens",
		"length":                 "max_tokens",
		"CONTENT_LENGTH_EXCEEDS": "max_tokens",
		"contentLengthExceeded":  "max_tokens",
		"tool_use":               "tool_use",
		"tool_calls":             "tool_use",
		"stop_sequence":          "stop_sequence",
		"refusal":                "refusal",
		"unknown_reason":         "",
		"":                       "",
	}
	for in, want := range cases {
		if got := canonicalizeStopReason(in); got != want {
			t.Fatalf("canonicalizeStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestModelSupportsAdaptiveThinking verifies the gate that decides which
// inbound Claude requests should default to thinking.type="adaptive". Only
// Claude 4-family models (sonnet-4*, opus-4*, haiku-4*) qualify.
func TestModelSupportsAdaptiveThinking(t *testing.T) {
	cases := map[string]bool{
		"claude-opus-4.7":              true,
		"claude-opus-4-7":              true,
		"claude-sonnet-4.6":            true,
		"claude-sonnet-4-5-20251101":   true,
		"claude-haiku-4.5":             true,
		"claude-3-opus":                false,
		"claude-3-sonnet":              false,
		"gpt-4o":                       false,
		"":                             false,
	}
	for in, want := range cases {
		if got := modelSupportsAdaptiveThinking(in); got != want {
			t.Fatalf("modelSupportsAdaptiveThinking(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestExtractEventHeadersExceptionType verifies the AWS Event Stream header
// parser also surfaces :exception-type so parseEventStream can detect
// ContentLengthExceededException frames and mark the response as truncated.
func TestExtractEventHeadersExceptionType(t *testing.T) {
	// Build a minimal headers buffer: one string-typed header with name
	// ":exception-type" and value "ContentLengthExceededException".
	name := ":exception-type"
	value := "ContentLengthExceededException"
	buf := []byte{}
	buf = append(buf, byte(len(name)))
	buf = append(buf, []byte(name)...)
	buf = append(buf, 7) // string type
	buf = append(buf, byte(len(value)>>8), byte(len(value)&0xff))
	buf = append(buf, []byte(value)...)

	_, exc, _ := extractEventHeaders(buf)
	if exc != value {
		t.Fatalf("expected :exception-type=%q, got %q", value, exc)
	}

	// Backward-compat: extractEventType still works for legacy callers.
	if got := extractEventType(buf); got != "" {
		t.Fatalf("expected empty event-type for an exception-only frame, got %q", got)
	}
}

