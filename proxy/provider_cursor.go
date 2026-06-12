package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
	"strings"
)

// cursorProvider serves Cursor IDE accounts via the api2.cursor.sh Connect-RPC
// endpoint, ported from 9router's open-sse/executors/cursor.js. Cursor speaks
// protobuf over Connect-RPC (application/connect+proto), authenticated with a Bearer
// access token + the x-cursor-checksum cipher (see cursor_checksum.go) and a machine
// id. The token + machine id are imported from the Cursor IDE (no browser OAuth);
// they're stored in AccessToken and MachineId. Inference responses are a stream of
// Connect-RPC frames carrying protobuf StreamUnifiedChatResponse messages, which we
// decode into text/thinking/tool-call events on the shared callback.
type cursorProvider struct{}

func init() {
	RegisterProvider(cursorProvider{})
}

const cursorChatURL = "https://api2.cursor.sh/aiserver.v1.ChatService/StreamUnifiedChatWithTools"

func (cursorProvider) Name() string { return "cursor" }

// RefreshToken is a no-op: Cursor has no public refresh endpoint, so the imported
// token is used until it lapses (then the operator re-imports).
func (cursorProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	return TokenSet{AccessToken: acct.AccessToken, ExpiresAt: 0}, nil
}

func (cursorProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	ids := []string{"gpt-5", "claude-4.5-sonnet", "claude-4.5-haiku", "gemini-2.5-pro", "o3", "auto"}
	out := make([]ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, ModelInfo{ModelId: id})
	}
	return out, nil
}

func (cursorProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(acct.MachineId) == "" {
		return fmt.Errorf("cursor: machine id is required (import it from the Cursor IDE)")
	}
	model := strings.TrimSpace(nr.Model)
	msgs, tools := cursorNormalizeRequest(nr)
	if len(msgs) == 0 {
		return fmt.Errorf("cursor: no messages to send")
	}

	payload := cBuildChatRequest(msgs, model, tools, strings.TrimSpace(nr.Effort))
	body := cWrapConnectFrame(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", cursorChatURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range buildCursorHeaders(acct.AccessToken, acct.MachineId, true) {
		req.Header.Set(k, v)
	}

	resp, err := GetClientForProxy(ResolveAccountProxyURL(acct)).Do(req)
	if err != nil {
		return classifyStreamError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		retryAfter := parseRetryAfter(resp.Header)
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return &QuotaError{Endpoints: []string{"cursor"}, RetryAfter: retryAfter}
	}
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("HTTP %d from cursor: %s", resp.StatusCode, string(errBody))
	}

	// Connect-RPC responses are a sequence of length-prefixed frames. Cursor sends
	// the whole stream as the body; we read it fully then walk the frames (the JS
	// implementation does the same — frames are small and the stream is bounded).
	raw, err := io.ReadAll(newIdleTimeoutReader(resp.Body, streamIdleTimeout, func() {}))
	if err != nil {
		return classifyStreamError(err)
	}
	return cursorWalkFrames(raw, cb)
}

// cursorWalkFrames iterates Connect-RPC frames, decompresses gzip frames, decodes
// each protobuf payload, and drives cb. A trailing JSON error frame after content
// is ignored; an error frame before any content is surfaced.
func cursorWalkFrames(buf []byte, cb *KiroStreamCallback) error {
	offset := 0
	sawContent := false
	toolSeq := 0
	for offset+5 <= len(buf) {
		flags := buf[offset]
		length := int(binary.BigEndian.Uint32(buf[offset+1 : offset+5]))
		if offset+5+length > len(buf) {
			break
		}
		payload := buf[offset+5 : offset+5+length]
		offset += 5 + length

		if flags&0x01 != 0 { // gzip
			if dec, derr := cursorGunzip(payload); derr == nil {
				payload = dec
			} else {
				continue
			}
		}
		if len(payload) == 0 {
			continue
		}
		// JSON error frame guard.
		if payload[0] == '{' {
			text := string(payload)
			if strings.Contains(text, `"error"`) {
				if sawContent {
					break
				}
				return fmt.Errorf("cursor error: %s", cursorTrimErr(text))
			}
		}

		ex := cExtractFromResponse(payload)
		if ex.thinking != "" && cb.OnText != nil {
			cb.OnText(ex.thinking, true)
			sawContent = true
		}
		if ex.text != "" && cb.OnText != nil {
			cb.OnText(ex.text, false)
			sawContent = true
		}
		if ex.toolName != "" && ex.toolID != "" && cb.OnToolUse != nil {
			args := map[string]interface{}{}
			if strings.TrimSpace(ex.toolArgs) != "" {
				_ = json.Unmarshal([]byte(ex.toolArgs), &args)
			}
			cb.OnToolUse(KiroToolUse{ToolUseID: ex.toolID, Name: ex.toolName, Input: args})
			toolSeq++
			sawContent = true
		}
	}
	if cb.OnStopReason != nil {
		if toolSeq > 0 {
			cb.OnStopReason("tool_use")
		} else {
			cb.OnStopReason("end_turn")
		}
	}
	return nil
}

// cursorNormalizeRequest flattens the normalized request into Cursor's message +
// tool shape. Tool-call/result history is reduced to text turns (this port focuses
// on chat + tool definitions; full agentic tool-result round-tripping is a future
// enhancement).
func cursorNormalizeRequest(nr *NormalizedRequest) ([]cursorMsg, []cursorTool) {
	var msgs []cursorMsg
	var tools []cursorTool

	switch {
	case nr.OpenAI != nil:
		for _, m := range nr.OpenAI.Messages {
			text := extractOpenAIMessageText(m.Content)
			if strings.TrimSpace(text) == "" {
				continue
			}
			msgs = append(msgs, cursorMsg{Role: m.Role, Content: text})
		}
		for _, t := range nr.OpenAI.Tools {
			tools = append(tools, cursorTool{Name: t.Function.Name, Description: t.Function.Description, Schema: t.Function.Parameters})
		}
	case nr.Claude != nil:
		if sys := extractClaudeSystemString(nr.Claude.System); sys != "" {
			msgs = append(msgs, cursorMsg{Role: "user", Content: sys})
		}
		for _, m := range nr.Claude.Messages {
			text := claudeMessagePlainText(m.Content)
			if strings.TrimSpace(text) == "" {
				continue
			}
			msgs = append(msgs, cursorMsg{Role: m.Role, Content: text})
		}
		for _, t := range nr.Claude.Tools {
			if strings.TrimSpace(t.Type) != "" {
				continue
			}
			tools = append(tools, cursorTool{Name: t.Name, Description: t.Description, Schema: t.InputSchema})
		}
	}
	return msgs, tools
}

func cursorGunzip(data []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

func cursorTrimErr(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}
