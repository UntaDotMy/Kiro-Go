package proxy

import "testing"

func TestThinkingSourceReasoningFirst(t *testing.T) {
	var source thinkingStreamSource

	if !allowReasoningSource(&source) {
		t.Fatalf("expected reasoning source to be accepted first")
	}
	if source != thinkingSourceReasoningEvent {
		t.Fatalf("expected source to be reasoning, got %v", source)
	}
	if allowTagSource(&source) {
		t.Fatalf("expected tag source to be rejected after reasoning source selected")
	}
}

func TestThinkingSourceTagFirst(t *testing.T) {
	var source thinkingStreamSource

	if !allowTagSource(&source) {
		t.Fatalf("expected tag source to be accepted first")
	}
	if source != thinkingSourceTagBlock {
		t.Fatalf("expected source to be tag, got %v", source)
	}
	if allowReasoningSource(&source) {
		t.Fatalf("expected reasoning source to be rejected after tag source selected")
	}
}

func TestThinkingSourceSameSourceRemainsAllowed(t *testing.T) {
	var source thinkingStreamSource

	if !allowTagSource(&source) {
		t.Fatalf("expected initial tag source selection to succeed")
	}
	if !allowTagSource(&source) {
		t.Fatalf("expected repeated tag source selection to stay allowed")
	}

	source = thinkingSourceUnknown
	if !allowReasoningSource(&source) {
		t.Fatalf("expected initial reasoning source selection to succeed")
	}
	if !allowReasoningSource(&source) {
		t.Fatalf("expected repeated reasoning source selection to stay allowed")
	}
}

func TestValidateOpenAIRequestShapeRejectsAssistantPrefill(t *testing.T) {
	req := &OpenAIRequest{
		Model: "gpt-4o",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "prefill"},
		},
	}

	if msg := validateOpenAIRequestShape(req); msg == "" {
		t.Fatalf("expected assistant-prefill final message to be rejected")
	}
}

func TestValidateOpenAIRequestShapeAllowsToolResultFinalTurn(t *testing.T) {
	req := &OpenAIRequest{
		Model: "gpt-4o",
		Messages: []OpenAIMessage{
			{Role: "user", Content: "find weather"},
			{
				Role: "assistant",
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
		},
	}

	if msg := validateOpenAIRequestShape(req); msg != "" {
		t.Fatalf("expected tool-result final turn to be valid, got %q", msg)
	}
}

func TestValidateClaudeRequestShapeRejectsAssistantPrefill(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.7",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "prefill"},
		},
	}

	if msg := validateClaudeRequestShape(req); msg == "" {
		t.Fatalf("expected assistant-prefill final message to be rejected")
	}
}

// TestValidateRequestShapeRejectsEmptyModel locks in the empty-model fix: a
// request omitting `model` must be rejected with a clear error, on both the
// Claude and OpenAI shapes. An empty model would otherwise slip past the
// per-key model-whitelist guard (which only fires when model != "").
func TestValidateRequestShapeRejectsEmptyModel(t *testing.T) {
	claude := &ClaudeRequest{
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
	}
	if msg := validateClaudeRequestShape(claude); msg == "" {
		t.Fatal("expected empty-model Claude request to be rejected")
	}

	openai := &OpenAIRequest{
		Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
	}
	if msg := validateOpenAIRequestShape(openai); msg == "" {
		t.Fatal("expected empty-model OpenAI request to be rejected")
	}

	// Whitespace-only model is also empty after trim.
	claudeWS := &ClaudeRequest{
		Model:    "   ",
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
	}
	if msg := validateClaudeRequestShape(claudeWS); msg == "" {
		t.Fatal("expected whitespace-only model to be rejected")
	}
}

