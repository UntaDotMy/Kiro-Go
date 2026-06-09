package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// codexProvider's sibling for Qoder. Qoder gates inference behind COSY signing
// (RSA+AES+MD5) + a WAF-bypass body encoding, and expects a bespoke request shape
// whose response is an OpenAI SSE chunk wrapped in a {statusCodeValue, body}
// envelope. Ported from 9router (open-sse/executors/qoder.js + lib/qoder/*).
type qoderProvider struct{}

func init() {
	RegisterProvider(qoderProvider{})
}

func (qoderProvider) Name() string { return "qoder" }

const (
	qoderChatBase    = "https://api3.qoder.sh"
	qoderChatSigPath = "/api/v2/service/pro/sse/agent_chat_generation"
	qoderChatURL     = qoderChatBase + "/algo" + qoderChatSigPath + "?FetchKeys=llm_model_result&AgentId=agent_common"
	qoderChatURLEnc  = qoderChatURL + "&Encode=1"
)

// qoderCanonicalModels is the set of canonical Qoder model keys (tier + frontier
// ids). Used for ListModels and to strip the "qoder/" prefix.
var qoderCanonicalModels = []string{
	"auto", "ultimate", "performance", "efficient", "lite",
	"qmodel", "qmodel_latest", "dmodel", "dfmodel", "gm51model", "kmodel", "mmodel",
}

// RefreshToken is a no-op for Qoder: device tokens last ~30 days and the upstream
// refresh endpoint returns 403 for this flow. The account is re-added on expiry.
func (qoderProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	return TokenSet{AccessToken: acct.AccessToken, RefreshToken: acct.RefreshToken, ExpiresAt: acct.ExpiresAt}, nil
}

// ListModels returns the canonical Qoder model keys. (The live /model/list
// endpoint carries richer model_config used at call time; the keys here are
// enough for routing + the dashboard.)
func (qoderProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	out := make([]ModelInfo, 0, len(qoderCanonicalModels))
	for _, id := range qoderCanonicalModels {
		out = append(out, ModelInfo{ModelId: id})
	}
	return out, nil
}

// Call builds the Qoder chat request from nr, encodes + COSY-signs it, POSTs to
// the inference endpoint, and unwraps the response SSE into cb.
func (qoderProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(acct.QoderUserID) == "" {
		return fmt.Errorf("qoder account %s is missing userId; reconnect the account", acct.ID)
	}
	if strings.TrimSpace(acct.AccessToken) == "" {
		return fmt.Errorf("qoder account %s is missing access token; reconnect the account", acct.ID)
	}

	qoderKey := strings.TrimPrefix(strings.TrimSpace(nr.Model), "qoder/")
	if qoderKey == "" {
		qoderKey = "auto"
	}

	payload := buildQoderPayload(nr, qoderKey, acct)
	plain, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	encoded := auth.QoderEncodeBody(plain)

	headers, err := auth.BuildCosyHeaders(encoded, qoderChatURLEnc, auth.QoderCreds{
		UserID:    acct.QoderUserID,
		AuthToken: acct.AccessToken,
		Name:      acct.Nickname,
		Email:     acct.Email,
		MachineID: acct.QoderMachineID,
	})
	if err != nil {
		return fmt.Errorf("qoder cosy signing failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", qoderChatURLEnc, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("X-Model-Key", qoderKey)
	req.Header.Set("X-Model-Source", "system")
	// gzip triggers signature validation on Qoder's CDN; force identity.
	req.Header.Set("Accept-Encoding", "identity")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := GetClientForProxy(ResolveAccountProxyURL(acct)).Do(req)
	if err != nil {
		return classifyStreamError(err)
	}
	if resp.StatusCode == 429 {
		retryAfter := parseRetryAfter(resp.Header)
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return &QuotaError{Endpoints: []string{"qoder"}, RetryAfter: retryAfter}
	}
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return fmt.Errorf("HTTP %d from qoder: %s", resp.StatusCode, string(errBody))
	}

	streamErr := func() error {
		defer resp.Body.Close()
		rdr := newIdleTimeoutReader(resp.Body, streamIdleTimeout, func() {})
		return parseQoderSSE(rdr, cb)
	}()
	return classifyStreamError(streamErr)
}

// buildQoderPayload maps the originating request into the exact shape Qoder
// expects: system text hoisted out of messages, a chat_context block mirroring
// the last user turn, and a business block with stable ids.
func buildQoderPayload(nr *NormalizedRequest, qoderKey string, acct *config.Account) map[string]interface{} {
	system, messages := qoderMessagesFromRequest(nr)
	maxTokens := 32768
	if nr.OpenAI != nil && nr.OpenAI.MaxTokens > 0 {
		maxTokens = nr.OpenAI.MaxTokens
	} else if nr.Claude != nil && nr.Claude.MaxTokens > 0 {
		maxTokens = nr.Claude.MaxTokens
	}

	lastUser := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i]["role"] == "user" {
			if s, ok := messages[i]["content"].(string); ok {
				lastUser = s
				break
			}
		}
	}

	sessionID := qoderStableHash("qoder-session", acct.QoderUserID, qoderKey)
	recordID := qoderStableChatRecordID(qoderKey, messages, maxTokens)

	return map[string]interface{}{
		"request_id":       uuid.New().String(),
		"request_set_id":   recordID,
		"chat_record_id":   recordID,
		"session_id":       sessionID,
		"stream":           true,
		"chat_task":        "FREE_INPUT",
		"is_reply":         true,
		"is_retry":         false,
		"source":           1,
		"version":          "3",
		"session_type":     "qodercli",
		"agent_id":         "agent_common",
		"task_id":          "common",
		"code_language":    "",
		"chat_prompt":      "",
		"image_urls":       nil,
		"aliyun_user_type": "",
		"system":           system,
		"messages":         messages,
		"tools":            []interface{}{},
		"parameters":       map[string]interface{}{"max_tokens": maxTokens},
		"chat_context": map[string]interface{}{
			"chatPrompt": "",
			"imageUrls":  nil,
			"extra": map[string]interface{}{
				"context":         []interface{}{},
				"modelConfig":     map[string]interface{}{"key": qoderKey, "is_reasoning": false},
				"originalContent": lastUser,
			},
			"features": []interface{}{},
			"text":     lastUser,
		},
		"model_config": map[string]interface{}{"key": qoderKey, "is_reasoning": false},
		"business": map[string]interface{}{
			"product":  "cli",
			"version":  "1.0.0",
			"type":     "agent",
			"stage":    "start",
			"id":       uuid.New().String(),
			"name":     qoderTruncate(lastUser, 30),
			"begin_at": time.Now().UnixMilli(),
		},
	}
}

