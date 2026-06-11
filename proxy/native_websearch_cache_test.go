package proxy

import (
	"strings"
	"testing"
)

// ============================================================================
// Native web-search citation surfacing + real upstream cache passthrough +
// per-family context window. All evidence-backed against primary sources:
//   - Gemini grounding shape: gemini-cli packages/core/src/tools/web-search.ts
//     (candidates[0].groundingMetadata.groundingChunks[].web.{uri,title})
//   - OpenAI cached tokens: usage.prompt_tokens_details.cached_tokens
//   - Gemini cached tokens: usageMetadata.cachedContentTokenCount
//   - Anthropic block shape: platform.claude.com web-search-tool docs
// ============================================================================

// --- Gemini native grounding -> OnWebSearchResults --------------------------

func TestParseGeminiSSE_GroundingCitations(t *testing.T) {
	// groundingMetadata documented to arrive on the final chunk alongside
	// usageMetadata; the parser captures the last non-empty occurrence.
	stream := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"Go 1.26 was released."}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP","groundingMetadata":{"webSearchQueries":["go 1.26 release"],"groundingChunks":[{"web":{"uri":"https://go.dev/blog/go1.26","title":"Go 1.26 Release Notes"}},{"web":{"uri":"https://example.com/go","title":"Go News"}}]}}],"usageMetadata":{"promptTokenCount":40,"candidatesTokenCount":10,"cachedContentTokenCount":32}}`,
	}, "\n")

	var query string
	var results []WebSearchResult
	var cacheRead, cacheCreation int
	cb := &KiroStreamCallback{
		OnText:       func(string, bool) {},
		OnComplete:   func(int, int) {},
		OnCacheUsage: func(r, c int) { cacheRead, cacheCreation = r, c },
		OnWebSearchResults: func(q string, res []WebSearchResult) {
			query = q
			results = res
		},
	}
	if err := parseGeminiSSE(strings.NewReader(stream), cb); err != nil {
		t.Fatalf("parseGeminiSSE: %v", err)
	}
	if query != "go 1.26 release" {
		t.Errorf("query = %q, want %q", query, "go 1.26 release")
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2: %+v", len(results), results)
	}
	if results[0].URL != "https://go.dev/blog/go1.26" || results[0].Title != "Go 1.26 Release Notes" {
		t.Errorf("result[0] = %+v", results[0])
	}
	// Gemini cachedContentTokenCount must pass through as real cache read.
	if cacheRead != 32 || cacheCreation != 0 {
		t.Errorf("cache = %d/%d, want 32/0", cacheRead, cacheCreation)
	}
}

func TestParseGeminiSSE_NoGroundingNoCallback(t *testing.T) {
	stream := `data: {"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2}}`
	called := false
	cacheCalled := false
	cb := &KiroStreamCallback{
		OnText:             func(string, bool) {},
		OnComplete:         func(int, int) {},
		OnCacheUsage:       func(int, int) { cacheCalled = true },
		OnWebSearchResults: func(string, []WebSearchResult) { called = true },
	}
	if err := parseGeminiSSE(strings.NewReader(stream), cb); err != nil {
		t.Fatalf("parseGeminiSSE: %v", err)
	}
	if called {
		t.Error("OnWebSearchResults fired with no grounding chunks")
	}
	if cacheCalled {
		t.Error("OnCacheUsage fired with no cachedContentTokenCount")
	}
}

// --- OpenAI real cached tokens ----------------------------------------------

func TestParseOpenAISSE_CachedTokensPassthrough(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":768}}}`,
		`data: [DONE]`,
	}, "\n")

	var cacheRead, cacheCreation int
	var cacheCalled bool
	cb := &KiroStreamCallback{
		OnText:       func(string, bool) {},
		OnComplete:   func(int, int) {},
		OnCacheUsage: func(r, c int) { cacheRead, cacheCreation = r, c; cacheCalled = true },
	}
	if err := parseOpenAISSE(strings.NewReader(stream), cb); err != nil {
		t.Fatalf("parseOpenAISSE: %v", err)
	}
	if !cacheCalled || cacheRead != 768 || cacheCreation != 0 {
		t.Errorf("cache = %d/%d called=%v, want 768/0 true", cacheRead, cacheCreation, cacheCalled)
	}
}

