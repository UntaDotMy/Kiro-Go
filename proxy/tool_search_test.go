package proxy

import (
	"reflect"
	"testing"
)

// ---- detection & partition -------------------------------------------------

func TestDetectToolSearchMode(t *testing.T) {
	cases := []struct {
		name  string
		tools []ClaudeTool
		want  toolSearchMode
	}{
		{"regex dated", []ClaudeTool{{Type: "tool_search_tool_regex_20251119"}}, toolSearchModeRegex},
		{"bm25 dated", []ClaudeTool{{Type: "tool_search_tool_bm25_20251119"}}, toolSearchModeBM25},
		{"regex undated alias", []ClaudeTool{{Type: "tool_search_tool_regex"}}, toolSearchModeRegex},
		{"bm25 undated alias", []ClaudeTool{{Type: "tool_search_tool_bm25"}}, toolSearchModeBM25},
		{"case-insensitive", []ClaudeTool{{Type: "TOOL_SEARCH_TOOL_BM25_20251119"}}, toolSearchModeBM25},
		{"none", []ClaudeTool{{Name: "read_file"}, {Type: "web_search_20250305"}}, toolSearchModeNone},
		{"empty", nil, toolSearchModeNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectToolSearchMode(tc.tools); got != tc.want {
				t.Fatalf("detectToolSearchMode = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPartitionToolSearchTools(t *testing.T) {
	tools := []ClaudeTool{
		{Name: "read_file"},                                                   // eager
		{Name: "get_weather", DeferLoading: true},                             // deferred
		{Name: "create_issue", DeferLoading: true},                            // deferred
		{Type: "tool_search_tool_regex_20251119", Name: "x"},                  // search tool (dropped)
		{Type: "web_search_20250305", Name: "web_search", DeferLoading: true}, // server tool: defer ignored → eager bucket
	}
	eager, deferred, mode := partitionToolSearchTools(tools)

	if mode != toolSearchModeRegex {
		t.Fatalf("mode = %v, want regex", mode)
	}
	// search tool must never appear in either bucket
	for _, e := range append(append([]ClaudeTool{}, eager...), deferred...) {
		if isToolSearchTool(e) {
			t.Fatalf("tool_search tool leaked into a bucket: %+v", e)
		}
	}
	if len(deferred) != 2 {
		t.Fatalf("deferred = %d tools, want 2 (%+v)", len(deferred), deferred)
	}
	// web_search (server tool) keeps its defer flag ignored → must be eager, not deferred
	var sawWebSearch bool
	for _, e := range eager {
		if e.Name == "web_search" {
			sawWebSearch = true
		}
	}
	if !sawWebSearch {
		t.Fatalf("server tool with defer_loading should land in eager bucket, eager=%+v", eager)
	}
}

func TestRequestHasToolSearch(t *testing.T) {
	// search tool + deferred tool → true
	if !requestHasToolSearch([]ClaudeTool{
		{Type: "tool_search_tool_bm25_20251119"},
		{Name: "t", DeferLoading: true},
	}) {
		t.Fatal("expected true for search tool + deferred tool")
	}
	// search tool but NO deferred tool → false (inert, falls through)
	if requestHasToolSearch([]ClaudeTool{
		{Type: "tool_search_tool_regex_20251119"},
		{Name: "t"},
	}) {
		t.Fatal("expected false when no deferred tools present")
	}
	// deferred tool but no search tool → false
	if requestHasToolSearch([]ClaudeTool{{Name: "t", DeferLoading: true}}) {
		t.Fatal("expected false when no search tool present")
	}
}

// ---- query extraction ------------------------------------------------------

func TestExtractToolSearchQuery(t *testing.T) {
	cases := []struct {
		in   map[string]interface{}
		want string
	}{
		{map[string]interface{}{"query": "weather"}, "weather"},
		{map[string]interface{}{"pattern": "create.*issue"}, "create.*issue"},
		{map[string]interface{}{"q": "  trimmed  "}, "trimmed"},
		{map[string]interface{}{"other": "x"}, ""},
		{map[string]interface{}{"query": ""}, ""},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := extractToolSearchQuery(tc.in); got != tc.want {
			t.Fatalf("extractToolSearchQuery(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---- regex search ----------------------------------------------------------

func TestSearchRegex(t *testing.T) {
	deferred := []ClaudeTool{
		{Name: "get_weather", Description: "Current weather and forecast"},
		{Name: "create_issue", Description: "Open a GitHub issue"},
		{Name: "close_issue", Description: "Close a GitHub issue"},
	}

	// name match
	got := searchDeferredTools(toolSearchModeRegex, "weather", deferred)
	if len(got) != 1 || got[0].Name != "get_weather" {
		t.Fatalf("regex name match = %+v", got)
	}

	// description / regex match across two tools
	got = searchDeferredTools(toolSearchModeRegex, "issue", deferred)
	if len(got) != 2 {
		t.Fatalf("regex 'issue' = %d tools, want 2", len(got))
	}

	// real regex
	got = searchDeferredTools(toolSearchModeRegex, "create.*issue", deferred)
	if len(got) != 1 || got[0].Name != "create_issue" {
		t.Fatalf("regex 'create.*issue' = %+v", got)
	}

	// no match
	if got := searchDeferredTools(toolSearchModeRegex, "nonexistent", deferred); len(got) != 0 {
		t.Fatalf("expected no match, got %+v", got)
	}
}

func TestSearchRegexInvalidPatternFallsBackToSubstring(t *testing.T) {
	deferred := []ClaudeTool{{Name: "get_weather", Description: "weather (forecast"}}
	// "(" is an invalid regex; must not panic and should substring-match.
	got := searchDeferredTools(toolSearchModeRegex, "weather (", deferred)
	if len(got) != 1 {
		t.Fatalf("invalid-regex fallback = %+v, want 1 substring match", got)
	}
}

func TestSearchRegexCapsResults(t *testing.T) {
	var deferred []ClaudeTool
	for i := 0; i < 20; i++ {
		deferred = append(deferred, ClaudeTool{Name: "tool_" + itoa(i), Description: "common keyword"})
	}
	got := searchDeferredTools(toolSearchModeRegex, "common", deferred)
	if len(got) != maxToolSearchResults {
		t.Fatalf("cap = %d, want %d", len(got), maxToolSearchResults)
	}
}

// ---- bm25 search -----------------------------------------------------------

func TestSearchBM25RanksRelevantFirst(t *testing.T) {
	deferred := []ClaudeTool{
		{Name: "create_issue", Description: "Open a new GitHub issue in a repository"},
		{Name: "get_weather", Description: "Weather forecast for a city"},
		{Name: "list_issues", Description: "List open issues; an issue tracker query"},
	}
	got := searchDeferredTools(toolSearchModeBM25, "issue tracker", deferred)
	if len(got) == 0 {
		t.Fatal("bm25 returned no results")
	}
	// "list_issues" mentions "issue" twice plus "tracker" → should rank first.
	if got[0].Name != "list_issues" {
		t.Fatalf("bm25 top = %q, want list_issues; full=%+v", got[0].Name, got)
	}
	// get_weather has zero overlap → must be excluded.
	for _, g := range got {
		if g.Name == "get_weather" {
			t.Fatalf("bm25 included zero-overlap tool: %+v", got)
		}
	}
}

func TestSearchEmptyInputs(t *testing.T) {
	deferred := []ClaudeTool{{Name: "x", Description: "y"}}
	if got := searchDeferredTools(toolSearchModeRegex, "", deferred); got != nil {
		t.Fatalf("empty query should return nil, got %+v", got)
	}
	if got := searchDeferredTools(toolSearchModeBM25, "x", nil); got != nil {
		t.Fatalf("empty corpus should return nil, got %+v", got)
	}
}

// ---- block shaping ---------------------------------------------------------

func TestBuildToolSearchResultBlocks(t *testing.T) {
	matched := []ClaudeTool{
		{Name: "get_weather"},
		{Name: "create_issue"},
	}
	blocks := buildToolSearchResultBlocks(toolSearchModeRegex, "srvtoolu_1", "weather", matched)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}

	stu := blocks[0]
	if stu["type"] != "server_tool_use" || stu["id"] != "srvtoolu_1" || stu["name"] != "tool_search_tool_regex" {
		t.Fatalf("server_tool_use block malformed: %+v", stu)
	}
	if in, ok := stu["input"].(map[string]interface{}); !ok || in["query"] != "weather" {
		t.Fatalf("server_tool_use input malformed: %+v", stu["input"])
	}

	res := blocks[1]
	if res["type"] != "tool_search_tool_result" || res["tool_use_id"] != "srvtoolu_1" {
		t.Fatalf("result block malformed: %+v", res)
	}
	content, ok := res["content"].(map[string]interface{})
	if !ok || content["type"] != "tool_search_tool_search_result" {
		t.Fatalf("result content malformed: %+v", res["content"])
	}
	refs, ok := content["tool_references"].([]map[string]interface{})
	if !ok || len(refs) != 2 {
		t.Fatalf("tool_references malformed: %+v", content["tool_references"])
	}
	if refs[0]["type"] != "tool_reference" || refs[0]["tool_name"] != "get_weather" {
		t.Fatalf("tool_reference[0] malformed: %+v", refs[0])
	}
}

func TestFormatToolSearchForModel(t *testing.T) {
	if s := formatToolSearchForModel("weather", nil); s == "" {
		t.Fatal("empty-match feedback should be non-empty")
	}
	matched := []ClaudeTool{{Name: "get_weather", Description: "Current weather"}}
	s := formatToolSearchForModel("weather", matched)
	if !contains(s, "get_weather") {
		t.Fatalf("feedback missing tool name: %q", s)
	}
}

// ---- history expansion -----------------------------------------------------

func TestDiscoveredToolNamesInHistory(t *testing.T) {
	messages := []ClaudeMessage{
		{Role: "user", Content: "hi"}, // string content tolerated
		{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "server_tool_use", "id": "s1", "name": "tool_search_tool_regex"},
			map[string]interface{}{
				"type":        "tool_search_tool_result",
				"tool_use_id": "s1",
				"content": map[string]interface{}{
					"type":            "tool_search_tool_search_result",
					"tool_references": []interface{}{map[string]interface{}{"type": "tool_reference", "tool_name": "get_weather"}},
				},
			},
			map[string]interface{}{"type": "tool_use", "id": "t1", "name": "create_issue"},
		}},
	}
	names := discoveredToolNamesInHistory(messages)
	if !names["get_weather"] {
		t.Fatal("expected get_weather discovered from tool_reference")
	}
	if !names["create_issue"] {
		t.Fatal("expected create_issue discovered from tool_use")
	}
}

func TestExpandDeferredByName(t *testing.T) {
	deferred := []ClaudeTool{
		{Name: "get_weather", DeferLoading: true},
		{Name: "create_issue", DeferLoading: true},
	}
	out := expandDeferredByName(deferred, map[string]bool{"get_weather": true})
	if len(out) != 1 || out[0].Name != "get_weather" {
		t.Fatalf("expand = %+v, want only get_weather", out)
	}
	if out[0].DeferLoading {
		t.Fatal("expanded tool must have DeferLoading cleared")
	}
	if got := expandDeferredByName(deferred, nil); got != nil {
		t.Fatalf("nil names should expand nothing, got %+v", got)
	}
}

// ---- tokenizer -------------------------------------------------------------

func TestTokenize(t *testing.T) {
	got := tokenize("Create_Issue, the GitHub-issue!")
	want := []string{"create", "issue", "the", "github", "issue"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokenize = %v, want %v", got, want)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
