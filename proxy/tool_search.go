package proxy

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// ============================================================================
// Anthropic Tool Search emulation — pure detection / search / block shaping.
//
// Anthropic's Tool Search (https://platform.claude.com/docs/en/agents-and-tools/
// tool-use/tool-search-tool) lets a client mark most of its tools
// `defer_loading: true` and add a server tool of type
// `tool_search_tool_regex_20251119` or `tool_search_tool_bm25_20251119`. The
// API then keeps deferred tool schemas OUT of the model's context until the
// model calls the search tool; on a hit the API emits a `tool_reference` and
// expands the matched tool's full schema so the model can call it.
//
// CodeWhisperer / Kiro has no concept of any of this, so — exactly like the
// web_search emulation in websearch_*.go — we EMULATE it proxy-side:
//   - withhold deferred tool schemas from the upstream payload,
//   - expose ONE synthetic function tool the upstream model can call,
//   - run the regex/BM25 match ourselves over the deferred descriptions,
//   - feed matched tool schemas back into the next round so the model can use
//     them, and splice native server_tool_use + tool_search_tool_result blocks
//     into the client response so Claude Code renders real tool-search results.
//
// Everything in THIS file is pure (no network, no globals) so it is fully unit
// testable. The agentic loop / handler wiring lives in tool_search_loop.go.
// ============================================================================

const (
	// toolSearchTypeRegex / toolSearchTypeBM25 are the dated server-tool type
	// stamps Anthropic uses. We match by prefix so undated aliases
	// ("tool_search_tool_regex") and future date stamps both resolve.
	toolSearchTypePrefixRegex = "tool_search_tool_regex"
	toolSearchTypePrefixBM25  = "tool_search_tool_bm25"

	// toolSearchFnName is the synthetic function tool we expose to the upstream
	// CodeWhisperer model so it can request a search. The model never sees the
	// dated server-tool type; it just calls this function with a query.
	toolSearchFnName = "tool_search"

	// maxToolSearchResults caps how many tool references a single search returns,
	// keeping the expanded context bounded. Anthropic surfaces a small set; we
	// match that intent.
	maxToolSearchResults = 5
)

// toolSearchMode is which search algorithm the client asked for.
type toolSearchMode int

const (
	toolSearchModeNone toolSearchMode = iota
	toolSearchModeRegex
	toolSearchModeBM25
)

// searchToolName returns the canonical Anthropic tool name for the active mode,
// used as the `name` on the emitted server_tool_use block.
func (m toolSearchMode) searchToolName() string {
	switch m {
	case toolSearchModeRegex:
		return "tool_search_tool_regex"
	case toolSearchModeBM25:
		return "tool_search_tool_bm25"
	default:
		return "tool_search"
	}
}

// detectToolSearchMode reports which tool-search server tool (if any) the
// request carries. Detection is by the hosted-tool `type` prefix, matching how
// isWebSearchTool detects web_search. Returns toolSearchModeNone when absent.
func detectToolSearchMode(tools []ClaudeTool) toolSearchMode {
	for _, t := range tools {
		typ := strings.ToLower(strings.TrimSpace(t.Type))
		switch {
		case strings.HasPrefix(typ, toolSearchTypePrefixRegex):
			return toolSearchModeRegex
		case strings.HasPrefix(typ, toolSearchTypePrefixBM25):
			return toolSearchModeBM25
		}
	}
	return toolSearchModeNone
}

// isToolSearchTool reports whether a single tool spec is a tool-search server
// tool (either variant). Used to drop it from the upstream payload.
func isToolSearchTool(t ClaudeTool) bool {
	typ := strings.ToLower(strings.TrimSpace(t.Type))
	return strings.HasPrefix(typ, toolSearchTypePrefixRegex) ||
		strings.HasPrefix(typ, toolSearchTypePrefixBM25)
}