func TestParseOpenAISSE_DeepSeekCacheHitTokens(t *testing.T) {
	// DeepSeek reports the cached prefix as a flat prompt_cache_hit_tokens.
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":500,"completion_tokens":5,"prompt_cache_hit_tokens":256}}`,
		`data: [DONE]`,
	}, "\n")
	var cacheRead int
	cb := &KiroStreamCallback{
		OnText:       func(string, bool) {},
		OnComplete:   func(int, int) {},
		OnCacheUsage: func(r, _ int) { cacheRead = r },
	}
	if err := parseOpenAISSE(strings.NewReader(stream), cb); err != nil {
		t.Fatalf("parseOpenAISSE: %v", err)
	}
	if cacheRead != 256 {
		t.Errorf("cacheRead = %d, want 256", cacheRead)
	}
}

func TestParseOpenAISSE_NoCacheNoCallback(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
		`data: [DONE]`,
	}, "\n")
	called := false
	cb := &KiroStreamCallback{
		OnText:       func(string, bool) {},
		OnComplete:   func(int, int) {},
		OnCacheUsage: func(int, int) { called = true },
	}
	if err := parseOpenAISSE(strings.NewReader(stream), cb); err != nil {
		t.Fatalf("parseOpenAISSE: %v", err)
	}
	if called {
		t.Error("OnCacheUsage fired when provider reported no cached tokens")
	}
}

// --- Anthropic real cache passthrough ---------------------------------------

func TestParseAnthropicSSE_CachePassthrough(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":2000,"cache_read_input_tokens":1500,"cache_creation_input_tokens":300}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		`data: {"type":"message_stop"}`,
	}, "\n")
	var cacheRead, cacheCreation int
	cb := &KiroStreamCallback{
		OnText:       func(string, bool) {},
		OnComplete:   func(int, int) {},
		OnCacheUsage: func(r, c int) { cacheRead, cacheCreation = r, c },
		OnStopReason: func(string) {},
	}
	if err := parseAnthropicSSE(strings.NewReader(stream), cb); err != nil {
		t.Fatalf("parseAnthropicSSE: %v", err)
	}
	if cacheRead != 1500 || cacheCreation != 300 {
		t.Errorf("cache = %d/%d, want 1500/300", cacheRead, cacheCreation)
	}
}

// --- resolveResponseCache: the cross-backend hard rule ----------------------

func TestResolveResponseCache_KiroUsesEstimate(t *testing.T) {
	est := promptCacheUsage{CacheReadInputTokens: 100, CacheCreationInputTokens: 50}
	usage, include := resolveResponseCache(true, est, true, 0, 0)
	if !include || usage.CacheReadInputTokens != 100 || usage.CacheCreationInputTokens != 50 {
		t.Errorf("kiro path: got %+v include=%v, want estimate emitted", usage, include)
	}
	// Kiro with no profile → no cache.
	if _, inc := resolveResponseCache(true, promptCacheUsage{}, false, 0, 0); inc {
		t.Error("kiro with no profile should not emit cache")
	}
}

func TestResolveResponseCache_NonKiroNeverEmitsEstimate(t *testing.T) {
	// A Kiro estimate must NEVER leak onto a non-Kiro response.
	est := promptCacheUsage{CacheReadInputTokens: 9999, CacheCreationInputTokens: 8888}
	usage, include := resolveResponseCache(false, est, true, 0, 0)
	if include {
		t.Errorf("non-Kiro with no real cache must emit nothing, got %+v", usage)
	}
}

func TestResolveResponseCache_NonKiroPassesRealCache(t *testing.T) {
	usage, include := resolveResponseCache(false, promptCacheUsage{}, false, 768, 0)
	if !include || usage.CacheReadInputTokens != 768 || usage.CacheCreationInputTokens != 0 {
		t.Errorf("non-Kiro real cache: got %+v include=%v, want 768/0 emitted", usage, include)
	}
}

// --- per-family context window fallback -------------------------------------

