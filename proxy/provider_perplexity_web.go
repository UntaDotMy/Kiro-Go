package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
	"regexp"
	"strings"
)

// perplexityWebProvider serves Perplexity via the perplexity.ai web backend
// (subscription session-cookie auth), ported from 9router's
// open-sse/executors/perplexity-web.js. The credential is the
// `__Secure-next-auth.session-token` cookie value (stored in Account.APIKey). The
// provider flattens the conversation into Perplexity's query envelope, posts to the
// SSE endpoint with browser-spoof headers + cookie, and streams the markdown blocks
// (with search-step "thinking" lines) into the shared callback. No token refresh —
// the cookie is the durable credential.
type perplexityWebProvider struct{}

func init() {
	RegisterProvider(perplexityWebProvider{})
}

const perplexityWebURL = "https://www.perplexity.ai/rest/sse/perplexity_ask"
const perplexityWebUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
const perplexityAPIVersion = "2.18"

type pplxModelInfo struct {
	mode      string
	modelPref string
}

// pplxModelMap maps public model ids to Perplexity (mode, model_preference).
var pplxModelMap = map[string]pplxModelInfo{
	"pplx-auto":     {"concise", "pplx_pro"},
	"pplx-sonar":    {"copilot", "experimental"},
	"pplx-gpt":      {"copilot", "gpt54"},
	"pplx-gemini":   {"copilot", "gemini31pro_high"},
	"pplx-sonnet":   {"copilot", "claude46sonnet"},
	"pplx-opus":     {"copilot", "claude46opus"},
	"pplx-nemotron": {"copilot", "nv_nemotron_3_super"},
}

var (
	pplxCitationRe = regexp.MustCompile(`\[\d+\]`)
	pplxXMLDeclRe  = regexp.MustCompile(`<\?xml[^?]*\?>`)
)

func (perplexityWebProvider) Name() string { return "perplexity-web" }

func (perplexityWebProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	return TokenSet{AccessToken: acct.AccessToken, ExpiresAt: 0}, nil
}

func (perplexityWebProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	out := make([]ModelInfo, 0, len(pplxModelMap))
	for id := range pplxModelMap {
		out = append(out, ModelInfo{ModelId: id})
	}
	return out, nil
}

func (perplexityWebProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	if ctx == nil {
		ctx = context.Background()
	}
	mi, ok := pplxModelMap[strings.TrimSpace(nr.Model)]
	if !ok {
		mi = pplxModelMap["pplx-auto"]
	}
	query := perplexityBuildQuery(nr)
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("perplexity-web: empty query after processing")
	}

	payload := map[string]interface{}{
		"query_str": query,
		"params": map[string]interface{}{
			"query_str":            query,
			"search_focus":         "internet",
			"mode":                 mi.mode,
			"model_preference":     mi.modelPref,
			"sources":              []string{"web"},
			"attachments":          []interface{}{},
			"frontend_uuid":        grokUUID(),
			"frontend_context_uuid": grokUUID(),
			"version":              perplexityAPIVersion,
			"language":             "en-US",
			"timezone":             "UTC",
			"is_incognito":         true,
			"use_schematized_api":  true,
		},
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", perplexityWebURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Origin", "https://www.perplexity.ai")
	req.Header.Set("Referer", "https://www.perplexity.ai/")
	req.Header.Set("User-Agent", perplexityWebUserAgent)
	req.Header.Set("X-App-ApiClient", "default")
	req.Header.Set("X-App-ApiVersion", perplexityAPIVersion)

	token := strings.TrimSpace(firstNonEmpty(acct.APIKey, acct.AccessToken))
	// Accept either a bare token or a full cookie string the operator pasted.
	if strings.Contains(token, "=") {
		req.Header.Set("Cookie", token)
	} else {
		req.Header.Set("Cookie", "__Secure-next-auth.session-token="+token)
	}

	resp, err := GetClientForProxy(ResolveAccountProxyURL(acct)).Do(req)
	if err != nil {
		return classifyStreamError(err)
	}
	if resp.StatusCode == 429 {
		retryAfter := parseRetryAfter(resp.Header)
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return &QuotaError{Endpoints: []string{"perplexity-web"}, RetryAfter: retryAfter}
	}
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return fmt.Errorf("perplexity-web auth failed (HTTP %d) — session cookie may be expired", resp.StatusCode)
		}
		return fmt.Errorf("HTTP %d from perplexity-web: %s", resp.StatusCode, string(errBody))
	}

	streamErr := func() error {
		defer resp.Body.Close()
		r := newIdleTimeoutReader(resp.Body, streamIdleTimeout, func() {})
		return parsePerplexityWebSSE(r, cb)
	}()
	return classifyStreamError(streamErr)
}