func TestResolveClaudeThinkingModeHonorsRequestThinking(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		thinking     *ClaudeThinkingConfig
		wantModel    string
		wantThinking bool
	}{
		{
			name:         "adaptive request enables thinking",
			model:        "claude-sonnet-4.6",
			thinking:     &ClaudeThinkingConfig{Type: "adaptive"},
			wantModel:    "claude-sonnet-4.6",
			wantThinking: true,
		},
		{
			name:         "enabled request enables thinking",
			model:        "claude-opus-4.5",
			thinking:     &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048},
			wantModel:    "claude-opus-4.5",
			wantThinking: true,
		},
		{
			name:         "disabled request keeps thinking off",
			model:        "claude-opus-4.7",
			thinking:     &ClaudeThinkingConfig{Type: "disabled"},
			wantModel:    "claude-opus-4.7",
			wantThinking: false,
		},
		{
			name:         "suffix remains supported when thinking is disabled",
			model:        "claude-sonnet-4.5-thinking",
			thinking:     &ClaudeThinkingConfig{Type: "disabled"},
			wantModel:    "claude-sonnet-4.5",
			wantThinking: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotModel, gotThinking := resolveClaudeThinkingMode(tc.model, tc.thinking, "-thinking")
			if gotModel != tc.wantModel {
				t.Fatalf("expected model %q, got %q", tc.wantModel, gotModel)
			}
			if gotThinking != tc.wantThinking {
				t.Fatalf("expected thinking=%v, got %v", tc.wantThinking, gotThinking)
			}
		})
	}
}

func TestCloneClaudeRequestForThinkingNoLongerInjectsEnvelope(t *testing.T) {
	req := &ClaudeRequest{
		Model:  "claude-sonnet-4.6",
		System: "Follow the user instructions.",
	}

	cloned := cloneClaudeRequestForThinking(req, true)
	if cloned == req {
		t.Fatalf("expected a clone, got the same pointer")
	}
	// The clone must NOT prepend any thinking-mode envelope to the system
	// prompt. Earlier revisions added "<thinking_mode>..." or natural-prose
	// token-budget directives; both were fingerprinted by the upstream model
	// as fake harness signals.
	got := extractSystemPrompt(cloned.System)
	if got != "Follow the user instructions." {
		t.Fatalf("expected system prompt to be unchanged after clone, got %q", got)
	}
	if original, ok := req.System.(string); !ok || original != "Follow the user instructions." {
		t.Fatalf("expected original request system prompt to stay unchanged, got %#v", req.System)
	}
}

func TestCloneClaudeRequestForThinkingPreservesStructuredSystemBlocks(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.6",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": "cached system",
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
					"ttl":  "5m",
				},
			},
		},
	}

	cloned := cloneClaudeRequestForThinking(req, true)
	blocks, ok := cloned.System.([]interface{})
	if !ok {
		t.Fatalf("expected structured system blocks, got %T", cloned.System)
	}
	// No injection: the cloned slice should pass through the original blocks
	// unchanged (cache_control intact).
	if len(blocks) != 1 {
		t.Fatalf("expected exactly 1 system block (no injection), got %d", len(blocks))
	}
	first, ok := blocks[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected original system block to remain a map, got %T", blocks[0])
	}
	if first["text"] != "cached system" {
		t.Fatalf("expected original block text to be preserved, got %#v", first["text"])
	}
	cacheControl, ok := first["cache_control"].(map[string]interface{})
	if !ok || cacheControl["type"] != "ephemeral" {
		t.Fatalf("expected original cache_control to be preserved, got %#v", first["cache_control"])
	}
}

func TestThinkingFlagDoesNotInflateClaudeTokenEstimate(t *testing.T) {
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.6",
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
	}

	baseTokens := estimateClaudeRequestInputTokens(req)
	thinkingTokens := estimateClaudeRequestInputTokens(cloneClaudeRequestForThinking(req, true))

	// thinking flag is now a no-op for the input shape; tokens must match.
	if thinkingTokens != baseTokens {
		t.Fatalf("expected thinking tokens (%d) to equal base tokens (%d) after no-op clone", thinkingTokens, baseTokens)
	}
}

