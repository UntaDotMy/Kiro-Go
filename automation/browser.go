// Package automation drives a real browser to log into provider web consoles and
// capture the credentials the HTTP-only flows cannot reach.
//
// Why a browser at all: some providers (CodeBuddy first) gate their login behind a
// Keycloak SSO form that the CLI OAuth token cannot complete on its own. To
// register an account we have to actually sign in to the web console.
//
// BROWSER: Camoufox (github.com/daijro/camoufox) — a custom Firefox build that
// patches anti-fingerprinting at the C++ ENGINE level, not via injected JavaScript.
// We switched off vanilla Chromium because Chrome driven over CDP carries the
// well-known Runtime.enable execution-context leak and a JS-patched fingerprint
// that a determined detector can still unmask; Camoufox instead (a) emits a genuine
// Firefox TLS/JA3 handshake, (b) has no CDP main-world leak, and (c) spoofs
// navigator/screen/WebGL/canvas/audio where Firefox actually produces them, so from
// page JS there is no overridden getter to find.
//
// Camoufox speaks Playwright's Firefox (Juggler) protocol, so playwright-go drives
// it with pw.Firefox.Launch(ExecutablePath=<camoufox>). The per-session fingerprint
// is passed to the binary as chunked CAMOU_CONFIG_* env vars (compact UTF-8 JSON).
// Because the binary reads that config ONCE per process (std::call_once), each
// account login gets its OWN Camoufox launch — one process per Session — so
// concurrent logins present distinct, isolated fingerprints.
package automation

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"

	"kiro-go/logger"

	"github.com/playwright-community/playwright-go"
)

// installOnce ensures the Playwright DRIVER (the Node + cli.js transport) is
// present. We do NOT install a Playwright-managed browser — Camoufox is a separate
// binary the operator supplies (CAMOUFOX_PATH). SkipInstallBrowsers gets just the
// driver so playwright-go can speak the Firefox protocol to Camoufox.
var (
	installOnce sync.Once
	installErr  error
)

func ensureInstalled() error {
	installOnce.Do(func() {
		logger.Infof("[Automation] ensuring Playwright driver is installed (no browser download; Camoufox is supplied separately)…")
		installErr = playwright.Install(&playwright.RunOptions{
			SkipInstallBrowsers: true,
			Verbose:             false,
		})
		if installErr != nil {
			logger.Errorf("[Automation] Playwright driver install failed: %v", installErr)
		}
	})
	return installErr
}

