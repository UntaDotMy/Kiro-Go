package automation

import (
	"context"
	"fmt"
	"math/rand"
	neturl "net/url"
	"sort"
	"strings"
	"time"

	"kiro-go/auth"
	"kiro-go/logger"

	"github.com/playwright-community/playwright-go"
)

// CodeBuddy automation login (Playwright).
//
// CodeBuddy's web login is Google/GitHub SSO behind a Keycloak form that is
// EMBEDDED IN AN IFRAME on www.codebuddy.ai/login (verified by live inspection:
// the iframe src is /auth/realms/copilot/... and the Google control is
// <a id="social-google"> inside it). The CLI OAuth handshake
// (auth/codebuddy_oauth.go) gives us {state, authUrl} to poll for the inference
// token; the piece the HTTP-only flow can't do is actually SIGN IN. So here we:
//
//  1. start the CLI OAuth handshake (StartCodeBuddyAuth) to get {state, authUrl}
//  2. drive Playwright to a normalized /login URL and complete the SSO login
//     (tick terms, click Google IN THE IFRAME, type email/password on Google,
//     pick the country/region on the post-login page, authorize the CLI /started
//     state) — this is a near-verbatim port of 9router_wyx0's
//     runGoogleAccountAutomation, which is proven against this exact site.
//  3. poll the CLI token endpoint in parallel until it flips to "ok"
//  4. capture the web-console session cookie for quota
//
// Playwright is the engine because its auto-waiting locators, native :has-text()
// text matching, and per-frame Locator handling are exactly what this
// Keycloak-in-an-iframe + Google-breakout flow needs.

// LoginStatus is the terminal outcome of one account login attempt.
type LoginStatus string

const (
	StatusSuccess            LoginStatus = "success"
	StatusNeedsManual        LoginStatus = "needs_manual"
	StatusInvalidCredentials LoginStatus = "failed_invalid_credentials"
	StatusFailed             LoginStatus = "failed"
	StatusCancelled          LoginStatus = "cancelled"
)

// CodeBuddyLoginInput parameters one account login.
type CodeBuddyLoginInput struct {
	// Engine is the shared per-job browser. The login opens its own isolated
	// Session (context) off this engine.
	Engine *Engine
	Email    string
	Password string
	// Backend selects the host: "codebuddy" (CN) or "codebuddy-ai" (international).
	Backend string
	// Fingerprint varies UA/viewport/timezone per session so concurrent logins
	// don't look identical. Zero value = deterministic default.
	Fingerprint Fingerprint
	// OnSession is invoked once the Session exists, so the job can capture live
	// screenshots for the preview frame while the login runs. Optional.
	OnSession func(s *Session)
}

// CodeBuddyLoginResult is what a finished login yields.
type CodeBuddyLoginResult struct {
	Status       LoginStatus
	Error        string
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
	WebCookie    string // captured web-console session cookie (for quota)
	Backend      string
	Host         string

	// Session is populated ONLY when Status == StatusNeedsManual so the caller can
	// keep the session alive for the operator to finish. The caller owns closing it
	// in that case.
	Session *Session
}

// progressFn reports human-readable steps to the job tracker.
type progressFn func(step, message string)

func noProgress(string, string) {}

// codeBuddyCookieDomains are the hosts whose cookies form the web-console session.
var codeBuddyCookieDomains = []string{"www.codebuddy.ai", "codebuddy.ai"}

