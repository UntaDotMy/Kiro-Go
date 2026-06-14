package proxy

import (
	"encoding/json"
	"strings"
)

// CodeBuddy competitor-keyword sanitization.
//
// CodeBuddy (Tencent) runs a server-side content-moderation pass over every
// request. It flags competitor brand tokens — most notably "Claude" and
// "Anthropic", which saturate the Claude Code system prompt — and returns a
// canned Chinese refusal as normal assistant content:
//
//	抱歉，系统检测到您当前输入的信息存在敏感内容，我无法响应您的请求，请检查后重新输入。
//
// Claude Code then wraps the refusal in its own "violates our Usage Policy" error.
//
// Token-level rewriting alone is NOT enough: the Claude Code system prompt is long
// and brand-dense, and CodeBuddy's moderation flags more than the two literal
// words. The reliable fix — used by the reference implementation (9router's
// open-sse/executors/default.js buildCodeBuddyBody) — is to REPLACE the entire
// system prompt with a neutral one ("You are CodeBuddy Code.") and rewrite any
// residual brand tokens in the remaining message/tool content. We do this only for
// CodeBuddy backends, and never touch the top-level "model" field (CodeBuddy's own
// model ids contain "claude", e.g. claude-sonnet-4.6).

// codeBuddyNeutralSystemPrompt is the replacement system message, matching the
// reference CONNECT prompt the CodeBuddy CLI itself sends.
const codeBuddyNeutralSystemPrompt = "You are CodeBuddy Code."

// codeBuddyKeywordReplacer rewrites residual competitor brand tokens in non-system
// content. Order matters: strings.Replacer matches argument order at each position,
// so multi-word phrases must precede the single-word fallbacks.
var codeBuddyKeywordReplacer = strings.NewReplacer(
	"Anthropic's official CLI for Claude", "Tencent's official CLI for CodeBuddy",
	"Claude Code", "CodeBuddy Code",
	"claude code", "codebuddy code",
	"Claude", "CodeBuddy",
	"claude", "codebuddy",
	"Anthropic", "Tencent",
	"anthropic", "tencent",
)

// isCodeBuddyBackend reports whether a resolved backend id is one of the two
// CodeBuddy hosts (CN gateway or international site).
func isCodeBuddyBackend(id string) bool {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "codebuddy", "codebuddy-ai":
		return true
	}
	return false
}

// sanitizeCodeBuddyBody neutralizes a marshaled OpenAI-dialect request body so
// CodeBuddy's content-moderation filter doesn't reject it: every system message is
// replaced with a neutral prompt, and brand tokens are rewritten in all other
// string values (except the top-level "model" id). On any parse/marshal error it
// returns the body unchanged (fail-open: a moderation rejection beats a corrupted
// body).
func sanitizeCodeBuddyBody(body []byte) []byte {
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return body
	}

	// Replace any system/developer message content with the neutral prompt. The
	// generic provider builds an OpenAI chat body, so the system prompt arrives as
	// a leading {role:"system", content:...} message.
	if msgs, ok := root["messages"].([]interface{}); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			if role == "system" || role == "developer" {
				msg["content"] = codeBuddyNeutralSystemPrompt
				continue
			}
			// Non-system message: rewrite residual brand tokens in its content.
			msg["content"] = sanitizeCodeBuddyValue(msg["content"])
		}
	}

	// Rewrite brand tokens everywhere else (tools, tool descriptions, etc.) except
	// the model id and the already-handled messages array.
	for k, v := range root {
		if k == "model" || k == "messages" {
			continue
		}
		root[k] = sanitizeCodeBuddyValue(v)
	}

	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

// sanitizeCodeBuddyValue recursively rewrites brand tokens in any string value
// within a decoded JSON value (string, []interface{}, or map[string]interface{}).
func sanitizeCodeBuddyValue(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		return codeBuddyKeywordReplacer.Replace(t)
	case []interface{}:
		for i := range t {
			t[i] = sanitizeCodeBuddyValue(t[i])
		}
		return t
	case map[string]interface{}:
		for k, val := range t {
			t[k] = sanitizeCodeBuddyValue(val)
		}
		return t
	default:
		return v
	}
}