// partitionToolSearchTools splits the inbound tool list into the three buckets
// the emulation cares about:
//   - eager: ordinary tools sent to the upstream model as-is,
//   - deferred: defer_loading tools withheld until discovered via search,
//   - mode: which search algorithm the client requested.
//
// The tool_search server tool itself is dropped from both tool buckets (it is
// represented by `mode`). A defer_loading flag on a hosted server tool is
// ignored — only real custom tools are deferrable.
func partitionToolSearchTools(tools []ClaudeTool) (eager, deferred []ClaudeTool, mode toolSearchMode) {
	mode = detectToolSearchMode(tools)
	for _, t := range tools {
		if isToolSearchTool(t) {
			continue // represented by `mode`, never forwarded
		}
		if t.DeferLoading && !isAnthropicServerTool(t) {
			deferred = append(deferred, t)
			continue
		}
		eager = append(eager, t)
	}
	return eager, deferred, mode
}

// toolSearchFnSpec returns the synthetic function tool we register so the
// upstream model can request a tool search. Kiro has no hosted-tool concept, so
// we expose search as an ordinary custom function the model invokes; we then
// run the match ourselves. Returned as a ClaudeTool so it flows through the
// normal ClaudeToKiro → convertClaudeTools conversion (name sanitization, schema
// cleanup) exactly like a client-supplied tool. The description steers the model
// to search BEFORE giving up when it lacks a tool for the task.
func toolSearchFnSpec() ClaudeTool {
	return ClaudeTool{
		Name: toolSearchFnName,
		Description: "Search the additional tool library for tools not currently " +
			"loaded. Many tools are available but their definitions are hidden to save context. " +
			"When you need a capability you don't see among the loaded tools, call this with a short " +
			"query (keywords or a regular expression) describing the capability; matching tools will " +
			"then become available for you to call directly.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type": "string",
					"description": "Keywords or a regular expression matching the capability you need " +
						"(e.g. \"weather\", \"create.*issue\").",
				},
			},
			"required": []interface{}{"query"},
		},
	}
}