// LoginCodeBuddy runs one full automated CodeBuddy login. It blocks until the
// flow reaches a terminal status or ctx is cancelled.
func LoginCodeBuddy(ctx context.Context, in CodeBuddyLoginInput, onStep progressFn) CodeBuddyLoginResult {
	if onStep == nil {
		onStep = noProgress
	}
	backend := in.Backend
	if backend == "" {
		backend = "codebuddy"
	}
	host := auth.CodeBuddyHostCN
	if backend == "codebuddy-ai" {
		host = auth.CodeBuddyHostIntl
	}
	res := CodeBuddyLoginResult{Status: StatusFailed, Backend: backend, Host: host}

	if in.Engine == nil {
		res.Error = "internal: no browser engine"
		return res
	}

	// 1. Start the CLI OAuth handshake to get the login URL + poll state.
	onStep("starting_oauth", "Requesting CodeBuddy login URL")
	ca, err := auth.StartCodeBuddyAuth(ctx, host)
	if err != nil {
		res.Error = fmt.Sprintf("could not start CodeBuddy auth: %v", err)
		return res
	}

	// 2. Open an isolated session (context + page) for this account.
	onStep("opening_session", "Opening isolated browser session")
	session, err := in.Engine.NewSession(in.Fingerprint)
	if err != nil {
		res.Error = fmt.Sprintf("could not open session: %v", err)
		return res
	}
	// On any non-manual exit we close the session; manual hands it to the caller.
	closeSession := true
	defer func() {
		if closeSession {
			session.Close()
		}
	}()
	page := session.Page()

	// Hand the live session to the job so it can screenshot for the preview frame.
	if in.OnSession != nil {
		in.OnSession(session)
	}

	// 3. Poll the CLI token endpoint in the background while the browser logs in.
	tokenCh := make(chan *auth.CodeBuddyTokens, 1)
	pollErrCh := make(chan error, 1)
	pollCtx, cancelPoll := context.WithCancel(ctx)
	defer cancelPoll()
	go pollCodeBuddyToken(pollCtx, host, ca.State, onStep, tokenCh, pollErrCh)

	// 4. Navigate to a NORMALIZED login URL (always the web /login that renders the
	// Keycloak iframe), then drive the state machine.
	loginURL := normalizeCodeBuddyAuthURL(host, ca.AuthURL, ca.State)
	onStep("opening_login", "Opening CodeBuddy login page")
	if _, err := page.Goto(loginURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		res.Error = fmt.Sprintf("could not open login page: %v", err)
		return res
	}
	// Wait for the Keycloak login form to actually be ready before driving the gate,
	// instead of a blind sleep. The real site is a React SPA: the iframe (and the
	// "Sign in with Google" control inside it) appears asynchronously after the page
	// renders, so acting too early finds nothing. We wait — across all frames — for
	// the Google control OR an email field OR the /started page to exist, up to a
	// bounded time; whatever shows first is what the loop will act on. Best-effort:
	// if nothing appears in time we still enter the loop (it keeps polling).
	waitForLoginReady(page, 25*time.Second)

	status, tokens, loopErr := runLoginLoop(ctx, page, in, tokenCh, pollErrCh, onStep)
	switch status {
	case StatusNeedsManual:
		onStep("needs_manual", "Login needs manual completion (2FA/CAPTCHA). Finish in the browser window.")
		res.Status = StatusNeedsManual
		res.Error = orDefaultStr(loopErr, "manual completion required (2FA/CAPTCHA or unexpected challenge)")
		res.Session = session
		closeSession = false // hand the session to the caller
		return res
	case StatusInvalidCredentials:
		res.Status = StatusInvalidCredentials
		res.Error = "invalid email or password"
		return res
	case StatusCancelled:
		res.Status = StatusCancelled
		res.Error = "cancelled"
		return res
	case StatusSuccess:
		if tokens == nil {
			res.Error = "login loop reported success without tokens"
			return res
		}
		res.AccessToken = tokens.AccessToken
		res.RefreshToken = tokens.RefreshToken
		res.ExpiresIn = tokens.ExpiresIn
	default:
		res.Error = orDefaultStr(loopErr, "login did not complete")
		return res
	}

	// 5. Capture the web-console session cookie for quota tracking.
	onStep("capturing_cookie", "Capturing web session for quota tracking")
	res.WebCookie = captureWebCookie(ctx, session, onStep) // empty is non-fatal

	res.Status = StatusSuccess
	onStep("done", "CodeBuddy login complete")
	return res
}

// orDefaultStr returns s if non-empty, else def.
func orDefaultStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// normalizeCodeBuddyAuthURL rebuilds the login URL so we always land on the page
// that renders the Keycloak iframe + "Sign in with Google" gate. Ported from
// 9router_wyx0's normalizeCodeBuddyAuthUrl, which deliberately IGNORES the raw
// authUrl's host and rebuilds against the known web login origin — pulling only
// platform + state from the raw URL. The state endpoint can hand back a URL on an
// API host that has no login form, so navigating to it leaves the loop with no
// button to click. We use the backend's web origin (www.codebuddy.ai for intl, the
// gateway host otherwise), force /login, and carry platform (default CLI) + state.
func normalizeCodeBuddyAuthURL(backendHost, rawURL, state string) string {
	platform := "CLI"
	effState := state

	if rawURL != "" {
		if u, err := neturl.Parse(rawURL); err == nil {
			if p := u.Query().Get("platform"); p != "" {
				platform = p
			}
			if effState == "" {
				effState = u.Query().Get("state")
			}
		}
	}

	origin := "https://www.codebuddy.ai"
	if backendHost != "" && !strings.Contains(backendHost, "codebuddy.ai") {
		origin = strings.TrimRight(backendHost, "/")
	}

	out, err := neturl.Parse(origin)
	if err != nil || out.Host == "" {
		out = &neturl.URL{Scheme: "https", Host: "www.codebuddy.ai"}
	}
	out.Path = "/login"
	q := neturl.Values{}
	q.Set("platform", platform)
	if effState != "" {
		q.Set("state", effState)
	}
	out.RawQuery = q.Encode()
	return out.String()
}

