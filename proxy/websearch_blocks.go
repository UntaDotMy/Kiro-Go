package proxy

import (
	"fmt"
	"strings"
)

// ============================================================================
// Web-search request detection + Anthropic block shaping.
//
// These helpers are pure (no network, no globals) so they are fully unit
// testable. The live MCP call lives in mcp_websearch.go; the streaming/handler
// wiring lives in the Claude handler. Splitting it this way keeps the risky
// hot-path integration thin and the reshaping logic verifiable in isolation.
// ============================================================================

const webSearchToolName = "web_search"

// findClaudeWebSearchTool reports whether the request carries Anthropic's
// hosted web_search tool. Detection is by the hosted-tool `type` prefix
// (e.g. "web_search_20250305") OR the canonical name "web_search" — matching
// how the working reference proxies detect it. Returns the matched tool and
// true when found.
func findClaudeWebSearchTool(tools []ClaudeTool) (ClaudeTool, bool) {
	for _, t := range tools {
		if isWebSearchTool(t) {
			return t, true
		}
	}
	return ClaudeTool{}, false
}

// isWebSearchTool matches a single tool spec against the web_search identity.
func isWebSearchTool(t ClaudeTool) bool {
	if strings.EqualFold(strings.TrimSpace(t.Name), webSearchToolName) {
		return true
	}
	typ := strings.ToLower(strings.TrimSpace(t.Type))
	return strings.HasPrefix(typ, "web_search")
}

// webSearchToolSpec returns the Kiro function-tool spec we register so the
// upstream model can CALL web_search (Kiro has no hosted tool concept, so we
// expose it as an ordinary function the model invokes; we then execute it
// ourselves). Kept consistent with Anthropic's documented input shape so the
// model populates `query`.
func webSearchToolSpec() KiroToolWrapper {
	w := KiroToolWrapper{}
	w.ToolSpecification.Name = webSearchToolName
	w.ToolSpecification.Description = "Search the web for current, up-to-date information. " +
		"Use this when the user asks about recent events, current data, or anything that may have " +
		"changed after your training cutoff. Provide a focused search query."
	w.ToolSpecification.InputSchema = InputSchema{JSON: map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "The search query.",
			},
		},
		"required": []interface{}{"query"},
	}}
	return w
}

// extractWebSearchQuery pulls the query string from a model-emitted web_search
// tool_use input. Falls back to a "query"-like field if the canonical key is
// absent. Returns "" when no usable query is present.
func extractWebSearchQuery(input map[string]interface{}) string {
	if input == nil {
		return ""
	}
	for _, key := range []string{"query", "q", "search_query", "searchQuery"} {
		if v, ok := input[key]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// buildWebSearchResultBlocks renders search results into the native Anthropic
// content-block pair the client expects: a server_tool_use block (echoing the
// query) followed by a web_search_tool_result block carrying the hits. This is
// what makes Claude Code / Anthropic SDKs render real citations.
//
// toolUseID ties the two blocks together; reuse the id from the model's
// tool_use so the conversation stays coherent.
func buildWebSearchResultBlocks(toolUseID, query string, results []WebSearchResult) []map[string]interface{} {
	serverToolUse := map[string]interface{}{
		"type":  "server_tool_use",
		"id":    toolUseID,
		"name":  webSearchToolName,
		"input": map[string]interface{}{"query": query},
	}

	resultItems := make([]map[string]interface{}, 0, len(results))
	for _, r := range results {
		item := map[string]interface{}{
			"type":  "web_search_result",
			"title": r.Title,
			"url":   r.URL,
		}
		if r.PublishedDate != "" {
			item["page_age"] = r.PublishedDate
		}
		resultItems = append(resultItems, item)
	}

	toolResult := map[string]interface{}{
		"type":        "web_search_tool_result",
		"tool_use_id": toolUseID,
		"content":     resultItems,
	}

	return []map[string]interface{}{serverToolUse, toolResult}
}

// formatWebSearchForModel renders results as compact text to feed BACK to the
// model as a tool_result, so the model can ground its answer on them. This is
// the agentic-loop payload (model → web_search → results → model continues).
func formatWebSearchForModel(query string, results []WebSearchResult) string {
	if len(results) == 0 {
		return fmt.Sprintf("Web search for %q returned no results.", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Web search results for %q:\n\n", query)
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, strings.TrimSpace(r.Title), strings.TrimSpace(r.URL))
		if s := strings.TrimSpace(r.Snippet); s != "" {
			fmt.Fprintf(&b, "   %s\n", s)
		}
		if d := strings.TrimSpace(r.PublishedDate); d != "" {
			fmt.Fprintf(&b, "   (published: %s)\n", d)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