func TestFamilyContextWindowFor(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"gemini-2.5-pro", 1_048_576},
		{"gemini-2.5-flash-preview", 1_048_576},
		{"gemini-1.5-pro-002", 2_000_000},
		{"gemini-1.5-flash", 1_000_000},
		{"gemini-3.0-ultra", 1_000_000}, // unknown minor → family default
		{"qwen-turbo-latest", 1_000_000},
		{"qwen-plus", 131_072},
		{"qwen-max-2025", 32_768},
		{"qwen3-235b", 262_144},
		{"glm-4.6", 200_000},
		{"glm-4-flash", 131_072},
		{"deepseek-chat", 131_072},
		{"claude-opus-4-7", 0}, // Claude handled by version parse, not this table
		{"totally-unknown-model", 0},
		{"", 0},
	}
	for _, c := range cases {
		if got := familyContextWindowFor(c.model); got != c.want {
			t.Errorf("familyContextWindowFor(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}

func TestGetContextWindowSize_NonKiroFamilies(t *testing.T) {
	if got := getContextWindowSize("gemini-2.5-pro"); got != 1_048_576 {
		t.Errorf("gemini-2.5-pro window = %d, want 1048576", got)
	}
	if got := getContextWindowSize("qwen-plus"); got != 131_072 {
		t.Errorf("qwen-plus window = %d, want 131072", got)
	}
	// Unknown model falls back to the flat default.
	if got := getContextWindowSize("some-random-model"); got != defaultContextWindow {
		t.Errorf("unknown window = %d, want %d", got, defaultContextWindow)
	}
}

// --- native citation block splicing -----------------------------------------

func TestSpliceNativeCitationBlocks_BeforeText(t *testing.T) {
	blocks := []ClaudeContentBlock{
		{Type: "thinking", Thinking: "..."},
		{Type: "text", Text: "Answer with sources."},
	}
	results := []WebSearchResult{{Title: "Src", URL: "https://x.test"}}
	out := spliceNativeCitationBlocks(blocks, "srvtoolu_1", "q", results)

	// Expect: thinking, server_tool_use, web_search_tool_result, text.
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4: %+v", len(out), out)
	}
	if out[0].Type != "thinking" {
		t.Errorf("out[0] = %q, want thinking", out[0].Type)
	}
	if out[1].Type != "server_tool_use" || out[1].ID != "srvtoolu_1" {
		t.Errorf("out[1] = %+v, want server_tool_use", out[1])
	}
	if out[2].Type != "web_search_tool_result" || out[2].ToolUseID != "srvtoolu_1" {
		t.Errorf("out[2] = %+v, want web_search_tool_result", out[2])
	}
	if out[3].Type != "text" {
		t.Errorf("out[3] = %q, want text (citations precede answer)", out[3].Type)
	}
}

func TestSpliceNativeCitationBlocks_NoTextAppends(t *testing.T) {
	blocks := []ClaudeContentBlock{{Type: "tool_use", Name: "x"}}
	out := spliceNativeCitationBlocks(blocks, "id", "q", []WebSearchResult{{URL: "u"}})
	if len(out) != 3 || out[1].Type != "server_tool_use" {
		t.Fatalf("append path wrong: %+v", out)
	}
}

func TestSpliceNativeCitationBlocks_Empty(t *testing.T) {
	blocks := []ClaudeContentBlock{{Type: "text", Text: "x"}}
	out := spliceNativeCitationBlocks(blocks, "id", "q", nil)
	if len(out) != 1 {
		t.Errorf("empty results should not alter blocks: %+v", out)
	}
}

func TestSpliceNativeCitationBlocks_CapsResults(t *testing.T) {
	many := make([]WebSearchResult, maxWebSearchResults+5)
	for i := range many {
		many[i] = WebSearchResult{URL: "u"}
	}
	out := spliceNativeCitationBlocks([]ClaudeContentBlock{{Type: "text"}}, "id", "q", many)
	// server_tool_use + web_search_tool_result + text = 3; result items capped.
	var wsr *ClaudeContentBlock
	for i := range out {
		if out[i].Type == "web_search_tool_result" {
			wsr = &out[i]
		}
	}
	if wsr == nil {
		t.Fatal("no web_search_tool_result block")
	}
	items, ok := wsr.Content.([]map[string]interface{})
	if !ok {
		t.Fatalf("content type = %T", wsr.Content)
	}
	if len(items) != maxWebSearchResults {
		t.Errorf("result items = %d, want capped to %d", len(items), maxWebSearchResults)
	}
}
