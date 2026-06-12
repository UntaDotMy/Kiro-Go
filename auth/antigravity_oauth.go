package auth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Antigravity (Google Cloud Code Assist) OAuth2, ported from 9router's
// ANTIGRAVITY_CONFIG + the `antigravity` handler. Like gemini-cli it is standard
// Google OAuth2 (no PKCE) with a loopback callback, but it uses its OWN client
// credentials and an expanded scope set, and inference targets the daily-cloudcode
// endpoints. The Google token helpers (googleTokenRequest/fetchGoogleEmail) are
// shared from gemini_cli_oauth.go via the *WithClient variants.
const (
	antigravityClientID     = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	antigravityClientSecret = "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"
	antigravityLoopbackPort = 8124
	antigravityRedirectURI  = "http://127.0.0.1:8124/callback"
	antigravityScopes       = "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile https://www.googleapis.com/auth/cclog https://www.googleapis.com/auth/experimentsandconfigs"
	antigravityLoginTimeout = 5 * time.Minute
)

// AntigravitySession is an in-flight loopback login.
type AntigravitySession struct {
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
	antigravitySessions   = make(map[string]*AntigravitySession)
	antigravitySessionsMu sync.RWMutex
)

// StartAntigravityLogin binds the loopback callback, builds the Google authorize
// URL with antigravity's client + scopes, and returns the session.
func StartAntigravityLogin() (*AntigravitySession, error) {
	state := generateCodeVerifier()
	params := url.Values{}
	params.Set("client_id", antigravityClientID)
	params.Set("redirect_uri", antigravityRedirectURI)
	params.Set("response_type", "code")
	params.Set("scope", antigravityScopes)
	params.Set("access_type", "offline")
	params.Set("prompt", "consent")
	params.Set("state", state)
	authURL := googleAuthorizeURL + "?" + params.Encode()

	sess := &AntigravitySession{
		ID:        GenerateAccountID(),
		State:     state,
		AuthURL:   authURL,
		ExpiresAt: time.Now().Add(antigravityLoginTimeout),
		status:    "pending",
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", antigravityLoopbackPort))
	if err != nil {
		return nil, fmt.Errorf("cannot bind Antigravity callback port %d: %w", antigravityLoopbackPort, err)
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
		tokens, exErr := exchangeGoogleCodeWithClient(r.Context(), code, antigravityRedirectURI, antigravityClientID, antigravityClientSecret)
		if exErr != nil {
			sess.fail(exErr.Error())
			writeCodexCallbackPage(w, false, "Token exchange failed. You can close this tab.")
			return
		}
		tokens.ProjectID = discoverCodeAssistProject(r.Context(), tokens.AccessToken)
		sess.complete(tokens)
		writeCodexCallbackPage(w, true, "Antigravity connected. You can close this tab and return to Kiro-Go.")
	})

	sess.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = sess.server.Serve(ln) }()

	antigravitySessionsMu.Lock()
	antigravitySessions[sess.ID] = sess
	antigravitySessionsMu.Unlock()

	go func() {
		time.Sleep(antigravityLoginTimeout + 30*time.Second)
		sess.shutdown()
		antigravitySessionsMu.Lock()
		delete(antigravitySessions, sess.ID)
		antigravitySessionsMu.Unlock()
	}()

	return sess, nil
}

// PollAntigravityLogin returns the current login status.
func PollAntigravityLogin(sessionID string) (status string, tokens *GoogleTokens, errMsg string, found bool) {
	antigravitySessionsMu.RLock()
	sess, ok := antigravitySessions[sessionID]
	antigravitySessionsMu.RUnlock()
	if !ok {
		return "", nil, "", false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.status, sess.tokens, sess.errMsg, true
}

// RefreshAntigravityToken renews via Google's token endpoint with antigravity's client.
func RefreshAntigravityToken(ctx context.Context, refreshToken string) (*GoogleTokens, error) {
	return RefreshGoogleTokenWithClient(ctx, refreshToken, antigravityClientID, antigravityClientSecret)
}

func (s *AntigravitySession) complete(t *GoogleTokens) {
	s.mu.Lock()
	s.status = "completed"
	s.tokens = t
	s.mu.Unlock()
	s.shutdown()
}

func (s *AntigravitySession) fail(msg string) {
	s.mu.Lock()
	s.status = "error"
	s.errMsg = msg
	s.mu.Unlock()
	s.shutdown()
}

func (s *AntigravitySession) shutdown() {
	s.shutdownOnce.Do(func() {
		if s.server != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = s.server.Shutdown(ctx)
		}
	})
}
