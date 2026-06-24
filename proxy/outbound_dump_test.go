package proxy

import "testing"

// TestBoundedBufferPreservesTail verifies the diagnostic capture buffer keeps the
// TAIL of a long stream (where truncation/completion behavior is most visible)
// and drops the oldest bytes once the cap is exceeded, never growing unbounded.
func TestBoundedBufferPreservesTail(t *testing.T) {
	b := newBoundedBuffer(8)
	b.Write([]byte("ABCDE"))  // 5 bytes, under cap
	if got := string(b.Bytes()); got != "ABCDE" {
		t.Fatalf("under cap = %q, want ABCDE", got)
	}
	b.Write([]byte("FGH"))    // 8 bytes, exactly at cap
	if got := string(b.Bytes()); got != "ABCDEFGH" {
		t.Fatalf("at cap = %q, want ABCDEFGH", got)
	}
	b.Write([]byte("IJKLMN")) // 14 bytes total -> keep last 8 ("GHIJKLMN")
	if got := string(b.Bytes()); got != "GHIJKLMN" {
		t.Fatalf("over cap = %q, want GHIJKLMN (tail preserved)", got)
	}
	if b.dropped != 6 {
		t.Errorf("dropped = %d, want 6", b.dropped)
	}
	// Never exceeds cap.
	if len(b.buf) > b.cap {
		t.Errorf("buffer grew past cap: len=%d cap=%d", len(b.buf), b.cap)
	}
}

// TestBoundedBufferZeroCapDefaults ensures a nonsensical cap falls back to a
// usable size rather than rejecting all writes.
func TestBoundedBufferZeroCapDefaults(t *testing.T) {
	b := newBoundedBuffer(0)
	if n, err := b.Write([]byte("x")); n != 1 || err != nil {
		t.Fatalf("write with default cap failed: n=%d err=%v", n, err)
	}
	if got := string(b.Bytes()); got != "x" {
		t.Fatalf("default cap = %q, want x", got)
	}
}
