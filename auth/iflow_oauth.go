package auth

import (
	"context"
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

// iFlow OAuth, ported from 9router's src/lib/oauth/providers.js (the `iflow`
// handler) and IFLOW_CONFIG. iFlow is an authorization-code flow whose KEY OUTPUT
// is an API key: after the token exchange, a userInfo call (authenticated by the
// access token) returns the account's apiKey, which is what authenticates inference
// at apis.iflow.cn (Bearer). We bind a loopback callback, exchange the code, then
// fetch the apiKey. After login, inference is plain OpenAI, so the proxy reuses the
// generic OpenAI provider with the apiKey as the Bearer credential.
const (
	iflowClientID     = "10009311001"
	iflowClientSecret = "4Z3YjXycVsQvyGF1etiNlIBB4RsqSDtW"
	iflowAuthorizeURL = "https://iflow.cn/oauth"
	iflowTokenURL     = "https://iflow.cn/oauth/token"
	iflowUserInfoURL  = "https://iflow.cn/api/oauth/getUserInfo"
	iflowLoopbackPort = 11345
	iflowRedirectURI  = "http://127.0.0.1:11345/callback"
	iflowLoginTimeout = 5 * time.Minute
)

// IFlowTokens is a completed iFlow login: OAuth tokens plus the resolved apiKey.
type IFlowTokens struct {
	AccessToken  string
	RefreshToken string
	APIKey       string // the inference Bearer credential (from userInfo)
	Email        string
	ExpiresIn    int
}

// IFlowSession is an in-flight loopback login.
type IFlowSession struct {
	ID        string
	State     string
	AuthURL   string
	ExpiresAt time.Time

	mu           sync.Mutex
	status       string
	tokens       *IFlowTokens
	errMsg       string
	server       *http.Server
	listener     net.Listener
	shutdownOnce sync.Once
}

var (
	iflowSessions   = make(map[string]*IFlowSession)
	iflowSessionsMu sync.RWMutex
)

// StartIFlowLogin binds the loopback callback, builds the authorize URL, and
// returns the session.
func StartIFlowLogin() (*IFlowSession, error) {
	state := generateCodeVerifier()
	params := url.Values{}
	params.Set("loginMethod", "phone")
	params.Set("type", "phone")
	params.Set("redirect", iflowRedirectURI)
	params.Set("state", state)
	params.Set("client_id", iflowClientID)
	authURL := iflowAuthorizeURL + "?" + params.Encode()

	sess := &IFlowSession{
		ID:        GenerateAccountID(),
		State:     state,
		AuthURL:   authURL,
		ExpiresAt: time.Now().Add(iflowLoginTimeout),
		status:    "pending",
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", iflowLoopbackPort))
	if err != nil {
		return nil, fmt.Errorf("cannot bind iFlow callback port %d: %w", iflowLoopbackPort, err)
	}
	sess.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := q.Get("code")
		if code == "" {
			sess.fail("missing code")
			writeCodexCallbackPage(w, false, "Login failed (no code). You can close this tab.")
			return
		}
		tokens, exErr := exchangeIFlowCode(r.Context(), code)
		if exErr != nil {
			sess.fail(exErr.Error())
			writeCodexCallbackPage(w, false, "Token exchange failed. You can close this tab.")
			return
		}
		sess.complete(tokens)
		writeCodexCallbackPage(w, true, "iFlow connected. You can close this tab and return to Kiro-Go.")
	})

	sess.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = sess.server.Serve(ln) }()

	iflowSessionsMu.Lock()
	iflowSessions[sess.ID] = sess
	iflowSessionsMu.Unlock()

	go func() {
		time.Sleep(iflowLoginTimeout + 30*time.Second)
		sess.shutdown()
		iflowSessionsMu.Lock()
		delete(iflowSessions, sess.ID)
		iflowSessionsMu.Unlock()
	}()

	return sess, nil
}

// PollIFlowLogin returns the current login status.
func PollIFlowLogin(sessionID string) (status string, tokens *IFlowTokens, errMsg string, found bool) {
	iflowSessionsMu.RLock()
	sess, ok := iflowSessions[sessionID]
	iflowSessionsMu.RUnlock()
	if !ok {
		return "", nil, "", false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.status, sess.tokens, sess.errMsg, true
}

func (s *IFlowSession) complete(t *IFlowTokens) {
	s.mu.Lock()
	s.status = "completed"
	s.tokens = t
	s.mu.Unlock()
	s.shutdown()
}

func (s *IFlowSession) fail(msg string) {
	s.mu.Lock()
	s.status = "error"
	s.errMsg = msg
	s.mu.Unlock()
	s.shutdown()
}

func (s *IFlowSession) shutdown() {
	s.shutdownOnce.Do(func() {
		if s.server != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = s.server.Shutdown(ctx)
		}
	})
}

func exchangeIFlowCode(ctx context.Context, code string) (*IFlowTokens, error) {
	basic := base64.StdEncoding.EncodeToString([]byte(iflowClientID + ":" + iflowClientSecret))
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", iflowRedirectURI)
	form.Set("client_id", iflowClientID)
	form.Set("client_secret", iflowClientSecret)

	req, err := http.NewRequestWithContext(ctx, "POST", iflowTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+basic)

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("iflow token exchange request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("iflow token exchange failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("iflow token parse failed: %w", err)
	}
	if raw.AccessToken == "" {
		return nil, fmt.Errorf("iflow token response missing access_token")
	}
	apiKey, email, uerr := fetchIFlowUserInfo(ctx, raw.AccessToken)
	if uerr != nil {
		return nil, uerr
	}
	return &IFlowTokens{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		APIKey:       apiKey,
		Email:        email,
		ExpiresIn:    raw.ExpiresIn,
	}, nil
}

// fetchIFlowUserInfo retrieves the account's apiKey (REQUIRED for inference) and
// email/phone. iFlow returns these under {success, data:{apiKey, email, phone}}.
func fetchIFlowUserInfo(ctx context.Context, accessToken string) (apiKey, email string, err error) {
	u := iflowUserInfoURL + "?accessToken=" + url.QueryEscape(accessToken)
	req, rerr := http.NewRequestWithContext(ctx, "GET", u, nil)
	if rerr != nil {
		return "", "", rerr
	}
	req.Header.Set("Accept", "application/json")
	resp, derr := httpClient().Do(req)
	if derr != nil {
		return "", "", fmt.Errorf("iflow userinfo request failed: %w", derr)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("iflow userinfo failed: HTTP %d %s", resp.StatusCode, string(body))
	}
	var d struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			APIKey string `json:"apiKey"`
			Email  string `json:"email"`
			Phone  string `json:"phone"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return "", "", fmt.Errorf("iflow userinfo parse failed: %w", err)
	}
	if !d.Success {
		return "", "", fmt.Errorf("iflow userinfo request failed: %s", d.Message)
	}
	if strings.TrimSpace(d.Data.APIKey) == "" {
		return "", "", fmt.Errorf("iflow returned an empty API key")
	}
	return d.Data.APIKey, firstNonEmptyStr(d.Data.Email, d.Data.Phone), nil
}
