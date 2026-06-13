package proxy

import (
	"strings"
	"testing"
)

// Story s7: the Responses (SSE + WS) stream paths route upstream text through
// the thinkingTextProcessor with an emitter that maps thinking-state to either
// output_text deltas (state 0) or reasoning_summary deltas (state 1/2/3). These
// tests exercise that exact emitter mapping to prove inline <thinking> never
// leaks into the message (output_text) stream.

// responsesEmitterRecorder mirrors the Responses-path emitter: state 0 -> the
// message (output_text) stream, state 1/2/3 -> the reasoning stream. This is the
// SAME mapping wired into handleResponsesStream and the WS path.
type responsesEmitterRecorder struct {
	message   strings.Builder
	reasoning strings.Builder
}

func (r *responsesEmitterRecorder) emit(text string, thinkingState int) {
	if thinkingState == 0 {
		r.message.WriteString(text)
		return
	}
	r.reasoning.WriteString(text)
}

// feed runs a slice of (text,isThinking) chunks through a processor wired to the
// Responses emitter, then finalizes.
func feedResponsesProcessor(thinking bool, chunks []struct {
	text       string
	isThinking bool
}) *responsesEmitterRecorder {
	rec := &responsesEmitterRecorder{}
	p := newThinkingProcessor(thinking, rec.emit, allowReasoningSource, allowTagSource)
	for _, c := range chunks {
		p.Process(c.text, c.isThinking)
	}
	p.Finalize()
	return rec
}

// TestResponsesInlineThinkingDoesNotLeakIntoOutputText is the core regression:
// inline <thinking>...</thinking> arriving as ordinary assistantResponseEvent
// text (isThinking=false) must be parsed into the reasoning stream, never
// emitted verbatim into output_text.
func TestResponsesInlineThinkingDoesNotLeakIntoOutputText(t *testing.T) {
	rec := feedResponsesProcessor(true, []struct {
		text       string
		isThinking bool
	}{
		{"Here is the answer. <thinking>let me reason about it</thinking>Done.", false},
	})

	msg := rec.message.String()
	if strings.Contains(msg, "<thinking>") || strings.Contains(msg, "</thinking>") {
		t.Fatalf("output_text leaked a thinking tag: %q", msg)
	}
	if strings.Contains(msg, "let me reason about it") {
		t.Fatalf("thinking CONTENT leaked into output_text: %q", msg)
	}
	if !strings.Contains(msg, "Here is the answer.") || !strings.Contains(msg, "Done.") {
		t.Fatalf("ordinary content missing from output_text: %q", msg)
	}
	if !strings.Contains(rec.reasoning.String(), "let me reason about it") {
		t.Fatalf("thinking content should appear in the reasoning stream, got %q", rec.reasoning.String())
	}
}

// TestResponsesInlineThinkingSplitAcrossChunks: a tag split across upstream
// chunks must still be parsed (the held-tail hedge), not leaked.
func TestResponsesInlineThinkingSplitAcrossChunks(t *testing.T) {
	rec := feedResponsesProcessor(true, []struct {
		text       string
		isThinking bool
	}{
		{"answer <think", false},
		{"ing>secret reasoning</thi", false},
		{"nking> tail", false},
	})

	msg := rec.message.String()
	if strings.Contains(msg, "<thinking>") || strings.Contains(msg, "</thinking>") {
		t.Fatalf("split tag leaked into output_text: %q", msg)
	}
	if strings.Contains(msg, "secret reasoning") {
		t.Fatalf("thinking content leaked into output_text across chunks: %q", msg)
	}
	if !strings.Contains(msg, "answer") || !strings.Contains(msg, "tail") {
		t.Fatalf("ordinary content missing: %q", msg)
	}
	if !strings.Contains(rec.reasoning.String(), "secret reasoning") {
		t.Fatalf("reasoning stream missing thinking content: %q", rec.reasoning.String())
	}
}

// TestResponsesReasoningEventStillFlows: native reasoningContentEvent text
// (isThinking=true) still routes to the reasoning stream, not output_text.
func TestResponsesReasoningEventStillFlows(t *testing.T) {
	rec := feedResponsesProcessor(true, []struct {
		text       string
		isThinking bool
	}{
		{"thinking via event", true},
		{"the answer", false},
	})
	if rec.reasoning.String() != "thinking via event" {
		t.Fatalf("reasoning stream = %q, want %q", rec.reasoning.String(), "thinking via event")
	}
	if rec.message.String() != "the answer" {
		t.Fatalf("message stream = %q, want %q", rec.message.String(), "the answer")
	}
}
