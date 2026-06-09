package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Codex (OpenAI ChatGPT) OAuth — authorization-code + PKCE against
// auth.openai.com, ported from 9router (lib/oauth/constants/oauth.js CODEX_CONFIG
// + services/codex.js) and codex-lb (app/core/clients/oauth.py). The flow:
//
//  1. StartCodexLogin spins a local callback server on the fixed port 1455 (the
//     real Codex CLI uses this exact port; OpenAI's allow-listed redirect URI is
//     http://localhost:1455/auth/callback and MUST NOT change), generates a PKCE
//     pair + state, and returns the authorize URL.
//  2. The user opens the URL, authenticates, and OpenAI redirects to the local
//     callback with ?code=...&state=...
//  3. PollCodexLogin (or the callback itself) exchanges the code at
//     /oauth/token (application/x-www-form-urlencoded) for access/refresh/id
//     tokens, decodes the id_token to extract chatgpt-account-id + plan, and
//     returns a completed CodexTokens.
//
// Token refresh uses the SAME /oauth/token endpoint but a JSON body with
// grant_type=refresh_token (codex-lb refresh.py). The classic port bug is
// form-vs-JSON between exchange and refresh — both are implemented explicitly.
const (
	codexClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexAuthorizeURL = "https://auth.openai.com/oauth/authorize"
	codexTokenURL     = "https://auth.openai.com/oauth/token"
	codexScope        = "openid profile email offline_access"
	codexOriginator   = "codex_cli_rs"
	// codexCallbackPort is fixed: OpenAI dislikes redirect-URI changes and the
	// real Codex CLI binds 1455. The redirect URI is derived from it.
	codexCallbackPort = 1455
	codexRedirectURI  = "http://localhost:1455/auth/callback"
	codexLoginTimeout = 5 * time.Minute
)

// CodexTokens is the result of a completed Codex OAuth exchange/refresh.
type CodexTokens struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresIn    int    // seconds; OpenAI access tokens are short-lived
	AccountID    string // chatgpt-account-id (from id_token claims)
	PlanType     string // chatgpt plan (plus/pro/team/...)
	Email        string
}

// CodexSession is an in-flight browser login.
type CodexSession struct {
	ID           string
	State        string
	CodeVerifier string
	AuthURL      string
	ExpiresAt    time.Time

	mu           sync.Mutex
	status       string // "pending" | "completed" | "error"
	tokens       *CodexTokens
	errMsg       string
	server       *http.Server
	listener     net.Listener
	shutdownOnce sync.Once
}

var (
	codexSessions   = make(map[string]*CodexSession)
	codexSessionsMu sync.RWMutex
)

// StartCodexLogin begins a Codex OAuth browser flow: it binds the local callback
// server on port 1455, builds the PKCE authorize URL, and returns the session so
// the caller can surface the URL to the user and poll for completion.
func StartCodexLogin() (*CodexSession, error) {
	verifier := generateCodeVerifier()
	challenge := codexPKCEChallenge(verifier)
	state := generateCodeVerifier() // reuse the 32-byte url-safe random for state

	authURL := buildCodexAuthURL(state, challenge)

	sess := &CodexSession{
		ID:           GenerateAccountID(),
		State:        state,
		CodeVerifier: verifier,
		AuthURL:      authURL,
		ExpiresAt:    time.Now().Add(codexLoginTimeout),
		status:       "pending",
	}

	// Bind the fixed callback port. If it's already in use (another login in
	// flight, or the real Codex CLI running), surface a clear error.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", codexCallbackPort))
	if err != nil {
		return nil, fmt.Errorf("cannot bind Codex callback port %d (is a Codex login or the Codex CLI already running?): %w", codexCallbackPort, err)
	}
	sess.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errStr := q.Get("error"); errStr != "" {
			sess.fail(firstNonEmptyStr(q.Get("error_description"), errStr))
			writeCodexCallbackPage(w, false, "Authorization was denied. You can close this tab.")
			return
		}
		code := q.Get("code")
		if code == "" || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(sess.State)) != 1 {
			sess.fail("missing code or state mismatch")
			writeCodexCallbackPage(w, false, "Login failed (state mismatch). You can close this tab.")
			return
		}
		tokens, exErr := exchangeCodexCode(r.Context(), code, sess.CodeVerifier)
		if exErr != nil {
			sess.fail(exErr.Error())
			writeCodexCallbackPage(w, false, "Token exchange failed. You can close this tab.")
			return
		}
		sess.complete(tokens)
		writeCodexCallbackPage(w, true, "Codex connected. You can close this tab and return to Kiro-Go.")
	})

	sess.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
	}
	go func() { _ = sess.server.Serve(ln) }()

	codexSessionsMu.Lock()
	codexSessions[sess.ID] = sess
	codexSessionsMu.Unlock()

	// Auto-clean after the timeout regardless of outcome.
	go func() {
		time.Sleep(codexLoginTimeout + 30*time.Second)
		sess.shutdown()
		codexSessionsMu.Lock()
		delete(codexSessions, sess.ID)
		codexSessionsMu.Unlock()
	}()

	return sess, nil
}

