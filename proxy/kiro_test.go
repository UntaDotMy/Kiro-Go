package proxy

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"net/url"
	"testing"
)

// collectStreamText runs a sequence of assistantResponseEvent content fragments
// through the real parseEventStream path and returns the concatenated text the
// client would receive. This is the ground-truth check that the proxy forwards
// incremental deltas verbatim (no dedup), matching how CodeWhisperer / Amazon Q
// actually streams (confirmed against AWS's own Q CLI, which does push_str).
func collectStreamText(t *testing.T, fragments ...string) string {
	t.Helper()
	var buf bytes.Buffer
	for _, f := range fragments {
		buf.Write(buildEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": f}))
	}
	var got string
	cb := &KiroStreamCallback{OnText: func(s string, isThinking bool) {
		if !isThinking {
			got += s
		}
	}}
	if err := parseEventStream(bytes.NewReader(buf.Bytes()), cb); err != nil {
		t.Fatalf("parseEventStream: %v", err)
	}
	return got
}

// TestStreamConcatenatesIncrementalDeltasVerbatim is the core regression for the
// character/space-drop corruption. The upstream sends INCREMENTAL fragments; the
// full message is their verbatim concatenation. The removed normalizeChunk
// deduper corrupted exactly these shapes — each case here reproduces a reported
// or class-equivalent bug and asserts the bytes now arrive intact.
func TestStreamConcatenatesIncrementalDeltasVerbatim(t *testing.T) {
	cases := []struct {
		name      string
		fragments []string
		want      string
	}{
		// "water" arriving as "wat"+"er" must NOT lose the final characters.
		{"split mid-word", []string{"wat", "er"}, "water"},
		// "i like you" split so a space-led fragment isn't dropped.
		{"space-led fragment preserved", []string{"i", " like", " you"}, "i like you"},
		// A fragment that is an exact repeat of the previous (e.g. "the the")
		// must be kept — the old chunk==prev branch dropped it.
		{"legitimate repeated token", []string{"the ", "the "}, "the the "},
		// A fragment that is a prefix of the previous one must be kept — the old
		// HasPrefix(prev, chunk) rewind branch dropped it.
		{"delta is prefix of prior", []string{"hello", "hel"}, "hellohel"},
		// A fragment whose head matches the previous fragment's tail must not be
		// trimmed ("lets "+" begin" -> "lets  begin" with both spaces).
		{"boundary space not eaten", []string{"lets ", " begin"}, "lets  begin"},
		// Cumulative-looking sequence is NOT cumulative here: "ab" then "abc" are
		// two separate deltas and both are kept verbatim.
		{"prefix-extension kept verbatim", []string{"ab", "abc"}, "ababc"},
		{"unicode boundary", []string{"caf", "é"}, "café"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := collectStreamText(t, tc.fragments...); got != tc.want {
				t.Fatalf("fragments %q concatenated to %q, want %q", tc.fragments, got, tc.want)
			}
		})
	}
}

// TestParseEventStreamRejectsOversizedFrame locks in the frame-allocation cap:
// a frame whose total-length prefix claims more than maxEventStreamFrameBytes
// must be rejected with an error BEFORE the proxy allocates for it, so a
// malformed/tampered upstream frame can't trigger a giant make([]byte) and OOM
// the process.
func TestParseEventStreamRejectsOversizedFrame(t *testing.T) {
	// Build a 12-byte prelude claiming a ~2 GiB total length.
	var prelude [12]byte
	binary.BigEndian.PutUint32(prelude[0:4], uint32(2<<30)) // totalLength ~2GiB
	binary.BigEndian.PutUint32(prelude[4:8], 0)             // headersLength
	// crc bytes [8:12] left zero — parse rejects on size before reading them.

	err := parseEventStream(bytes.NewReader(prelude[:]), &KiroStreamCallback{})
	if err == nil {
		t.Fatal("expected an error for an oversized frame, got nil (would have allocated ~2GiB)")
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

// TestInitKiroHttpClientTimeoutShape validates the asymmetric timeout
// strategy: REST client keeps a short wall-clock cap, streaming client
// has Client.Timeout = 0 (idleTimeoutReader handles streaming) plus a
// transport-level ResponseHeaderTimeout so a stalled handshake can't
// hang the request.
func TestInitKiroHttpClientTimeoutShape(t *testing.T) {
	InitKiroHttpClient("")
	t.Cleanup(func() { InitKiroHttpClient("") })

	streamClient := kiroHttpStore.Load()
	restClient := kiroRestHttpStore.Load()

	if streamClient.Timeout != 0 {
		t.Fatalf("expected streaming Client.Timeout to be 0 (governed by idleTimeoutReader), got %s", streamClient.Timeout)
	}
	if restClient.Timeout != restRequestTimeout {
		t.Fatalf("expected REST timeout to be %s, got %s", restRequestTimeout, restClient.Timeout)
	}
	tr, ok := streamClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", streamClient.Transport)
	}
	if tr.ResponseHeaderTimeout != responseHeaderTimeout {
		t.Fatalf("expected ResponseHeaderTimeout %s, got %s", responseHeaderTimeout, tr.ResponseHeaderTimeout)
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