// pollCodeBuddyToken polls the CLI token endpoint until success/terminal error.
func pollCodeBuddyToken(ctx context.Context, host, state string, onStep progressFn, out chan<- *auth.CodeBuddyTokens, errOut chan<- error) {
	const interval = 3 * time.Second
	transient := 0
	for {
		if ctx.Err() != nil {
			return
		}
		status, tokens, err := auth.PollCodeBuddyToken(ctx, host, state)
		if err != nil {
			transient++
			if transient > 8 {
				select {
				case errOut <- err:
				case <-ctx.Done():
				}
				return
			}
		} else if status == "ok" && tokens != nil {
			select {
			case out <- tokens:
			case <-ctx.Done():
			}
			return
		}
		sleepCtx(ctx, interval)
	}
}

// waitForLoginReady polls all frames until the login form is actually interactable
// — the Google control, an email field, or the /started page exists — or the
// timeout elapses. This replaces a blind post-navigation sleep: the real site is a
// React SPA whose Keycloak iframe (and its "Sign in with Google" control) appears
// asynchronously, so the loop must not start acting until something is there.
// Best-effort: returns once anything matches, or after the timeout regardless.
func waitForLoginReady(page playwright.Page, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		scopes := scopesOf(page)
		for _, f := range scopes {
			for _, sel := range []string{googleLoginSelectors[0], `a[href*="broker/google/login" i]`, emailSelector} {
				loc := f.Locator(sel).First()
				if n, err := loc.Count(); err == nil && n > 0 {
					if vis, _ := loc.IsVisible(); vis {
						return
					}
				}
			}
			// /started can be reached directly if a prior session is still valid.
			if u := frameURL(f); strings.Contains(u, "/started") {
				return
			}
		}
		page.WaitForTimeout(400)
	}
}

// runLoginLoop drives the CodeBuddy→Google login as a STATE MACHINE, ported from
// 9router_wyx0's runGoogleAccountAutomation. Rather than a fixed click→type
// sequence, it loops and reacts to whatever is currently on screen across the main
// page AND every iframe/child frame. The success signal is the background token
// poll, raced each tick.
//
// Returns (status, tokens, errMsg). On StatusSuccess tokens is non-nil.
func runLoginLoop(ctx context.Context, page playwright.Page, in CodeBuddyLoginInput, tokenCh <-chan *auth.CodeBuddyTokens, pollErrCh <-chan error, onStep progressFn) (LoginStatus, *auth.CodeBuddyTokens, string) {
	deadline := time.Now().Add(DefaultLoginTimeout)

	// First pass: open the provider gate immediately (tick terms + click Google),
	// like the reference does before its loop.
	handleProviderLoginGate(ctx, page, onStep)

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return StatusCancelled, nil, "cancelled"
		}
		// Token arrived (browser approval drove the CLI state to "ok")?
		select {
		case tk := <-tokenCh:
			if tk != nil {
				onStep("token_received", "CodeBuddy access token received")
				return StatusSuccess, tk, ""
			}
		case err := <-pollErrCh:
			return StatusFailed, nil, fmt.Sprintf("token poll failed: %v", err)
		default:
		}

		scopes := scopesOf(page)

		// 1) Google consent ("...wants access" / Allow).
		if handleGoogleConsent(ctx, page, scopes, onStep) {
			continue
		}

		// 2) Terminal text states (invalid creds / manual challenge).
		text := readAllText(scopes)
		if includesAnyText(text, invalidCredentialMarkers) {
			onStep("invalid_credentials", "Google rejected the email or password")
			return StatusInvalidCredentials, nil, "invalid email or password"
		}
		if includesAnyText(text, manualAssistMarkers) {
			onStep("manual_assist_required", "Google requested CAPTCHA/2FA/recovery verification")
			return StatusNeedsManual, nil, "manual assist required (CAPTCHA, 2FA, recovery, or suspicious-login challenge)"
		}

		// 3) CodeBuddy post-login: authorize the CLI /started?state=... step (what
		// makes the token poll flip to "ok"). Checked BEFORE the Google gate so once
		// we've reached /started we don't re-click a stale Google button.
		if handleStartedAuthorization(ctx, scopes, onStep) {
			continue
		}

		// 3b) CodeBuddy region/country page (new accounts hit it after consent).
		if handleRegionPage(ctx, scopes, onStep) {
			continue
		}

		// 4) Email field anywhere? Focus + human-type + Next.
		if loc := firstVisibleLocator(scopes, emailSelector); loc != nil {
			onStep("entering_email", "Entering email")
			if humanFill(ctx, page, loc, in.Email) {
				clickFirst(ctx, scopes, nextButtonSelectors)
				page.WaitForTimeout(jitterPW(900, 400))
				continue
			}
		}

		// 5) Password field anywhere? Focus + human-type + Next.
		if loc := firstVisibleLocator(scopes, passwordSelector); loc != nil {
			onStep("entering_password", "Entering password")
			if humanFill(ctx, page, loc, in.Password) {
				clickFirst(ctx, scopes, nextButtonSelectors)
				page.WaitForTimeout(jitterPW(900, 400))
				continue
			}
		}

		// 6) Provider login gate: tick terms, click "Sign in with Google". Only
		// reached when there's no email/password field and we're not yet on /started.
		if handleProviderLoginGate(ctx, page, onStep) {
			continue
		}

		// 7) Generic approve/continue (consent screens, onboarding).
		if clickFirst(ctx, scopes, approveButtonSelectors) {
			onStep("approving_consent", "Approving consent")
			page.WaitForTimeout(700)
			continue
		}

		// Nothing actionable this tick.
		onStep("waiting_for_next_screen", "Waiting: "+describeScreen(scopes))
		page.WaitForTimeout(700)
	}

	// Timed out. One last token check before declaring manual.
	select {
	case tk := <-tokenCh:
		if tk != nil {
			return StatusSuccess, tk, ""
		}
	default:
	}
	return StatusNeedsManual, nil, "login did not complete automatically within the time limit"
}