// PollCodexLogin returns the current status of a login session: "pending",
// "completed" (with tokens), or "error" (with message).
func PollCodexLogin(sessionID string) (status string, tokens *CodexTokens, errMsg string, found bool) {
	codexSessionsMu.RLock()
	sess, ok := codexSessions[sessionID]
	codexSessionsMu.RUnlock()
	if !ok {
		return "", nil, "", false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.status, sess.tokens, sess.errMsg, true
}

func (s *CodexSession) complete(t *CodexTokens) {
	s.mu.Lock()
	s.status = "completed"
	s.tokens = t
	s.mu.Unlock()
	s.shutdown()
}

func (s *CodexSession) fail(msg string) {
	s.mu.Lock()
	s.status = "error"
	s.errMsg = msg
	s.mu.Unlock()
	s.shutdown()
}

func (s *CodexSession) shutdown() {
	// sync.Once: the callback handler (via complete/fail) and the auto-cleanup
	// goroutine both call shutdown() concurrently; without this they raced on
	// s.server (one goroutine could null it between another's nil-check and
	// Shutdown call). Once also makes the double-shutdown a clean no-op.
	s.shutdownOnce.Do(func() {
		if s.server != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = s.server.Shutdown(ctx)
		}
	})
}

// buildCodexAuthURL builds the authorize URL with the Codex CLI's exact extra
// params. Spaces in the scope are encoded as %20 (url.Values.Encode uses +,
// which OpenAI's authorize endpoint rejects), so we assemble manually.
func buildCodexAuthURL(state, challenge string) string {
	params := []struct{ k, v string }{
		{"response_type", "code"},
		{"client_id", codexClientID},
		{"redirect_uri", codexRedirectURI},
		{"scope", codexScope},
		{"code_challenge", challenge},
		{"code_challenge_method", "S256"},
		{"id_token_add_organizations", "true"},
		{"codex_cli_simplified_flow", "true"},
		{"originator", codexOriginator},
		{"state", state},
	}
	var sb strings.Builder
	sb.WriteString(codexAuthorizeURL)
	sb.WriteString("?")
	for i, p := range params {
		if i > 0 {
			sb.WriteString("&")
		}
		sb.WriteString(p.k)
		sb.WriteString("=")
		sb.WriteString(url.QueryEscape(p.v))
	}
	return sb.String()
}

// exchangeCodexCode swaps an authorization code for tokens. OpenAI's token
// endpoint expects application/x-www-form-urlencoded for the code exchange.
func exchangeCodexCode(ctx context.Context, code, verifier string) (*CodexTokens, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", codexClientID)
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("redirect_uri", codexRedirectURI)

	req, err := http.NewRequestWithContext(ctx, "POST", codexTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex token exchange request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("codex token exchange failed: %d %s", resp.StatusCode, string(body))
	}
	return parseCodexTokenResponse(body)
}