// resolveCamoufoxPath finds the Camoufox executable. Order:
//  1. $CAMOUFOX_PATH (explicit operator override — the launcher binary)
//  2. a few conventional locations
//
// Camoufox is distributed as a portable bundle from its GitHub releases; the
// operator unpacks it and points CAMOUFOX_PATH at the executable (camoufox on
// Linux/macOS, camoufox.exe on Windows). We never auto-download it.
func resolveCamoufoxPath() (string, error) {
	if p := os.Getenv("CAMOUFOX_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("CAMOUFOX_PATH=%q does not exist", p)
	}
	candidates := []string{
		"/usr/local/bin/camoufox",
		"/opt/camoufox/camoufox",
		"/app/camoufox/camoufox",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("Camoufox binary not found: set CAMOUFOX_PATH to the Camoufox executable (download from https://github.com/daijro/camoufox/releases)")
}

// Fingerprint is one COHERENT per-session Firefox-on-Windows device persona. The
// rule from fingerprint research holds for Camoufox too: random-but-incoherent
// signals are MORE detectable than none, because detectors score cross-signal
// consistency. So we never randomize axes independently — we pick one complete
// persona where every field already agrees, and let Camoufox apply it at the engine
// level.
//
// Personas are Firefox on Windows (the dominant real-user identity for this UA
// family). Note the Firefox-specific shape vs the old Chrome path:
//   - UA is a Firefox/Gecko UA, NOT Chrome; oscpu is set (Firefox-only property)
//   - there is NO deviceMemory (Firefox does not implement navigator.deviceMemory;
//     spoofing it would itself be a leak)
//   - WebGL on Firefox-Windows still routes through ANGLE→Direct3D11
//   - canvas/audio/font noise is done by Camoufox via per-session SEEDS, not by us
type Fingerprint struct {
	UserAgent      string
	OSCPU          string // navigator.oscpu (Firefox-only): "Windows NT 10.0; Win64; x64"
	Platform       string // navigator.platform: "Win32"
	AcceptLanguage string // e.g. "en-US"
	Locale         string // BCP-47, e.g. "en-US"
	Timezone       string // IANA, e.g. "America/New_York"

	HardwareCores int     // navigator.hardwareConcurrency ∈ {4,8,12,16}
	ScreenW       int     // screen.width (>= viewport width)
	ScreenH       int     // screen.height
	DPR           float64 // window.devicePixelRatio
	Width         int     // viewport / inner width
	Height        int     // viewport / inner height

	WebGLVendor   string // webGl:vendor
	WebGLRenderer string // webGl:renderer (ANGLE/D3D11 on Windows)

	// Per-session randomization seeds — Camoufox's DESIGNED-SAFE way to make canvas,
	// audio, and font-spacing fingerprints unique per session without breaking
	// coherence. Different every session.
	CanvasSeed int
	AudioSeed  int
	FontSeed   int
}

// firefoxPersona is a complete, internally-consistent Firefox-on-Windows profile.
type firefoxPersona struct {
	ffVersion     string // Firefox version, e.g. "128.0"
	cores         int
	screenW       int
	screenH       int
	dpr           float64
	webglVendor   string
	webglRenderer string
}

// firefoxPersonas are curated Firefox-on-Windows profiles, each self-consistent:
// Win32 platform + matching oscpu, realistic core counts, ANGLE/Direct3D11 WebGL
// (the real Firefox-on-Windows graphics path), DPR matching the resolution. Verify
// the WebGL strings against creepjs with a real Camoufox build before relying on
// them; they follow the documented Firefox-on-Windows ANGLE shape.
var firefoxPersonas = []firefoxPersona{
	{"128.0", 8, 1920, 1080, 1.0, "Google Inc.", "ANGLE (NVIDIA, NVIDIA GeForce RTX 3060 Direct3D11 vs_5_0 ps_5_0, D3D11)"},
	{"128.0", 16, 2560, 1440, 1.0, "Google Inc.", "ANGLE (NVIDIA, NVIDIA GeForce RTX 2060 SUPER Direct3D11 vs_5_0 ps_5_0, D3D11)"},
	{"127.0", 8, 1536, 864, 1.25, "Google Inc.", "ANGLE (Intel, Intel(R) Iris(R) Xe Graphics Direct3D11 vs_5_0 ps_5_0, D3D11)"},
	{"128.0", 12, 1920, 1080, 1.0, "Google Inc.", "ANGLE (AMD, AMD Radeon RX 6600 Direct3D11 vs_5_0 ps_5_0, D3D11)"},
	{"126.0", 4, 1366, 768, 1.0, "Google Inc.", "ANGLE (Intel, Intel(R) UHD Graphics 630 Direct3D11 vs_5_0 ps_5_0, D3D11)"},
	{"128.0", 8, 1920, 1080, 1.0, "Google Inc.", "ANGLE (Intel, Intel(R) UHD Graphics 770 Direct3D11 vs_5_0 ps_5_0, D3D11)"},
}

// fingerprintLocales: locale + timezone must agree, and at runtime SHOULD also
// agree with the proxy/IP geolocation (a timezone↔IP mismatch is one of the
// strongest proxy tells).
var fingerprintLocales = []struct{ lang, tz string }{
	{"en-US", "America/New_York"},
	{"en-US", "America/Los_Angeles"},
	{"en-US", "America/Chicago"},
	{"en-GB", "Europe/London"},
}

// NewFingerprint returns ONE coherent Firefox-on-Windows persona for a session.
func NewFingerprint() Fingerprint {
	p := firefoxPersonas[rand.Intn(len(firefoxPersonas))]
	loc := fingerprintLocales[rand.Intn(len(fingerprintLocales))]

	// Viewport sits inside the screen (room for browser chrome).
	vw := p.screenW
	vh := p.screenH - 110
	if vh < 600 {
		vh = p.screenH - 80
	}

	ua := fmt.Sprintf("Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:%s) Gecko/20100101 Firefox/%s", p.ffVersion, p.ffVersion)
	return Fingerprint{
		UserAgent:      ua,
		OSCPU:          "Windows NT 10.0; Win64; x64",
		Platform:       "Win32",
		AcceptLanguage: loc.lang,
		Locale:         loc.lang,
		Timezone:       loc.tz,
		HardwareCores:  p.cores,
		ScreenW:        p.screenW,
		ScreenH:        p.screenH,
		DPR:            p.dpr,
		Width:          vw,
		Height:         vh,
		WebGLVendor:    p.webglVendor,
		WebGLRenderer:  p.webglRenderer,
		CanvasSeed:     rand.Intn(1 << 30),
		AudioSeed:      rand.Intn(1 << 30),
		FontSeed:       rand.Intn(1 << 30),
	}
}

func (f Fingerprint) withDefaults() Fingerprint {
	if f.UserAgent == "" {
		return NewFingerprint()
	}
	if f.OSCPU == "" {
		f.OSCPU = "Windows NT 10.0; Win64; x64"
	}
	if f.Platform == "" {
		f.Platform = "Win32"
	}
	if f.AcceptLanguage == "" {
		f.AcceptLanguage = "en-US"
	}
	if f.Locale == "" {
		f.Locale = f.AcceptLanguage
	}
	if f.Timezone == "" {
		f.Timezone = "America/New_York"
	}
	if f.HardwareCores == 0 {
		f.HardwareCores = 8
	}
	if f.ScreenW == 0 || f.ScreenH == 0 {
		f.ScreenW, f.ScreenH = 1920, 1080
	}
	if f.DPR == 0 {
		f.DPR = 1.0
	}
	if f.Width == 0 || f.Height == 0 {
		f.Width, f.Height = 1920, 970
	}
	return f
}

// camouConfig builds the Camoufox config map for this persona — the exact dotted/
// colon key names the binary validates against settings/properties.json. Only keys
// we set are masked; everything else falls through to genuine Firefox values.
func (f Fingerprint) camouConfig() map[string]interface{} {
	availW := f.ScreenW
	availH := f.ScreenH - 48 // typical Windows taskbar
	cfg := map[string]interface{}{
		"navigator.userAgent":           f.UserAgent,
		"navigator.oscpu":               f.OSCPU,
		"navigator.platform":            f.Platform,
		"navigator.hardwareConcurrency": f.HardwareCores,
		"navigator.language":            f.Locale,
		"navigator.languages":           langList(f.Locale),

		"screen.width":       f.ScreenW,
		"screen.height":      f.ScreenH,
		"screen.availWidth":  availW,
		"screen.availHeight": availH,
		"screen.colorDepth":  24,
		"screen.pixelDepth":  24,

		"window.innerWidth":       f.Width,
		"window.innerHeight":      f.Height,
		"window.outerWidth":       availW,
		"window.outerHeight":      availH,
		"window.devicePixelRatio": f.DPR,
		"window.screenX":          0,
		"window.screenY":          0,

		"headers.User-Agent":      f.UserAgent,
		"headers.Accept-Language": f.AcceptLanguage + ",en;q=0.9",

		"timezone":      f.Timezone,
		"locale:all":    f.Locale,
		"locale:region": localeRegion(f.Locale),

		// Per-session seeds — Camoufox's safe canvas/audio/font randomization.
		"canvas:seed":        f.CanvasSeed,
		"audio:seed":         f.AudioSeed,
		"fonts:spacing_seed": f.FontSeed,
	}
	// WebGL strings only (numeric params left to genuine Firefox for consistency).
	if f.WebGLVendor != "" && f.WebGLRenderer != "" {
		cfg["webGl:vendor"] = f.WebGLVendor
		cfg["webGl:renderer"] = f.WebGLRenderer
	}
	return cfg
}

func langList(locale string) []string {
	if i := strings.Index(locale, "-"); i > 0 {
		return []string{locale, locale[:i]}
	}
	return []string{locale}
}

func localeRegion(locale string) string {
	if i := strings.Index(locale, "-"); i > 0 && i+1 < len(locale) {
		return locale[i+1:]
	}
	return "US"
}

// camouEnv chunks the config JSON into CAMOU_CONFIG_1..N exactly as Camoufox's
// launcher does: compact UTF-8 JSON, ≤2047 chars/chunk on Windows (≤32767
// elsewhere), 1-indexed contiguous variable names. The binary concatenates the
// chunks in order, validates, and parses.
func camouEnv(cfg map[string]interface{}) (map[string]string, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal camoufox config: %w", err)
	}
	s := string(raw)
	chunk := 32767
	// Windows env-var size limit is smaller.
	if isWindows() {
		chunk = 2047
	}
	env := map[string]string{}
	// Chunk by rune to stay byte-exact across multibyte boundaries (the binary just
	// concatenates, so any boundary works as long as it reassembles the same JSON).
	runes := []rune(s)
	idx := 1
	for i := 0; i < len(runes); i += chunk {
		end := i + chunk
		if end > len(runes) {
			end = len(runes)
		}
		env[fmt.Sprintf("CAMOU_CONFIG_%d", idx)] = string(runes[i:end])
		idx++
	}
	if idx == 1 { // empty config edge case
		env["CAMOU_CONFIG_1"] = s
	}
	return env, nil
}

