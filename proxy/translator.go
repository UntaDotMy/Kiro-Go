package proxy

import (
	"encoding/base64"
	"encoding/json"
	"kiro-go/config"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// 模型映射（有序，长 key 优先匹配，避免 "claude-sonnet-4" 误匹配 "claude-sonnet-4.5"）
type modelMapping struct {
	key   string
	value string
}

var modelMapOrdered = []modelMapping{
	{"claude-sonnet-4-20250514", "claude-sonnet-4"},
	{"claude-sonnet-4-5", "claude-sonnet-4.5"},
	{"claude-sonnet-4.5", "claude-sonnet-4.5"},
	{"claude-sonnet-4-6", "claude-sonnet-4.6"},
	{"claude-sonnet-4.6", "claude-sonnet-4.6"},
	{"claude-opus-4-7", "claude-opus-4.7"},
	{"claude-opus-4.7", "claude-opus-4.7"},
	{"claude-haiku-4-5", "claude-haiku-4.5"},
	{"claude-haiku-4.5", "claude-haiku-4.5"},
	{"claude-opus-4-5", "claude-opus-4.5"},
	{"claude-opus-4.5", "claude-opus-4.5"},
	{"claude-opus-4-6", "claude-opus-4.6"},
	{"claude-opus-4.6", "claude-opus-4.6"},
	{"claude-sonnet-4", "claude-sonnet-4"},
	{"claude-3-5-sonnet", "claude-sonnet-4.5"},
	{"claude-3-opus", "claude-sonnet-4.5"},
	{"claude-3-sonnet", "claude-sonnet-4"},
	{"claude-3-haiku", "claude-haiku-4.5"},
	{"gpt-4-turbo", "claude-sonnet-4.5"},
	{"gpt-4o", "claude-sonnet-4.5"},
	{"gpt-4", "claude-sonnet-4.5"},
	{"gpt-3.5-turbo", "claude-sonnet-4.5"},
}

// Thinking mode no longer injects any envelope into the system prompt.
//
// Earlier revisions added "<thinking_mode>...</thinking_mode>" tags or
// natural-prose token-budget directives to cue the upstream model. Both
// shapes were correctly identified by Claude as fake harness signals (no
// real Anthropic / Bedrock API exposes a token budget inside the system
// prompt — the budget is a request-level "thinking.budget_tokens" field that
// Kiro's payload doesn't accept).
//
// The "-thinking" suffix and the request "thinking" config still flag the
// proxy's response-side parsing (extractThinkingFromContent / reasoning_content
// stream callback / OpenAI thinking format) so that any reasoning the model
// emits via <think> tags or reasoning events is surfaced correctly. We just
// no longer prepend a synthetic cue.

// minimalFallbackUserContent is the placeholder used when the user-facing
// content of a turn is empty (e.g. only structured tool results). Earlier
// revisions used "." which the upstream model recognized as a synthetic
// filler period and called out. Empty string lets the structured fields
// (UserInputMessageContext.ToolResults, Images) carry the meaning without
// the model seeing a lone period as input.
const minimalFallbackUserContent = ""

// ParseModelAndThinking 解析模型名称，返回实际模型和是否启用 thinking
func ParseModelAndThinking(model string, thinkingSuffix string) (string, bool) {
	lower := strings.ToLower(model)
	thinking := false

	// 使用配置的后缀检查
	suffixLower := strings.ToLower(thinkingSuffix)
	if strings.HasSuffix(lower, suffixLower) {
		thinking = true
		model = model[:len(model)-len(thinkingSuffix)]
		lower = strings.ToLower(model)
	}

	// 映射模型（有序匹配，长 key 优先）
	for _, m := range modelMapOrdered {
		if strings.Contains(lower, m.key) {
			return m.value, thinking
		}
	}

	// 如果已经是有效的 Kiro 模型，直接返回
	if strings.HasPrefix(lower, "claude-") {
		return model, thinking
	}

	return model, thinking
}

func resolveClaudeThinkingMode(model string, thinkingCfg *ClaudeThinkingConfig, thinkingSuffix string) (string, bool) {
	actualModel, suffixThinking := ParseModelAndThinking(model, thinkingSuffix)
	return actualModel, suffixThinking || isClaudeThinkingRequested(thinkingCfg)
}

func isClaudeThinkingRequested(thinkingCfg *ClaudeThinkingConfig) bool {
	if thinkingCfg == nil {
		return false
	}
	kind := strings.ToLower(strings.TrimSpace(thinkingCfg.Type))
	return kind == "enabled" || kind == "adaptive"
}

func MapModel(model string) string {
	mapped, _ := ParseModelAndThinking(model, "-thinking")
	return mapped
}

// ==================== Claude API 类型 ====================

type ClaudeRequest struct {
	Model       string                `json:"model"`
	Messages    []ClaudeMessage       `json:"messages"`
	MaxTokens   int                   `json:"max_tokens"`
	Temperature float64               `json:"temperature,omitempty"`
	TopP        float64               `json:"top_p,omitempty"`
	Stream      bool                  `json:"stream,omitempty"`
	System      interface{}           `json:"system,omitempty"` // string or []SystemBlock
	Thinking    *ClaudeThinkingConfig `json:"thinking,omitempty"`
	Tools       []ClaudeTool          `json:"tools,omitempty"`
	ToolChoice  interface{}           `json:"tool_choice,omitempty"`
}

type ClaudeThinkingConfig struct {
	Type         string `json:"type,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"`
}

type ClaudeMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string or []ContentBlock
}

type ClaudeContentBlock struct {
	Type      string       `json:"type"`
	Text      string       `json:"text,omitempty"`
	Thinking  string       `json:"thinking,omitempty"`
	Signature string       `json:"signature,omitempty"`
	ID        string       `json:"id,omitempty"`
	Name      string       `json:"name,omitempty"`
	Input     interface{}  `json:"input,omitempty"`
	ToolUseID string       `json:"tool_use_id,omitempty"`
	Content   interface{}  `json:"content,omitempty"` // for tool_result
	Source    *ImageSource `json:"source,omitempty"`
}