// perplexityBuildQuery flattens system/history/current into Perplexity's query
// envelope (a JSON object with instructions/history/query). Mirrors 9router's
// buildQuery for the first turn (no follow-up uuid tracking here).
func perplexityBuildQuery(nr *NormalizedRequest) string {
	var systemMsg strings.Builder
	type rt struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var history []rt
	add := func(role, text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		if role == "developer" {
			role = "system"
		}
		if role == "system" {
			systemMsg.WriteString(text)
			systemMsg.WriteString("\n")
			return
		}
		if role == "user" || role == "assistant" {
			history = append(history, rt{role, text})
		}
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

	current := ""
	if n := len(history); n > 0 && history[n-1].Role == "user" {
		current = history[n-1].Content
		history = history[:n-1]
	}

	obj := map[string]interface{}{}
	var instr []string
	if s := strings.TrimSpace(systemMsg.String()); s != "" {
		instr = append(instr, s)
	}
	instr = append(instr, "You have built-in web search. Answer questions directly using search results.")
	obj["instructions"] = instr
	if len(history) > 0 {
		obj["history"] = history
	}
	obj["query"] = current
	b, _ := json.Marshal(obj)
	s := string(b)
	if len(s) > 96000 {
		s = s[len(s)-96000:]
	}
	return s
}

// parsePerplexityWebSSE reads Perplexity's SSE stream and drives cb. Markdown
// blocks carry the answer (emitted as deltas vs the running length); plan/search
// steps become "thinking" text; error fields are surfaced as a terminal error.
func parsePerplexityWebSSE(r io.Reader, cb *KiroStreamCallback) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var dataLines []string
	seenLen := 0
	flush := func() (map[string]interface{}, bool) {
		if len(dataLines) == 0 {
			return nil, false
		}
		payload := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = dataLines[:0]
		if payload == "" || payload == "[DONE]" {
			return nil, false
		}
		var obj map[string]interface{}
		if json.Unmarshal([]byte(payload), &obj) != nil {
			return nil, false
		}
		return obj, true
	}

	handle := func(ev map[string]interface{}) error {
		if ec, ok := ev["error_message"].(string); ok && ec != "" {
			return fmt.Errorf("perplexity-web error: %s", ec)
		}
		blocks, _ := ev["blocks"].([]interface{})
		for _, b := range blocks {
			block, ok := b.(map[string]interface{})
			if !ok {
				continue
			}
			usage, _ := block["intended_usage"].(string)
			if !strings.Contains(usage, "markdown") {
				continue
			}
			mb, ok := block["markdown_block"].(map[string]interface{})
			if !ok {
				continue
			}
			chunksRaw, _ := mb["chunks"].([]interface{})
			var sb strings.Builder
			for _, c := range chunksRaw {
				if s, ok := c.(string); ok {
					sb.WriteString(s)
				}
			}
			full := sb.String()
			if len(full) > seenLen {
				delta := perplexityClean(full[seenLen:])
				seenLen = len(full)
				if delta != "" && cb.OnText != nil {
					cb.OnText(delta, false)
				}
			}
		}
		return nil
	}

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			ev, ok := flush()
			if ok {
				if err := handle(ev); err != nil {
					return err
				}
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		if line == "event: end_of_stream" {
			break
		}
	}
	if ev, ok := flush(); ok {
		if err := handle(ev); err != nil {
			return err
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

// perplexityClean strips citation markers and XML declarations from answer text,
// mirroring 9router's cleanResponse (minus the aggressive whitespace collapse, so
// streamed deltas keep their spacing).
func perplexityClean(text string) string {
	t := pplxXMLDeclRe.ReplaceAllString(text, "")
	t = pplxCitationRe.ReplaceAllString(t, "")
	return t
}
