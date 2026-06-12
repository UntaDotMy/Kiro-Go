package automation

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveLogin runs the REAL production LoginCodeBuddy flow against the live
// CodeBuddy site with a real account, in a VISIBLE Chrome window, and reports every
// step plus exactly what data we captured (tokens + web cookie). This is the honest
// end-to-end test: it exercises the same code production runs.
//
// Skipped unless CBDIAG_LIVE=1. Credentials come from env so they never live in
// the repo:
//
//	CBDIAG_LIVE=1 CBDIAG_EMAIL=user@example.com CBDIAG_PASSWORD=secret \
//	  go test ./automation/ -run TestLiveLogin -v -timeout 300s
func TestLiveLogin(t *testing.T) {
	if os.Getenv("CBDIAG_LIVE") != "1" {
		t.Skip("set CBDIAG_LIVE=1 to run the live login diagnostic")
	}
	email := os.Getenv("CBDIAG_EMAIL")
	password := os.Getenv("CBDIAG_PASSWORD")
	if email == "" || password == "" {
		t.Skip("set CBDIAG_EMAIL and CBDIAG_PASSWORD to run the live login")
	}
	backend := os.Getenv("CBDIAG_BACKEND")
	if backend == "" {
		backend = "codebuddy-ai" // international, the one we use
	}
	headless := os.Getenv("CBDIAG_HEADLESS") == "1" // default: visible window to watch

	ctx, cancel := context.WithTimeout(context.Background(), 280*time.Second)
	defer cancel()

	eng, err := StartEngine(EngineOptions{Headless: headless})
	if err != nil {
		t.Fatalf("start engine: %v", err)
	}
	defer eng.Close()

	onStep := func(step, msg string) {
		t.Logf("[%s] STEP %-34s %s", time.Now().Format("15:04:05"), step, msg)
	}

	res := LoginCodeBuddy(ctx, CodeBuddyLoginInput{
		Engine:      eng,
		Email:       email,
		Password:    password,
		Backend:     backend,
		Fingerprint: NewFingerprint(),
	}, onStep)

	t.Logf("================ RESULT ================")
	t.Logf("status        = %s", res.Status)
	t.Logf("error         = %q", res.Error)
	t.Logf("backend/host  = %s / %s", res.Backend, res.Host)
	t.Logf("accessToken   = %s", redact(res.AccessToken))
	t.Logf("refreshToken  = %s", redact(res.RefreshToken))
	t.Logf("expiresIn     = %d", res.ExpiresIn)
	t.Logf("webCookie     = %s", summarizeCookie(res.WebCookie))
	t.Logf("========================================")

	// If it parked for manual, keep the window open a bit so we can SEE the screen
	// it got stuck on (that's the diagnostic).
	if res.Status == StatusNeedsManual && res.Session != nil {
		t.Logf("NEEDS MANUAL — holding the window open 60s so the stuck screen is visible")
		time.Sleep(60 * time.Second)
		res.Session.Close()
	}

	switch res.Status {
	case StatusSuccess:
		if res.AccessToken == "" {
			t.Errorf("success but no access token captured")
		}
	default:
		t.Errorf("login did not succeed: status=%s error=%s", res.Status, res.Error)
	}
}

func redact(s string) string {
	if s == "" {
		return "(empty)"
	}
	if len(s) <= 12 {
		return "(" + itoa(len(s)) + " chars)"
	}
	return s[:6] + "…" + s[len(s)-4:] + " (" + itoa(len(s)) + " chars)"
}

func summarizeCookie(c string) string {
	if c == "" {
		return "(empty)"
	}
	n := 1
	for _, r := range c {
		if r == ';' {
			n++
		}
	}
	return itoa(n) + " cookie(s), " + itoa(len(c)) + " chars"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