// extractToolSearchQuery pulls the query string from a model-emitted tool_search
// tool_use input, tolerating the common key variants. Returns "" when absent.
func extractToolSearchQuery(input map[string]interface{}) string {
	if input == nil {
		return ""
	}
	for _, key := range []string{"query", "q", "pattern", "search", "search_query", "searchQuery"} {
		if v, ok := input[key]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// searchDeferredTools runs the requested algorithm over the deferred tools and
// returns the matched tools, best first, capped at maxToolSearchResults. A
// regex query that fails to compile falls back to case-insensitive substring
// matching so a malformed pattern never returns a hard zero. BM25 scores over
// the tool name + description token sets.
func searchDeferredTools(mode toolSearchMode, query string, deferred []ClaudeTool) []ClaudeTool {
	query = strings.TrimSpace(query)
	if query == "" || len(deferred) == 0 {
		return nil
	}
	switch mode {
	case toolSearchModeBM25:
		return searchBM25(query, deferred)
	default:
		// regex (and any unspecified mode) → regex match
		return searchRegex(query, deferred)
	}
}

// searchRegex matches the query as a regular expression against each tool's
// name and description. Falls back to substring matching if the pattern does
// not compile. Match order is preserved from the input (stable, no scoring).
func searchRegex(query string, deferred []ClaudeTool) []ClaudeTool {
	re, err := regexp.Compile("(?i)" + query)
	var match func(haystack string) bool
	if err != nil {
		needle := strings.ToLower(query)
		match = func(h string) bool { return strings.Contains(strings.ToLower(h), needle) }
	} else {
		match = re.MatchString
	}

	out := make([]ClaudeTool, 0, maxToolSearchResults)
	for _, t := range deferred {
		if match(t.Name) || match(t.Description) {
			out = append(out, t)
			if len(out) >= maxToolSearchResults {
				break
			}
		}
	}
	return out
}

// bm25 tuning constants (Robertson/Spärck Jones defaults).
const (
	bm25K1 = 1.5
	bm25B  = 0.75
)

// searchBM25 ranks deferred tools by Okapi BM25 over their name+description
// token sets, returning the top matches. A tool with zero query-term overlap is
// excluded. This is a compact, dependency-free implementation sufficient for
// the small corpora (tens of tools) we search.
func searchBM25(query string, deferred []ClaudeTool) []ClaudeTool {
	qTerms := tokenize(query)
	if len(qTerms) == 0 {
		return nil
	}

	docs := make([][]string, len(deferred))
	var totalLen int
	for i, t := range deferred {
		docs[i] = tokenize(t.Name + " " + t.Description)
		totalLen += len(docs[i])
	}
	avgLen := float64(totalLen) / float64(len(deferred))
	if avgLen == 0 {
		avgLen = 1
	}

	// Document frequency per query term.
	df := make(map[string]int, len(qTerms))
	for _, qt := range qTerms {
		for _, d := range docs {
			if containsTerm(d, qt) {
				df[qt]++
			}
		}
	}

	type scored struct {
		idx   int
		score float64
	}
	ranked := make([]scored, 0, len(deferred))
	n := float64(len(deferred))
	for i, d := range docs {
		tf := termFreqs(d)
		var score float64
		for _, qt := range qTerms {
			f := float64(tf[qt])
			if f == 0 {
				continue
			}
			idf := math.Log(1 + (n-float64(df[qt])+0.5)/(float64(df[qt])+0.5))
			denom := f + bm25K1*(1-bm25B+bm25B*float64(len(d))/avgLen)
			score += idf * (f * (bm25K1 + 1)) / denom
		}
		if score > 0 {
			ranked = append(ranked, scored{idx: i, score: score})
		}
	}

	// Highest score first; stable on ties by original index for determinism.
	sort.SliceStable(ranked, func(a, b int) bool { return ranked[a].score > ranked[b].score })

	limit := len(ranked)
	if limit > maxToolSearchResults {
		limit = maxToolSearchResults
	}
	out := make([]ClaudeTool, 0, limit)
	for _, r := range ranked[:limit] {
		out = append(out, deferred[r.idx])
	}
	return out
}

// tokenSplitRe splits on any run of non-alphanumeric characters, lowercasing
// is applied by the caller. Compiled once.
var tokenSplitRe = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// tokenize lowercases and splits text into alphanumeric tokens, dropping empties.
func tokenize(s string) []string {
	parts := tokenSplitRe.Split(strings.ToLower(s), -1)
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func termFreqs(tokens []string) map[string]int {
	m := make(map[string]int, len(tokens))
	for _, t := range tokens {
		m[t]++
	}
	return m
}

func containsTerm(tokens []string, term string) bool {
	for _, t := range tokens {
		if t == term {
			return true
		}
	}
	return false
}

// buildToolSearchResultBlocks renders a completed search into the native
// Anthropic content-block pair the client expects: a server_tool_use block
// echoing the query, followed by a tool_search_tool_result block whose nested
// content carries the tool_reference list. This is what makes Claude Code /
// Anthropic SDKs treat the search as a real server-side tool search.
//
// srvToolUseID ties the two blocks together (the result references the
// server_tool_use id). matched is the ordered match set from searchDeferredTools.
func buildToolSearchResultBlocks(mode toolSearchMode, srvToolUseID, query string, matched []ClaudeTool) []map[string]interface{} {
	serverToolUse := map[string]interface{}{
		"type":  "server_tool_use",
		"id":    srvToolUseID,
		"name":  mode.searchToolName(),
		"input": map[string]interface{}{"query": query},
	}

	refs := make([]map[string]interface{}, 0, len(matched))
	for _, t := range matched {
		refs = append(refs, map[string]interface{}{
			"type":      "tool_reference",
			"tool_name": t.Name,
		})
	}

	toolResult := map[string]interface{}{
		"type":        "tool_search_tool_result",
		"tool_use_id": srvToolUseID,
		"content": map[string]interface{}{
			"type":            "tool_search_tool_search_result",
			"tool_references": refs,
		},
	}

	return []map[string]interface{}{serverToolUse, toolResult}
}

// formatToolSearchForModel renders the matched tools as compact text fed BACK to
// the upstream model as the tool_result for its tool_search call, so it knows
// which tools just became available. The full schemas are also re-injected into
// the next round's tool list (see the loop); this text is the human-readable
// announcement that anchors the model on them.
func formatToolSearchForModel(query string, matched []ClaudeTool) string {
	if len(matched) == 0 {
		return "No additional tools matched \"" + query + "\". Answer using the tools already " +
			"available, or tell the user no matching tool exists."
	}
	var b strings.Builder
	b.WriteString("Found ")
	b.WriteString(itoa(len(matched)))
	b.WriteString(" tool(s) matching \"")
	b.WriteString(query)
	b.WriteString("\". They are now available for you to call directly:\n")
	for _, t := range matched {
		b.WriteString("- ")
		b.WriteString(t.Name)
		if d := strings.TrimSpace(t.Description); d != "" {
			b.WriteString(": ")
			b.WriteString(firstLine(d))
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// firstLine returns the first non-empty line of s, trimmed, capped to keep the
// feedback compact.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	const cap = 200
	if len(s) > cap {
		s = s[:cap] + "…"
	}
	return s
}

// itoa is a tiny strconv.Itoa to avoid an import churn in this pure file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// discoveredToolNamesInHistory scans prior conversation turns for tools that
// have already been "discovered" via tool search, so a follow-up request can
// re-expand them up front instead of forcing the model to search again. This
// mirrors Anthropic's documented behaviour: tool_reference blocks are expanded
// throughout the whole conversation history.
//
// It collects names from three signals in any message's content blocks:
//   - tool_reference blocks (tool_name) emitted by a prior search result,
//   - tool_search_tool_result blocks (their nested content.tool_references),
//   - tool_use blocks (name) — the model actually called a deferred tool, so
//     that tool must remain available on subsequent turns.
//
// Pure: tolerates both JSON-decoded ([]interface{} of map[string]interface{})
// and typed ([]ClaudeContentBlock) content shapes.
func discoveredToolNamesInHistory(messages []ClaudeMessage) map[string]bool {
	names := make(map[string]bool)
	for _, msg := range messages {
		blocks, ok := msg.Content.([]interface{})
		if !ok {
			continue
		}
		for _, raw := range blocks {
			b, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			switch b["type"] {
			case "tool_reference":
				if n, ok := b["tool_name"].(string); ok && n != "" {
					names[n] = true
				}
			case "tool_use":
				if n, ok := b["name"].(string); ok && n != "" {
					names[n] = true
				}
			case "tool_search_tool_result":
				collectToolReferenceNames(b["content"], names)
			}
		}
	}
	return names
}

// collectToolReferenceNames pulls tool_name values out of a
// tool_search_tool_result's nested content (an object holding tool_references,
// or directly an array of references).
func collectToolReferenceNames(content interface{}, into map[string]bool) {
	var refs []interface{}
	switch c := content.(type) {
	case map[string]interface{}:
		if arr, ok := c["tool_references"].([]interface{}); ok {
			refs = arr
		}
	case []interface{}:
		refs = c
	}
	for _, raw := range refs {
		if r, ok := raw.(map[string]interface{}); ok {
			if n, ok := r["tool_name"].(string); ok && n != "" {
				into[n] = true
			}
		}
	}
}

// expandDeferredByName returns the subset of deferred tools whose name is in
// `names`, with DeferLoading cleared so they flow to the upstream model as
// ordinary callable tools. Used to pre-expand tools discovered in earlier turns.
func expandDeferredByName(deferred []ClaudeTool, names map[string]bool) []ClaudeTool {
	if len(names) == 0 {
		return nil
	}
	out := make([]ClaudeTool, 0, len(names))
	for _, t := range deferred {
		if names[t.Name] {
			ct := t
			ct.DeferLoading = false
			out = append(out, ct)
		}
	}
	return out
}
