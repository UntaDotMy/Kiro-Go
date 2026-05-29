// Idle-timeout reader for streaming responses.
//
// Background: http.Client.Timeout is a wall-clock cap on the entire
// request including reading body. For long-running streaming generations
// (especially thinking-mode output), it kills connections that are still
// happily delivering data. The error surfaces to the user as
// "context deadline exceeded (Client.Timeout … while reading body)".
//
// We replace it with a per-read deadline: a stream is healthy as long as
// data keeps arriving within streamIdleTimeout. Once the gap between two
// reads exceeds the threshold, the wrapped cancel func is invoked, which
// in turn cancels the request context and aborts the stream cleanly.
//
// The design uses a single supervisor goroutine per stream that wakes
// every poll interval and checks the last-read timestamp. This is cheaper
// than scheduling a fresh timer on every Read (which can fire hundreds of
// times per second on bursty SSE).
package proxy

import (
	"io"
	"sync/atomic"
	"time"
)

// idleTimeoutReader wraps an io.Reader with an idle deadline. If no Read
// returns ≥1 byte within `timeout`, the supplied cancel func is called.
// The supervisor goroutine exits when Close is called or the underlying
// reader returns EOF / error.
type idleTimeoutReader struct {
	src      io.ReadCloser
	timeout  time.Duration
	cancel   func()
	lastRead atomic.Int64 // unix nano of last successful Read
	done     chan struct{}
	closed   atomic.Bool
}

// newIdleTimeoutReader wraps src so that a gap of ≥timeout between reads
// invokes cancel(). The caller is still responsible for closing src;
// closing the returned reader is a no-op for the underlying body but
// stops the supervisor.
func newIdleTimeoutReader(src io.ReadCloser, timeout time.Duration, cancel func()) *idleTimeoutReader {
	r := &idleTimeoutReader{
		src:     src,
		timeout: timeout,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	r.lastRead.Store(time.Now().UnixNano())
	go r.supervise()
	return r
}

func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 {
		r.lastRead.Store(time.Now().UnixNano())
	}
	if err != nil {
		// Stop the supervisor on any terminal condition (EOF, network error).
		r.stopSupervisor()
	}
	return n, err
}

func (r *idleTimeoutReader) Close() error {
	r.stopSupervisor()
	// We do NOT close src here — CallKiroAPI's deferred resp.Body.Close()
	// owns that. Returning nil keeps interface{} satisfied without
	// double-closing.
	return nil
}

func (r *idleTimeoutReader) stopSupervisor() {
	if r.closed.CompareAndSwap(false, true) {
		close(r.done)
	}
}

// supervise runs in its own goroutine and trips cancel() once the idle
// deadline has been crossed. Poll interval is timeout/4 (clamped to a
// 100ms..30s window) so we don't burn CPU on streams that legitimately
// go many minutes between events, and stay responsive for tight idle
// thresholds (used in tests, also useful for ops who want quick failure
// detection on short polling endpoints).
func (r *idleTimeoutReader) supervise() {
	poll := r.timeout / 4
	if poll < 100*time.Millisecond {
		poll = 100 * time.Millisecond
	}
	if poll > 30*time.Second {
		poll = 30 * time.Second
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-r.done:
			return
		case now := <-t.C:
			last := r.lastRead.Load()
			if now.UnixNano()-last >= int64(r.timeout) {
				// Idle threshold crossed. Cancel the request context;
				// the in-flight Read will return ctx.Err() shortly after.
				if r.cancel != nil {
					r.cancel()
				}
				r.stopSupervisor()
				return
			}
		}
	}
}