type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type ClaudeTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"input_schema"`
}

type ClaudeResponse struct {
	ID           string               `json:"id"`
	Type         string               `json:"type"`
	Role         string               `json:"role"`
	Content      []ClaudeContentBlock `json:"content"`
	Model        string               `json:"model"`
	StopReason   string               `json:"stop_reason"`
	StopSequence *string              `json:"stop_sequence"`
	Usage        ClaudeUsage          `json:"usage"`
}

type ClaudeCacheCreationUsage struct {
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens,omitempty"`
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens,omitempty"`
}

type ClaudeUsage struct {
	InputTokens              int                       `json:"input_tokens"`
	OutputTokens             int                       `json:"output_tokens"`
	CacheCreationInputTokens int                       `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int                       `json:"cache_read_input_tokens,omitempty"`
	CacheCreation            *ClaudeCacheCreationUsage `json:"cache_creation,omitempty"`
}

// ==================== Claude -> Kiro 转换 ====================

const maxToolDescLen = 10237

func ClaudeToKiro(req *ClaudeRequest, thinking bool) *KiroPayload {
	modelID := MapModel(req.Model)
	origin := "AI_EDITOR"

	// 提取系统提示
	systemPrompt := buildClaudeSystemPrompt(req.System, thinking)

	// 构建历史消息
	history := make([]KiroHistoryMessage, 0)
	var currentContent string
	var currentImages []KiroImage
	var currentToolResults []KiroToolResult

	for i, msg := range req.Messages {
		isLast := i == len(req.Messages)-1

		if msg.Role == "user" {
			content, images, toolResults := extractClaudeUserContent(msg.Content)
			content = normalizeUserContent(content, len(images) > 0)

			if isLast {
				currentContent = content
				currentImages = images
				currentToolResults = toolResults
			} else {
				userMsg := KiroUserInputMessage{
					Content: content,
					ModelID: modelID,
					Origin:  origin,
				}
				if len(images) > 0 {
					userMsg.Images = images
				}
				if len(toolResults) > 0 {
					userMsg.UserInputMessageContext = &UserInputMessageContext{
						ToolResults: toolResults,
					}
				}
				history = append(history, KiroHistoryMessage{
					UserInputMessage: &userMsg,
				})
			}
		} else if msg.Role == "assistant" {
			content, toolUses := extractClaudeAssistantContent(msg.Content)
			history = append(history, KiroHistoryMessage{
				AssistantResponseMessage: &KiroAssistantResponseMessage{
					Content:  content,
					ToolUses: toolUses,
				},
			})
		}
	}

	history = trimLeadingAssistantHistory(history)

	// 构建最终内容
	finalContent := ""
	if systemPrompt != "" {
		// Always prepend the cleaned system prompt as plain text. Earlier
		// revisions wrapped it with "--- SYSTEM PROMPT ---" / "--- END SYSTEM
		// PROMPT ---" markers; the upstream model fingerprinted those as
		// synthetic envelope and called them out to the user, so we drop the
		// markers entirely and rely on the system prompt sitting at the top of
		// the user content as a natural preamble.
		finalContent = systemPrompt + "\n\n"
	}
	if currentContent != "" {
		finalContent += currentContent
	} else if len(currentImages) > 0 {
		finalContent += normalizeUserContent("", true)
	} else if len(currentToolResults) > 0 {
		// Structured tool results are attached via UserInputMessageContext.ToolResults
		// below — that is the field Kiro / CodeWhisperer actually consumes.
		// We deliberately do NOT prepend a synthetic "Tool results:" prose envelope
		// here: the upstream model would see two parallel representations of the
		// same data and fingerprint the prose form as a fake harness signal.
		finalContent += minimalFallbackUserContent
	} else {
		finalContent += minimalFallbackUserContent
	}

	// 转换工具
	kiroTools, toolNameMap := convertClaudeTools(req.Tools)

	// 构建 payload
	payload := &KiroPayload{}
	payload.ToolNameMap = toolNameMap
	payload.ConversationState.ChatTriggerType = "MANUAL"
	payload.ConversationState.AgentTaskType = "vibe"
	payload.ConversationState.AgentContinuationId = uuid.New().String()
	payload.ConversationState.ConversationID = buildConversationID(modelID, systemPrompt, firstClaudeConversationAnchor(req.Messages))
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: finalContent,
		ModelID: modelID,
		Origin:  origin,
		Images:  currentImages,
	}

	if len(kiroTools) > 0 || len(currentToolResults) > 0 {
		payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{
			Tools:       kiroTools,
			ToolResults: currentToolResults,
		}
	}

	if len(history) > 0 {
		payload.ConversationState.History = history
	}

	if req.MaxTokens > 0 || req.Temperature > 0 || req.TopP > 0 {
		payload.InferenceConfig = &InferenceConfig{
			MaxTokens:   req.MaxTokens,
			Temperature: req.Temperature,
			TopP:        req.TopP,
		}
	}

	return payload
}

func buildClaudeSystemPrompt(system interface{}, thinking bool) string {
	// Note: `thinking` is intentionally unused for input shaping. Reasoning is
	// surfaced response-side (extractThinkingFromContent + reasoning_content
	// callbacks); we no longer inject a system-prompt cue.
	_ = thinking
	systemPrompt := extractSystemPrompt(system)
	return applySystemPromptFilters(systemPrompt)
}

// applySystemPromptFilters runs the full filter chain on a system prompt:
//
//  1. Detect Claude Code CLI system prompt -> replace with claudeCodeBackendPrompt
//     (gated by FilterClaudeCode toggle).
//  2. Strip "--- SYSTEM PROMPT ---" boundary markers (gated by FilterStripBoundaries).
//  3. Strip environment-noise lines and <system-reminder> blocks (gated by FilterEnvNoise).
//  4. Apply user-defined regex / line-filter rules.
//
// Use this only on the system prompt. For user-message text use
// applyUserMessageFilters which deliberately skips step 1 (we must never
// replace the user's actual question with the proxy's backend prompt).
func applySystemPromptFilters(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return ""
	}

	// 1. Detect Claude Code CLI system prompt → replace with minimal backend prompt.
	if config.GetFilterClaudeCode() && isClaudeCodeSystemPrompt(prompt) {
		return claudeCodeBackendPrompt
	}

	return applySharedFilters(prompt)
}

// applyUserMessageFilters runs only the noise-stripping subset of the filter
// chain, so a user message that happens to look like a Claude Code system
// prompt (e.g. when the user asks Claude to review a system prompt) does not
// get its content replaced with the backend prompt. Strips:
//
//   - <system-reminder>...</system-reminder> blocks injected by Claude Code 2.x
//   - x-anthropic-billing-header lines
//   - "--- SYSTEM PROMPT ---" boundary markers (when FilterStripBoundaries is on)
//   - ## Environment / ## auto memory sections (when FilterEnvNoise is on)
//   - User-defined regex / line-filter rules
//
// It does NOT replace the prompt with claudeCodeBackendPrompt.
func applyUserMessageFilters(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return ""
	}
	return applySharedFilters(prompt)
}

// applySharedFilters runs the noise-strip and user-rule steps shared between
// system-prompt and user-message filtering.
func applySharedFilters(prompt string) string {
	if config.GetFilterStripBoundaries() {
		prompt = stripBoundaryMarkers(prompt)
	}
	if config.GetFilterEnvNoise() {
		prompt = stripEnvNoiseLines(prompt)
	}
	rules := config.GetPromptFilterRules()
	for _, rule := range rules {
		if !rule.Enabled || prompt == "" {
			continue
		}
		prompt = applyFilterRule(prompt, rule)
	}
	return strings.TrimSpace(prompt)
}

// applyPromptFilters is retained as a backward-compatible alias for the
// system-prompt filter chain. New code should call applySystemPromptFilters
// or applyUserMessageFilters explicitly.
func applyPromptFilters(prompt string) string {
	return applySystemPromptFilters(prompt)
}

// applyFilterRule applies a single user-defined filter rule.
func applyFilterRule(prompt string, rule config.PromptFilterRule) string {
	switch rule.Type {
	case "regex":
		re, err := regexp.Compile(rule.Match)
		if err != nil {
			return prompt // invalid regex: skip silently
		}
		return re.ReplaceAllString(prompt, rule.Replace)
	case "lines-containing", "contains":
		// Remove lines that contain the match substring (case-insensitive).
		// This is line-level, not whole-prompt replacement — much safer.
		lower := strings.ToLower(rule.Match)
		lines := strings.Split(prompt, "\n")
		out := make([]string, 0, len(lines))
		for _, line := range lines {
			if !strings.Contains(strings.ToLower(line), lower) {
				out = append(out, line)
			}
		}
		return strings.TrimSpace(collapseBlankLines(strings.Join(out, "\n")))
	}
	return prompt
}

// stripBoundaryMarkers removes --- SYSTEM PROMPT --- and --- END SYSTEM PROMPT --- lines.
func stripBoundaryMarkers(prompt string) string {
	lines := strings.Split(prompt, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--- SYSTEM PROMPT ---") ||
			strings.HasPrefix(trimmed, "--- END SYSTEM PROMPT ---") {
			continue
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// systemReminderBlockRe matches a full <system-reminder>...</system-reminder>
// block, including any whitespace/newlines inside. Claude Code 2.x injects these
// into both the system prompt and the first user message; the contents
// (deferred-tool catalog, skills index, currentDate, auto memory, environment,
// etc.) are exactly what trips Kiro / Bedrock content moderation.
var systemReminderBlockRe = regexp.MustCompile(`(?is)<system-reminder>.*?</system-reminder>`)

// billingHeaderLineRe matches the Claude Code attribution line in any position
// (line start, indented, quoted, embedded). Bedrock rejects this string with
// 400 "x-anthropic-billing-header is a reserved keyword and may not be used in
// the system prompt", so it must be removed wherever it appears.
var billingHeaderLineRe = regexp.MustCompile(`(?im)^[ \t>"']*x-anthropic-billing-header:[^\n]*\n?`)
var billingHeaderInlineRe = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^\n]*`)

// claudeCodeNoisySectionPrefixes lists heading lines whose entire section
// (until the next heading at the same or higher level) should be dropped.
// Match is case-insensitive and tolerates one or two leading hashes.
var claudeCodeNoisySectionPrefixes = []string{
	"# environment",
	"## environment",
	"# auto memory",
	"## auto memory",
	"# session-specific guidance",
	"## session-specific guidance",
}

// isNoisySectionHeading reports whether a heading line begins a section that
// should be skipped. Returns true for "# Environment", "## Environment",
// "# auto memory", "## auto memory", etc.
func isNoisySectionHeading(trimmed string) bool {
	lower := strings.ToLower(trimmed)
	for _, p := range claudeCodeNoisySectionPrefixes {
		if lower == p || strings.HasPrefix(lower, p+" ") || strings.HasPrefix(lower, p+":") {
			return true
		}
	}
	return false
}

// isAnyHeading reports whether a line is a markdown heading at any level
// (used to terminate a noisy section skip).
func isAnyHeading(trimmed string) bool {
	return strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "## ") ||
		strings.HasPrefix(trimmed, "### ") || strings.HasPrefix(trimmed, "#### ")
}

// stripEnvNoiseLines removes environment metadata lines and sections from a
// system prompt or user-message text block.
//
// Strips:
//   - <system-reminder>...</system-reminder> blocks (entire block, multiline)
//   - x-anthropic-billing-header: ... lines (Bedrock reserved keyword)
//   - # Environment / ## Environment / # auto memory / ## auto memory sections
//   - # session-specific guidance / ## session-specific guidance sections
//   - gitStatus, Recent commits, knowledge cutoff, fast_mode_info, etc.
//   - Various inline "Claude Code" identity markers
func stripEnvNoiseLines(prompt string) string {
	// 1. Block-level: drop whole <system-reminder>...</system-reminder> blocks.
	prompt = systemReminderBlockRe.ReplaceAllString(prompt, "")

	// 2. Line-level: drop the Bedrock reserved keyword wherever it appears,
	//    even if Claude Code emits it indented or wrapped.
	prompt = billingHeaderLineRe.ReplaceAllString(prompt, "")
	prompt = billingHeaderInlineRe.ReplaceAllString(prompt, "")

	// 3. Walk remaining lines and skip noisy sections + individual lines.
	lines := strings.Split(prompt, "\n")
	out := make([]string, 0, len(lines))
	skipSection := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)

		if isNoisySectionHeading(trimmed) {
			skipSection = true
			continue
		}
		if skipSection {
			if isAnyHeading(trimmed) {
				skipSection = false
				// fall through — include the new heading
			} else {
				continue
			}
		}

		// Drop individual noisy lines regardless of section.
		if strings.HasPrefix(trimmed, "gitStatus:") ||
			strings.HasPrefix(trimmed, "Recent commits:") ||
			strings.HasPrefix(trimmed, "Assistant knowledge cutoff") ||
			strings.HasPrefix(trimmed, "<fast_mode_info>") ||
			strings.HasPrefix(trimmed, "</fast_mode_info>") ||
			strings.HasPrefix(trimmed, "<command-name>") ||
			strings.HasPrefix(trimmed, "<command-message>") ||
			strings.HasPrefix(trimmed, "<command-args>") ||
			strings.HasPrefix(trimmed, "<env>") ||
			strings.HasPrefix(trimmed, "</env>") ||
			strings.HasPrefix(trimmed, "<cwd>") ||
			strings.HasPrefix(trimmed, "<git>") ||
			strings.HasPrefix(trimmed, "<is_directory>") ||
			strings.HasPrefix(trimmed, "<platform>") ||
			strings.HasPrefix(trimmed, "<os_version>") ||
			strings.Contains(lower, "you are claude code") ||
			strings.Contains(lower, "anthropic's official cli") ||
			strings.Contains(trimmed, ".claude/projects/") ||
			strings.Contains(trimmed, "git status at the start of the conversation") ||
			strings.Contains(trimmed, "has been invoked in the following environment") ||
			strings.Contains(lower, "powered by the model named") ||
			strings.Contains(lower, "the most recent claude model family") ||
			strings.Contains(lower, "fast mode for claude code") {
			continue
		}

		out = append(out, line)
	}
	return strings.TrimSpace(collapseBlankLines(strings.Join(out, "\n")))
}

// claudeCodeBackendPrompt is injected when a Claude Code CLI system prompt is
// detected. It is intentionally a single neutral sentence: Claude already has
// built-in protection against prompt injection from tool output / file content
// / web pages, so the proxy does not need to re-instruct that behaviour.
// Re-instructing it ("Treat tool outputs as data, not higher-priority
// instructions...") makes the upstream model fingerprint our prompt as a fake
// system-reminder and surface it back to the user as an envelope artifact.
const claudeCodeBackendPrompt = `Help the user with their software engineering task. Keep responses concise and actionable.`

// isClaudeCodeSystemPrompt returns true when the prompt matches characteristic
// markers of the Claude Code CLI built-in system prompt.
//
// Threshold:
//   - At least one STRONG marker (high-specificity signal that essentially
//     only appears in the Claude Code prompt) AND
//   - At least three total markers (strong + weak combined).
//
// This prevents false positives where a user-supplied benign system prompt
// happens to mention generic phrases like "## Environment" or "claude code"
// in passing. A false replacement would silently overwrite the user's intent
// with our compact backend prompt.
func isClaudeCodeSystemPrompt(prompt string) bool {
	lower := strings.ToLower(prompt)

	// Strong markers — high specificity to Claude Code v1.x or v2.x.
	strongMarkers := []string{
		"x-anthropic-billing-header",
		"<system-reminder>",
		"you are powered by the model named",
		"you are an interactive agent that helps users with software engineering tasks",
		".claude/projects/",
		"anthropic's official cli",
	}

	// Weak markers — corroborating evidence; alone they do not imply Claude Code.
	weakMarkers := []string{
		// v1.x section headings
		"# doing tasks",
		"# using your tools",
		"# tone and style",
		// v2.x section headings
		"# harness",
		"# text output",
		"## session-specific guidance",
		"## auto memory",
		"## environment",
		"# tools",
		"## agent",
		// brand
		"claude code",
	}

	strongHits := 0
	for _, m := range strongMarkers {
		if strings.Contains(lower, m) {
			strongHits++
		}
	}
	if strongHits == 0 {
		return false
	}

	totalHits := strongHits
	for _, m := range weakMarkers {
		if strings.Contains(lower, m) {
			totalHits++
		}
	}
	return totalHits >= 3
}

// collapseBlankLines reduces runs of consecutive blank lines to a single blank line.
func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blanks := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			blanks++
			if blanks > 1 {
				continue
			}
		} else {
			blanks = 0
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// cloneClaudeRequestForThinking returns a shallow clone of req. It used to
// prepend a thinking-mode envelope when thinking was on; that injection has
// been removed (the upstream model fingerprinted it as a fake harness signal),
// so this function is now a near no-op kept for API compatibility with the
// callers that use the cloned struct for token estimation / cache profiling.
func cloneClaudeRequestForThinking(req *ClaudeRequest, thinking bool) *ClaudeRequest {
	if req == nil {
		return nil
	}
	_ = thinking
	cloned := *req
	return &cloned
}

func extractSystemPrompt(system interface{}) string {
	if system == nil {
		return ""
	}
	if s, ok := system.(string); ok {
		return s
	}
	if blocks, ok := system.([]interface{}); ok {
		var parts []string
		for _, b := range blocks {
			if block, ok := b.(map[string]interface{}); ok {
				if text, ok := block["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func extractClaudeUserContent(content interface{}) (string, []KiroImage, []KiroToolResult) {
	var text string
	var images []KiroImage
	var toolResults []KiroToolResult

	if s, ok := content.(string); ok {
		return applyUserMessageFilters(s), nil, nil
	}

	if blocks, ok := content.([]interface{}); ok {
		for _, b := range blocks {
			block, ok := b.(map[string]interface{})
			if !ok {
				continue
			}

			blockType, _ := block["type"].(string)
			switch blockType {
			case "text", "input_text":
				if t, ok := block["text"].(string); ok {
					text += applyUserMessageFilters(t)
				}
			case "image", "image_url", "input_image":
				if img := extractImageFromClaudeBlock(block); img != nil {
					images = append(images, *img)
				}
			case "tool_result":
				toolUseID, _ := block["tool_use_id"].(string)
				resultContent := extractToolResultContent(block["content"])
				toolResults = append(toolResults, KiroToolResult{
					ToolUseID: toolUseID,
					Content:   []KiroResultContent{{Text: resultContent}},
					Status:    "success",
				})
			}
		}
	}

	return text, images, toolResults
}

func extractImageFromClaudeBlock(block map[string]interface{}) *KiroImage {
	if source, ok := block["source"].(map[string]interface{}); ok {
		if data, ok := source["data"].(string); ok {
			if img := parseDataURL(data); img != nil {
				return img
			}
			mediaType, _ := source["media_type"].(string)
			if mediaType == "" {
				mediaType, _ = source["mediaType"].(string)
			}
			if mediaType == "" {
				mediaType, _ = source["mime_type"].(string)
			}
			format := strings.TrimPrefix(strings.ToLower(mediaType), "image/")
			if img := parseBase64Image(data, format); img != nil {
				return img
			}
		}
		if url, ok := source["url"].(string); ok {
			if img := parseDataURL(url); img != nil {
				return img
			}
		}
	}

	if img := extractImageFromOpenAIPart(block); img != nil {
		return img
	}

	if data, ok := block["data"].(string); ok {
		if img := parseDataURL(data); img != nil {
			return img
		}
	}

	return nil
}

func extractToolResultContent(content interface{}) string {
	if s, ok := content.(string); ok {
		return s
	}
	if blocks, ok := content.([]interface{}); ok {
		var parts []string
		for _, b := range blocks {
			if block, ok := b.(map[string]interface{}); ok {
				if text, ok := block["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

func extractClaudeAssistantContent(content interface{}) (string, []KiroToolUse) {
	var text string
	var toolUses []KiroToolUse

	if s, ok := content.(string); ok {
		return s, nil
	}

	if blocks, ok := content.([]interface{}); ok {
		for _, b := range blocks {
			block, ok := b.(map[string]interface{})
			if !ok {
				continue
			}

			blockType, _ := block["type"].(string)
			switch blockType {
			case "text":
				if t, ok := block["text"].(string); ok {
					text += t
				}
			case "tool_use":
				id, _ := block["id"].(string)
				name, _ := block["name"].(string)
				input, _ := block["input"].(map[string]interface{})
				if input == nil {
					input = make(map[string]interface{})
				}
				toolUses = append(toolUses, KiroToolUse{
					ToolUseID: id,
					Name:      name,
					Input:     input,
				})
			}
		}
	}

	return text, toolUses
}

func convertClaudeTools(tools []ClaudeTool) ([]KiroToolWrapper, map[string]string) {
	if len(tools) == 0 {
		return nil, nil
	}

	result := make([]KiroToolWrapper, 0, len(tools))
	nameMap := make(map[string]string)
	for _, tool := range tools {
		desc := tool.Description
		if len(desc) > maxToolDescLen {
			desc = desc[:maxToolDescLen] + "..."
		}
		sanitized := shortenToolName(sanitizeToolName(tool.Name))
		if sanitized != tool.Name {
			nameMap[sanitized] = tool.Name
		}
		w := KiroToolWrapper{}
		w.ToolSpecification.Name = sanitized
		w.ToolSpecification.Description = desc
		w.ToolSpecification.InputSchema = InputSchema{JSON: ensureObjectSchema(tool.InputSchema)}
		result = append(result, w)
	}
	return result, nameMap
}

// ensureObjectSchema ensures the JSON schema has "type": "object" at the top level
// and removes invalid null values from "required" fields (recursively).
// Kiro API rejects tool schemas with "required": null.
func ensureObjectSchema(schema interface{}) interface{} {
	m, ok := schema.(map[string]interface{})
	if !ok {
		return map[string]interface{}{"type": "object"}
	}
	cleanSchema(m)
	if _, hasType := m["type"]; !hasType {
		m["type"] = "object"
	}
	return m
}

// cleanSchema recursively removes or fixes invalid "required": null entries
// in a JSON Schema tree.
func cleanSchema(m map[string]interface{}) {
	// Fix "required" field: must be array or absent
	if req, exists := m["required"]; exists {
		if req == nil {
			delete(m, "required")
		} else if arr, ok := req.([]interface{}); ok && len(arr) == 0 {
			delete(m, "required")
		}
	}

	// Recurse into "properties"
	if props, ok := m["properties"].(map[string]interface{}); ok {
		for _, v := range props {
			if sub, ok := v.(map[string]interface{}); ok {
				cleanSchema(sub)
			}
		}
	}

	// Recurse into "items"
	if items, ok := m["items"].(map[string]interface{}); ok {
		cleanSchema(items)
	}

	// Recurse into nested object schemas (e.g., additionalProperties, allOf, oneOf, anyOf)
	for _, key := range []string{"additionalProperties"} {
		if sub, ok := m[key].(map[string]interface{}); ok {
			cleanSchema(sub)
		}
	}
	for _, key := range []string{"allOf", "oneOf", "anyOf"} {
		if arr, ok := m[key].([]interface{}); ok {
			for _, item := range arr {
				if sub, ok := item.(map[string]interface{}); ok {
					cleanSchema(sub)
				}
			}
		}
	}
}

// sanitizeToolName normalizes a tool name to characters the Kiro API accepts.
// Kiro tool names must be pure camelCase (no underscores or dashes).
// Separators (_, -, and multi-underscore namespace prefixes) are converted to camelCase boundaries.
func sanitizeToolName(name string) string {
	// Split on underscores and dashes, including multi-underscore namespace prefixes.
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == '_' || r == '-'
	})
	if len(parts) == 0 {
		return "tool"
	}
	// Build camelCase: first part lowercase start, rest capitalize first letter
	var b strings.Builder
	for i, part := range parts {
		if part == "" {
			continue
		}
		if i == 0 {
			b.WriteString(strings.ToLower(part[:1]) + part[1:])
		} else {
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	result := b.String()
	if result == "" {
		return "tool"
	}
	return result
}

func shortenToolName(name string) string {
	if len(name) <= 64 {
		return name
	}
	// MCP tools: mcp__server__tool -> mcp__tool
	if strings.HasPrefix(name, "mcp__") {
		lastIdx := strings.LastIndex(name, "__")
		if lastIdx > 5 {
			shortened := "mcp__" + name[lastIdx+2:]
			if len(shortened) <= 64 {
				return shortened
			}
		}
	}
	return name[:64]
}

// ==================== Kiro -> Claude 转换 ====================

func KiroToClaudeResponse(content, thinkingContent string, includeEmptyThinkingBlock bool, toolUses []KiroToolUse, inputTokens, outputTokens int, model string) *ClaudeResponse {
	blocks := make([]ClaudeContentBlock, 0)

	if thinkingContent != "" || includeEmptyThinkingBlock {
		blocks = append(blocks, ClaudeContentBlock{
			Type:     "thinking",
			Thinking: thinkingContent,
		})
	}

	if content != "" {
		blocks = append(blocks, ClaudeContentBlock{
			Type: "text",
			Text: content,
		})
	}

	for _, tu := range toolUses {
		blocks = append(blocks, ClaudeContentBlock{
			Type:  "tool_use",
			ID:    tu.ToolUseID,
			Name:  tu.Name,
			Input: tu.Input,
		})
	}

	stopReason := "end_turn"
	if len(toolUses) > 0 {
		stopReason = "tool_use"
	}

	return &ClaudeResponse{
		ID:         "msg_" + uuid.New().String(),
		Type:       "message",
		Role:       "assistant",
		Content:    blocks,
		Model:      model,
		StopReason: stopReason,
		Usage: ClaudeUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
		},
	}
}

// ==================== OpenAI API 类型 ====================

type OpenAIRequest struct {
	Model       string          `json:"model"`
	Messages    []OpenAIMessage `json:"messages"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	TopP        float64         `json:"top_p,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Tools       []OpenAITool    `json:"tools,omitempty"`
}

type OpenAIMessage struct {
	Role       string      `json:"role"`
	Content    interface{} `json:"content"`
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type OpenAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string      `json:"name"`
		Description string      `json:"description"`
		Parameters  interface{} `json:"parameters"`
	} `json:"function"`
}

type OpenAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage"`
}

