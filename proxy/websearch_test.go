package proxy

import (
	"strings"
	"testing"
)

// ---- detection -------------------------------------------------------------

func TestIsWebSearchTool(t *testing.T) {
	cases := []struct {
		name string
		tool ClaudeTool
		want bool
	}{
		{"hosted type stamp", ClaudeTool{Type: "web_search_20250305", Name: "web_search"}, true},
		{"name only", ClaudeTool{Name: "web_search"}, true},
		{"name case-insensitive", ClaudeTool{Name: "Web_Search"}, true},
		{"type prefix only", ClaudeTool{Type: "web_search_20990101"}, true},
		{"custom tool", ClaudeTool{Name: "read_file"}, false},
		{"other server tool", ClaudeTool{Type: "computer_20250124", Name: "computer"}, false},
		{"empty", ClaudeTool{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWebSearchTool(tc.tool); got != tc.want {
				t.Fatalf("isWebSearchTool(%+v) = %v, want %v", tc.tool, got, tc.want)
			}
		})
	}
}

func TestFindClaudeWebSearchTool(t *testing.T) {
	tools := []ClaudeTool{
		{Name: "read_file"},
		{Type: "web_search_20250305", Name: "web_search"},
		{Name: "write_file"},
	}
	got, ok := findClaudeWebSearchTool(tools)
	if !ok {
		t.Fatal("expected to find web_search tool")
	}
	if got.Name != "web_search" {
		t.Fatalf("got tool %q, want web_search", got.Name)
	}

	if _, ok := findClaudeWebSearchTool([]ClaudeTool{{Name: "read_file"}}); ok {
		t.Fatal("did not expect to find web_search in custom-only tools")
	}
	if _, ok := findClaudeWebSearchTool(nil); ok {
		t.Fatal("did not expect to find web_search in nil tools")
	}
}

// ---- query extraction ------------------------------------------------------