func isWindows() bool { return strings.Contains(strings.ToLower(os.Getenv("OS")), "windows") || os.PathSeparator == '\\' }

// EngineOptions configures the per-job browser launches.
type EngineOptions struct {
	// Headless runs Camoufox with no visible window. Bulk runs headless; an operator
	// can force a visible window for debugging/manual assist.
	Headless bool
	// ProxyURL routes all browser traffic through a proxy. Empty = direct. Strongly
	// recommended: against a post-login risk engine, IP reputation matters more than
	// any fingerprint tweak.
	ProxyURL string
}

// Engine holds the shared Playwright driver and the launch config. It does NOT hold
// a browser — each Session launches its own Camoufox process so per-account
// fingerprints are independent (Camoufox reads its config once per process).
type Engine struct {
	pw           *playwright.Playwright
	camoufoxPath string
	headless     bool
	proxyURL     string
}

// StartEngine installs the driver (if needed), resolves the Camoufox binary, and
// starts Playwright. Browsers are launched lazily per Session.
func StartEngine(opts EngineOptions) (*Engine, error) {
	if err := ensureInstalled(); err != nil {
		return nil, fmt.Errorf("playwright not available: %w", err)
	}
	camoufox, err := resolveCamoufoxPath()
	if err != nil {
		return nil, err
	}
	pw, err := playwright.Run()
	if err != nil {
		return nil, fmt.Errorf("start playwright: %w", err)
	}
	return &Engine{pw: pw, camoufoxPath: camoufox, headless: opts.Headless, proxyURL: opts.ProxyURL}, nil
}