func TestValidateClaudeThinkingConfig(t *testing.T) {
	tests := []struct {
		name        string
		thinking    *ClaudeThinkingConfig
		maxTokens   int
		expectError bool
	}{
		{
			name:        "adaptive is valid",
			thinking:    &ClaudeThinkingConfig{Type: "adaptive"},
			maxTokens:   4096,
			expectError: false,
		},
		{
			name:        "enabled requires budget",
			thinking:    &ClaudeThinkingConfig{Type: "enabled"},
			maxTokens:   4096,
			expectError: true,
		},
		{
			name:        "enabled requires at least 1024 budget tokens",
			thinking:    &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 512},
			maxTokens:   4096,
			expectError: true,
		},
		{
			name:        "enabled rejects max tokens zero",
			thinking:    &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048},
			maxTokens:   0,
			expectError: true,
		},
		{
			name:        "enabled budget must stay below max tokens",
			thinking:    &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 4096},
			maxTokens:   4096,
			expectError: true,
		},
		{
			name:        "disabled rejects display",
			thinking:    &ClaudeThinkingConfig{Type: "disabled", Display: "summarized"},
			maxTokens:   4096,
			expectError: true,
		},
		{
			name:        "missing type is rejected",
			thinking:    &ClaudeThinkingConfig{},
			maxTokens:   4096,
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errMsg := validateClaudeThinkingConfig(tc.thinking, tc.maxTokens)
			if tc.expectError && errMsg == "" {
				t.Fatalf("expected validation error")
			}
			if !tc.expectError && errMsg != "" {
				t.Fatalf("expected thinking config to be valid, got %q", errMsg)
			}
		})
	}
}

func TestResolveClaudeThinkingResponseOptions(t *testing.T) {
	tests := []struct {
		name       string
		thinking   *ClaudeThinkingConfig
		defaultFmt string
		wantFmt    string
		wantOmit   bool
	}{
		{
			name:       "default config is preserved when display unset",
			thinking:   &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048},
			defaultFmt: "think",
			wantFmt:    "think",
			wantOmit:   false,
		},
		{
			name:       "summarized forces official thinking blocks",
			thinking:   &ClaudeThinkingConfig{Type: "adaptive", Display: "summarized"},
			defaultFmt: "reasoning_content",
			wantFmt:    "thinking",
			wantOmit:   false,
		},
		{
			name:       "omitted forces official thinking blocks and hides content",
			thinking:   &ClaudeThinkingConfig{Type: "adaptive", Display: "omitted"},
			defaultFmt: "think",
			wantFmt:    "thinking",
			wantOmit:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := resolveClaudeThinkingResponseOptions(tc.thinking, tc.defaultFmt)
			if opts.Format != tc.wantFmt {
				t.Fatalf("expected format %q, got %q", tc.wantFmt, opts.Format)
			}
			if opts.OmitDisplay != tc.wantOmit {
				t.Fatalf("expected omitDisplay=%v, got %v", tc.wantOmit, opts.OmitDisplay)
			}
		})
	}
}

func TestMergeUniqueModelsPreservesUnionAcrossAccounts(t *testing.T) {
	base := []ModelInfo{
		{ModelId: "claude-sonnet-4.5", InputTypes: []string{"TEXT"}},
	}
	incoming := []ModelInfo{
		{ModelId: "claude-sonnet-4.5", InputTypes: []string{"image"}},
		{ModelId: "claude-opus-4-7", InputTypes: []string{"text"}},
	}

	merged := mergeUniqueModels(base, incoming)
	if len(merged) != 2 {
		t.Fatalf("expected 2 unique models, got %d", len(merged))
	}
	if !modelSupportsImage(merged[0].InputTypes) {
		t.Fatalf("expected merged input types to preserve image capability, got %#v", merged[0].InputTypes)
	}
	if merged[1].ModelId != "claude-opus-4-7" {
		t.Fatalf("expected second model to be claude-opus-4-7, got %q", merged[1].ModelId)
	}
}