// firstVisibleLocator returns the first VISIBLE locator matching selector across
// all frames, or nil. Playwright locators auto-wait, but here we want a single
// immediate check per tick, so we use Count()+IsVisible() without a long timeout.
func firstVisibleLocator(scopes []playwright.Frame, selector string) playwright.Locator {
	for _, f := range scopes {
		loc := f.Locator(selector).First()
		if n, err := loc.Count(); err != nil || n == 0 {
			continue
		}
		if vis, err := loc.IsVisible(); err == nil && vis {
			return loc
		}
	}
	return nil
}

// humanFill focuses a field and types one character at a time with irregular
// per-key delays (and occasional longer "thinking" pauses), so the input cadence
// reads like a person rather than an instant paste — a primary bot tell. Proven in
// the cbcheck probe. Returns whether it typed. Falls back to Fill on focus error.
func humanFill(ctx context.Context, page playwright.Page, loc playwright.Locator, text string) bool {
	if err := loc.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(5000)}); err != nil {
		// Could not focus by click; try a direct fill so we don't stall the flow.
		return loc.Fill(text, playwright.LocatorFillOptions{Timeout: playwright.Float(10000)}) == nil
	}
	kb := page.Keyboard()
	for _, r := range text {
		if ctx.Err() != nil {
			return false
		}
		_ = kb.Type(string(r))
		d := 60 + rand.Intn(110) // ~60–170ms between keys
		if rand.Float64() < 0.10 {
			d += 200 + rand.Intn(300) // occasional pause-to-think
		}
		page.WaitForTimeout(float64(d))
	}
	return true
}

// jitterPW returns base±spread ms (never negative) for WaitForTimeout, so dwell
// times between actions aren't machine-regular.
func jitterPW(base, spread int) float64 {
	d := base + rand.Intn(2*spread+1) - spread
	if d < 0 {
		d = 0
	}
	return float64(d)
}

// clickFirst clicks the first visible+enabled match for any selector across all
// frames. Returns whether it clicked.
func clickFirst(ctx context.Context, scopes []playwright.Frame, selectors []string) bool {
	for _, f := range scopes {
		for _, sel := range selectors {
			loc := f.Locator(sel).First()
			if n, err := loc.Count(); err != nil || n == 0 {
				continue
			}
			if vis, err := loc.IsVisible(); err != nil || !vis {
				continue
			}
			if err := loc.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(5000)}); err == nil {
				return true
			}
		}
	}
	return false
}