type OpenAIChoice struct {
	Index        int           `json:"index"`
	Message      OpenAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ==================== OpenAI -> Kiro 转换 ====================

func OpenAIToKiro(req *OpenAIRequest, thinking bool) *KiroPayload {
	modelID := MapModel(req.Model)
	origin := "AI_EDITOR"

	// 提取系统提示
	var systemPrompt string
	var nonSystemMessages []OpenAIMessage

	for _, msg := range req.Messages {
		if msg.Role == "system" {
			if s := extractOpenAIMessageText(msg.Content); s != "" {
				systemPrompt += s + "\n"
			}
		} else {
			nonSystemMessages = append(nonSystemMessages, msg)
		}
	}

	// Run the same filter chain Claude requests use, so OpenAI-shaped clients
	// (Roo/Cline/Continue/etc. wrapping Claude Code) get the same protection.
	systemPrompt = applySystemPromptFilters(systemPrompt)

	// thinking flag no longer alters the outbound system prompt; the proxy
	// surfaces reasoning response-side.
	_ = thinking

	// 构建历史消息
	history := make([]KiroHistoryMessage, 0)
	var currentContent string
	var currentImages []KiroImage
	var currentToolResults []KiroToolResult
	systemMerged := false

	for i, msg := range nonSystemMessages {
		isLast := i == len(nonSystemMessages)-1

		switch msg.Role {
		case "user":
			content, images := extractOpenAIUserContent(msg.Content)
			content = normalizeUserContent(content, len(images) > 0)

			// 第一条 user 消息合并 system prompt
			if !systemMerged && systemPrompt != "" {
				content = systemPrompt + "\n" + content
				systemMerged = true
			}

			if isLast {
				currentContent = content
				currentImages = images
			} else {
				history = append(history, KiroHistoryMessage{
					UserInputMessage: &KiroUserInputMessage{
						Content: content,
						ModelID: modelID,
						Origin:  origin,
						Images:  images,
					},
				})
			}

		case "assistant":
			content := extractOpenAIMessageText(msg.Content)

			var toolUses []KiroToolUse
			for _, tc := range msg.ToolCalls {
				var input map[string]interface{}
				json.Unmarshal([]byte(tc.Function.Arguments), &input)
				if input == nil {
					input = make(map[string]interface{})
				}
				toolUses = append(toolUses, KiroToolUse{
					ToolUseID: tc.ID,
					Name:      tc.Function.Name,
					Input:     input,
				})
			}

			history = append(history, KiroHistoryMessage{
				AssistantResponseMessage: &KiroAssistantResponseMessage{
					Content:  content,
					ToolUses: toolUses,
				},
			})

		case "tool":
			content := extractOpenAIMessageText(msg.Content)
			currentToolResults = append(currentToolResults, KiroToolResult{
				ToolUseID: msg.ToolCallID,
				Content:   []KiroResultContent{{Text: content}},
				Status:    "success",
			})

			// 检查下一条是否还是 tool
			nextIdx := i + 1
			if nextIdx >= len(nonSystemMessages) || nonSystemMessages[nextIdx].Role != "tool" {
				if !isLast {
					history = append(history, KiroHistoryMessage{
						UserInputMessage: &KiroUserInputMessage{
							// Structured tool results live in
							// UserInputMessageContext.ToolResults; no prose envelope.
							Content: minimalFallbackUserContent,
							ModelID: modelID,
							Origin:  origin,
							UserInputMessageContext: &UserInputMessageContext{
								ToolResults: currentToolResults,
							},
						},
					})
					currentToolResults = nil
				}
			}
		}
	}

	// 构建最终内容
	finalContent := currentContent
	if finalContent == "" {
		if len(currentImages) > 0 {
			finalContent = normalizeUserContent("", true)
		} else if len(currentToolResults) > 0 {
			// Structured tool results travel via UserInputMessageContext.ToolResults
			// — no synthetic prose envelope here.
			finalContent = minimalFallbackUserContent
		} else {
			finalContent = minimalFallbackUserContent
		}
	}
	if !systemMerged && systemPrompt != "" {
		finalContent = systemPrompt + "\n" + finalContent
	}

	// 转换工具
	kiroTools := convertOpenAITools(req.Tools)

	// 构建 payload
	payload := &KiroPayload{}
	payload.ConversationState.ChatTriggerType = "MANUAL"
	payload.ConversationState.ConversationID = buildConversationID(modelID, systemPrompt, firstOpenAIConversationAnchor(nonSystemMessages))
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: finalContent,
		ModelID: modelID,
		Origin:  origin,
		Images:  currentImages,
	}

	if len(kiroTools) > 0 || len(currentToolResults) > 0 {
		payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &UserInputMessageContext{
			Tools:       kiroTools,
			ToolResults: currentToolResults,
		}
	}

	if len(history) > 0 {
		payload.ConversationState.History = history
	}

	if req.MaxTokens > 0 || req.Temperature > 0 || req.TopP > 0 {
		payload.InferenceConfig = &InferenceConfig{
			MaxTokens:   req.MaxTokens,
			Temperature: req.Temperature,
			TopP:        req.TopP,
		}
	}

	return payload
}

