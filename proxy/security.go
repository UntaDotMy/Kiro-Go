package proxy

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// This file centralizes the HTTP security posture for a public VPS
// deployment: per-path CORS scoping, security response headers, and a
// path-jailed static file server. It was added during the production
// hardening pass; see the security audit notes in the repo history.
//
// Design:
//   - The inference API (/v1/*, /messages, /chat/completions, /responses,
//     /models) is a genuinely public, cross-origin surface consumed by SDKs,
//     so it keeps the permissive CORS wildcard.
//   - The admin panel, customer portal, landing page, and health endpoint are
//     NOT cross-origin surfaces. They get same-origin treatment (no ACAO
//     wildcard) plus hardening headers (CSP, X-Frame-Options, nosniff, ...).
//   - The customer key-status endpoint is a public, key-authenticated read API
//     (like /v1/models) so it keeps wildcard CORS.

// inferenceCORSPaths are the request paths that legitimately need a wildcard
// CORS origin because browser-based SDKs call them cross-origin. Everything
// else (admin, portal, landing, health) is same-origin and must not advertise
// itself as freely readable cross-origin.
func isInferenceCORSPath(path string) bool {
	switch path {
	case "/v1/messages", "/messages", "/anthropic/v1/messages",
		"/v1/messages/count_tokens", "/messages/count_tokens",
		"/v1/chat/completions", "/chat/completions",
		"/v1/responses", "/responses", "/openai/v1/responses", "/backend-api/codex/responses",
		"/v1/models", "/models",
		"/v1/key-status", "/portal/api/key-status":
		return true
	}
	return false
}

// setCORSHeaders applies CORS headers scoped to the request path. The inference
// surface keeps the wildcard it needs for SDK callers; the admin/portal/landing
// surfaces are same-origin and receive no wildcard (a cross-origin page can no
// longer read their responses). Returns true if the caller should short-circuit
// an OPTIONS preflight with 204.
func setCORSHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	if isInferenceCORSPath(r.URL.Path) {
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, anthropic-version, anthropic-beta, x-api-key, x-stainless-os, x-stainless-lang, x-stainless-package-version, x-stainless-runtime, x-stainless-runtime-version, x-stainless-arch")
		h.Set("Access-Control-Expose-Headers", "x-request-id, x-ratelimit-limit-requests, x-ratelimit-limit-tokens, x-ratelimit-remaining-requests, x-ratelimit-remaining-tokens, x-ratelimit-reset-requests, x-ratelimit-reset-tokens")
		h.Set("Vary", "Origin")
		return
	}
	// Admin / portal / landing / health: same-origin only. We intentionally
	// emit NO Access-Control-Allow-Origin so a cross-origin page cannot read
	// the response. Same-origin requests (the actual UI) don't need ACAO.
	// Admin mutating verbs are still allowed for same-origin XHR.
	h.Set("Vary", "Origin")
}

// adminCSP is the Content-Security-Policy for the bundled single-file UIs
// (admin, portal, landing). Those pages inline their CSS and JS (and the admin
// uses inline event handlers), so 'unsafe-inline' is required for script-src
// and style-src — refactoring to external assets with nonces is a larger change
// tracked separately. The rest of the policy is tight: no plugins, framing
// denied, base-uri locked, connections limited to self + the WebSocket dashboard
// + the GitHub raw endpoint the admin update-check fetches.
// connect-src is 'self' (which covers same-origin ws:// and wss:// for the
// dashboard WebSocket) plus the GitHub raw host the admin update-check fetches.
// We deliberately do NOT use the bare `ws:`/`wss:` scheme tokens — those match
// ANY WebSocket host and would let an XSS payload exfiltrate the dashboard
// snapshot to an attacker-controlled server.
const adminCSP = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self' data:; " +
	"connect-src 'self' https://raw.githubusercontent.com; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// setSecurityHeaders applies hardening headers. The HTML UI surfaces
// (admin/portal/landing) get the full set including CSP + X-Frame-Options. The
// JSON API surfaces get the cheap, always-safe subset (nosniff + referrer
// policy). HSTS is emitted only when the request arrived over TLS (or through a
// TLS-terminating proxy that set X-Forwarded-Proto=https) so a plain-HTTP local
// install isn't forced onto HTTPS it can't serve.
func setSecurityHeaders(w http.ResponseWriter, r *http.Request, isHTMLSurface bool) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")

	if isHTMLSurface {
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", adminCSP)
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), payment=()")
	}

	if requestIsHTTPS(r) {
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
}

// requestIsHTTPS reports whether the request reached us over TLS, either
// directly (r.TLS set) or via a trusted TLS-terminating reverse proxy that set
// X-Forwarded-Proto=https. We only trust the forwarded header when the immediate
// peer is in the KIRO_TRUSTED_PROXIES allowlist (same gate clientIP uses), so a
// direct client can't spoof HSTS emission.
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if peerIsTrustedProxy(r) {
		if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			return true
		}
	}
	return false
}

// isHTMLSurfacePath reports whether the path serves one of the bundled HTML UIs
// (landing, admin, portal) — used to decide whether to attach the full CSP +
// frame-deny header set. The admin/portal API JSON routes are deliberately
// excluded: they get only the cheap nosniff/referrer headers.
func isHTMLSurfacePath(path string) bool {
	switch {
	case path == "/" :
		return true
	case path == "/admin" || path == "/admin/":
		return true
	case path == "/portal" || path == "/portal/":
		return true
	case strings.HasPrefix(path, "/admin/") && !strings.HasPrefix(path, "/admin/api/") && path != "/admin/ws/status":
		return true
	case strings.HasPrefix(path, "/portal/") && !strings.HasPrefix(path, "/portal/api/"):
		return true
	}
	return false
}

// webRoot is the absolute path of the web/ asset directory, resolved once at
// startup against the process working directory. Resolving it per-request (the
// prior form) cost an os.Getwd() syscall on every static hit and made the jail
// check depend on a cwd that could in principle change after start. The cwd is
// fixed at process start, so caching is both faster and more robust.
var (
	webRootOnce sync.Once
	webRootAbs  string
	webRootErr  error
)

func resolvedWebRoot() (string, error) {
	webRootOnce.Do(func() {
		webRootAbs, webRootErr = filepath.Abs("web")
	})
	return webRootAbs, webRootErr
}

// serveJailedStaticFile serves a file from the web/ directory, refusing any path
// that escapes it. Go's http.ServeFile already rejects request paths containing
// ".." segments with 400, so this is defense-in-depth: it guarantees the
// resolved absolute path stays under web/ even if a future caller constructs the
// path differently. stripPrefix is the request path prefix to strip (e.g. "/admin/").
func (h *Handler) serveJailedStaticFile(w http.ResponseWriter, r *http.Request, stripPrefix string) {
	rel := strings.TrimPrefix(r.URL.Path, stripPrefix)
	// Clean to an absolute-rooted path then drop the leading separator so a
	// value like "../x" collapses to "x" rather than escaping web/.
	cleaned := filepath.Clean("/" + rel)
	abs := filepath.Join("web", cleaned)
	webRoot, err := resolvedWebRoot()
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	absResolved, err := filepath.Abs(abs)
	if err != nil || (absResolved != webRoot && !strings.HasPrefix(absResolved, webRoot+string(os.PathSeparator))) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	http.ServeFile(w, r, abs)
}