// checkFirst ticks the first checkbox match across frames. CodeBuddy's consent is
// a CUSTOM checkbox: the real <input id="agree-policy-account"
// onchange="handlePolicyChange(this)"> is HIDDEN (visible=false) inside a
// <label class="custom-checkbox">, and the label is the visible control. This was
// verified live — the ONLY technique that toggles it is clicking the input's
// closest <label> (which fires the framework's onchange). Playwright's Check(force)
// refuses because the input is invisible, and setting el.checked directly doesn't
// stick because the framework owns the state.
//
// CRITICAL (the reason a single attempt fails): the input exists in the DOM BEFORE
// its onchange handler is wired, so the first label-click silently no-ops. The
// proven fix (validated by the cbcheck harness) is to RETRY the click and re-verify
// IsChecked() until it actually sticks — concentrated here, not diluted across the
// outer loop's other handlers.
func checkFirst(ctx context.Context, scopes []playwright.Frame, selectors []string) bool {
	// Find the checkbox locator (first match across frames+selectors).
	var loc playwright.Locator
	var owner playwright.Frame
	for _, f := range scopes {
		for _, sel := range selectors {
			cand := f.Locator(sel).First()
			if n, err := cand.Count(); err == nil && n > 0 {
				loc, owner = cand, f
				break
			}
		}
		if loc != nil {
			break
		}
	}
	if loc == nil {
		return false
	}
	if checked, err := loc.IsChecked(); err == nil && checked {
		return true
	}

	// Retry the proven label-click until it sticks (input's onchange may not be
	// wired yet on early attempts). Mirrors cbcheck.tickCheckbox.
	for attempt := 0; attempt < 8; attempt++ {
		// 1) Click the closest <label> in-page (the proven winner for hidden inputs).
		_, _ = loc.Evaluate(`el => { const l = el.closest('label'); if (l) { l.click(); return true; } return false; }`, nil)
		owner.WaitForTimeout(300)
		if checked, err := loc.IsChecked(); err == nil && checked {
			return true
		}

		// 2) A real Playwright click on the visible label element.
		lbl := owner.Locator(`label.custom-checkbox`).First()
		if n, _ := lbl.Count(); n > 0 {
			_ = lbl.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(2500)})
			owner.WaitForTimeout(300)
			if checked, err := loc.IsChecked(); err == nil && checked {
				return true
			}
		}

		// 3) label[for=id], if one exists.
		if id, err := loc.GetAttribute("id"); err == nil && id != "" {
			fl := owner.Locator(`label[for="` + id + `"]`).First()
			if n, _ := fl.Count(); n > 0 {
				_ = fl.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(2500)})
				if checked, err := loc.IsChecked(); err == nil && checked {
					return true
				}
			}
		}

		owner.WaitForTimeout(500)
	}

	// Last resort: force-check + direct JS set (rarely needed; the framework usually
	// owns the state, but try anyway so we don't silently give up).
	_ = loc.Check(playwright.LocatorCheckOptions{Force: playwright.Bool(true), Timeout: playwright.Float(2500)})
	if checked, err := loc.IsChecked(); err == nil && checked {
		return true
	}
	_, _ = loc.Evaluate(`el => { el.checked = true; el.dispatchEvent(new Event('input',{bubbles:true})); el.dispatchEvent(new Event('change',{bubbles:true})); }`, nil)
	if checked, err := loc.IsChecked(); err == nil && checked {
		return true
	}
	return false
}

// handleProviderLoginGate ticks the terms checkbox and clicks "Sign in with
// Google" (which lives inside the Keycloak iframe), confirming any privacy dialog.
// Returns whether it did something.
func handleProviderLoginGate(ctx context.Context, page playwright.Page, onStep progressFn) bool {
	scopes := scopesOf(page)

	// An already-open privacy/confirm dialog?
	if clickFirst(ctx, scopes, privacyConfirmSelectors) {
		onStep("accepting_provider_privacy_dialog", "Confirmed provider privacy/terms dialog")
		page.WaitForTimeout(1000)
		return true
	}

	// TERMS-FIRST. Keycloak won't proceed to the provider unless the consent
	// checkbox is accepted, and on a fresh SPA load the custom checkbox's input
	// exists before its onchange is wired — so the tick can silently fail on the
	// first pass (this was the "nothing is checked" symptom). So: if a terms
	// checkbox is present but NOT yet checked, tick it and return WITHOUT clicking
	// Google. The loop re-enters next tick; only once terms are confirmed checked do
	// we click Google. If no checkbox exists, fall straight through to Google.
	if termsPresent(scopes) {
		if !termsChecked(scopes) {
			if checkFirst(ctx, scopes, termsCheckboxSelectors) {
				onStep("accepting_provider_terms", "Accepted provider terms")
			} else {
				onStep("accepting_provider_terms", "Ticking provider terms…")
			}
			page.WaitForTimeout(400)
			return true // re-enter the loop; don't click Google until terms stick
		}
	}

	if clickFirst(ctx, scopes, googleLoginSelectors) {
		onStep("selecting_google_login", "Selecting Google login")
		page.WaitForTimeout(1000)
		// A confirmation dialog may pop after the click.
		if clickFirst(ctx, scopes, privacyConfirmSelectors) {
			onStep("accepting_provider_privacy_dialog", "Confirmed provider privacy dialog")
			page.WaitForTimeout(1000)
		}
		return true
	}
	return false
}

// termsPresent reports whether any terms checkbox exists across frames.
func termsPresent(scopes []playwright.Frame) bool {
	for _, f := range scopes {
		for _, sel := range termsCheckboxSelectors {
			if n, err := f.Locator(sel).First().Count(); err == nil && n > 0 {
				return true
			}
		}
	}
	return false
}

// termsChecked reports whether the first terms checkbox is checked.
func termsChecked(scopes []playwright.Frame) bool {
	for _, f := range scopes {
		for _, sel := range termsCheckboxSelectors {
			loc := f.Locator(sel).First()
			if n, err := loc.Count(); err != nil || n == 0 {
				continue
			}
			if c, err := loc.IsChecked(); err == nil {
				return c
			}
		}
	}
	return false
}

// handleGoogleConsent approves Google's OAuth consent ("...wants to access").
func handleGoogleConsent(ctx context.Context, page playwright.Page, scopes []playwright.Frame, onStep progressFn) bool {
	text := readAllText(scopes)
	if !includesAnyText(text, []string{"wants to access", "wants access", "ingin mengakses"}) {
		return false
	}
	if clickFirst(ctx, scopes, approveButtonSelectors) {
		onStep("approving_google_consent", "Approving Google OAuth consent")
		page.WaitForTimeout(1000)
		return true
	}
	return false
}