func TestBuildAnthropicModelsResponseDedupedNoThinkingVariants(t *testing.T) {
	models := buildAnthropicModelsResponse([]ModelInfo{{
		ModelId:    "claude-sonnet-4.5",
		InputTypes: []string{"text", "image"},
	}}, "-thinking")

	// We emit only the dashed Anthropic id and the dotted Kiro alias. No
	// dated suffix, no -thinking variant — the suffix is response-side only,
	// so listing the suffixed forms doubled the picker entries without changing
	// behavior. See buildAnthropicModelsResponse for the full rationale.
	if len(models) != 2 {
		t.Fatalf("expected dashed + dotted alias only, got %d", len(models))
	}
	if models[0]["id"] != "claude-sonnet-4-5" {
		t.Fatalf("expected primary id to be dashed (no date), got %#v", models[0]["id"])
	}
	if models[1]["id"] != "claude-sonnet-4.5" {
		t.Fatalf("expected dotted Kiro id alias, got %#v", models[1]["id"])
	}
	if supportsImage, ok := models[0]["supports_image"].(bool); !ok || !supportsImage {
		t.Fatalf("expected image capability to be preserved, got %#v", models[0]["supports_image"])
	}
}

// TestBuildAnthropicModelsResponseCollapsesAlreadyDashedID confirms that a
// Kiro id already in dashed form ("claude-opus-4-7") yields a single entry
// rather than duplicating itself as both canonical and alias.
func TestBuildAnthropicModelsResponseCollapsesAlreadyDashedID(t *testing.T) {
	models := buildAnthropicModelsResponse([]ModelInfo{{
		ModelId:    "claude-opus-4-7",
		InputTypes: []string{"text"},
	}}, "-thinking")
	if len(models) != 1 {
		t.Fatalf("expected single entry for already-dashed id, got %d", len(models))
	}
	if models[0]["id"] != "claude-opus-4-7" {
		t.Fatalf("expected claude-opus-4-7, got %#v", models[0]["id"])
	}
}

func TestFallbackAnthropicModelsIncludesOpus47PickerAlias(t *testing.T) {
	models := fallbackAnthropicModels("-thinking")
	seen := map[string]bool{}
	for _, model := range models {
		if id, ok := model["id"].(string); ok {
			seen[id] = true
		}
	}
	if !seen["claude-opus-4-7"] || !seen["claude-opus-4.7"] {
		t.Fatalf("expected fallback to include Opus 4.7 picker and Kiro aliases, got %#v", seen)
	}
}

// TestBuildAnthropicModelsResponseHandlesAutoAndKnownAliases confirms that
// when Kiro itself returns one of the alias names (e.g. "auto") in its
// model list, buildAnthropicModelsResponse passes it through as a single
// entry. The alias-dedup pass in handleListModels then sees it in the
// `seen` set and skips re-appending — preventing the double "auto" entry
// the user reported.
func TestBuildAnthropicModelsResponseHandlesAutoAndKnownAliases(t *testing.T) {
	models := buildAnthropicModelsResponse([]ModelInfo{
		{ModelId: "auto", InputTypes: []string{"text", "image"}},
		{ModelId: "claude-opus-4.7", InputTypes: []string{"text", "image"}},
	}, "-thinking")

	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m["id"].(string))
	}
	want := []string{"auto", "claude-opus-4-7", "claude-opus-4.7"}
	if len(ids) != len(want) {
		t.Fatalf("expected %d entries, got %d (%v)", len(want), len(ids), ids)
	}
	for i, w := range want {
		if ids[i] != w {
			t.Fatalf("entry %d: want %q, got %q (full: %v)", i, w, ids[i], ids)
		}
	}
}

