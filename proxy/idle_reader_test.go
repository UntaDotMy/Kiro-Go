package proxy

import (
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// blockingReader pretends to be a streaming response body that never
// produces bytes. Used to drive idleTimeoutReader past its deadline.
type blockingReader struct {
	closed atomic.Bool
}

func (b *blockingReader) Read(p []byte) (int, error) {
	for !b.closed.Load() {
		time.Sleep(50 * time.Millisecond)
	}
	return 0, io.EOF
}

func (b *blockingReader) Close() error {
	b.closed.Store(true)
	return nil
}

// TestIdleTimeoutReaderFiresCancel ensures that a stalled body trips
// cancel() once the idle window elapses.
func TestIdleTimeoutReaderFiresCancel(t *testing.T) {
	br := &blockingReader{}
	defer br.Close()

	var cancelled atomic.Bool
	r := newIdleTimeoutReader(br, 200*time.Millisecond, func() { cancelled.Store(true) })
	defer r.Close()

	// Read in a goroutine so we don't block the test on the supervisor.
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 16)
		_, _ = r.Read(buf)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("expected cancel to fire within 2s")
		default:
		}
		if cancelled.Load() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestIdleTimeoutReaderResetsOnRead ensures live streams aren't killed
// just because they're slow — every successful Read should bump the
// last-activity timestamp. Margins (100ms feed / 800ms timeout) are
// deliberately generous so a paused CI runner doesn't false-trip.
func TestIdleTimeoutReaderResetsOnRead(t *testing.T) {
	pipe := newPipeReader()
	defer pipe.Close()

	var cancelled atomic.Bool
	r := newIdleTimeoutReader(pipe, 800*time.Millisecond, func() { cancelled.Store(true) })
	defer r.Close()

	go func() {
		// Six reads at 100ms intervals — total 600ms < timeout, and
		// no individual gap exceeds the threshold even on a stalled
		// scheduler.
		for i := 0; i < 6; i++ {
			pipe.feed([]byte{byte('a' + i)})
			time.Sleep(100 * time.Millisecond)
		}
		pipe.eof()
	}()

	buf := make([]byte, 8)
	for {
		n, err := r.Read(buf)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		_ = n
	}
	if cancelled.Load() {
		t.Fatalf("cancel fired even though stream stayed active")
	}
}

// pipeReader is a simple in-memory ReadCloser we can feed bytes to.
type pipeReader struct {
	ch     chan []byte
	closed atomic.Bool
}

func newPipeReader() *pipeReader { return &pipeReader{ch: make(chan []byte, 16)} }
func (p *pipeReader) feed(b []byte) {
	if !p.closed.Load() {
		p.ch <- b
	}
}
func (p *pipeReader) eof() {
	if p.closed.CompareAndSwap(false, true) {
		close(p.ch)
	}
}
func (p *pipeReader) Close() error {
	p.eof()
	return nil
}
func (p *pipeReader) Read(buf []byte) (int, error) {
	chunk, ok := <-p.ch
	if !ok {
		return 0, io.EOF
	}
	n := copy(buf, chunk)
	return n, nil
}
