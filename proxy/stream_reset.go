// HTTP/2 stream-level error classification.
//
// Background: the upstream Kiro endpoints serve an AWS EventStream
// response over HTTP/2. When AWS abruptly closes a stream — e.g. an
// idle middlebox silently dropped the connection, an internal AWS
// error, or a long think-pause tripped a transport-level reaper — the
// http2 client surfaces the failure as *http2.StreamError{Code:
// INTERNAL_ERROR}, which unwraps to a string of the form
//
//	stream error: stream ID N; INTERNAL_ERROR; received from peer
//
// That string was being passed verbatim to the client and also being
// classified as a *non-retryable* upstream error, so a single RST killed
// the request even though a fresh transport (or a peer account) might
// have succeeded. The types in this file wrap the original cause in a
// sentinel *ErrUpstreamStreamReset so:
//
//   - the failover dispatcher can rotate accounts (peer can ride over
//     a transient peer RST);
//   - the post-commit SSE error event can emit an
//     "overloaded_error" with a stable, friendly message instead of
//     leaking the raw Go error string to the API consumer.
package proxy

import (
	"errors"
	"io"
	"strings"

	"kiro-go/logger"

	"golang.org/x/net/http2"
)

// ErrUpstreamStreamReset is the sentinel returned (wrapping the
// original cause) when the upstream HTTP/2 peer sends a RST_STREAM or
// a GOAWAY-shape error. Detect with errors.As(err, &sre).
type ErrUpstreamStreamReset struct {
	Cause error
}

func (e *ErrUpstreamStreamReset) Error() string {
	return "upstream stream reset: " + e.Cause.Error()
}

func (e *ErrUpstreamStreamReset) Unwrap() error { return e.Cause }


// classifyStreamError returns a *ErrUpstreamStreamReset if err looks
// like an HTTP/2 stream reset or connection-level GOAWAY; otherwise
// returns err unchanged. Safe to call with nil.
//
// Detection order (cheapest first):
//
//  1. Typed *http2.StreamError / *http2.GoAwayError from the local
//     http2 transport.
//  2. Substring fallback for stdlib vendored variants and any future
//     http2 errors that don't expose a typed value: the canonical
//     INTERNAL_ERROR phrase, "stream error", RST_STREAM, GOAWAY.
//
// The substring fallback is intentionally narrow so unrelated errors
// that happen to contain the word "stream" don't get reclassified.
// The fallback's downside (a misclassification causes a peer rotation
// in isRetryableUpstreamError) is the correct behavior anyway —
// rotation is what we want for any RST/GOAWAY shape.
func classifyStreamError(err error) error {
	if err == nil {
		return nil
	}
	// Mid-frame truncation: parseEventStream uses io.ReadFull for the 12-byte
	// prelude and the frame body, which returns io.ErrUnexpectedEOF when the
	// connection ends PART-WAY through a frame (as opposed to a clean io.EOF at a
	// frame boundary, which the parser treats as normal end-of-stream — see the
	// EOF branch there). CodeWhisperer / Amazon Q has no application-level
	// terminal event, so a mid-frame cut is the one definitively-detectable
	// truncation shape: the upstream was still sending a frame when the
	// connection dropped. Treat it like a stream reset — retryable pre-commit
	// (a fresh transport / peer account re-runs cleanly), friendly message
	// post-commit instead of leaking "unexpected EOF" to the client.
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return &ErrUpstreamStreamReset{Cause: err}
	}
	var se *http2.StreamError
	if errors.As(err, &se) {
		return &ErrUpstreamStreamReset{Cause: err}
	}
	var ge *http2.GoAwayError
	if errors.As(err, &ge) {
		return &ErrUpstreamStreamReset{Cause: err}
	}
	msg := err.Error()
	if strings.Contains(msg, "INTERNAL_ERROR") ||
		strings.Contains(msg, "stream error") ||
		strings.Contains(msg, "RST_STREAM") ||
		strings.Contains(msg, "GOAWAY") ||
		// "http2: client connection lost" — created inline (errors.New) inside
		// http2.(*ClientConn).closeForLostPing when a health-check PING is not
		// ACKed within PingTimeout, OR the conn is otherwise lost mid-flight. It
		// is NOT an exported sentinel, so string match is the only option. This
		// is the connection-died-while-in-flight shape: a fresh conn (or peer
		// account) is clean, so treat it exactly like a RST/GOAWAY — retryable
		// pre-commit, friendly message post-commit. Was previously only caught by
		// the broad "connection" substring in isRetryableUpstreamError (so
		// failover worked) but never wrapped here, so the raw Go string leaked to
		// the client post-commit as "API Error: http2: client connection lost".
		strings.Contains(msg, "client connection lost") ||
		strings.Contains(msg, "connection lost") {
		return &ErrUpstreamStreamReset{Cause: err}
	}
	return err
}

// upstreamStreamResetMessage is the client-facing, human-friendly
// message used in the post-commit SSE error event when the cause is
// a stream reset. Logged at WARN with the original cause for operator
// diagnosis; the client never sees the raw Go string.
const upstreamStreamResetMessage = "upstream stream reset (INTERNAL_ERROR); please retry"

// safeStreamErrorMessage returns a client-facing message for a post-commit
// upstream failure that never leaks raw Go/transport internals (ARNs, hostnames,
// "received from peer", "http2: client connection lost", etc.). A recognized
// stream reset / connection-lost gets the stable, friendly retry message; any
// other upstream error collapses to a generic line. The real cause is logged at
// WARN for operators. Use this at every post-commit error-emit site (Claude SSE,
// Responses SSE, Responses WS) so a deliberately-triggered upstream failure can
// never surface backend detail to the API consumer.
func safeStreamErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	var sre *ErrUpstreamStreamReset
	if errors.As(err, &sre) {
		logger.Warnf("[StreamReset] post-commit upstream stream reset: %v", err)
		return upstreamStreamResetMessage
	}
	logger.Warnf("[Upstream] post-commit upstream error: %v", err)
	return "upstream error; please retry"
}