// RefreshCodexToken renews a Codex access token. OpenAI's token endpoint expects
// a JSON body for the refresh grant (NOT form-encoded — this asymmetry with the
// code exchange is intentional and matches codex-lb).
func RefreshCodexToken(refreshToken string) (*CodexTokens, error) {
	payload := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     codexClientID,
		"refresh_token": refreshToken,
		"scope":         codexScope,
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", codexTokenURL, strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex token refresh request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("codex token refresh failed: %d %s", resp.StatusCode, string(body))
	}
	tokens, err := parseCodexTokenResponse(body)
	if err != nil {
		return nil, err
	}
	// A refresh response may omit refresh_token (keep the old one) — the caller
	// handles that, but default it here too for safety.
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = refreshToken
	}
	return tokens, nil
}

// parseCodexTokenResponse parses an OpenAI token response and decodes the
// id_token to extract chatgpt-account-id + plan + email.
func parseCodexTokenResponse(body []byte) (*CodexTokens, error) {
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("codex token response parse failed: %w", err)
	}
	if raw.AccessToken == "" {
		return nil, fmt.Errorf("codex token response missing access_token")
	}
	t := &CodexTokens{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		IDToken:      raw.IDToken,
		ExpiresIn:    raw.ExpiresIn,
	}
	if raw.IDToken != "" {
		acct, plan, email := DecodeCodexIDToken(raw.IDToken)
		t.AccountID = acct
		t.PlanType = plan
		t.Email = email
	}
	return t, nil
}

// DecodeCodexIDToken decodes (without signature verification — we only trust the
// token because we just received it over TLS from OpenAI) the id_token JWT and
// extracts the chatgpt account id, plan type, and email. OpenAI nests these under
// the "https://api.openai.com/auth" claim; ChatGPT website tokens use top-level
// account_id/plan_type. Both are checked.
func DecodeCodexIDToken(idToken string) (accountID, planType, email string) {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return "", "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		// Try standard padding as a fallback.
		payload, err = base64.URLEncoding.DecodeString(padBase64(parts[1]))
		if err != nil {
			return "", "", ""
		}
	}
	var claims struct {
		Email string `json:"email"`
		Auth  *struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
			ChatGPTPlanType  string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
		// ChatGPT website token top-level fallbacks.
		AccountID string `json:"account_id"`
		PlanType  string `json:"plan_type"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", ""
	}
	email = claims.Email
	if claims.Auth != nil {
		accountID = claims.Auth.ChatGPTAccountID
		planType = claims.Auth.ChatGPTPlanType
	}
	if accountID == "" {
		accountID = claims.AccountID
	}
	if planType == "" {
		planType = claims.PlanType
	}
	return accountID, planType, email
}

// ImportCodexToken builds CodexTokens from a pasted raw access token / id_token
// (the "access_token" auth mode in 9router). If the pasted value is an id_token
// (JWT with the auth claim) we decode identity from it; otherwise the caller must
// supply the account id separately.
func ImportCodexToken(accessToken string) (*CodexTokens, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, fmt.Errorf("empty token")
	}
	t := &CodexTokens{AccessToken: accessToken}
	if strings.HasPrefix(accessToken, "eyJ") && strings.Count(accessToken, ".") >= 2 {
		t.IDToken = accessToken
		t.AccountID, t.PlanType, t.Email = DecodeCodexIDToken(accessToken)
	}
	return t, nil
}

func codexPKCEChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func padBase64(s string) string {
	s = strings.ReplaceAll(s, "-", "+")
	s = strings.ReplaceAll(s, "_", "/")
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	return s
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func writeCodexCallbackPage(w http.ResponseWriter, ok bool, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	color := "#16a34a"
	if !ok {
		color = "#dc2626"
	}
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Kiro-Go · Codex</title></head>`+
		`<body style="font-family:system-ui;background:#0f172a;color:#e2e8f0;display:flex;align-items:center;justify-content:center;height:100vh;margin:0">`+
		`<div style="text-align:center"><div style="font-size:48px;color:%s">%s</div><p style="font-size:16px;margin-top:12px">%s</p></div></body></html>`,
		color, map[bool]string{true: "✓", false: "✗"}[ok], msg)
}

// ensure crypto/rand is used (generateCodeVerifier in iam_sso.go already imports
// it, but keep a reference so this file is self-documenting about the source of
// entropy for PKCE/state).
var _ = rand.Read
