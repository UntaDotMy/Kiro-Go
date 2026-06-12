package auth

import (
	"context"
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

// Gemini CLI (Google Cloud Code Assist) OAuth2, ported from 9router's
// src/lib/oauth/services/gemini.js + providers.js (the `gemini-cli` handler) and
// GEMINI_CONFIG. Standard Google OAuth2 authorization-code (NO PKCE) against a
// loopback callback, followed by a loadCodeAssist call to discover the GCP project
// id. After auth, inference goes to cloudcode-pa.googleapis.com via the Cloud Code
// Assist envelope (see proxy/provider_gemini_cli.go); this package owns login +
// the Google token refresh.
const (
	geminiCLIClientID     = "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com"
	geminiCLIClientSecret = "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"
	googleAuthorizeURL    = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL        = "https://oauth2.googleapis.com/token"
	googleUserInfoURL     = "https://www.googleapis.com/oauth2/v1/userinfo"
	geminiCLILoopbackPort = 8123
	geminiCLIRedirectURI  = "http://127.0.0.1:8123/callback"
	geminiCLIScopes       = "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile"
	geminiCLILoginTimeout = 5 * time.Minute

	loadCodeAssistURL = "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
)

// GoogleTokens is a completed Google OAuth2 exchange/refresh, plus the discovered
// GCP project id used by Cloud Code Assist.
type GoogleTokens struct {
	AccessToken  string
	RefreshToken string
	Email        string
	ProjectID    string
	ExpiresIn    int
}

// GeminiCLISession is an in-flight loopback login.
type GeminiCLISession struct {
	ID        string
	State     string
	AuthURL   string
	ExpiresAt time.Time

	mu           sync.Mutex
	status       string
	tokens       *GoogleTokens
	errMsg       string
	server       *http.Server
	listener     net.Listener
	shutdownOnce sync.Once
}

var (
	geminiCLISessions   = make(map[string]*GeminiCLISession)
	geminiCLISessionsMu sync.RWMutex
)

// StartGeminiCLILogin binds the loopback callback, builds the Google authorize URL,
// and returns the session.
func StartGeminiCLILogin() (*GeminiCLISession, error) {
	state := generateCodeVerifier()
	params := url.Values{}
	params.Set("client_id", geminiCLIClientID)
	params.Set("redirect_uri", geminiCLIRedirectURI)
	params.Set("response_type", "code")
	params.Set("scope", geminiCLIScopes)
	params.Set("access_type", "offline")
	params.Set("prompt", "consent")
	params.Set("state", state)
	authURL := googleAuthorizeURL + "?" + params.Encode()

	sess := &GeminiCLISession{
		ID:        GenerateAccountID(),
		State:     state,
		AuthURL:   authURL,
		ExpiresAt: time.Now().Add(geminiCLILoginTimeout),
		status:    "pending",
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", geminiCLILoopbackPort))
	if err != nil {
		return nil, fmt.Errorf("cannot bind Gemini CLI callback port %d: %w", geminiCLILoopbackPort, err)
	}
	sess.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errStr := q.Get("error"); errStr != "" {
			sess.fail(firstNonEmptyStr(q.Get("error_description"), errStr))
			writeCodexCallbackPage(w, false, "Authorization was denied. You can close this tab.")
			return
		}
		code := q.Get("code")
		if code == "" || q.Get("state") != sess.State {
			sess.fail("missing code or state mismatch")
			writeCodexCallbackPage(w, false, "Login failed (state mismatch). You can close this tab.")
			return
		}
		tokens, exErr := exchangeGoogleCode(r.Context(), code, geminiCLIRedirectURI)
		if exErr != nil {
			sess.fail(exErr.Error())
			writeCodexCallbackPage(w, false, "Token exchange failed. You can close this tab.")
			return
		}
		// Discover the GCP project id via loadCodeAssist (best-effort).
		tokens.ProjectID = discoverCodeAssistProject(r.Context(), tokens.AccessToken)
		sess.complete(tokens)
		writeCodexCallbackPage(w, true, "Gemini CLI connected. You can close this tab and return to Kiro-Go.")
	})

	sess.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = sess.server.Serve(ln) }()

	geminiCLISessionsMu.Lock()
	geminiCLISessions[sess.ID] = sess
	geminiCLISessionsMu.Unlock()

	go func() {
		time.Sleep(geminiCLILoginTimeout + 30*time.Second)
		sess.shutdown()
		geminiCLISessionsMu.Lock()
		delete(geminiCLISessions, sess.ID)
		geminiCLISessionsMu.Unlock()
	}()

	return sess, nil
}