// isGoogleFrame reports whether a frame is on Google's auth domain. We use this to
// SKIP frames during CodeBuddy-specific handling (started-auth, region) so we never
// interfere with Google's own pages — but we do NOT require a codebuddy domain,
// because the started-auth JS self-gates strictly on the /started path and the
// region JS self-gates on the region page markers. Requiring codebuddy.* would
// break same-origin test servers and any white-labeled host.
func isGoogleFrame(f playwright.Frame) bool {
	u := frameURL(f)
	return strings.Contains(u, "accounts.google.com") || strings.Contains(u, ".google.com/")
}

// handleStartedAuthorization performs the CLI-state authorization CodeBuddy
// requires after Google login: on /started?state=..., it calls
// /console/auth/login?state=...&domain=... (credentials: include) which flips the
// CLI token poll to "ok". Ported from the reference's
// handleCodeBuddyStartedAuthorization. Returns whether it acted.
func handleStartedAuthorization(ctx context.Context, scopes []playwright.Frame, onStep progressFn) bool {
	for _, f := range scopes {
		if isGoogleFrame(f) {
			continue
		}
		raw, err := f.Evaluate(startedAuthJS)
		if err != nil || raw == nil {
			continue
		}
		val := fmt.Sprint(raw)
		if strings.Contains(val, "authorized") {
			onStep("authorizing_codebuddy_cli_state", "Authorized CodeBuddy CLI login state")
			f.WaitForTimeout(1200)
			return true
		}
		if strings.Contains(val, "attempted") {
			onStep("authorizing_codebuddy_cli_state", "Attempted CodeBuddy CLI login-state authorization")
			f.WaitForTimeout(1200)
			return true
		}
	}
	return false
}

const startedAuthJS = `async () => {
	const url = new URL(window.location.href);
	if (!/\/started\/?$/.test(url.pathname)) return null;
	const platform = url.searchParams.get("platform") || "CLI";
	const state = url.searchParams.get("state");
	if (!state) return null;
	const domain = window.location.hostname || "www.codebuddy.ai";
	const authUrl = new URL("/console/auth/login", window.location.origin);
	authUrl.searchParams.set("platform", platform);
	authUrl.searchParams.set("state", state);
	authUrl.searchParams.set("domain", domain);
	try {
		const r = await fetch(authUrl.toString(), { method: "GET", credentials: "include", redirect: "manual", headers: { "x-requested-with": "XMLHttpRequest", "X-Domain": domain } });
		if (r.type === "opaqueredirect" || (r.status >= 300 && r.status < 400)) return "attempted";
		const t = await r.text(); let d = null; try { d = t ? JSON.parse(t) : null; } catch { d = { raw: t }; }
		if (r.ok && (!d || d.code === 0 || d.code === 200 || typeof d.code === "undefined")) return "authorized";
		if (r.ok) return "attempted";
	} catch (e) {}
	return "failed";
}`

// handleRegionPage handles the CodeBuddy region/country page a new account hits
// after Google consent and before /started. A single in-frame evaluate (ported
// from the reference's handleCodeBuddyRegionPage): submit if a button is present,
// else pick a preferred country, else open the dropdown. Returns whether it acted.
func handleRegionPage(ctx context.Context, scopes []playwright.Frame, onStep progressFn) bool {
	for _, f := range scopes {
		if isGoogleFrame(f) {
			continue
		}
		raw, err := f.Evaluate(regionPageJS)
		if err != nil || raw == nil {
			continue
		}
		val := fmt.Sprint(raw)
		switch {
		case strings.Contains(val, "submitted"):
			onStep("submitting_region", "Submitted CodeBuddy region/country selection")
			f.WaitForTimeout(1200)
			return true
		case strings.Contains(val, "selected"):
			onStep("selecting_region", "Selected CodeBuddy region/country")
			f.WaitForTimeout(800)
			return true
		case strings.Contains(val, "opened"):
			onStep("opening_region_selector", "Opening CodeBuddy region/country selector")
			f.WaitForTimeout(700)
			return true
		}
	}
	return false
}

