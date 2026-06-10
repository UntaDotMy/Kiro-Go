package auth

import "testing"

// TestQwenBaseURLFromResource pins the resource_url -> OpenAI-compatible base
// normalization against qwen-code's getCurrentEndpoint(): empty falls back to the
// DashScope default; a bare host gets https:// prepended; /v1 is appended when
// absent; an already-complete base is returned unchanged.
func TestQwenBaseURLFromResource(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		{"   ", "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		{"portal.qwen.ai", "https://portal.qwen.ai/v1"},
		{"dashscope-intl.aliyuncs.com/compatible-mode", "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"},
		{"https://dashscope.aliyuncs.com/compatible-mode/v1", "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		{"https://example.com/compatible-mode/v1/", "https://example.com/compatible-mode/v1"},
		{"https://example.com/api", "https://example.com/api/v1"},
		{"http://insecure.example.com", "http://insecure.example.com/v1"},
	}
	for _, c := range cases {
		if got := QwenBaseURLFromResource(c.in); got != c.want {
			t.Errorf("QwenBaseURLFromResource(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseQwenTokenResponse covers the token-response shapes: a normal response,
// the resource_url vs endpoint fallback, and a missing access_token error.
func TestParseQwenTokenResponse(t *testing.T) {
	t.Run("full", func(t *testing.T) {
		body := []byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600,"resource_url":"portal.qwen.ai"}`)
		tok, err := parseQwenTokenResponse(body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tok.AccessToken != "at" || tok.RefreshToken != "rt" || tok.ExpiresIn != 3600 || tok.ResourceURL != "portal.qwen.ai" {
			t.Fatalf("parsed wrong: %+v", tok)
		}
	})
	t.Run("endpoint fallback", func(t *testing.T) {
		// When resource_url is absent, the endpoint field is used.
		body := []byte(`{"access_token":"at","endpoint":"dashscope-intl.aliyuncs.com/compatible-mode"}`)
		tok, err := parseQwenTokenResponse(body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tok.ResourceURL != "dashscope-intl.aliyuncs.com/compatible-mode" {
			t.Fatalf("endpoint fallback failed: %+v", tok)
		}
	})
	t.Run("missing access_token", func(t *testing.T) {
		if _, err := parseQwenTokenResponse([]byte(`{"refresh_token":"rt"}`)); err == nil {
			t.Fatal("expected error on missing access_token")
		}
	})
}

// TestQwenCodeChallengeIsS256URLSafe sanity-checks the PKCE pair: the verifier is
// url-safe base64 (no padding), and the challenge is a distinct url-safe digest.
func TestQwenCodeChallengeIsS256URLSafe(t *testing.T) {
	v := qwenCodeVerifier()
	if v == "" || len(v) < 40 {
		t.Fatalf("verifier too short: %q", v)
	}
	c := qwenCodeChallenge(v)
	if c == "" || c == v {
		t.Fatalf("challenge invalid (empty or equals verifier): %q", c)
	}
	for _, s := range []string{v, c} {
		for _, ch := range s {
			ok := (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_'
			if !ok {
				t.Fatalf("non-url-safe char %q in %q", ch, s)
			}
		}
	}
}
