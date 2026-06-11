// Regression tests for the HTTP/2 stream reset classification.
// Before this, an upstream RST_STREAM (e.g. "stream error: stream ID 7;
// INTERNAL_ERROR; received from peer") was returned verbatim to the
// client AND was classified as non-retryable, so a single transient
// RST killed the request even though a peer account could have
// succeeded. The classifier wraps the original cause in
// *ErrUpstreamStreamReset and isRetryableUpstreamError returns true.
package proxy

import (
	"context"
	"errors"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// TestClassifyStreamError_TypedStreamError wraps a real
// *http2.StreamError and asserts it is recognized and re-wrapped.
func TestClassifyStreamError_TypedStreamError(t *testing.T) {
	cause := &http2.StreamError{Code: http2.ErrCodeInternal, StreamID: 7}
	got := classifyStreamError(cause)
	if got == nil {
		t.Fatal("expected non-nil classification")
	}
	var sre *ErrUpstreamStreamReset
	if !errors.As(got, &sre) {
		t.Fatalf("expected *ErrUpstreamStreamReset, got %T", got)
	}
	if !errors.Is(got, cause) {
		t.Fatal("errors.Is must reach the original *http2.StreamError via Unwrap")
	}
}

// TestClassifyStreamError_TypedGoAway wraps a real *http2.GoAwayError.
func TestClassifyStreamError_TypedGoAway(t *testing.T) {
	cause := &http2.GoAwayError{LastStreamID: 11, ErrCode: http2.ErrCodeInternal}
	got := classifyStreamError(cause)
	var sre *ErrUpstreamStreamReset
	if !errors.As(got, &sre) {
		t.Fatalf("expected *ErrUpstreamStreamReset, got %T", got)
	}
}

// TestClassifyStreamError_SubstringFallback covers the case where a
// stdlib vendored variant doesn't expose a typed value but the Error()
// string contains the canonical "INTERNAL_ERROR" / "stream error" /
// "RST_STREAM" / "GOAWAY" markers.
func TestClassifyStreamError_SubstringFallback(t *testing.T) {
	cases := []string{
		"stream error: stream ID 7; INTERNAL_ERROR; received from peer",
		"http2: RST_STREAM frame sent",
		"http2: received GOAWAY frame",
	}
	for _, msg := range cases {
		got := classifyStreamError(errors.New(msg))
		var sre *ErrUpstreamStreamReset
		if !errors.As(got, &sre) {
			t.Errorf("expected substring fallback to classify %q, got %T", msg, got)
		}
	}
}

// TestClassifyStreamError_ClientConnectionLost is the regression guard for the
// "API Error: http2: client connection lost" leak. That string is created
// inline (errors.New) inside http2.(*ClientConn).closeForLostPing when a
// health-check PING is not ACKed in time — it is NOT a typed/exported sentinel,
// so it must be caught by substring. Before the fix it matched none of the
// classifier markers, so it was never wrapped and the raw Go string leaked to
// the client post-commit.
func TestClassifyStreamError_ClientConnectionLost(t *testing.T) {
	cases := []string{
		"http2: client connection lost",
		"Post \"https://q.us-east-1.amazonaws.com/\": http2: client connection lost",
		"http2: server sent GOAWAY and closed the connection; LastStreamID=5",
	}
	for _, msg := range cases {
		got := classifyStreamError(errors.New(msg))
		var sre *ErrUpstreamStreamReset
		if !errors.As(got, &sre) {
			t.Errorf("expected %q to be classified as a stream reset, got %T", msg, got)
		}
		if !isRetryableUpstreamError(got) {
			t.Errorf("a connection-lost error must be retryable so the dispatcher fails over: %q", msg)
		}
	}
}

func TestClassifyStreamError_UnrelatedPassesThrough(t *testing.T) {
	notAReset := errors.New("connection refused")
	if got := classifyStreamError(notAReset); got != notAReset {
		t.Fatalf("classifier must not wrap non-reset errors, got %v", got)
	}
	if got := classifyStreamError(nil); got != nil {
		t.Fatalf("classifier(nil) must be nil, got %v", got)
	}
}

// TestIsRetryableUpstreamError_StreamReset confirms the new sentinel
// is classified as retryable so the dispatcher can rotate accounts
// on a transient peer RST.
func TestIsRetryableUpstreamError_StreamReset(t *testing.T) {
	cause := &http2.StreamError{Code: http2.ErrCodeInternal, StreamID: 7}
	wrapped := &ErrUpstreamStreamReset{Cause: cause}
	if !isRetryableUpstreamError(wrapped) {
		t.Fatal("ErrUpstreamStreamReset must be classified as retryable")
	}
	// Substring variant (no typed cause) must also be retryable.
	stringVariant := &ErrUpstreamStreamReset{Cause: errors.New("stream error: stream ID 7; INTERNAL_ERROR; received from peer")}
	if !isRetryableUpstreamError(stringVariant) {
		t.Fatal("string-caught stream reset must also be retryable")
	}
}

// TestIsRetryableUpstreamError_ContextCanceledNotStreamReset pins
// the existing contract: client-side cancellation must remain
// non-retryable even though the message contains the substring "stream".
func TestIsRetryableUpstreamError_ContextCanceledNotStreamReset(t *testing.T) {
	if isRetryableUpstreamError(context.Canceled) {
		t.Fatal("context.Canceled must remain non-retryable")
	}
	if isRetryableUpstreamError(context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded must remain non-retryable")
	}
}

// TestSafeStreamErrorMessageNeverLeaks is the regression guard for the
// post-commit error-emit path (Claude SSE, Responses SSE, Responses WS). The
// client-facing message must NEVER contain raw Go/transport internals — no
// "http2: client connection lost", no "received from peer", no ARNs/hosts — for
// ANY upstream error, recognized stream reset or not.
func TestSafeStreamErrorMessageNeverLeaks(t *testing.T) {
	leaky := []error{
		&ErrUpstreamStreamReset{Cause: errors.New("http2: client connection lost")},
		&ErrUpstreamStreamReset{Cause: &http2.StreamError{Code: http2.ErrCodeInternal, StreamID: 7}},
		errors.New("http2: client connection lost"),
		errors.New("Post \"https://q.us-east-1.amazonaws.com/\": net/http: TLS handshake timeout"),
		errors.New("HTTP 500 from Kiro IDE: arn:aws:codewhisperer:us-east-1:123:profile/ABC internal failure"),
	}
	banned := []string{"http2:", "received from peer", "arn:aws", "amazonaws.com", "stream ID", "RST_STREAM"}
	for _, err := range leaky {
		msg := safeStreamErrorMessage(err)
		if msg == "" {
			t.Errorf("non-nil error must yield a non-empty client message, got empty for %v", err)
		}
		for _, b := range banned {
			if strings.Contains(msg, b) {
				t.Errorf("client message %q leaks internal token %q (from %v)", msg, b, err)
			}
		}
	}
	if safeStreamErrorMessage(nil) != "" {
		t.Fatal("safeStreamErrorMessage(nil) must be empty")
	}
}

// TestUpstreamStreamResetMessageSanity pins the user-facing message.
// If this string changes, downstream clients that match on the exact
// "overloaded_error" / "upstream stream reset" wording need a heads-up.
func TestUpstreamStreamResetMessageSanity(t *testing.T) {
	if !strings.Contains(upstreamStreamResetMessage, "stream reset") {
		t.Fatal("client-facing message must describe a stream reset so callers can distinguish from quota exhaustion")
	}
	if strings.Contains(upstreamStreamResetMessage, "received from peer") {
		t.Fatal("client-facing message must NOT leak the raw http2 'received from peer' Go internals")
	}
}

// TestCallKiroAPIReturnsUpstreamStreamReset uses a stub http.RoundTripper
// that returns a *http2.StreamError directly. This deterministically
// exercises the parseEventStream → classifyStreamError → CallKiroAPI
// return path without depending on a real HTTP/2 test server, which
// is awkward to spin up (httptest.NewServer is HTTP/1.1; a raw hijack
// + close surfaces as a plain TCP "unexpected EOF", not an h2 RST).
func TestCallKiroAPIReturnsUpstreamStreamReset(t *testing.T) {
	swapKiroHttpClientForTest(t)

	cause := &http2.StreamError{Code: http2.ErrCodeInternal, StreamID: 7}
	prevStore := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Transport: &streamResetRoundTripper{err: cause}})
	t.Cleanup(func() { kiroHttpStore.Store(prevStore) })

	// swapKiroEndpoints requires exactly 3 URLs; the other two are
	// empty-200 no-ops since our stub transport short-circuits.
	make200 := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			w.WriteHeader(http.StatusOK)
		}))
	}
	s1, s2, s3 := make200(), make200(), make200()
	defer s1.Close()
	defer s2.Close()
	defer s3.Close()
	swapKiroEndpoints(t, []string{s1.URL, s2.URL, s3.URL})

	acct := &config.Account{ID: "test-reset", Email: "r@example.com", AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "hi"
	cb := &KiroStreamCallback{
		OnText:         func(string, bool) {},
		OnToolUse:      func(KiroToolUse) {},
		OnComplete:     func(int, int) {},
		OnCredits:      func(float64) {},
		OnContextUsage: func(float64) {},
		OnStopReason:   func(string) {},
	}
	err := CallKiroAPI(acct, payload, cb)
	if err == nil {
		t.Fatal("expected an error from the stub RST, got nil")
	}
	var sre *ErrUpstreamStreamReset
	if !errors.As(err, &sre) {
		t.Fatalf("expected *ErrUpstreamStreamReset, got %T (%v)", err, err)
	}
	if !isRetryableUpstreamError(err) {
		t.Fatal("the stream-reset error must be classified as retryable so the dispatcher rotates accounts")
	}
}

// streamResetRoundTripper is a minimal http.RoundTripper stub that
// returns a canned error. Used by the live-stream-reset test above
// to inject a *http2.StreamError into the parseEventStream path
// without depending on a real HTTP/2 test server.
type streamResetRoundTripper struct{ err error }

func (s *streamResetRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, s.err
}