const regionPageJS = `() => {
  const visible = (el) => {
    if (!(el instanceof HTMLElement)) return false;
    const s = window.getComputedStyle(el);
    if (s.visibility === "hidden" || s.display === "none" || Number(s.opacity) === 0) return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };
  const root = document.querySelector(".page-region");
  const bodyText = (document.body && document.body.innerText) || "";
  const looksLikeRegion = root || /select\s+region|region|country|area|get started|complete/i.test(bodyText);
  if (!looksLikeRegion) return null;
  const click = (el) => {
    el.scrollIntoView({ block: "center", inline: "center" });
    for (const t of ["pointerdown","mousedown","pointerup","mouseup","click"]) {
      el.dispatchEvent(new MouseEvent(t, { bubbles: true, cancelable: true, view: window, buttons: t.endsWith("down") ? 1 : 0 }));
    }
  };
  const optionPatterns = [/global|international|default/i, /singapore|^sg$/i, /united states|^us$/i, /indonesia|^id$/i, /japan|^jp$/i];
  const searchRoot = root || document.body;
  const submitSel = ["button","[role='button']","input[type='submit']",".t-button","[class*='button']"];
  const submits = [...searchRoot.querySelectorAll(submitSel.join(","))].filter(visible).filter((el) => {
    const txt = (el.innerText || "") + " " + (el.getAttribute("aria-label") || "") + " " + (el.getAttribute("value") || "");
    return /submit|start|continue|confirm|done|get started|complete|下一步|完成|开始|确定/i.test(txt);
  });
  if (submits.length) { click(submits[0]); return "submitted"; }
  const optSel = ["ul.dropdown-section li",".dropdown-section li","[role='option']",".t-select-option","[class*='option']","[class*='dropdown'] li"];
  const opts = [...document.querySelectorAll(optSel.join(","))].filter(visible).filter((el) => ((el.innerText || el.textContent || "").trim()));
  if (opts.length) {
    const pick = optionPatterns.map((p) => opts.find((el) => p.test((el.innerText || el.textContent || "").trim()))).find(Boolean) || opts[0];
    click(pick);
    return "selected";
  }
  const ctrlSel = ["select","[role='combobox']",".t-select","[class*='t-select']","[class*='select']","input[placeholder]","[class*='cursor-pointer']"];
  const ctrls = [...searchRoot.querySelectorAll(ctrlSel.join(","))].filter(visible).filter((el) => {
    const txt = (el.innerText || "") + " " + (el.getAttribute("placeholder") || "") + " " + (el.getAttribute("aria-label") || "");
    return /region|country|area|select|地区|国家|选择/i.test(txt) || (el.matches && el.matches(".t-select,[class*='t-select'],input[placeholder],[class*='select'],select"));
  });
  if (ctrls.length) {
    const native = ctrls.find((el) => el.tagName === "SELECT");
    if (native) {
      const options = [...native.options].filter((o) => !o.disabled && o.value !== "");
      const pref = options.find((o) => optionPatterns.some((p) => p.test((o.label || "") + " " + (o.textContent || "") + " " + o.value))) || options[0];
      if (pref) {
        native.value = pref.value;
        native.dispatchEvent(new Event("input", { bubbles: true }));
        native.dispatchEvent(new Event("change", { bubbles: true }));
        return "selected";
      }
    }
    click(ctrls[0]);
    return "opened";
  }
  return null;
}`

// describeScreen returns a short summary of what's on screen across all frames —
// active URL plus visible button/link labels — so a stalled login reports WHERE
// it's stuck instead of an opaque "waiting".
func describeScreen(scopes []playwright.Frame) string {
	url := ""
	for _, f := range scopes {
		if u := f.URL(); u != "" && u != "about:blank" {
			url = u
		}
	}
	var labels []string
	seen := map[string]bool{}
	for _, f := range scopes {
		raw, err := f.Evaluate(`() => {
			const out = [];
			const vis = (el) => { const r = el.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
			const els = document.querySelectorAll('button, a, [role="button"], input[type="submit"], input[type="button"], [class*="btn" i], [class*="button" i]');
			const seen = new Set();
			for (const el of els) {
				if (!vis(el)) continue;
				const t = (el.innerText || el.value || el.getAttribute('aria-label') || '').trim();
				if (!t || t.length > 40 || seen.has(t)) continue;
				seen.add(t); out.push(t.slice(0, 30));
				if (out.length >= 8) break;
			}
			return out.join(' | ');
		}`)
		if err == nil && raw != nil {
			for _, l := range strings.Split(fmt.Sprint(raw), " | ") {
				l = strings.TrimSpace(l)
				if l != "" && !seen[l] {
					seen[l] = true
					labels = append(labels, l)
				}
			}
		}
	}
	host := url
	if u, err := neturl.Parse(url); err == nil && u.Host != "" {
		host = u.Host + u.Path
	}
	if len(labels) == 0 {
		return fmt.Sprintf("on %s, no clickable controls visible yet", orDefaultStr(host, "(blank)"))
	}
	if len(labels) > 6 {
		labels = labels[:6]
	}
	return fmt.Sprintf("on %s, buttons: %s", orDefaultStr(host, "(blank)"), strings.Join(labels, ", "))
}

// readAllText concatenates visible body text across all frames (lowercased).
func readAllText(scopes []playwright.Frame) string {
	var b strings.Builder
	for _, f := range scopes {
		raw, err := f.Evaluate(`() => document.body ? document.body.innerText.toLowerCase() : ""`)
		if err == nil && raw != nil {
			b.WriteString(fmt.Sprint(raw))
			b.WriteString("\n")
		}
	}
	return b.String()
}

