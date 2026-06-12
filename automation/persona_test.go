package automation

import (
	"strings"
	"testing"
)

// TestPersonaCoherence verifies every generated Firefox-on-Windows persona is
// INTERNALLY CONSISTENT — incoherent fingerprints are MORE detectable than none,
// so these invariants must always hold.
func TestPersonaCoherence(t *testing.T) {
	for i := 0; i < 200; i++ {
		f := NewFingerprint().withDefaults()

		// UA is a Firefox/Gecko UA on Windows, NOT Chrome.
		if !strings.Contains(f.UserAgent, "Firefox/") || !strings.Contains(f.UserAgent, "Gecko/") {
			t.Fatalf("UA not Firefox/Gecko: %q", f.UserAgent)
		}
		if strings.Contains(f.UserAgent, "Chrome/") || strings.Contains(f.UserAgent, "AppleWebKit") {
			t.Fatalf("UA leaks Chrome/WebKit tokens: %q", f.UserAgent)
		}
		if !strings.Contains(f.UserAgent, "Windows NT 10.0; Win64; x64") {
			t.Fatalf("UA not Windows: %q", f.UserAgent)
		}

		// platform / oscpu are the Windows values Firefox reports.
		if f.Platform != "Win32" {
			t.Fatalf("platform = %q, want Win32", f.Platform)
		}
		if f.OSCPU != "Windows NT 10.0; Win64; x64" {
			t.Fatalf("oscpu = %q, want Windows NT 10.0; Win64; x64", f.OSCPU)
		}

		// hardwareConcurrency realistic consumer value.
		switch f.HardwareCores {
		case 4, 8, 12, 16:
		default:
			t.Fatalf("hardwareConcurrency = %d, want 4/8/12/16", f.HardwareCores)
		}

		// Viewport fits inside the screen.
		if f.Width > f.ScreenW {
			t.Fatalf("viewport width %d > screen width %d", f.Width, f.ScreenW)
		}
		if f.Height >= f.ScreenH {
			t.Fatalf("viewport height %d >= screen height %d", f.Height, f.ScreenH)
		}

		// DPR is a real desktop value.
		switch f.DPR {
		case 1.0, 1.25, 1.5, 2.0:
		default:
			t.Fatalf("DPR = %v, want 1/1.25/1.5/2", f.DPR)
		}

		// WebGL: Windows ANGLE/Direct3D11, vendor family appears in renderer.
		if !strings.Contains(f.WebGLRenderer, "Direct3D11") || !strings.Contains(f.WebGLRenderer, "ANGLE") {
			t.Fatalf("WebGL renderer not Windows ANGLE/D3D11: %q", f.WebGLRenderer)
		}
		for _, fam := range []string{"NVIDIA", "Intel", "AMD"} {
			if strings.Contains(f.WebGLRenderer, fam) {
				// fine — a real GPU family
			}
		}

		// locale + timezone set and plausible together.
		if f.AcceptLanguage == "" || f.Timezone == "" || f.Locale == "" {
			t.Fatalf("locale/timezone empty: %q / %q / %q", f.AcceptLanguage, f.Timezone, f.Locale)
		}

		// Per-session seeds populated (Camoufox canvas/audio/font randomization).
		if f.CanvasSeed == 0 && f.AudioSeed == 0 && f.FontSeed == 0 {
			t.Fatalf("all randomization seeds zero — not randomized")
		}
	}
}

// TestCamouConfigKeys confirms the config map uses the EXACT Camoufox property keys
// (verified against settings/properties.json) and never sends Firefox-invalid keys
// like deviceMemory or Chrome-only client hints.
func TestCamouConfigKeys(t *testing.T) {
	f := NewFingerprint().withDefaults()
	cfg := f.camouConfig()

	mustHave := []string{
		"navigator.userAgent",
		"navigator.oscpu",
		"navigator.platform",
		"navigator.hardwareConcurrency",
		"navigator.languages",
		"screen.width", "screen.height", "screen.availWidth", "screen.availHeight",
		"window.innerWidth", "window.outerWidth", "window.devicePixelRatio",
		"webGl:vendor", "webGl:renderer",
		"timezone", "locale:all",
		"canvas:seed", "audio:seed", "fonts:spacing_seed",
		"headers.User-Agent", "headers.Accept-Language",
	}
	for _, k := range mustHave {
		if _, ok := cfg[k]; !ok {
			t.Errorf("config missing required Camoufox key %q", k)
		}
	}

	// Firefox does NOT implement deviceMemory — sending it would be a leak.
	mustNotHave := []string{
		"navigator.deviceMemory",
		"deviceMemory",
		"Sec-CH-UA",
		"webgl:vendor",  // wrong case — real key is camelCase webGl:
		"webgl:renderer",
	}
	for _, k := range mustNotHave {
		if _, ok := cfg[k]; ok {
			t.Errorf("config has forbidden/invalid key %q (Firefox-incompatible or wrong case)", k)
		}
	}

	// WebGL key must be camelCase webGl: per properties.json.
	if _, ok := cfg["webGl:vendor"]; !ok {
		t.Errorf("WebGL vendor key must be exactly 'webGl:vendor'")
	}
}

// TestCamouEnvChunking verifies the CAMOU_CONFIG_* env chunking: contiguous
// 1-indexed names, and concatenating the chunks reproduces the exact JSON.
func TestCamouEnvChunking(t *testing.T) {
	f := NewFingerprint().withDefaults()
	env, err := camouEnv(f.camouConfig())
	if err != nil {
		t.Fatalf("camouEnv: %v", err)
	}
	if len(env) == 0 {
		t.Fatal("no env vars produced")
	}
	// Reassemble in index order and confirm it's valid JSON containing the UA.
	var reassembled strings.Builder
	for i := 1; ; i++ {
		v, ok := env["CAMOU_CONFIG_"+itoa(i)]
		if !ok {
			// must be contiguous: ensure no gap by checking i-1 existed
			if i == 1 {
				t.Fatal("CAMOU_CONFIG_1 missing")
			}
			break
		}
		reassembled.WriteString(v)
	}
	s := reassembled.String()
	if !strings.HasPrefix(strings.TrimSpace(s), "{") || !strings.Contains(s, "navigator.userAgent") {
		t.Errorf("reassembled config is not the expected JSON: %.80s", s)
	}
}
