package proxy

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"
)

// buildEventStreamFrame assembles one AWS EventStream frame the way
// parseEventStream reads it: a 12-byte prelude (total_len, headers_len, prelude
// crc), the headers block, the JSON payload, and a trailing 4-byte message crc.
// parseEventStream does NOT validate either CRC (it slices by length only), so
// the crc fields are left zero. Only the ":event-type" string header is emitted,
// which is all extractEventHeaders needs to dispatch.
func buildEventStreamFrame(t *testing.T, eventType string, payload map[string]interface{}) []byte {
	t.Helper()

	name := ":event-type"
	var hdr bytes.Buffer
	hdr.WriteByte(byte(len(name)))
	hdr.WriteString(name)
	hdr.WriteByte(7) // string type
	var vlen [2]byte
	binary.BigEndian.PutUint16(vlen[:], uint16(len(eventType)))
	hdr.Write(vlen[:])
	hdr.WriteString(eventType)
	headers := hdr.Bytes()

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	totalLen := 12 + len(headers) + len(body) + 4
	out := make([]byte, 0, totalLen)
	var pre [12]byte
	binary.BigEndian.PutUint32(pre[0:4], uint32(totalLen))
	binary.BigEndian.PutUint32(pre[4:8], uint32(len(headers)))
	// pre[8:12] prelude crc — left zero, not validated.
	out = append(out, pre[:]...)
	out = append(out, headers...)
	out = append(out, body...)
	out = append(out, 0, 0, 0, 0) // message crc — left zero, not validated.
	return out
}

// TestCleanFrameBoundaryEOFIsSuccess pins the upstream contract confirmed by
// research: CodeWhisperer / Amazon Q generateAssistantResponse has NO
// application-level terminal event (no messageStop / stopReason). A stream that
// delivers content and then ends cleanly at a frame boundary is a NORMAL
// completion — parseEventStream must return nil, not an error, and all content
// must have reached the client. (A drop on a frame boundary is indistinguishable
// from completion even to AWS's own client, so we must not flag it.)
func TestCleanFrameBoundaryEOFIsSuccess(t *testing.T) {
	frame := buildEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "i like you"})
	var got string
	cb := &KiroStreamCallback{OnText: func(s string, _ bool) { got += s }}

	if err := parseEventStream(bytes.NewReader(frame), cb); err != nil {
		t.Fatalf("a clean frame-boundary EOF is normal completion, must not error; got %v", err)
	}
	if got != "i like you" {
		t.Fatalf("content = %q, want %q", got, "i like you")
	}
}

// TestMidFramePreludeTruncationIsRetryableReset verifies the one
// definitively-detectable truncation: the connection drops PART-WAY through the
// 12-byte prelude. io.ReadFull then returns io.ErrUnexpectedEOF, which
// classifyStreamError must wrap as a retryable *ErrUpstreamStreamReset so the
// dispatcher rotates to a peer pre-commit instead of reporting a clean turn-end.
func TestMidFramePreludeTruncationIsRetryableReset(t *testing.T) {
	// Only 5 of the 12 prelude bytes — a mid-prelude cut.
	partial := []byte{0, 0, 0, 0, 0}
	cb := &KiroStreamCallback{OnText: func(string, bool) {}}

	err := parseEventStream(bytes.NewReader(partial), cb)
	if err == nil {
		t.Fatal("a mid-prelude truncation must surface an error, got nil")
	}
	var sre *ErrUpstreamStreamReset
	if !errors.As(err, &sre) {
		t.Fatalf("mid-frame truncation should classify as *ErrUpstreamStreamReset, got %T: %v", err, err)
	}
	if !isRetryableUpstreamError(err) {
		t.Fatal("a mid-frame truncation must be retryable so the dispatcher fails over")
	}
}

// TestMidFrameBodyTruncationIsRetryableReset verifies a cut in the frame BODY
// (after a valid prelude promised more bytes than arrive) is likewise treated as
// a retryable reset. This is the common real-world shape: a full prelude is read,
// then the connection drops while the payload is still streaming.
func TestMidFrameBodyTruncationIsRetryableReset(t *testing.T) {
	full := buildEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello world"})
	// Keep the 12-byte prelude (which promises the full totalLen) but drop the
	// last 8 bytes of the body so io.ReadFull on the message buffer short-reads.
	truncated := full[:len(full)-8]
	cb := &KiroStreamCallback{OnText: func(string, bool) {}}

	err := parseEventStream(bytes.NewReader(truncated), cb)
	if err == nil {
		t.Fatal("a mid-body truncation must surface an error, got nil")
	}
	var sre *ErrUpstreamStreamReset
	if !errors.As(err, &sre) {
		t.Fatalf("mid-body truncation should classify as *ErrUpstreamStreamReset, got %T: %v", err, err)
	}
}

// TestCompleteStreamWithToolUseIsSuccess verifies a normal tool-use stream
// (content + a toolUseEvent with stop:true, then clean EOF) completes without
// error — guarding against the mid-frame classifier over-firing on healthy
// multi-frame streams.
func TestCompleteStreamWithToolUseIsSuccess(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "let me check"}))
	buf.Write(buildEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "tu_1", "name": "search", "input": "{}", "stop": true,
	}))

	var text string
	var gotTool bool
	cb := &KiroStreamCallback{
		OnText:    func(s string, _ bool) { text += s },
		OnToolUse: func(KiroToolUse) { gotTool = true },
	}
	if err := parseEventStream(bytes.NewReader(buf.Bytes()), cb); err != nil {
		t.Fatalf("a complete tool-use stream must not error, got %v", err)
	}
	if text != "let me check" {
		t.Fatalf("content = %q, want %q", text, "let me check")
	}
	if !gotTool {
		t.Fatal("expected the tool use to be delivered")
	}
}
