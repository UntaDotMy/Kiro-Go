package proxy

import (
	"strings"
	"testing"
)

// thinkingProcessorRecorder collects emit calls so tests can assert the
// exact (text, state) sequence produced by drain/Finalize.
type thinkingProcessorRecorder struct {
	calls []struct {
		text  string
		state int
	}
}

func (r *thinkingProcessorRecorder) emit(text string, state int) {
	r.calls = append(r.calls, struct {
		text  string
		state int
	}{text, state})
}

// content returns just the ordinary-content (state==0) text concatenated.
func (r *thinkingProcessorRecorder) content() string {
	var b strings.Builder
	for _, c := range r.calls {
		if c.state == 0 {
			b.WriteString(c.text)
		}
	}
	return b.String()
}

// thinkingContent returns just the thinking-state (state 1/2/3) text
// concatenated.
func (r *thinkingProcessorRecorder) thinkingContent() string {
	var b strings.Builder
	for _, c := range r.calls {
		if c.state == 1 || c.state == 2 || c.state == 3 {
			b.WriteString(c.text)
		}
	}
	return b.String()
}

func newRecordingProcessor(thinking bool) (*thinkingTextProcessor, *thinkingProcessorRecorder) {
	rec := &thinkingProcessorRecorder{}
	p := newThinkingProcessor(thinking, rec.emit, allowReasoningSource, allowTagSource)
	return p, rec
}

func TestProcessorPassesThroughOrdinaryContentAcrossChunks(t *testing.T) {
	// Regression: this is the same shape as the truncation bug — ordinary
	// content streamed in many small chunks must reassemble verbatim.
	p, rec := newRecordingProcessor(false)
	for _, c := range []string{"lets ", " begin", " sleep"} {
		p.Process(c, false)
	}
	p.Finalize()
	if got := rec.content(); got != "lets  begin sleep" {
		t.Fatalf("content = %q, want %q", got, "lets  begin sleep")
	}
}

func TestProcessorEmitsThinkingBlockBoundaries(t *testing.T) {
	p, rec := newRecordingProcessor(true)
	p.Process("hello <thinking>internal</thinking> world", false)
	p.Finalize()

	if got := rec.content(); got != "hello  world" {
		t.Fatalf("ordinary content = %q, want %q", got, "hello  world")
	}
	if got := rec.thinkingContent(); got != "internal" {
		t.Fatalf("thinking content = %q, want %q", got, "internal")
	}
	// Verify a state==1 (open) and state==3 (close) were emitted.
	var sawOpen, sawClose bool
	for _, c := range rec.calls {
		switch c.state {
		case 1:
			sawOpen = true
		case 3:
			sawClose = true
		}
	}
	if !sawOpen || !sawClose {
		t.Fatalf("expected open(1) and close(3) emits, got %+v", rec.calls)
	}
}

func TestProcessorHandlesThinkingTagSplitAcrossChunks(t *testing.T) {
	// The 15-rune tail-hold exists for exactly this case: the open or close
	// tag straddles two chunks. Without it, "<think" would be emitted as
	// ordinary content and "ing>" would be lost.
	p, rec := newRecordingProcessor(true)
	p.Process("prefix <think", false)
	p.Process("ing>secret</thi", false)
	p.Process("nking> suffix", false)
	p.Finalize()

	if got := rec.content(); got != "prefix  suffix" {
		t.Fatalf("ordinary content = %q, want %q", got, "prefix  suffix")
	}
	if got := rec.thinkingContent(); got != "secret" {
		t.Fatalf("thinking content = %q, want %q", got, "secret")
	}
}

func TestProcessorFinalizeFlushesUnclosedThinking(t *testing.T) {
	// If upstream cuts the stream mid-thinking, Finalize must still close
	// the block so the client sees a coherent state.
	p, rec := newRecordingProcessor(true)
	p.Process("a<thinking>truncated", false)
	p.Finalize()

	if got := rec.content(); got != "a" {
		t.Fatalf("ordinary content = %q, want %q", got, "a")
	}
	if got := rec.thinkingContent(); got != "truncated" {
		t.Fatalf("thinking content = %q, want %q", got, "truncated")
	}
	var sawClose bool
	for _, c := range rec.calls {
		if c.state == 3 {
			sawClose = true
		}
	}
	if !sawClose {
		t.Fatalf("Finalize must emit a close(3); got %+v", rec.calls)
	}
}

func TestProcessorReasoningEventBypassesTagParser(t *testing.T) {
	// reasoningContentEvent text is already classified as thinking and
	// must pass through directly without inline-tag parsing.
	p, rec := newRecordingProcessor(true)
	p.Process("step 1 ", true)
	p.Process("step 2", true)
	// Switching back to ordinary content should close the thinking block.
	p.Process("answer", false)
	p.Finalize()

	if got := rec.thinkingContent(); got != "step 1 step 2" {
		t.Fatalf("thinking content = %q, want %q", got, "step 1 step 2")
	}
	if got := rec.content(); got != "answer" {
		t.Fatalf("ordinary content = %q, want %q", got, "answer")
	}
}

func TestProcessorIgnoresThinkingWhenDisabled(t *testing.T) {
	p, rec := newRecordingProcessor(false)
	p.Process("step 1", true)
	p.Process("hello", false)
	p.Finalize()

	if got := rec.content(); got != "hello" {
		t.Fatalf("ordinary content = %q, want %q", got, "hello")
	}
	if rec.thinkingContent() != "" {
		t.Fatalf("expected no thinking emits, got %+v", rec.calls)
	}
}

func TestProcessorFinalizeIsIdempotent(t *testing.T) {
	p, rec := newRecordingProcessor(true)
	p.Process("hi", false)
	p.Finalize()
	before := len(rec.calls)
	p.Finalize()
	p.Finalize()
	if len(rec.calls) != before {
		t.Fatalf("Finalize must be idempotent; calls grew %d → %d", before, len(rec.calls))
	}
}