func includesAnyText(text string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// ---- Selector sets (Playwright selector syntax; ported from the reference) ----

const emailSelector = `input[type="email"], input[autocomplete="username"], #identifierId, input[name="identifier"]`
const passwordSelector = `input[type="password"], input[name="Passwd"]`

var nextButtonSelectors = []string{
	`#identifierNext button`,
	`#passwordNext button`,
	`#identifierNext`,
	`#passwordNext`,
	`button:has-text("Next")`,
	`button:has-text("Continue")`,
	`button:has-text("Berikutnya")`,
	`button:has-text("Lanjutkan")`,
	`button[type="submit"]`,
	`input[type="submit"]`,
}

var googleLoginSelectors = []string{
	`#social-google`,
	`a#social-google`,
	`a[href*="broker/google/login" i]`,
	`[data-provider*="google" i]`,
	`a:has-text("Sign up with Google")`,
	`a:has-text("Log in with Google")`,
	`button:has-text("Sign up with Google")`,
	`button:has-text("Log in with Google")`,
	`button:has-text("Continue with Google")`,
	`:has-text("Sign up with Google")`,
	`:has-text("Log in with Google")`,
}

var termsCheckboxSelectors = []string{
	`#agree-policy-account`,
	`#agree-policy`,
	`#agree-policy-sso`,
	`input[type="checkbox"][id*="agree" i]`,
	`input[type="checkbox"][id*="policy" i]`,
	`input[type="checkbox"][name*="agree" i]`,
	`.policy-wrapper input[type="checkbox"]`,
	`.custom-checkbox input[type="checkbox"]`,
	`input[type="checkbox"]`,
}

var approveButtonSelectors = []string{
	`#submit_approve_access`,
	`#submit_approve_access button`,
	`button:has-text("Allow")`,
	`button:has-text("Continue")`,
	`button:has-text("Accept")`,
	`button:has-text("Authorize")`,
	`button:has-text("Agree")`,
	`button:has-text("Izinkan")`,
	`button:has-text("Lanjutkan")`,
	`button:has-text("Setuju")`,
	`div[role="button"]:has-text("Allow")`,
	`div[role="button"]:has-text("Continue")`,
	`input[type="submit"][value="Allow" i]`,
}

var privacyConfirmSelectors = []string{
	`.ui-dialog button.confirm`,
	`.dialog button.confirm`,
	`button:has-text("Confirm")`,
	`button:has-text("I agree")`,
	`button:has-text("Agree")`,
	`button:has-text("同意")`,
	`button:has-text("确认")`,
}

var invalidCredentialMarkers = []string{
	"wrong password", "incorrect password", "couldn't find your google account",
	"couldn’t find your google account", "enter a valid email", "couldn't sign you in",
	"couldn’t sign you in", "invalid email or password", "password is incorrect",
}

var manualAssistMarkers = []string{
	"2-step verification", "verify it's you", "verify it’s you",
	"check your phone", "confirm it's you", "confirm it’s you",
	"recovery email", "recovery phone", "captcha", "unusual activity",
	"suspicious", "try again later",
}

// captureWebCookie navigates to the web console (so the session cookies are set
// for the codebuddy.ai domain) and serializes them into a single Cookie header
// string. Returns "" if no useful cookie is found — non-fatal.
func captureWebCookie(ctx context.Context, session *Session, onStep progressFn) string {
	page := session.Page()
	// Land on a console page so the SSO session materializes as site cookies.
	_, _ = page.Goto("https://www.codebuddy.ai/home", playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(20000),
	})
	page.WaitForTimeout(1500)

	cookies, err := session.Cookies("https://www.codebuddy.ai", "https://codebuddy.ai")
	if err != nil || len(cookies) == 0 {
		// Fall back to the whole-context cookie jar.
		all, e2 := session.Cookies()
		if e2 != nil {
			logger.Warnf("[CodeBuddy] cookie capture failed: %v", err)
			return ""
		}
		cookies = all
	}

	type kv struct{ name, value string }
	var useful []kv
	seen := map[string]bool{}
	for _, c := range cookies {
		domain := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		isCB := false
		for _, d := range codeBuddyCookieDomains {
			if domain == d || strings.HasSuffix(domain, ".codebuddy.ai") {
				isCB = true
				break
			}
		}
		if !isCB || c.Name == "" || c.Value == "" || seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		useful = append(useful, kv{c.Name, c.Value})
	}
	if len(useful) == 0 {
		onStep("cookie_missing", "No web session cookie captured (quota tracking will need a manual cookie)")
		return ""
	}
	sort.Slice(useful, func(i, j int) bool { return useful[i].name < useful[j].name })
	parts := make([]string, 0, len(useful))
	for _, c := range useful {
		parts = append(parts, c.name+"="+c.value)
	}
	onStep("cookie_captured", fmt.Sprintf("Captured web session (%d cookies)", len(useful)))
	return strings.Join(parts, "; ")
}
