package automation

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"kiro-go/auth"

	"github.com/playwright-community/playwright-go"
)

// Integration tests that drive the REAL runLoginLoop state machine through a REAL
// Chromium (Playwright) against synthetic pages whose structure mirrors the ACTUAL
// CodeBuddy login (verified by live inspection):
//   - the login form is inside an <iframe> (Keycloak), not the main page
//   - the Google button is id="social-google" inside that iframe
//   - Google uses #identifierId (email) and name="Passwd" (password)
//   - after login, CodeBuddy lands on /started?state=... and the CLI is authorized
//     by fetching /console/auth/login?state=... (this is what flips the token poll)
//
// The loop's success signal is the token poll, so the server records the
// /console/auth/login hit and the test feeds a token into the channel then — i.e.
// it proves the whole chain reaches the CLI-authorization step the real flow needs.
//
// Skips when no browser is available, so CI without a Playwright install stays green.

// engineAvailable reports whether Playwright + Chromium can be launched.
func engineAvailable(t *testing.T) *Engine {
	t.Helper()
	eng, err := StartEngine(EngineOptions{Headless: true})
	if err != nil {
		t.Skipf("no Playwright browser available: %v", err)
	}
	return eng
}

// ssoTestServer mirrors the real CodeBuddy+Google flow end to end.
type ssoTestServer struct {
	wrongPass   bool // password step shows an invalid-creds error
	mu          sync.Mutex
	gotEmail    string
	gotPassword string
	cliAuthDone bool // /console/auth/login was hit (the CLI-authorization step)
}

func (s *ssoTestServer) handler() http.Handler {
	mux := http.NewServeMux()

	// CodeBuddy login page — Keycloak form lives inside an <iframe>, like the real one.
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><html><body><div id="root">
			<iframe src="/keycloak" title="login-iframe" style="width:100%;height:600px"></iframe>
		</div></body></html>`)
	})

	// Keycloak realm page (inside iframe): terms checkbox + social-google button.
	mux.HandleFunc("/keycloak", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><html><body>
			<div class="policy-wrapper"><label class="custom-checkbox">
			  <input type="checkbox" id="agree-policy-account"></label></div>
			<div id="kc-social-providers-login">
			  <a id="social-google" href="/google/identifier">Log in with Google</a>
			</div></body></html>`)
	})

	// Google email step — real field id #identifierId + #identifierNext.
	mux.HandleFunc("/google/identifier", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><html><body><h1>Sign in</h1>
			<form action="/google/pwd" method="get">
			  <input type="email" id="identifierId" name="identifier" autocomplete="username">
			  <div id="identifierNext"><button type="submit">Next</button></div>
			</form></body></html>`)
	})

	// Google password step — real field name="Passwd" + #passwordNext.
	mux.HandleFunc("/google/pwd", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.gotEmail = r.URL.Query().Get("identifier")
		s.mu.Unlock()
		fmt.Fprint(w, `<!doctype html><html><body><h1>Welcome</h1>
			<form action="/google/done" method="get">
			  <input type="password" name="Passwd">
			  <div id="passwordNext"><button type="submit">Next</button></div>
			</form></body></html>`)
	})

	// After password: invalid-creds error, or redirect to CodeBuddy /started.
	mux.HandleFunc("/google/done", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.gotPassword = r.URL.Query().Get("Passwd")
		wrong := s.wrongPass
		s.mu.Unlock()
		if wrong {
			fmt.Fprint(w, `<!doctype html><html><body><p>Wrong password. Try again.</p>
			  <input type="password" name="Passwd"></body></html>`)
			return
		}
		// Land on the CLI /started page — the loop must authorize the CLI state next.
		http.Redirect(w, r, "/started?platform=CLI&state=test-state-123", http.StatusFound)
	})

	// /started page: a plain page; handleStartedAuthorization will fetch
	// /console/auth/login from here (same origin).
	mux.HandleFunc("/started", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><html><body><h1>Completing sign-in...</h1></body></html>`)
	})

	// The CLI-authorization endpoint the loop calls — records the hit and returns
	// the success envelope the real gateway returns.
	mux.HandleFunc("/console/auth/login", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.cliAuthDone = true
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"msg":"ok"}`)
	})

	return mux
}

func (s *ssoTestServer) cliAuthorized() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.cliAuthDone }
func (s *ssoTestServer) creds() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gotEmail, s.gotPassword
}

// runLoopAgainst drives runLoginLoop against srv. It feeds a token into tokenCh
// once the server records the /console/auth/login hit (the real success trigger),
// so the loop terminates with StatusSuccess exactly as it would in production.
func runLoopAgainst(t *testing.T, eng *Engine, s *ssoTestServer, srv *httptest.Server, email, pass string) (LoginStatus, string, []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	session, err := eng.NewSession(NewFingerprint())
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer session.Close()
	page := session.Page()
	if _, err := page.Goto(srv.URL+"/login", playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
	}); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	tokenCh := make(chan *auth.CodeBuddyTokens, 1)
	pollErrCh := make(chan error, 1)
	// Watch for the CLI-auth hit and deliver a token (mirrors the real poller).
	go func() {
		for ctx.Err() == nil {
			if s.cliAuthorized() {
				tokenCh <- &auth.CodeBuddyTokens{AccessToken: "tok-abc", RefreshToken: "ref-xyz", ExpiresIn: 3600}
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()

	var steps []string
	var stepsMu sync.Mutex
	onStep := func(step, msg string) { stepsMu.Lock(); steps = append(steps, step); stepsMu.Unlock() }
	in := CodeBuddyLoginInput{Email: email, Password: pass, Backend: "codebuddy-ai"}
	status, _, errMsg := runLoginLoop(ctx, page, in, tokenCh, pollErrCh, onStep)
	t.Logf("status=%s err=%q steps: %v", status, errMsg, steps)
	return status, errMsg, steps
}

// TestLoginLoop_FullChain proves the full state-machine login: iframe →
// terms+Google → email → password → /started → CLI-authorize → token → success.
func TestLoginLoop_FullChain(t *testing.T) {
	if os.Getenv("CBDIAG_SKIP_BROWSER") == "1" {
		t.Skip("CBDIAG_SKIP_BROWSER=1")
	}
	eng := engineAvailable(t)
	defer eng.Close()

	s := &ssoTestServer{}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	status, errMsg, _ := runLoopAgainst(t, eng, s, srv, "tester@example.com", "s3cret-pw")
	if status != StatusSuccess {
		t.Fatalf("status=%s err=%q, want success", status, errMsg)
	}
	if !s.cliAuthorized() {
		t.Errorf("the CLI /console/auth/login step was never reached")
	}
	gotE, gotP := s.creds()
	if gotE != "tester@example.com" || gotP != "s3cret-pw" {
		t.Errorf("creds delivered to Google = (%q,%q), want (tester@example.com, s3cret-pw)", gotE, gotP)
	}
}

// TestLoginLoop_InvalidCreds proves wrong-password detection returns the
// invalid-credentials status (not a generic failure or hang).
func TestLoginLoop_InvalidCreds(t *testing.T) {
	if os.Getenv("CBDIAG_SKIP_BROWSER") == "1" {
		t.Skip("CBDIAG_SKIP_BROWSER=1")
	}
	eng := engineAvailable(t)
	defer eng.Close()

	s := &ssoTestServer{wrongPass: true}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	status, _, _ := runLoopAgainst(t, eng, s, srv, "bad@example.com", "nope")
	if status != StatusInvalidCredentials {
		t.Fatalf("status=%s, want %s", status, StatusInvalidCredentials)
	}
}