func TestExtractWebSearchQuery(t *testing.T) {
	cases := []struct {
		name  string
		input map[string]interface{}
		want  string
	}{
		{"canonical query", map[string]interface{}{"query": "golang generics"}, "golang generics"},
		{"trims whitespace", map[string]interface{}{"query": "  spaced  "}, "spaced"},
		{"alt key q", map[string]interface{}{"q": "fallback"}, "fallback"},
		{"alt key search_query", map[string]interface{}{"search_query": "sq"}, "sq"},
		{"empty string ignored", map[string]interface{}{"query": "   "}, ""},
		{"nil input", nil, ""},
		{"no usable key", map[string]interface{}{"unrelated": "x"}, ""},
		{"non-string value", map[string]interface{}{"query": 42}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractWebSearchQuery(tc.input); got != tc.want {
				t.Fatalf("extractWebSearchQuery(%v) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ---- tool spec -------------------------------------------------------------

func TestWebSearchToolSpec(t *testing.T) {
	spec := webSearchToolSpec()
	if spec.ToolSpecification.Name != "web_search" {
		t.Fatalf("spec name = %q, want web_search", spec.ToolSpecification.Name)
	}
	if spec.ToolSpecification.Description == "" {
		t.Fatal("spec description must be non-empty (Kiro 400s on empty descriptions)")
	}
	schema, ok := spec.ToolSpecification.InputSchema.JSON.(map[string]interface{})
	if !ok {
		t.Fatal("schema must be a JSON object")
	}
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v, want object", schema["type"])
	}
	props, ok := schema["properties"].(map[string]interface{})
	if !ok || props["query"] == nil {
		t.Fatal("schema must declare a 'query' property")
	}
}

// ---- block shaping ---------------------------------------------------------

func TestBuildWebSearchResultBlocks(t *testing.T) {
	results := []WebSearchResult{
		{Title: "First", URL: "https://a.example", Snippet: "snip a", PublishedDate: "2026-01-01"},
		{Title: "Second", URL: "https://b.example"},
	}
	blocks := buildWebSearchResultBlocks("srvtoolu_1", "my query", results)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (server_tool_use + result), got %d", len(blocks))
	}

	stu := blocks[0]
	if stu["type"] != "server_tool_use" {
		t.Fatalf("block 0 type = %v, want server_tool_use", stu["type"])
	}
	if stu["id"] != "srvtoolu_1" || stu["name"] != "web_search" {
		t.Fatalf("server_tool_use id/name mismatch: %+v", stu)
	}
	in, ok := stu["input"].(map[string]interface{})
	if !ok || in["query"] != "my query" {
		t.Fatalf("server_tool_use input mismatch: %+v", stu["input"])
	}

	res := blocks[1]
	if res["type"] != "web_search_tool_result" {
		t.Fatalf("block 1 type = %v, want web_search_tool_result", res["type"])
	}
	if res["tool_use_id"] != "srvtoolu_1" {
		t.Fatalf("result tool_use_id = %v, want srvtoolu_1", res["tool_use_id"])
	}
	items, ok := res["content"].([]map[string]interface{})
	if !ok || len(items) != 2 {
		t.Fatalf("expected 2 result items, got %v", res["content"])
	}
	if items[0]["type"] != "web_search_result" || items[0]["title"] != "First" {
		t.Fatalf("item 0 mismatch: %+v", items[0])
	}
	if items[0]["page_age"] != "2026-01-01" {
		t.Fatalf("item 0 should carry page_age, got %+v", items[0])
	}
	// Second result has no published date — page_age must be absent, not empty.
	if _, present := items[1]["page_age"]; present {
		t.Fatalf("item 1 should omit page_age when no date, got %+v", items[1])
	}
}

func TestBuildWebSearchResultBlocksEmpty(t *testing.T) {
	blocks := buildWebSearchResultBlocks("id", "q", nil)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks even with no results, got %d", len(blocks))
	}
	items, ok := blocks[1]["content"].([]map[string]interface{})
	if !ok || len(items) != 0 {
		t.Fatalf("expected empty content slice, got %v", blocks[1]["content"])
	}
}

// ---- model-facing formatting ----------------------------------------------

func TestFormatWebSearchForModel(t *testing.T) {
	none := formatWebSearchForModel("nothing", nil)
	if !strings.Contains(none, "no results") {
		t.Fatalf("empty results should say 'no results', got %q", none)
	}

	out := formatWebSearchForModel("q", []WebSearchResult{
		{Title: "T1", URL: "https://u1", Snippet: "S1", PublishedDate: "2026-02-02"},
	})
	for _, want := range []string{"T1", "https://u1", "S1", "2026-02-02"} {
		if !strings.Contains(out, want) {
			t.Fatalf("formatted output missing %q:\n%s", want, out)
		}
	}
}

// ---- MCP response parsing (no network) -------------------------------------

func TestParseMCPWebSearchResponse_WrappedResults(t *testing.T) {
	// result.content[0].text is a JSON STRING — note the escaping.
	raw := []byte(`{
		"jsonrpc":"2.0","id":"1",
		"result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"A\",\"url\":\"https://a\",\"snippet\":\"sa\"},{\"title\":\"B\",\"url\":\"https://b\"}]}"}],"isError":false}
	}`)
	results, err := parseMCPWebSearchResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Title != "A" || results[0].URL != "https://a" {
		t.Fatalf("result 0 mismatch: %+v", results[0])
	}
}

func TestParseMCPWebSearchResponse_BareArray(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"[{\"title\":\"X\",\"url\":\"https://x\"}]"}]}}`)
	results, err := parseMCPWebSearchResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].Title != "X" {
		t.Fatalf("expected 1 result X, got %+v", results)
	}
}

func TestParseMCPWebSearchResponse_Truncation(t *testing.T) {
	// More than maxWebSearchResults should be capped.
	var sb strings.Builder
	sb.WriteString(`{"results":[`)
	for i := 0; i < maxWebSearchResults+5; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"title":"t","url":"https://u"}`)
	}
	sb.WriteString(`]}`)
	inner := strings.ReplaceAll(sb.String(), `"`, `\"`)
	raw := []byte(`{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"` + inner + `"}]}}`)

	results, err := parseMCPWebSearchResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != maxWebSearchResults {
		t.Fatalf("expected results capped at %d, got %d", maxWebSearchResults, len(results))
	}
}

func TestParseMCPWebSearchResponse_Errors(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"jsonrpc error", `{"jsonrpc":"2.0","id":"1","error":{"code":-32601,"message":"Method not found"}}`},
		{"nil result", `{"jsonrpc":"2.0","id":"1"}`},
		{"empty content", `{"jsonrpc":"2.0","id":"1","result":{"content":[]}}`},
		{"tool isError", `{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"boom"}],"isError":true}}`},
		{"invalid json", `not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseMCPWebSearchResponse([]byte(tc.raw)); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestParseMCPWebSearchResponse_EmptyResultsIsNotError(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"{\"results\":[]}"}]}}`)
	results, err := parseMCPWebSearchResponse(raw)
	if err != nil {
		t.Fatalf("a search that found nothing should not error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(results))
	}
}

func TestMCPQHostForRegion(t *testing.T) {
	cases := map[string]string{
		"us-east-1":      "https://q.us-east-1.amazonaws.com",
		"eu-west-1":      "https://q.eu-west-1.amazonaws.com",
		"":               "https://q.us-east-1.amazonaws.com",
		"ap-northeast-1": "https://q.ap-northeast-1.amazonaws.com",
	}
	for region, want := range cases {
		if got := mcpQHostForRegion(region); got != want {
			t.Fatalf("mcpQHostForRegion(%q) = %q, want %q", region, got, want)
		}
	}
}