func extractOpenAIUserContent(content interface{}) (string, []KiroImage) {
	if s, ok := content.(string); ok {
		return applyUserMessageFilters(s), nil
	}

	var text string
	var images []KiroImage

	if part, ok := content.(map[string]interface{}); ok {
		if t, ok := extractOpenAITextPart(part); ok {
			text += applyUserMessageFilters(t)
		}
		if img := extractImageFromOpenAIPart(part); img != nil {
			images = append(images, *img)
		}
	}

	if parts, ok := content.([]interface{}); ok {
		for _, p := range parts {
			part, ok := p.(map[string]interface{})
			if !ok {
				continue
			}

			if t, ok := extractOpenAITextPart(part); ok {
				text += applyUserMessageFilters(t)
			}
			if img := extractImageFromOpenAIPart(part); img != nil {
				images = append(images, *img)
			}
		}
	}

	if len(images) > 0 {
		text = sanitizeImagePlaceholders(text)
	}

	return text, images
}

func extractOpenAIMessageText(content interface{}) string {
	if content == nil {
		return ""
	}

	if s, ok := content.(string); ok {
		return s
	}

	if text, _ := extractOpenAIUserContent(content); strings.TrimSpace(text) != "" {
		return text
	}

	switch v := content.(type) {
	case map[string]interface{}:
		if nested, ok := v["content"]; ok {
			if nestedText := extractOpenAIMessageText(nested); strings.TrimSpace(nestedText) != "" {
				return nestedText
			}
		}
		if raw, err := json.Marshal(v); err == nil {
			return string(raw)
		}
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			partText := extractOpenAIMessageText(item)
			if strings.TrimSpace(partText) != "" {
				parts = append(parts, partText)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "")
		}
		if raw, err := json.Marshal(v); err == nil {
			return string(raw)
		}
	default:
		if raw, err := json.Marshal(v); err == nil {
			return string(raw)
		}
	}

	return ""
}

