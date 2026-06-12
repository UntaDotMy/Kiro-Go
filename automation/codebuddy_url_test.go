package automation

import (
	"net/url"
	"testing"
)

// TestNormalizeCodeBuddyAuthURL verifies the login URL is always rebuilt against
// the web login origin with /login + platform + state — the fix for logins that
// stalled on a non-login raw authUrl ("waiting for next screen" forever).
func TestNormalizeCodeBuddyAuthURL(t *testing.T) {
	cases := []struct {
		name        string
		backendHost string
		rawURL      string
		state       string
		wantHost    string
		wantPath    string
		wantState   string
		wantPlat    string
	}{
		{
			name:        "intl backend ignores raw api host",
			backendHost: "https://www.codebuddy.ai",
			rawURL:      "https://api.codebuddy.ai/v2/plugin/auth/redirect?platform=CLI&state=abc123",
			state:       "abc123",
			wantHost:    "www.codebuddy.ai",
			wantPath:    "/login",
			wantState:   "abc123",
			wantPlat:    "CLI",
		},
		{
			name:        "cn gateway host used when not codebuddy.ai",
			backendHost: "https://copilot.tencent.com",
			rawURL:      "",
			state:       "st-9",
			wantHost:    "copilot.tencent.com",
			wantPath:    "/login",
			wantState:   "st-9",
			wantPlat:    "CLI",
		},
		{
			name:        "pulls state+platform from raw url when state empty",
			backendHost: "https://www.codebuddy.ai",
			rawURL:      "https://whatever.example/login?platform=VSCODE&state=fromraw",
			state:       "",
			wantHost:    "www.codebuddy.ai",
			wantPath:    "/login",
			wantState:   "fromraw",
			wantPlat:    "VSCODE",
		},
		{
			name:        "empty everything falls back to codebuddy.ai",
			backendHost: "",
			rawURL:      "",
			state:       "",
			wantHost:    "www.codebuddy.ai",
			wantPath:    "/login",
			wantState:   "",
			wantPlat:    "CLI",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeCodeBuddyAuthURL(c.backendHost, c.rawURL, c.state)
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("result not a valid URL %q: %v", got, err)
			}
			if u.Host != c.wantHost {
				t.Errorf("host = %q, want %q (url=%s)", u.Host, c.wantHost, got)
			}
			if u.Path != c.wantPath {
				t.Errorf("path = %q, want %q (url=%s)", u.Path, c.wantPath, got)
			}
			if s := u.Query().Get("state"); s != c.wantState {
				t.Errorf("state = %q, want %q (url=%s)", s, c.wantState, got)
			}
			if p := u.Query().Get("platform"); p != c.wantPlat {
				t.Errorf("platform = %q, want %q (url=%s)", p, c.wantPlat, got)
			}
		})
	}
}
