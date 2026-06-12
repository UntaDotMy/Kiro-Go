// Command fpverify uses the PRODUCTION automation Engine/Session to open a real
// browser session and dump the fingerprint the page actually observes — proving
// the coherent persona is applied consistently (navigator.platform, cores, memory,
// screen, DPR, WebGL vendor/renderer, webdriver, languages). Run a few times to see
// the per-session randomization.
//
// Run:  go run ./cmd/fpverify          (headless)
//       go run ./cmd/fpverify -show     (visible)
package main

import (
	"flag"
	"fmt"
	"os"

	"kiro-go/automation"
)

func main() {
	show := flag.Bool("show", false, "visible window")
	flag.Parse()

	eng, err := automation.StartEngine(automation.EngineOptions{Headless: !*show})
	if err != nil {
		fmt.Println("engine:", err)
		os.Exit(1)
	}
	defer eng.Close()

	// Two sessions to show per-session randomization differs but each is coherent.
	for s := 1; s <= 2; s++ {
		sess, err := eng.NewSession(automation.NewFingerprint())
		if err != nil {
			fmt.Println("session:", err)
			os.Exit(1)
		}
		page := sess.Page()
		// data: URL so no network needed; the init script still runs.
		if _, err := page.Goto("about:blank"); err != nil {
			fmt.Println("goto:", err)
		}
		raw, err := page.Evaluate(`() => {
			let webgl = {};
			try {
				const c = document.createElement('canvas');
				const gl = c.getContext('webgl') || c.getContext('experimental-webgl');
				webgl.vendor = gl.getParameter(37445);
				webgl.renderer = gl.getParameter(37446);
			} catch (e) { webgl.err = String(e); }
			return JSON.stringify({
				webdriver: navigator.webdriver,
				platform: navigator.platform,
				cores: navigator.hardwareConcurrency,
				memory: navigator.deviceMemory,
				languages: navigator.languages,
				vendor: navigator.vendor,
				ua: navigator.userAgent,
				screen: { w: screen.width, h: screen.height, availH: screen.availHeight },
				inner: { w: window.innerWidth, h: window.innerHeight },
				outer: { w: window.outerWidth, h: window.outerHeight },
				dpr: window.devicePixelRatio,
				webgl,
				plugins: navigator.plugins.length,
				tz: Intl.DateTimeFormat().resolvedOptions().timeZone,
			}, null, 2);
		}`)
		if err != nil {
			fmt.Println("eval:", err)
		} else {
			fmt.Printf("==== SESSION %d (as the page sees it) ====\n%v\n\n", s, raw)
		}
		sess.Close()
	}
}