func trimLeadingAssistantHistory(history []KiroHistoryMessage) []KiroHistoryMessage {
	idx := 0
	for idx < len(history) && history[idx].AssistantResponseMessage != nil {
		idx++
	}
	if idx == 0 {
		return history
	}
	if idx >= len(history) {
		return nil
	}
	return history[idx:]
}

func firstClaudeConversationAnchor(messages []ClaudeMessage) string {
	for _, msg := range messages {
		if msg.Role != "user" {
			continue
		}
		text, _, toolResults := extractClaudeUserContent(msg.Content)
		if strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
		if len(toolResults) > 0 {
			continue
		}
	}

	return ""
}

func firstOpenAIConversationAnchor(messages []OpenAIMessage) string {
	for _, msg := range messages {
		if msg.Role != "user" {
			continue
		}
		text := extractOpenAIMessageText(msg.Content)
		if strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}

	return ""
}

func buildConversationID(modelID, systemPrompt, anchor string) string {
	anchor = strings.TrimSpace(anchor)
	if isSyntheticConversationAnchor(anchor) {
		return uuid.New().String()
	}
	seed := strings.Join([]string{modelID, strings.TrimSpace(systemPrompt), anchor}, "\n")
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(seed)).String()
}

func isSyntheticConversationAnchor(anchor string) bool {
	if strings.TrimSpace(anchor) == "" {
		return true
	}

	normalized := strings.ToLower(strings.Join(strings.Fields(anchor), " "))
	switch normalized {
	case ".", "begin conversation", "please analyze the attached image.", strings.ToLower(minimalFallbackUserContent):
		return true
	default:
		return false
	}
}