// Close stops Playwright. Per-session browsers are closed by Session.Close().
func (e *Engine) Close() {
	if e == nil {
		return
	}
	if e.pw != nil {
		_ = e.pw.Stop()
	}
}

// Session is one isolated Camoufox browser + context + page — one per account
// login. It owns its OWN browser process (so the per-session fingerprint applies),
// torn down in Close().
type Session struct {
	browser playwright.Browser
	ctx     playwright.BrowserContext
	page    playwright.Page
	fp      Fingerprint
	mu      sync.RWMutex // guards closed; serializes Close vs readers
	closed  bool
}

// NewSession launches a fresh Camoufox process carrying this fingerprint (via
// CAMOU_CONFIG_* env), then opens a context + page in it.
func (e *Engine) NewSession(fp Fingerprint) (*Session, error) {
	f := fp.withDefaults()
	env, err := camouEnv(f.camouConfig())
	if err != nil {
		return nil, err
	}

	launch := playwright.BrowserTypeLaunchOptions{
		ExecutablePath: playwright.String(e.camoufoxPath),
		Headless:       playwright.Bool(e.headless),
		Env:            env,
		// Keep WebGL enabled so the spoofed renderer is actually exercised (Camoufox
		// recommends this when spoofing WebGL).
		FirefoxUserPrefs: map[string]interface{}{
			"webgl.force-enabled": true,
		},
	}
	if e.proxyURL != "" {
		launch.Proxy = &playwright.Proxy{Server: e.proxyURL}
	}

	browser, err := e.pw.Firefox.Launch(launch)
	if err != nil {
		return nil, fmt.Errorf("launch camoufox: %w (is CAMOUFOX_PATH a valid Camoufox build matching the Playwright driver version?)", err)
	}

	// One context; Camoufox already applies the fingerprint at the engine level, so
	// we set only the viewport/locale/timezone Playwright needs to stay consistent.
	bctx, err := browser.NewContext(playwright.BrowserNewContextOptions{
		Viewport:   &playwright.Size{Width: f.Width, Height: f.Height},
		Locale:     playwright.String(f.Locale),
		TimezoneId: playwright.String(f.Timezone),
	})
	if err != nil {
		_ = browser.Close()
		return nil, fmt.Errorf("new context: %w", err)
	}
	page, err := bctx.NewPage()
	if err != nil {
		_ = browser.Close()
		return nil, fmt.Errorf("new page: %w", err)
	}
	// Bound Playwright's action/navigation timeouts so a frame mid-load can't block a
	// per-tick probe for the full 30s default (keeps the login loop responsive).
	bctx.SetDefaultTimeout(4000)
	bctx.SetDefaultNavigationTimeout(60000)

	return &Session{browser: browser, ctx: bctx, page: page, fp: f}, nil
}

