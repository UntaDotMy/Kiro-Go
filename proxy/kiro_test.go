package proxy

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestNormalizeChunkBasicProgression(t *testing.T) {
	prev := ""

	if got := normalizeChunk("abc", &prev); got != "abc" {
		t.Fatalf("expected first chunk to pass through, got %q", got)
	}
	if got := normalizeChunk("abcde", &prev); got != "de" {
		t.Fatalf("expected appended delta, got %q", got)
	}
}

func TestNormalizeChunkPrefixRewindDoesNotReplay(t *testing.T) {
	prev := ""

	_ = normalizeChunk("abcde", &prev)
	if got := normalizeChunk("abc", &prev); got != "" {
		t.Fatalf("expected rewind chunk to be ignored, got %q", got)
	}
	if prev != "abcde" {
		t.Fatalf("expected previous snapshot to remain longest version, got %q", prev)
	}
	if got := normalizeChunk("abcdef", &prev); got != "f" {
		t.Fatalf("expected only unseen suffix after rewind, got %q", got)
	}
}

func TestNormalizeChunkPreservesNonOverlappingDeltas(t *testing.T) {
	// Regression: an earlier suffix-overlap heuristic in normalizeChunk would
	// strip leading characters from a fresh chunk whenever they coincidentally
	// matched the tail of the prior snapshot. That produced user-visible
	// truncations like "sleep" -> "slep" or "lets begin" -> "letsbegin" any
	// time a chunk boundary aligned with a repeated character or whitespace.
	// Each case below exercises a previously-buggy boundary; with the fix the
	// chunk must pass through verbatim.
	cases := []struct {
		name string
		prev string
		next string
		want string
	}{
		{"single trailing letter matches leading letter", "the e", "easy", "easy"},
		{"trailing space matches leading space", "lets ", " begin", " begin"},
		{"trailing space and char match leading", "halt ", " sleep", " sleep"},
		{"punctuation tail", "wow!", "!extra", "!extra"},
		{"unicode tail and head", "café", "éclair", "éclair"},
		{"long shared multi-rune tail does not eat delta", "abc xyz", "xyz123", "xyz123"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := tc.prev
			got := normalizeChunk(tc.next, &prev)
			if got != tc.want {
				t.Fatalf("normalizeChunk(%q, prev=%q) = %q, want %q",
					tc.next, tc.prev, got, tc.want)
			}
			if prev != tc.next {
				t.Fatalf("snapshot after delta = %q, want %q", prev, tc.next)
			}
		})
	}
}

func TestNormalizeChunkStillDedupesCumulativeReplay(t *testing.T) {
	// Confirm the cumulative/replay branches the heuristic was bolted onto
	// continue to work after the suffix-overlap removal.
	prev := ""
	if got := normalizeChunk("hello", &prev); got != "hello" {
		t.Fatalf("first chunk = %q, want %q", got, "hello")
	}
	if got := normalizeChunk("hello world", &prev); got != " world" {
		t.Fatalf("cumulative extension = %q, want %q", got, " world")
	}
	if got := normalizeChunk("hello world", &prev); got != "" {
		t.Fatalf("exact replay should be dropped, got %q", got)
	}
	if got := normalizeChunk("hello", &prev); got != "" {
		t.Fatalf("rewind should be dropped, got %q", got)
	}
	if prev != "hello world" {
		t.Fatalf("snapshot must keep longest version, got %q", prev)
	}
}

func TestBuildKiroTransportUsesExplicitProxyURL(t *testing.T) {
	transport := buildKiroTransport("http://proxy.local:8080")
	req := &http.Request{URL: mustParseURL(t, "https://q.us-east-1.amazonaws.com")}

	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("unexpected proxy error: %v", err)
	}
	assertProxyURL(t, got, "http://proxy.local:8080")
}

func TestBuildKiroTransportFallsBackToEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://env-proxy.local:2323")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	transport := buildKiroTransport("")
	req := &http.Request{URL: mustParseURL(t, "https://q.us-east-1.amazonaws.com")}

	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("unexpected proxy error: %v", err)
	}
	assertProxyURL(t, got, "http://env-proxy.local:2323")
}

func TestInitKiroHttpClientKeepsShortRestTimeout(t *testing.T) {
	InitKiroHttpClient("")
	t.Cleanup(func() { InitKiroHttpClient("") })

	streamClient := kiroHttpStore.Load()
	restClient := kiroRestHttpStore.Load()

	if streamClient.Timeout != 5*time.Minute {
		t.Fatalf("expected streaming timeout to be 5m, got %s", streamClient.Timeout)
	}
	if restClient.Timeout != 30*time.Second {
		t.Fatalf("expected REST timeout to stay 30s, got %s", restClient.Timeout)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("invalid test URL: %v", err)
	}
	return parsed
}

func assertProxyURL(t *testing.T, got *url.URL, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("expected proxy URL %q, got nil", want)
	}
	if got.String() != want {
		t.Fatalf("expected proxy URL %q, got %q", want, got.String())
	}
}