// PollGeminiCLILogin returns the current login status.
func PollGeminiCLILogin(sessionID string) (status string, tokens *GoogleTokens, errMsg string, found bool) {
	geminiCLISessionsMu.RLock()
	sess, ok := geminiCLISessions[sessionID]
	geminiCLISessionsMu.RUnlock()
	if !ok {
		return "", nil, "", false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.status, sess.tokens, sess.errMsg, true
}

func (s *GeminiCLISession) complete(t *GoogleTokens) {
	s.mu.Lock()
	s.status = "completed"
	s.tokens = t
	s.mu.Unlock()
	s.shutdown()
}

func (s *GeminiCLISession) fail(msg string) {
	s.mu.Lock()
	s.status = "error"
	s.errMsg = msg
	s.mu.Unlock()
	s.shutdown()
}

func (s *GeminiCLISession) shutdown() {
	s.shutdownOnce.Do(func() {
		if s.server != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = s.server.Shutdown(ctx)
		}
	})
}

// exchangeGoogleCode swaps an authorization code for Google tokens + the userinfo
// email. Shared by gemini-cli and antigravity (both standard Google OAuth2).
func exchangeGoogleCode(ctx context.Context, code, redirectURI string) (*GoogleTokens, error) {
	return exchangeGoogleCodeWithClient(ctx, code, redirectURI, geminiCLIClientID, geminiCLIClientSecret)
}

// exchangeGoogleCodeWithClient is the parameterized form used when the OAuth client
// differs (antigravity uses its own clientId/secret).
func exchangeGoogleCodeWithClient(ctx context.Context, code, redirectURI, clientID, clientSecret string) (*GoogleTokens, error) {
	form := url.Values{}
	form.Set("code", code)
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", "authorization_code")

	t, err := googleTokenRequest(ctx, form)
	if err != nil {
		return nil, err
	}
	t.Email = fetchGoogleEmail(ctx, t.AccessToken)
	return t, nil
}

// RefreshGoogleToken renews a Google access token via the refresh grant. Shared by
// gemini-cli and antigravity.
func RefreshGoogleToken(ctx context.Context, refreshToken string) (*GoogleTokens, error) {
	return RefreshGoogleTokenWithClient(ctx, refreshToken, geminiCLIClientID, geminiCLIClientSecret)
}

// RefreshGoogleTokenWithClient is the parameterized refresh used by antigravity.
func RefreshGoogleTokenWithClient(ctx context.Context, refreshToken, clientID, clientSecret string) (*GoogleTokens, error) {
	form := url.Values{}
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("grant_type", "refresh_token")

	t, err := googleTokenRequest(ctx, form)
	if err != nil {
		return nil, err
	}
	if t.RefreshToken == "" {
		t.RefreshToken = refreshToken
	}
	return t, nil
}

func googleTokenRequest(ctx context.Context, form url.Values) (*GoogleTokens, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("google token request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("google token request failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("google token parse failed: %w", err)
	}
	if raw.AccessToken == "" {
		return nil, fmt.Errorf("google token response missing access_token")
	}
	return &GoogleTokens{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		ExpiresIn:    raw.ExpiresIn,
	}, nil
}

func fetchGoogleEmail(ctx context.Context, accessToken string) string {
	req, err := http.NewRequestWithContext(ctx, "GET", googleUserInfoURL+"?alt=json", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := httpClient().Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return ""
	}
	var u struct {
		Email string `json:"email"`
	}
	_ = json.Unmarshal(body, &u)
	return u.Email
}

// discoverCodeAssistProject calls loadCodeAssist to resolve the GCP project id used
// by Cloud Code Assist requests. Best-effort: returns "" on any failure (the
// provider then omits the project field, which Google tolerates for some tiers).
func discoverCodeAssistProject(ctx context.Context, accessToken string) string {
	payload := map[string]interface{}{
		"metadata": map[string]interface{}{"ideType": 9, "platform": 3, "pluginType": 2},
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", loadCodeAssistURL, strings.NewReader(string(b)))
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "google-api-nodejs-client/9.15.1")
	resp, err := httpClient().Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return ""
	}
	var d struct {
		CloudaicompanionProject interface{} `json:"cloudaicompanionProject"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return ""
	}
	switch v := d.CloudaicompanionProject.(type) {
	case string:
		return v
	case map[string]interface{}:
		if id, ok := v["id"].(string); ok {
			return id
		}
	}
	return ""
}