// Page returns the session's page, or nil once closed. Callers must nil-check.
func (s *Session) Page() playwright.Page {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil
	}
	return s.page
}

// OnGoogle reports whether the session's page is currently on a Google domain.
// Used to SUPPRESS preview screenshots there — the Google password screen shows the
// typed password, and a JPEG of it would leak the credential into the dashboard
// snapshot. Fail safe: nil/closed reports true (don't shoot).
func (s *Session) OnGoogle() bool {
	if s == nil {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.page == nil {
		return true
	}
	u := strings.ToLower(s.page.URL())
	return strings.Contains(u, "google.com") || strings.Contains(u, "gstatic.com")
}

// Close tears down the context AND the Camoufox browser process. Idempotent and
// race-safe: marks closed under the lock (readers bail) without nilling fields.
func (s *Session) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	already := s.closed
	s.closed = true
	browser := s.browser
	s.mu.Unlock()
	if already {
		return
	}
	if browser != nil {
		_ = browser.Close()
	}
}

// Cookies returns the context's cookies for the given URLs (all if none given).
func (s *Session) Cookies(urls ...string) ([]playwright.Cookie, error) {
	if s == nil {
		return nil, fmt.Errorf("no session")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.ctx == nil {
		return nil, fmt.Errorf("no session")
	}
	return s.ctx.Cookies(urls...)
}

// Screenshot captures the page's current viewport as a JPEG at the given quality.
func Screenshot(page playwright.Page, quality int) ([]byte, error) {
	if page == nil {
		return nil, fmt.Errorf("nil page")
	}
	if quality <= 0 || quality > 100 {
		quality = 50
	}
	return page.Screenshot(playwright.PageScreenshotOptions{
		Type:    playwright.ScreenshotTypeJpeg,
		Quality: playwright.Int(quality),
	})
}

// scopesOf returns every frame to search this tick — the main frame plus every
// child frame (Keycloak login form, Google, etc).
func scopesOf(page playwright.Page) []playwright.Frame {
	if page == nil {
		return nil
	}
	return page.Frames()
}

// frameURL returns a frame's URL, lowercased, best-effort.
func frameURL(f playwright.Frame) string {
	if f == nil {
		return ""
	}
	return strings.ToLower(f.URL())
}