// TestBuildAnthropicModelsResponseContextWindowFromKiro pins the compaction
// fix: the advertised context_window is sourced from Kiro's tokenLimits when
// present, and otherwise falls back to a version-parse of the model id (Claude
// >= 4.6 and any major >= 5 are 1M; earlier are 200K). Advertising the TRUE
// window keeps a context-aware client's (Claude Code) usage numerator and window
// denominator consistent so its ~95%-of-window auto-compaction threshold fires
// at the right point rather than the gauge sailing past 100% with no compaction.
func TestBuildAnthropicModelsResponseContextWindowFromKiro(t *testing.T) {
	models := buildAnthropicModelsResponse([]ModelInfo{
		// Kiro reports a real 1M window for this model — honor it.
		{ModelId: "claude-sonnet-4-6", InputTypes: []string{"text"}, TokenLimits: tokenLimits(1_000_000, 8192)},
		// Kiro reports a 200K window — honor it (authoritative, even though the
		// id parses as >=4.6: the upstream's explicit figure always wins).
		{ModelId: "claude-opus-4-6", InputTypes: []string{"text"}, TokenLimits: tokenLimits(200_000, 32000)},
		// No tokenLimits at all — fall back to the version parse: opus-4-8 is
		// >=4.6 so it resolves to the 1M window.
		{ModelId: "claude-opus-4-8", InputTypes: []string{"text"}},
		// No tokenLimits, id parses < 4.6 — fall back to the 200K default.
		{ModelId: "claude-opus-4-5", InputTypes: []string{"text"}},
	}, "-thinking")

	got := map[string]int{}
	for _, m := range models {
		id, _ := m["id"].(string)
		cw, _ := m["context_window"].(int)
		cl, _ := m["context_length"].(int)
		if cw != cl {
			t.Fatalf("%s: context_window (%d) and context_length (%d) must agree", id, cw, cl)
		}
		got[id] = cw
	}

	if got["claude-sonnet-4-6"] != 1_000_000 {
		t.Errorf("sonnet-4-6 should report Kiro's 1M window, got %d", got["claude-sonnet-4-6"])
	}
	if got["claude-opus-4-6"] != 200_000 {
		t.Errorf("opus-4-6 should report Kiro's authoritative 200K window, got %d", got["claude-opus-4-6"])
	}
	if got["claude-opus-4-8"] != 1_000_000 {
		t.Errorf("opus-4-8 WITHOUT tokenLimits must version-parse to the 1M window, got %d", got["claude-opus-4-8"])
	}
	if got["claude-opus-4-5"] != defaultContextWindow {
		t.Errorf("opus-4-5 WITHOUT tokenLimits must fall back to the %d default, got %d", defaultContextWindow, got["claude-opus-4-5"])
	}
}

// TestApplyAdaptiveThinkingDefaultInjectsAdaptive verifies that requests
// against Claude 4-family models with no thinking config gain
// thinking.type="adaptive" so Claude Code displays the thinking indicator.
func TestApplyAdaptiveThinkingDefaultInjectsAdaptive(t *testing.T) {
	req := &ClaudeRequest{Model: "claude-opus-4.7"}
	applyAdaptiveThinkingDefault(req)
	if req.Thinking == nil || req.Thinking.Type != "adaptive" {
		t.Fatalf("expected adaptive thinking to be injected, got %#v", req.Thinking)
	}
	if req.Thinking.BudgetTokens != 0 {
		t.Fatalf("expected no budget tokens for adaptive default, got %d", req.Thinking.BudgetTokens)
	}
}

// TestApplyAdaptiveThinkingDefaultRespectsExisting verifies that an
// explicit thinking config from the client is left intact (so users who
// disabled thinking keep that choice). The function only fills in defaults.
func TestApplyAdaptiveThinkingDefaultRespectsExisting(t *testing.T) {
	req := &ClaudeRequest{
		Model:    "claude-opus-4.7",
		Thinking: &ClaudeThinkingConfig{Type: "disabled"},
	}
	applyAdaptiveThinkingDefault(req)
	if req.Thinking.Type != "disabled" {
		t.Fatalf("expected explicit disabled to be preserved, got %q", req.Thinking.Type)
	}
}

// TestApplyAdaptiveThinkingDefaultSkipsNonClaude verifies that non-Claude
// models (e.g. legacy GPT routes during passthrough) don't get an adaptive
// thinking config injected.
func TestApplyAdaptiveThinkingDefaultSkipsNonClaude(t *testing.T) {
	req := &ClaudeRequest{Model: "gpt-4o"}
	applyAdaptiveThinkingDefault(req)
	if req.Thinking != nil {
		t.Fatalf("expected no thinking config for non-Claude model, got %#v", req.Thinking)
	}
}