func extractOpenAITextPart(part map[string]interface{}) (string, bool) {
	partType, _ := part["type"].(string)
	switch partType {
	case "text", "input_text":
		if t, ok := part["text"].(string); ok {
			return t, true
		}
	}

	if t, ok := part["text"].(string); ok {
		return t, true
	}

	return "", false
}

func extractImageFromOpenAIPart(part map[string]interface{}) *KiroImage {
	partType, _ := part["type"].(string)
	if partType != "" {
		switch partType {
		case "image", "image_url", "input_image", "file", "input_file":
		default:
			return nil
		}
	}

	if fileObj, ok := part["file"].(map[string]interface{}); ok {
		if img := extractImageFromOpenAIPart(fileObj); img != nil {
			return img
		}
	}

	if sourceObj, ok := part["source"].(map[string]interface{}); ok {
		if img := extractImageFromOpenAIPart(sourceObj); img != nil {
			return img
		}
	}

	if raw, ok := part["mime"].(string); ok && !strings.HasPrefix(strings.ToLower(raw), "image/") {
		return nil
	}
	if raw, ok := part["media_type"].(string); ok && !strings.HasPrefix(strings.ToLower(raw), "image/") {
		return nil
	}
	if raw, ok := part["mime_type"].(string); ok && !strings.HasPrefix(strings.ToLower(raw), "image/") {
		return nil
	}

	if raw, ok := part["url"].(string); ok {
		if img := parseDataURL(raw); img != nil {
			return img
		}
	}

	if raw, ok := part["b64_json"].(string); ok {
		if img := parseBase64Image(raw, "png"); img != nil {
			return img
		}
	}

	if raw, ok := part["image_url"]; ok {
		switch v := raw.(type) {
		case string:
			if img := parseDataURL(v); img != nil {
				return img
			}
		case map[string]interface{}:
			if u, ok := v["url"].(string); ok {
				if img := parseDataURL(u); img != nil {
					return img
				}
			}
		}
	}

	if raw, ok := part["image_base64"].(string); ok {
		if img := parseBase64Image(raw, "png"); img != nil {
			return img
		}
	}
	if raw, ok := part["data"].(string); ok {
		if img := parseDataURL(raw); img != nil {
			return img
		}
		if img := parseBase64Image(raw, "png"); img != nil {
			return img
		}
	}

	return nil
}