// qoderMessagesFromRequest flattens the originating request into Qoder's
// (systemText, messages[]) shape. System messages are hoisted out; content is
// flattened to plain text.
func qoderMessagesFromRequest(nr *NormalizedRequest) (string, []map[string]interface{}) {
	var sys strings.Builder
	var out []map[string]interface{}
	add := func(role, text string) {
		if role == "system" {
			if text != "" {
				if sys.Len() > 0 {
					sys.WriteString("\n\n")
				}
				sys.WriteString(text)
			}
			return
		}
		out = append(out, map[string]interface{}{"role": role, "content": text})
	}

	switch {
	case nr.Claude != nil:
		if s := extractClaudeSystemString(nr.Claude.System); s != "" {
			sys.WriteString(s)
		}
		for _, m := range nr.Claude.Messages {
			add(m.Role, claudeMessageText(m.Content))
		}
	case nr.OpenAI != nil:
		for _, m := range nr.OpenAI.Messages {
			add(m.Role, extractOpenAIMessageText(m.Content))
		}
	}
	return sys.String(), out
}

func qoderStableHash(prefix string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(prefix))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

func qoderStableChatRecordID(model string, messages []map[string]interface{}, maxTokens int) string {
	h := sha256.New()
	h.Write([]byte("qoder-record\x00"))
	h.Write([]byte(model))
	for _, m := range messages {
		if role, ok := m["role"].(string); ok && role != "" {
			h.Write([]byte{0})
			h.Write([]byte(role))
		}
		if content, ok := m["content"].(string); ok && content != "" {
			h.Write([]byte{0})
			h.Write([]byte(content))
		}
	}
	h.Write([]byte(fmt.Sprintf("\x00mt=%d", maxTokens)))
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

func qoderTruncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// parseQoderSSE unwraps Qoder's {statusCodeValue, body} envelope. The inner body
// is an OpenAI streaming chunk; we parse its choices[0].delta into the callback
// (text + tool calls), accumulating tool-call args by index and flushing at end.
func parseQoderSSE(r io.Reader, cb *KiroStreamCallback) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	type toolAcc struct {
		id, name string
		args     strings.Builder
	}
	tools := map[int]*toolAcc{}
	var order []int
	var stopReason string
	var inTok, outTok int

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var env struct {
			StatusCodeValue int    `json:"statusCodeValue"`
			Body            string `json:"body"`
		}
		if err := json.Unmarshal([]byte(data), &env); err != nil {
			continue
		}
		if env.StatusCodeValue != 0 && env.StatusCodeValue != 200 {
			return fmt.Errorf("qoder upstream status %d: %s", env.StatusCodeValue, qoderTruncate(env.Body, 200))
		}
		inner := strings.TrimSpace(env.Body)
		if inner == "" || inner == "[DONE]" {
			continue
		}
		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(inner), &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Reasoning != "" && cb.OnText != nil {
				cb.OnText(ch.Delta.Reasoning, true)
			}
			if ch.Delta.Content != "" && cb.OnText != nil {
				cb.OnText(ch.Delta.Content, false)
			}
			for _, tc := range ch.Delta.ToolCalls {
				ta := tools[tc.Index]
				if ta == nil {
					ta = &toolAcc{}
					tools[tc.Index] = ta
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					ta.id = tc.ID
				}
				if tc.Function.Name != "" {
					ta.name = tc.Function.Name
				}
				ta.args.WriteString(tc.Function.Arguments)
			}
			if ch.FinishReason != "" {
				stopReason = mapOpenAIFinishReason(ch.FinishReason)
			}
		}
		if chunk.Usage != nil {
			inTok = chunk.Usage.PromptTokens
			outTok = chunk.Usage.CompletionTokens
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// Flush accumulated tool calls.
	for _, idx := range order {
		ta := tools[idx]
		if ta == nil || ta.name == "" || cb.OnToolUse == nil {
			continue
		}
		var input map[string]interface{}
		argStr := strings.TrimSpace(ta.args.String())
		if argStr == "" {
			argStr = "{}"
		}
		_ = json.Unmarshal([]byte(argStr), &input)
		if input == nil {
			input = map[string]interface{}{}
		}
		cb.OnToolUse(KiroToolUse{ToolUseID: ta.id, Name: ta.name, Input: input})
	}
	if cb.OnComplete != nil && (inTok > 0 || outTok > 0) {
		cb.OnComplete(inTok, outTok)
	}
	if stopReason != "" && cb.OnStopReason != nil {
		cb.OnStopReason(stopReason)
	}
	return nil
}
