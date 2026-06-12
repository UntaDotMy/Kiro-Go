package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
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

// xAI (Grok) OAuth — authorization-code + PKCE against auth.x.ai, ported from
// 9router (constants/xai.js + the `xai` provider handler), which mirrors
// CLIProxyAPI. The flow binds a loopback callback on the fixed port 56121, opens
// the authorize URL, and exchanges the returned code at /oauth2/token. After auth,
// inference is plain OpenAI /v1/chat/completions at api.x.ai with a Bearer token,
// so the proxy reuses the generic OpenAI provider; this package owns login + refresh.
const (
	xaiClientID       = "b1a00492-073a-47ea-816f-4c329264a828"
	xaiIssuer         = "https://auth.x.ai"
	xaiAuthorizeURL   = xaiIssuer + "/oauth2/authorize"
	xaiTokenURL       = xaiIssuer + "/oauth2/token"
	xaiScope          = "openid profile email offline_access grok-cli:access api:access"
	xaiLoopbackPort   = 56121
	xaiRedirectURI    = "http://127.0.0.1:56121/callback"
	xaiUserAgent      = "grok-cli/kiro-go"
	xaiLoginTimeout   = 5 * time.Minute
	xaiPKCEVerifBytes = 96
)

// XaiTokens is a completed xAI OAuth exchange/refresh.
type XaiTokens struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresIn    int
	Email        string
}

// XaiSession is an in-flight browser login (loopback callback).
type XaiSession struct {
	ID           string
	State        string
	CodeVerifier string
	AuthURL      string
	ExpiresAt    time.Time

	mu           sync.Mutex
	status       string
	tokens       *XaiTokens
	errMsg       string
	server       *http.Server
	listener     net.Listener
	shutdownOnce sync.Once
}

var (
	xaiSessions   = make(map[string]*XaiSession)
	xaiSessionsMu sync.RWMutex
)

// StartXaiLogin binds the loopback callback on port 56121, builds the PKCE
// authorize URL, and returns the session.
func StartXaiLogin() (*XaiSession, error) {
	verifier := xaiCodeVerifier()
	challenge := claudePKCEChallenge(verifier) // base64url(SHA256(verifier)), shared helper
	state := generateCodeVerifier()

	authURL := buildXaiAuthURL(state, challenge)

	sess := &XaiSession{
		ID:           GenerateAccountID(),
		State:        state,
		CodeVerifier: verifier,
		AuthURL:      authURL,
		ExpiresAt:    time.Now().Add(xaiLoginTimeout),
		status:       "pending",
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", xaiLoopbackPort))
	if err != nil {
		return nil, fmt.Errorf("cannot bind xAI callback port %d (is a login or the grok CLI already running?): %w", xaiLoopbackPort, err)
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
		if code == "" || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(sess.State)) != 1 {
			sess.fail("missing code or state mismatch")
			writeCodexCallbackPage(w, false, "Login failed (state mismatch). You can close this tab.")
			return
		}
		tokens, exErr := exchangeXaiCode(r.Context(), code, sess.CodeVerifier)
		if exErr != nil {
			sess.fail(exErr.Error())
			writeCodexCallbackPage(w, false, "Token exchange failed. You can close this tab.")
			return
		}
		sess.complete(tokens)
		writeCodexCallbackPage(w, true, "xAI connected. You can close this tab and return to Kiro-Go.")
	})

	sess.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = sess.server.Serve(ln) }()

	xaiSessionsMu.Lock()
	xaiSessions[sess.ID] = sess
	xaiSessionsMu.Unlock()

	go func() {
		time.Sleep(xaiLoginTimeout + 30*time.Second)
		sess.shutdown()
		xaiSessionsMu.Lock()
		delete(xaiSessions, sess.ID)
		xaiSessionsMu.Unlock()
	}()

	return sess, nil
}

// PollXaiLogin returns the current login status.
func PollXaiLogin(sessionID string) (status string, tokens *XaiTokens, errMsg string, found bool) {
	xaiSessionsMu.RLock()
	sess, ok := xaiSessions[sessionID]
	xaiSessionsMu.RUnlock()
	if !ok {
		return "", nil, "", false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.status, sess.tokens, sess.errMsg, true
}

func (s *XaiSession) complete(t *XaiTokens) {
	s.mu.Lock()
	s.status = "completed"
	s.tokens = t
	s.mu.Unlock()
	s.shutdown()
}

func (s *XaiSession) fail(msg string) {
	s.mu.Lock()
	s.status = "error"
	s.errMsg = msg
	s.mu.Unlock()
	s.shutdown()
}

func (s *XaiSession) shutdown() {
	s.shutdownOnce.Do(func() {
		if s.server != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = s.server.Shutdown(ctx)
		}
	})
}

func buildXaiAuthURL(state, challenge string) string {
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	params := []struct{ k, v string }{
		{"response_type", "code"},
		{"client_id", xaiClientID},
		{"redirect_uri", xaiRedirectURI},
		{"scope", xaiScope},
		{"code_challenge", challenge},
		{"code_challenge_method", "S256"},
		{"state", state},
		{"nonce", hex.EncodeToString(nonce)},
		{"plan", "generic"},
		{"referrer", "cli-proxy-api"},
	}
	var sb strings.Builder
	sb.WriteString(xaiAuthorizeURL)
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

func exchangeXaiCode(ctx context.Context, code, verifier string) (*XaiTokens, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", xaiClientID)
	form.Set("code", code)
	form.Set("redirect_uri", xaiRedirectURI)
	form.Set("code_verifier", verifier)

	req, err := http.NewRequestWithContext(ctx, "POST", xaiTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", xaiUserAgent)

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("xai token exchange request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("xai token exchange failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	return parseXaiTokenResponse(body)
}

// RefreshXaiToken renews an access token via the refresh grant.
func RefreshXaiToken(ctx context.Context, refreshToken string) (*XaiTokens, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", xaiClientID)
	form.Set("refresh_token", refreshToken)

	req, err := http.NewRequestWithContext(ctx, "POST", xaiTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", xaiUserAgent)

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("xai token refresh request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("xai token refresh failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	t, err := parseXaiTokenResponse(body)
	if err != nil {
		return nil, err
	}
	if t.RefreshToken == "" {
		t.RefreshToken = refreshToken
	}
	return t, nil
}

func parseXaiTokenResponse(body []byte) (*XaiTokens, error) {
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("xai token response parse failed: %w", err)
	}
	if raw.AccessToken == "" {
		return nil, fmt.Errorf("xai token response missing access_token")
	}
	t := &XaiTokens{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		IDToken:      raw.IDToken,
		ExpiresIn:    raw.ExpiresIn,
	}
	if raw.IDToken != "" {
		_, _, t.Email = DecodeCodexIDToken(raw.IDToken) // generic JWT email claim
	}
	return t, nil
}

func xaiCodeVerifier() string {
	b := make([]byte, xaiPKCEVerifBytes)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