func sanitizeImagePlaceholders(text string) string {
	re := regexp.MustCompile(`\[Image\s+\d+\]`)
	cleaned := re.ReplaceAllString(text, "")
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	return strings.TrimSpace(cleaned)
}

func normalizeUserContent(text string, hasImages bool) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" && hasImages {
		return "Please analyze the attached image."
	}
	return trimmed
}

func parseDataURL(url string) *KiroImage {
	cleaned := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(url, "\n", ""), "\r", ""))
	if strings.Contains(cleaned, "[Image") {
		return nil
	}
	re := regexp.MustCompile(`^data:image/([a-zA-Z0-9+.-]+)(;[a-zA-Z0-9=._:+-]+)*;base64,(.+)$`)
	matches := re.FindStringSubmatch(cleaned)
	if len(matches) == 4 {
		return parseBase64Image(matches[3], matches[1])
	}
	if len(matches) != 3 {
		return nil
	}

	return parseBase64Image(matches[2], matches[1])
}

func parseBase64Image(data, format string) *KiroImage {
	format = strings.ToLower(format)
	if format == "jpg" {
		format = "jpeg"
	}

	// 验证 base64
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		if _, errRaw := base64.RawStdEncoding.DecodeString(data); errRaw != nil {
			if _, errURL := base64.URLEncoding.DecodeString(data); errURL != nil {
				if _, errRawURL := base64.RawURLEncoding.DecodeString(data); errRawURL != nil {
					return nil
				}
			}
		}
	}

	if format == "" {
		format = "png"
	}

	return &KiroImage{
		Format: format,
		Source: struct {
			Bytes string `json:"bytes"`
		}{Bytes: data},
	}
}

