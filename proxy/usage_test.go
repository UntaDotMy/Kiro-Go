package proxy

import (
	"strings"
	"testing"
)

// captureUsage runs a parser over a raw upstream payload and returns the
// UpstreamUsage the producer normalized and fired via OnUsage. captured stays
// false when the parser fired no usage (e.g. an empty payload), which lets a
// test distinguish "no real counts" from a zero-valued real payload.
func captureUsage(t *testing.T, parser func(*KiroStreamCallback) error) (UpstreamUsage, bool) {
	t.Helper()
	var got UpstreamUsage
	captured := false
	cb := &KiroStreamCallback{
		OnText:    func(string, bool) {},
		OnToolUse: func(KiroToolUse) {},
		OnUsage: func(u UpstreamUsage) {
			got = u
			captured = true
		},
	}
	if err := parser(cb); err != nil {
		t.Fatalf("parser returned error: %v", err)
	}
	return got, captured
}

func TestParserUpstreamUsage(t *testing.T) {
	cases := []struct {
		name   string
		parse  func(*KiroStreamCallback) error
		expect UpstreamUsage
	}{
		{
			name: "openai chat completion_tokens already includes reasoning",
			parse: func(cb *KiroStreamCallback) error {
				payload := `data: {"choices":[{"delta":{"content":"hi"}}]}
data: {"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":40,"total_tokens":140,"prompt_tokens_details":{"cached_tokens":25},"completion_tokens_details":{"reasoning_tokens":12}}}
data: [DONE]
`
				return parseOpenAISSE(strings.NewReader(payload), cb)
			},
			expect: UpstreamUsage{
				InputTokens:     100,
				OutputTokens:    40,
				TotalTokens:     140,
				CacheReadTokens: 25,
				ReasoningTokens: 12,
				HasRealCounts:   true,
			},
		},
		{
			name: "openai deepseek flat cache field",
			parse: func(cb *KiroStreamCallback) error {
				payload := `data: {"choices":[{"delta":{"content":"x"}}],"usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60,"prompt_cache_hit_tokens":30}}
data: [DONE]
`
				return parseOpenAISSE(strings.NewReader(payload), cb)
			},
			expect: UpstreamUsage{
				InputTokens:     50,
				OutputTokens:    10,
				TotalTokens:     60,
				CacheReadTokens: 30,
				HasRealCounts:   true,
			},
		},
		{
			name: "anthropic input/output + cache straight through, no reasoning split",
			parse: func(cb *KiroStreamCallback) error {
				payload := `data: {"type":"message_start","message":{"usage":{"input_tokens":200,"cache_read_input_tokens":50,"cache_creation_input_tokens":30}}}
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":75}}
`
				return parseAnthropicSSE(strings.NewReader(payload), cb)
			},
			expect: UpstreamUsage{
				InputTokens:         200,
				OutputTokens:        75,
				CacheReadTokens:     50,
				CacheCreationTokens: 30,
				HasRealCounts:       true,
			},
		},
		{
			name: "gemini candidates EXCLUDES thoughts, fold into output",
			parse: func(cb *KiroStreamCallback) error {
				payload := `data: {"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":300,"candidatesTokenCount":40,"thoughtsTokenCount":15,"cachedContentTokenCount":20}}
`
				return parseGeminiSSE(strings.NewReader(payload), cb)
			},
			expect: UpstreamUsage{
				InputTokens:     300,
				OutputTokens:    55,
				CacheReadTokens: 20,
				ReasoningTokens: 15,
				HasRealCounts:   true,
			},
		},
		{
			name: "ollama prompt_eval->input, eval->output, no cache/reasoning",
			parse: func(cb *KiroStreamCallback) error {
				payload := `{"message":{"content":"hi"},"done":false}
{"done":true,"done_reason":"stop","prompt_eval_count":120,"eval_count":35}
`
				return parseOllamaStream(strings.NewReader(payload), cb)
			},
			expect: UpstreamUsage{
				InputTokens:   120,
				OutputTokens:  35,
				HasRealCounts: true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, captured := captureUsage(t, tc.parse)
			if !captured {
				t.Fatalf("expected parser to fire OnUsage, got none")
			}
			if got != tc.expect {
				t.Fatalf("usage mismatch\n got:  %+v\n want: %+v", got, tc.expect)
			}
			// Per-provider invariant: reasoning is a subset of output, never additive.
			if got.OutputTokens < got.ReasoningTokens {
				t.Fatalf("invariant violated: OutputTokens(%d) < ReasoningTokens(%d)", got.OutputTokens, got.ReasoningTokens)
			}
		})
	}
}

