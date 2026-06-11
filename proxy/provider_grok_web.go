package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"math/big"
	"net/http"
	"strings"
)

// grokWebProvider serves Grok via the grok.com web backend (subscription cookie
// auth), ported from 9router's open-sse/executors/grok-web.js. The credential is
// the `sso` cookie value (stored in Account.APIKey; a leading "sso=" is stripped).
// The provider flattens the conversation into Grok's single-message web payload,
// posts with browser-spoof headers, and parses the NDJSON stream into the shared
// callback. There is no token to refresh — the cookie is the durable credential.
type grokWebProvider struct{}

func init() {
	RegisterProvider(grokWebProvider{})
}

const grokWebChatURL = "https://grok.com/rest/app-chat/conversations/new"
const grokWebUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"

type grokModelInfo struct {
	grokModel string
	modelMode string
	thinking  bool
}

// grokModelMap maps public model ids to Grok web model+mode. Mirrors 9router's MODEL_MAP.
var grokModelMap = map[string]grokModelInfo{
	"grok-3":          {"grok-3", "MODEL_MODE_GROK_3", false},
	"grok-3-mini":     {"grok-3", "MODEL_MODE_GROK_3_MINI_THINKING", true},
	"grok-3-thinking": {"grok-3", "MODEL_MODE_GROK_3_THINKING", true},
	"grok-4":          {"grok-4", "MODEL_MODE_GROK_4", false},
	"grok-4-mini":     {"grok-4-mini", "MODEL_MODE_GROK_4_MINI_THINKING", true},
	"grok-4-thinking": {"grok-4", "MODEL_MODE_GROK_4_THINKING", true},
	"grok-4-heavy":    {"grok-4", "MODEL_MODE_HEAVY", true},
	"grok-4.1-fast":   {"grok-4-1-thinking-1129", "MODEL_MODE_FAST", false},
	"grok-4.1-expert": {"grok-4-1-thinking-1129", "MODEL_MODE_EXPERT", true},
	"grok-4.2":        {"grok-420", "MODEL_MODE_GROK_420", false},
}

func (grokWebProvider) Name() string { return "grok-web" }

// RefreshToken is a no-op: the sso cookie is a static credential.
func (grokWebProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	return TokenSet{AccessToken: acct.AccessToken, ExpiresAt: 0}, nil
}

func (grokWebProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	out := make([]ModelInfo, 0, len(grokModelMap))
	for id := range grokModelMap {
		out = append(out, ModelInfo{ModelId: id})
	}
	return out, nil
}