func convertOpenAITools(tools []OpenAITool) []KiroToolWrapper {
	if len(tools) == 0 {
		return nil
	}

	result := make([]KiroToolWrapper, 0, len(tools))
	for _, tool := range tools {
		if tool.Type != "function" {
			continue
		}
		desc := tool.Function.Description
		if len(desc) > maxToolDescLen {
			desc = desc[:maxToolDescLen] + "..."
		}
		wrapper := KiroToolWrapper{}
		wrapper.ToolSpecification.Name = shortenToolName(tool.Function.Name)
		wrapper.ToolSpecification.Description = desc
		wrapper.ToolSpecification.InputSchema = InputSchema{JSON: tool.Function.Parameters}
		result = append(result, wrapper)
	}
	return result
}

// ==================== Kiro -> OpenAI 转换 ====================

func KiroToOpenAIResponse(content string, toolUses []KiroToolUse, inputTokens, outputTokens int, model string) *OpenAIResponse {
	msg := OpenAIMessage{
		Role: "assistant",
	}

	finishReason := "stop"

	if len(toolUses) > 0 {
		msg.Content = nil
		msg.ToolCalls = make([]ToolCall, len(toolUses))
		for i, tu := range toolUses {
			args, _ := json.Marshal(tu.Input)
			msg.ToolCalls[i] = ToolCall{
				ID:   tu.ToolUseID,
				Type: "function",
			}
			msg.ToolCalls[i].Function.Name = tu.Name
			msg.ToolCalls[i].Function.Arguments = string(args)
		}
		finishReason = "tool_calls"
	} else {
		msg.Content = content
	}

	return &OpenAIResponse{
		ID:      "chatcmpl-" + uuid.New().String(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []OpenAIChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: finishReason,
		}},
		Usage: OpenAIUsage{
			PromptTokens:     inputTokens,
			CompletionTokens: outputTokens,
			TotalTokens:      inputTokens + outputTokens,
		},
	}
}

// extractThinkingFromContent 从内容中提取 <thinking> 标签内的内容
func extractThinkingFromContent(content string) (string, string) {
	var reasoning string
	result := content

	for {
		start := strings.Index(result, "<thinking>")
		if start == -1 {
			break
		}
		end := strings.Index(result[start:], "</thinking>")
		if end == -1 {
			break
		}
		end += start

		// 提取 thinking 内容
		thinkingContent := result[start+10 : end]
		reasoning += thinkingContent

		// 从结果中移除 thinking 标签
		result = result[:start] + result[end+11:]
	}

	return strings.TrimSpace(result), reasoning
}

// KiroToOpenAIResponseWithReasoning 带 reasoning_content 的 OpenAI 响应
func KiroToOpenAIResponseWithReasoning(content, reasoningContent string, toolUses []KiroToolUse, inputTokens, outputTokens int, model, thinkingFormat string) map[string]interface{} {
	finishReason := "stop"

	message := map[string]interface{}{
		"role": "assistant",
	}

	if len(toolUses) > 0 {
		message["content"] = nil
		toolCalls := make([]map[string]interface{}, len(toolUses))
		for i, tu := range toolUses {
			args, _ := json.Marshal(tu.Input)
			toolCalls[i] = map[string]interface{}{
				"id":   tu.ToolUseID,
				"type": "function",
				"function": map[string]string{
					"name":      tu.Name,
					"arguments": string(args),
				},
			}
		}
		message["tool_calls"] = toolCalls
		finishReason = "tool_calls"
	} else {
		// 根据配置格式化 thinking 输出
		if reasoningContent != "" {
			switch thinkingFormat {
			case "thinking":
				message["content"] = "<thinking>" + reasoningContent + "</thinking>" + content
			case "think":
				message["content"] = "<think>" + reasoningContent + "</think>" + content
			default: // "reasoning_content"
				message["content"] = content
				message["reasoning_content"] = reasoningContent
			}
		} else {
			message["content"] = content
		}
	}

	return map[string]interface{}{
		"id":      "chatcmpl-" + uuid.New().String(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": map[string]int{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
		},
	}
}
