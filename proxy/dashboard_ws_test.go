package proxy

import (
	"testing"
	"time"
)

// TestDashboardHubBroadcasts confirms a subscriber receives a payload
// pushed through broadcast(), and that closing the subscriber via
// remove() prevents further deliveries (no panic on closed-channel send,
// because broadcast() goes through a non-blocking select default).
func TestDashboardHubBroadcasts(t *testing.T) {
	hub := newDashboardHub()
	sub := &dashboardSubscriber{
		send: make(chan []byte, 4),
		done: make(chan struct{}),
	}
	hub.add(sub)

	hub.broadcast([]byte(`{"type":"status"}`))
	select {
	case msg := <-sub.send:
		if string(msg) != `{"type":"status"}` {
			t.Fatalf("unexpected payload: %s", msg)
		}
	case <-time.After(time.Second):
		t.Fatalf("no broadcast received")
	}

	hub.remove(sub)
	select {
	case <-sub.done:
		// Expected.
	case <-time.After(time.Second):
		t.Fatalf("done channel not closed after remove")
	}

	// Further broadcasts must not panic. We can't observe whether the
	// payload was delivered (subscriber is unregistered), only that the
	// call returns cleanly.
	hub.broadcast([]byte(`{"type":"status","seq":2}`))
}

// TestDashboardHubDropsSlowConsumer ensures broadcast() does not block
// when a subscriber's buffer is full. Send 100 payloads to a 4-buffer
// subscriber; broadcast must return promptly each time.
func TestDashboardHubDropsSlowConsumer(t *testing.T) {
	hub := newDashboardHub()
	sub := &dashboardSubscriber{
		send: make(chan []byte, 4),
		done: make(chan struct{}),
	}
	hub.add(sub)
	defer hub.remove(sub)

	deadline := time.After(2 * time.Second)
	for i := 0; i < 100; i++ {
		select {
		case <-deadline:
			t.Fatalf("broadcast appears to be blocking on a full subscriber")
		default:
		}
		hub.broadcast([]byte(`{"type":"status"}`))
	}
}