func (grokWebProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	if ctx == nil {
		ctx = context.Background()
	}
	mi, ok := grokModelMap[strings.TrimSpace(nr.Model)]
	if !ok {
		mi = grokModelMap["grok-4.1-fast"]
	}
	message := grokFlattenMessages(nr)
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("grok-web: empty query after processing")
	}

	payload := map[string]interface{}{
		"temporary": true, "modelName": mi.grokModel, "modelMode": mi.modelMode, "message": message,
		"fileAttachments": []interface{}{}, "imageAttachments": []interface{}{},
		"disableSearch": false, "enableImageGeneration": false, "returnImageBytes": false,
		"returnRawGrokInXaiRequest": false, "enableImageStreaming": false, "imageGenerationCount": 0,
		"forceConcise": false, "toolOverrides": map[string]interface{}{}, "enableSideBySide": true,
		"sendFinalMetadata": true, "isReasoning": false, "disableTextFollowUps": false, "disableMemory": true,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", grokWebChatURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	traceID := grokRandHex(16)
	spanID := grokRandHex(8)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://grok.com")
	req.Header.Set("Referer", "https://grok.com/")
	req.Header.Set("Sec-Ch-Ua", `"Google Chrome";v="136", "Chromium";v="136", "Not(A:Brand";v="24"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", `"macOS"`)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("User-Agent", grokWebUserAgent)
	req.Header.Set("x-statsig-id", grokStatsigID())
	req.Header.Set("x-xai-request-id", grokUUID())
	req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-00")

	token := strings.TrimSpace(firstNonEmpty(acct.APIKey, acct.AccessToken))
	token = strings.TrimPrefix(token, "sso=")
	req.Header.Set("Cookie", "sso="+token)

	resp, err := GetClientForProxy(ResolveAccountProxyURL(acct)).Do(req)
	if err != nil {
		return classifyStreamError(err)
	}
	if resp.StatusCode == 429 {
		retryAfter := parseRetryAfter(resp.Header)
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return &QuotaError{Endpoints: []string{"grok-web"}, RetryAfter: retryAfter}
	}
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return fmt.Errorf("grok-web auth failed (HTTP %d) — sso cookie may be expired", resp.StatusCode)
		}
		return fmt.Errorf("HTTP %d from grok-web: %s", resp.StatusCode, string(errBody))
	}

	streamErr := func() error {
		defer resp.Body.Close()
		r := newIdleTimeoutReader(resp.Body, streamIdleTimeout, func() {})
		return parseGrokWebNDJSON(r, mi.thinking, cb)
	}()
	return classifyStreamError(streamErr)
}

// grokFlattenMessages flattens the conversation into Grok's single-message format:
// every prior turn is prefixed with its role, the final user turn is bare. Mirrors
// 9router's parseOpenAIMessages.
func grokFlattenMessages(nr *NormalizedRequest) string {
	type rt struct{ role, text string }
	var extracted []rt
	add := func(role, text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		if role == "developer" {
			role = "system"
		}
		extracted = append(extracted, rt{role, text})
	}

	switch {
	case nr.OpenAI != nil:
		for _, m := range nr.OpenAI.Messages {
			add(m.Role, extractOpenAIMessageText(m.Content))
		}
	case nr.Claude != nil:
		if sys := extractClaudeSystemString(nr.Claude.System); sys != "" {
			add("system", sys)
		}
		for _, m := range nr.Claude.Messages {
			add(m.Role, claudeMessagePlainText(m.Content))
		}
	}

	lastUser := -1
	for i := len(extracted) - 1; i >= 0; i-- {
		if extracted[i].role == "user" {
			lastUser = i
			break
		}
	}
	var parts []string
	for i, e := range extracted {
		if i == lastUser {
			parts = append(parts, e.text)
		} else {
			parts = append(parts, e.role+": "+e.text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// claudeMessagePlainText extracts concatenated text from a Claude message content
// (string or block array), ignoring tool blocks — the web backend is text-only.
func claudeMessagePlainText(content interface{}) string {
	if s, ok := content.(string); ok {
		return s
	}
	blocks, ok := content.([]interface{})
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		if blk, ok := b.(map[string]interface{}); ok {
			if t, ok := blk["text"].(string); ok {
				sb.WriteString(t)
			}
		}
	}
	return sb.String()
}

// parseGrokWebNDJSON reads Grok's NDJSON stream and drives cb. Each line is a JSON
// event; resp.token carries a text delta, resp.modelResponse.message the full text
// (used for thinking models' reasoning), and event.error a terminal error.
func parseGrokWebNDJSON(r io.Reader, thinking bool, cb *KiroStreamCallback) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	thinkOpen := thinking
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev struct {
			Error *struct {
				Message string `json:"message"`
				Code    int    `json:"code"`
			} `json:"error"`
			Result struct {
				Response *struct {
					Token         string `json:"token"`
					ResponseID    string `json:"responseId"`
					ModelResponse *struct {
						Message string `json:"message"`
					} `json:"modelResponse"`
				} `json:"response"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Error != nil {
			return fmt.Errorf("grok-web error: %s", firstNonEmpty(ev.Error.Message, fmt.Sprintf("code %d", ev.Error.Code)))
		}
		resp := ev.Result.Response
		if resp == nil {
			continue
		}
		if resp.ModelResponse != nil {
			if thinkOpen && thinking && resp.ModelResponse.Message != "" && cb.OnText != nil {
				cb.OnText(resp.ModelResponse.Message, true)
				thinkOpen = false
			}
			continue
		}
		if resp.Token != "" && cb.OnText != nil {
			cb.OnText(resp.Token, false)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if cb.OnStopReason != nil {
		cb.OnStopReason("end_turn")
	}
	return nil
}

// --- header entropy helpers (mirror grok-web.js) ---

func grokRandHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func grokUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func grokStatsigID() string {
	letters := "abcdefghijklmnopqrstuvwxyz"
	pick := func(n int) string {
		var sb strings.Builder
		for i := 0; i < n; i++ {
			idx, _ := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
			sb.WriteByte(letters[idx.Int64()])
		}
		return sb.String()
	}
	msg := "e:TypeError: Cannot read properties of undefined (reading '" + pick(10) + "')"
	return base64.StdEncoding.EncodeToString([]byte(msg))
}
