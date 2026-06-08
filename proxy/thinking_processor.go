package proxy

import "strings"

// thinkingTextProcessor buffers an upstream text stream and parses inline
// <thinking>...</thinking> tags into discrete state transitions for the
// caller's emitter. It is the single source of truth for both the Anthropic
// /v1/messages and OpenAI /v1/chat/completions streaming paths — earlier
// revisions had two near-identical copies that drifted (most recently a
// 15-rune tail-hold that interacted badly with the upstream delta handling).
//
// Lifecycle:
//   p := newThinkingProcessor(thinking, emit)
//   for each upstream text or thinking chunk:
//     p.Process(text, isThinking)
//   when the upstream stream ends:
//     p.Finalize()
//
// The emitter receives (text, thinkingState):
//   thinkingState == 0 → ordinary assistant content
//   thinkingState == 1 → first chunk of a thinking block (caller opens block)
//   thinkingState == 2 → middle chunk of a thinking block
//   thinkingState == 3 → last chunk of a thinking block (caller closes block)
//
// The 15-rune tail-hold preserves enough buffered text across chunk
// boundaries that a partial "<thinking>" or "</thinking>" tag straddling
// two upstream chunks is detected on the next call instead of being emitted
// verbatim. Finalize() is the only path that flushes the held tail, so
// every error path on the caller's side MUST call Finalize().
type thinkingTextProcessor struct {
	thinking       bool                                       // whether thinking output is enabled at all
	emit           func(text string, thinkingState int)       // downstream emitter
	allowReasoning func(*thinkingStreamSource) bool           // gate for reasoningContentEvent path
	allowTag       func(*thinkingStreamSource) bool           // gate for inline <thinking> path

	textBuffer        string
	inThinkingBlock   bool
	dropTagThinking   bool
	thinkingStarted   bool
	eventThinkingOpen bool
	thinkingSource    thinkingStreamSource
}

// newThinkingProcessor wires the processor with the caller's downstream
// emitter and the source-gating helpers. allowReasoning and allowTag are
// the existing allowReasoningSource / allowTagSource functions from
// handler.go — they are injected so this file does not have to import
// the symbols transitively.
func newThinkingProcessor(
	thinking bool,
	emit func(text string, thinkingState int),
	allowReasoning func(*thinkingStreamSource) bool,
	allowTag func(*thinkingStreamSource) bool,
) *thinkingTextProcessor {
	return &thinkingTextProcessor{
		thinking:       thinking,
		emit:           emit,
		allowReasoning: allowReasoning,
		allowTag:       allowTag,
	}
}

// Process feeds one upstream chunk into the processor.
//   - isThinking == true → the chunk came from a reasoningContentEvent and
//     should be emitted directly as a thinking-state stream (subject to the
//     reasoning-source gate).
//   - isThinking == false → the chunk came from assistantResponseEvent and
//     may contain inline <thinking>...</thinking> tags that have to be
//     parsed out across chunk boundaries.
func (p *thinkingTextProcessor) Process(text string, isThinking bool) {
	if isThinking && !p.thinking {
		return
	}

	// reasoningContentEvent path: pass straight through, gated by source.
	if isThinking {
		if !p.allowReasoning(&p.thinkingSource) {
			return
		}
		if !p.thinkingStarted {
			p.emit(text, 1)
			p.thinkingStarted = true
			p.eventThinkingOpen = true
		} else {
			p.emit(text, 2)
		}
		return
	}

	// Switching from a reasoning stream back to ordinary content: close the
	// thinking block on the wire so the assistant's next text isn't tagged
	// as reasoning.
	if p.eventThinkingOpen {
		p.emit("", 3)
		p.eventThinkingOpen = false
		p.thinkingStarted = false
	}

	p.textBuffer += text
	p.drain(false)
}

// Finalize flushes any buffered text and closes any open thinking block.
// Idempotent — safe to call multiple times.
func (p *thinkingTextProcessor) Finalize() {
	p.drain(true)
	if p.eventThinkingOpen {
		p.emit("", 3)
		p.eventThinkingOpen = false
		p.thinkingStarted = false
	}
}

// drain runs the inline <thinking> state machine over the current buffer.
// forceFlush=true means the upstream stream is ending and any partial-tag
// hedge no longer applies.
func (p *thinkingTextProcessor) drain(forceFlush bool) {
	for {
		if !p.inThinkingBlock {
			start := strings.Index(p.textBuffer, "<thinking>")
			if start != -1 {
				if start > 0 {
					p.emit(p.textBuffer[:start], 0)
				}
				p.textBuffer = p.textBuffer[start+len("<thinking>"):]
				p.inThinkingBlock = true
				p.dropTagThinking = !p.allowTag(&p.thinkingSource)
				p.thinkingStarted = false
				continue
			}
			// No tag found. Emit everything except a 15-rune tail that might
			// be the start of "<thinking>" straddling the next chunk.
			if forceFlush || len([]rune(p.textBuffer)) > 50 {
				runes := []rune(p.textBuffer)
				safeLen := len(runes)
				if !forceFlush {
					safeLen = max(0, len(runes)-15)
				}
				if safeLen > 0 {
					p.emit(string(runes[:safeLen]), 0)
					p.textBuffer = string(runes[safeLen:])
				}
			}
			return
		}

		// Inside a thinking block. Look for </thinking>.
		end := strings.Index(p.textBuffer, "</thinking>")
		if end != -1 {
			content := p.textBuffer[:end]
			if !p.dropTagThinking {
				if !p.thinkingStarted {
					p.emit(content, 1)
					p.emit("", 3)
				} else {
					p.emit(content, 3)
				}
			}
			p.textBuffer = p.textBuffer[end+len("</thinking>"):]
			p.inThinkingBlock = false
			p.dropTagThinking = false
			p.thinkingStarted = false
			continue
		}

		if forceFlush {
			if p.textBuffer != "" {
				if !p.dropTagThinking {
					if !p.thinkingStarted {
						p.emit(p.textBuffer, 1)
						p.emit("", 3)
					} else {
						p.emit(p.textBuffer, 3)
					}
				}
				p.textBuffer = ""
			}
			p.inThinkingBlock = false
			p.dropTagThinking = false
			p.thinkingStarted = false
			return
		}

		// Stream the thinking content out, holding back 15 runes that
		// might be the start of </thinking>.
		runes := []rune(p.textBuffer)
		if len(runes) > 20 {
			safeLen := len(runes) - 15
			if safeLen > 0 {
				if !p.dropTagThinking {
					if !p.thinkingStarted {
						p.emit(string(runes[:safeLen]), 1)
						p.thinkingStarted = true
					} else {
						p.emit(string(runes[:safeLen]), 2)
					}
				}
				p.textBuffer = string(runes[safeLen:])
			}
		}
		return
	}
}
