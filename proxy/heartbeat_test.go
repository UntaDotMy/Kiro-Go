package proxy

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStartSSEHeartbeatTicksThenStops verifies the heartbeat fires while
// running and stops cleanly: no ticks land after stop() returns. This is the
// invariant that keeps a `ping` from ever following `message_stop`.
func TestStartSSEHeartbeatTicksThenStops(t *testing.T) {
	var ticks int32
	stop := startSSEHeartbeat(5*time.Millisecond, func() {
		atomic.AddInt32(&ticks, 1)
	})

	// Let several ticks accumulate.
	time.Sleep(40 * time.Millisecond)
	stop() // blocks until the goroutine has exited

	got := atomic.LoadInt32(&ticks)
	if got == 0 {
		t.Fatal("expected the heartbeat to tick at least once before stop")
	}

	// After stop() returns, the goroutine is joined — the count must be frozen.
	time.Sleep(30 * time.Millisecond)
	if after := atomic.LoadInt32(&ticks); after != got {
		t.Fatalf("heartbeat ticked after stop(): %d -> %d (a ping could follow message_stop)", got, after)
	}
}

// TestStartSSEHeartbeatStopIsIdempotent verifies stop() can be called more than
// once (handler calls it on the upstream-return path AND via defer) without
// panicking on a double close.
func TestStartSSEHeartbeatStopIsIdempotent(t *testing.T) {
	stop := startSSEHeartbeat(time.Millisecond, func() {})
	stop()
	stop() // must not panic
	stop()
}

// TestStartSSEHeartbeatConcurrentStop exercises stop() racing with active ticks
// to surface any obvious ordering bug even without the race detector (cgo is
// unavailable in this environment).
func TestStartSSEHeartbeatConcurrentStop(t *testing.T) {
	var mu sync.Mutex
	stop := startSSEHeartbeat(time.Millisecond, func() {
		mu.Lock()
		// Simulate the writeMu-guarded SSE write the real tick performs.
		mu.Unlock()
	})
	time.Sleep(5 * time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); stop() }()
	}
	wg.Wait()
}

// TestStartSSEHeartbeatZeroIntervalIsNoop verifies a disabled heartbeat never
// ticks and its stop is a safe no-op.
func TestStartSSEHeartbeatZeroIntervalIsNoop(t *testing.T) {
	var ticks int32
	stop := startSSEHeartbeat(0, func() { atomic.AddInt32(&ticks, 1) })
	time.Sleep(20 * time.Millisecond)
	stop()
	if got := atomic.LoadInt32(&ticks); got != 0 {
		t.Fatalf("zero interval must disable the heartbeat, got %d ticks", got)
	}
}

// TestStreamHeartbeatIntervalSanity pins the relationship between the
// heartbeat cadence and the upstream timeouts: the client must receive a
// keepalive well before any client-side idle timeout could plausibly fire, and
// the cadence must be far under the 5-minute upstream idle window so a quiet
// stream is kept visibly alive.
func TestStreamHeartbeatIntervalSanity(t *testing.T) {
	if streamHeartbeatInterval <= 0 {
		t.Fatal("streamHeartbeatInterval must be positive")
	}
	if streamHeartbeatInterval >= streamIdleTimeout {
		t.Fatalf("heartbeat interval %v must be far below streamIdleTimeout %v",
			streamHeartbeatInterval, streamIdleTimeout)
	}
	// A keepalive every <=30s is the commonly safe ceiling for SSE clients
	// behind proxies; guard against a future bump that would defeat the point.
	if streamHeartbeatInterval > 30*time.Second {
		t.Fatalf("heartbeat interval %v too large to reliably keep clients alive", streamHeartbeatInterval)
	}
}
