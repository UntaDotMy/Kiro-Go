package proxy

import (
	"errors"
	"kiro-go/auth"
	"strings"
	"testing"
)

// TestParseCodexResponsesSSE exercises the Codex Responses SSE parser: text
// deltas, reasoning deltas (as thinking), a streamed function_call, and the
// final usage + stop reason on response.completed.
func TestParseCodexResponsesSSE(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Hel"}`,
		`data: {"type":"response.output_text.delta","delta":"lo"}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"thinking..."}`,
		`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"item_1","call_id":"call_42","name":"get_time"}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_1","delta":"{\"tz\":"}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_1","delta":"\"utc\"}"}`,
		`data: {"type":"response.output_item.done","item":{"type":"function_call","id":"item_1","call_id":"call_42","name":"get_time","arguments":"{\"tz\":\"utc\"}"}}`,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":7}}}`,
	}, "\n")

	var text, thinking strings.Builder
	var tools []KiroToolUse
	var inTok, outTok int
	var stop string
	cb := &KiroStreamCallback{
		OnText: func(t string, isThinking bool) {
			if isThinking {
				thinking.WriteString(t)
			} else {
				text.WriteString(t)
			}
		},
		OnToolUse:    func(tu KiroToolUse) { tools = append(tools, tu) },
		OnComplete:   func(in, out int) { inTok, outTok = in, out },
		OnStopReason: func(r string) { stop = r },
	}
	if err := parseCodexResponsesSSE(strings.NewReader(stream), cb); err != nil {
		t.Fatalf("parseCodexResponsesSSE: %v", err)
	}
	if text.String() != "Hello" {
		t.Errorf("text = %q, want Hello", text.String())
	}
	if thinking.String() != "thinking..." {
		t.Errorf("thinking = %q", thinking.String())
	}
	if len(tools) != 1 || tools[0].Name != "get_time" || tools[0].ToolUseID != "call_42" {
		t.Fatalf("tool wrong: %+v", tools)
	}
	if tools[0].Input["tz"] != "utc" {
		t.Errorf("tool arg tz = %v, want utc", tools[0].Input["tz"])
	}
	if inTok != 11 || outTok != 7 {
		t.Errorf("tokens = %d/%d, want 11/7", inTok, outTok)
	}
	// tool_use takes precedence as the stop reason once a function_call completes.
	if stop != "tool_use" {
		t.Errorf("stop = %q, want tool_use", stop)
	}
}

// TestParseCodexResponsesFailed verifies a response.failed event surfaces as an
// error. A capacity/quota refusal (usage_limit_reached) must become a QuotaError
// so the pool cools the account and failover tries the next one; a generic
// failure stays a plain error.
func TestParseCodexResponsesFailed(t *testing.T) {
	// Quota/capacity refusal -> QuotaError.
	stream := `data: {"type":"response.failed","response":{"error":{"message":"usage_limit_reached"}}}`
	err := parseCodexResponsesSSE(strings.NewReader(stream), &KiroStreamCallback{})
	if err == nil {
		t.Fatal("expected an error for response.failed")
	}
	var qe *QuotaError
	if !errors.As(err, &qe) {
		t.Fatalf("usage_limit_reached should surface as *QuotaError, got %T: %v", err, err)
	}

	// Generic failure -> plain error carrying the message.
	stream2 := `data: {"type":"response.failed","response":{"error":{"message":"something else broke"}}}`
	err2 := parseCodexResponsesSSE(strings.NewReader(stream2), &KiroStreamCallback{})
	if err2 == nil || !strings.Contains(err2.Error(), "something else broke") {
		t.Fatalf("generic failure should be a plain error with the message, got %v", err2)
	}
	if errors.As(err2, &qe) {
		t.Fatalf("generic failure must NOT be a QuotaError, got %v", err2)
	}
}

// TestBuildCodexResponsesBody verifies the request body forces store=false,
// stream=true, includes encrypted reasoning, and builds instructions+input from a
// Claude request.
func TestBuildCodexResponsesBody(t *testing.T) {
	nr := &NormalizedRequest{
		Model:  "gpt-5-codex",
		Effort: "high",
		Claude: &ClaudeRequest{
			System:   "be brief",
			Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
		},
	}
	raw, err := buildCodexResponsesBody(nr, "gpt-5-codex")
	if err != nil {
		t.Fatalf("buildCodexResponsesBody: %v", err)
	}
	s := string(raw)
	for _, want := range []string{`"store":false`, `"stream":true`, `"reasoning.encrypted_content"`, `"instructions":"be brief"`, `"effort":"high"`} {
		if !strings.Contains(s, want) {
			t.Errorf("body missing %q\nbody: %s", want, s)
		}
	}
}

// TestNormalizeCodexEffort pins the effort mapping.
func TestNormalizeCodexEffort(t *testing.T) {
	cases := map[string]string{"low": "low", "medium": "medium", "high": "high", "xhigh": "high", "max": "high", "minimal": "", "": ""}
	for in, want := range cases {
		if got := normalizeCodexEffort(in); got != want {
			t.Errorf("normalizeCodexEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDecodeCodexIDToken decodes a hand-built id_token JWT with the OpenAI auth
// claim and confirms the account id + plan + email are extracted. (No signature
// verification — we only decode the payload segment.)
func TestDecodeCodexIDToken(t *testing.T) {
	// header.payload.signature; only the payload (segment 1) matters.
	// payload = {"email":"a@b.com","https://api.openai.com/auth":{"chatgpt_account_id":"acct_9","chatgpt_plan_type":"pro"}}
	payload := `eyJlbWFpbCI6ImFAYi5jb20iLCJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdF85IiwiY2hhdGdwdF9wbGFuX3R5cGUiOiJwcm8ifX0`
	tok := "eyJhbGciOiJub25lIn0." + payload + ".sig"
	acct, plan, email := auth.DecodeCodexIDToken(tok)
	if acct != "acct_9" {
		t.Errorf("accountID = %q, want acct_9", acct)
	}
	if plan != "pro" {
		t.Errorf("plan = %q, want pro", plan)
	}
	if email != "a@b.com" {
		t.Errorf("email = %q, want a@b.com", email)
	}
}