func TestResolveUsageRealValueWins(t *testing.T) {
	up := UpstreamUsage{
		InputTokens:     1000,
		OutputTokens:    200,
		TotalTokens:     1200,
		CacheReadTokens: 100,
		ReasoningTokens: 40,
		HasRealCounts:   true,
	}
	// Estimates are deliberately wrong; the real upstream values must win.
	got := resolveUsage(false, up, promptCacheUsage{}, false, 5, 7, 9)
	if got.InputTokens != 1000 {
		t.Fatalf("InputTokens: got %d want 1000 (real must beat estimate)", got.InputTokens)
	}
	if got.OutputTokens != 200 {
		t.Fatalf("OutputTokens: got %d want 200 (real must beat estimate)", got.OutputTokens)
	}
	if got.TotalTokens != 1200 {
		t.Fatalf("TotalTokens: got %d want 1200 (real total verbatim)", got.TotalTokens)
	}
	if got.ReasoningTokens != 40 {
		t.Fatalf("ReasoningTokens: got %d want 40", got.ReasoningTokens)
	}
	if got.OutputTokens < got.ReasoningTokens {
		t.Fatalf("invariant violated: OutputTokens(%d) < ReasoningTokens(%d)", got.OutputTokens, got.ReasoningTokens)
	}
}

func TestResolveUsageTotalNeverAddsReasoning(t *testing.T) {
	// No real total reported → resolver derives Input+Output and must NOT add
	// reasoning on top (reasoning already lives inside output).
	up := UpstreamUsage{
		InputTokens:     100,
		OutputTokens:    60,
		ReasoningTokens: 25,
		HasRealCounts:   true,
	}
	got := resolveUsage(false, up, promptCacheUsage{}, false, 0, 0, 0)
	if got.TotalTokens != 160 {
		t.Fatalf("TotalTokens: got %d want 160 (Input+Output, never +Reasoning)", got.TotalTokens)
	}
}

func TestResolveUsageKiroEstimateNotOverwritten(t *testing.T) {
	// Kiro path: upstream reports NO real token integers (HasRealCounts=false),
	// only a contextUsagePercentage-derived input and a local output estimate.
	// The resolver must keep the estimates, never zero them.
	up := UpstreamUsage{} // Kiro never sets this
	contextDerivedInput := 8000
	estimatedOutput := 350
	got := resolveUsage(true, up, promptCacheUsage{}, false, contextDerivedInput, 1234, estimatedOutput)
	if got.InputTokens != contextDerivedInput {
		t.Fatalf("InputTokens: got %d want %d (context-derived estimate preserved)", got.InputTokens, contextDerivedInput)
	}
	if got.OutputTokens != estimatedOutput {
		t.Fatalf("OutputTokens: got %d want %d (local estimate preserved)", got.OutputTokens, estimatedOutput)
	}
	if got.TotalTokens != contextDerivedInput+estimatedOutput {
		t.Fatalf("TotalTokens: got %d want %d", got.TotalTokens, contextDerivedInput+estimatedOutput)
	}
}

func TestResolveUsageKiroCacheEstimateVsNonKiroPassthrough(t *testing.T) {
	estimate := promptCacheUsage{
		CacheReadInputTokens:       400,
		CacheCreationInputTokens:   100,
		CacheCreation5mInputTokens: 100,
	}

	// Kiro: the local estimate is authoritative and present when a profile exists.
	kiro := resolveUsage(true, UpstreamUsage{}, estimate, true, 5000, 0, 200)
	if !kiro.CachePresent {
		t.Fatalf("kiro: expected CachePresent=true when estimator profile exists")
	}
	if kiro.CacheReadTokens != 400 || kiro.CacheCreationTokens != 100 {
		t.Fatalf("kiro: cache estimate not used, got read=%d creation=%d", kiro.CacheReadTokens, kiro.CacheCreationTokens)
	}

	// Non-Kiro with no real cache reported → never emit a Kiro-style estimate.
	nonKiro := resolveUsage(false, UpstreamUsage{InputTokens: 100, OutputTokens: 10, HasRealCounts: true}, estimate, true, 0, 0, 0)
	if nonKiro.CachePresent {
		t.Fatalf("non-kiro: must NOT emit cache when provider reported none")
	}
	if nonKiro.CacheReadTokens != 0 || nonKiro.CacheCreationTokens != 0 {
		t.Fatalf("non-kiro: leaked estimated cache, got read=%d creation=%d", nonKiro.CacheReadTokens, nonKiro.CacheCreationTokens)
	}

	// Non-Kiro WITH a real reported cache → pass it through.
	real := resolveUsage(false, UpstreamUsage{InputTokens: 100, OutputTokens: 10, CacheReadTokens: 70, HasRealCounts: true}, estimate, true, 0, 0, 0)
	if !real.CachePresent || real.CacheReadTokens != 70 {
		t.Fatalf("non-kiro real cache: got present=%v read=%d want present=true read=70", real.CachePresent, real.CacheReadTokens)
	}
}
