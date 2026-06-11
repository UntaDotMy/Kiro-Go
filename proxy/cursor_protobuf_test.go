package proxy

import (
	"encoding/binary"
	"testing"
)

// TestCursorVarintRoundTrip checks the varint codec against known protobuf encodings.
func TestCursorVarintRoundTrip(t *testing.T) {
	cases := []uint64{0, 1, 127, 128, 300, 16384, 1 << 21}
	for _, v := range cases {
		enc := cEncodeVarint(v)
		got, pos := cDecodeVarint(enc, 0)
		if got != v {
			t.Errorf("varint %d: decoded %d", v, got)
		}
		if pos != len(enc) {
			t.Errorf("varint %d: consumed %d of %d bytes", v, pos, len(enc))
		}
	}
	// 300 must encode to the canonical two bytes 0xAC 0x02.
	if enc := cEncodeVarint(300); len(enc) != 2 || enc[0] != 0xac || enc[1] != 0x02 {
		t.Errorf("varint 300 = % x, want ac 02", enc)
	}
}

// TestCursorFieldDecode round-trips a varint and a length-delimited field through
// the encoder and decoder.
func TestCursorFieldDecode(t *testing.T) {
	msg := append(cFieldVarint(2, 19), cFieldLen(9, "hello")...)
	fields := cDecodeMessage(msg)
	if f, ok := fields[2]; !ok || len(f) != 1 || f[0].varint != 19 {
		t.Fatalf("field 2 varint: %+v", fields[2])
	}
	if f, ok := fields[9]; !ok || len(f) != 1 || string(f[0].value) != "hello" {
		t.Fatalf("field 9 len: %+v", fields[9])
	}
}

// TestCursorConnectFrame checks the 5-byte Connect-RPC frame header.
func TestCursorConnectFrame(t *testing.T) {
	payload := []byte("abcdef")
	frame := cWrapConnectFrame(payload)
	if frame[0] != 0x00 {
		t.Errorf("flags = %d, want 0 (uncompressed)", frame[0])
	}
	if got := binary.BigEndian.Uint32(frame[1:5]); int(got) != len(payload) {
		t.Errorf("length header = %d, want %d", got, len(payload))
	}
	if string(frame[5:]) != "abcdef" {
		t.Errorf("payload = %q", frame[5:])
	}
}

// TestCursorExtractText builds a StreamUnifiedChatResponseWithTools with a text
// response (field 2 -> nested field 1) and confirms the extractor reads it.
func TestCursorExtractText(t *testing.T) {
	inner := cFieldLen(cfRespText, "hi there")     // StreamUnifiedChatResponse.text
	outer := cFieldLen(cfRespResponse, inner)      // wrapper field 2
	ex := cExtractFromResponse(outer)
	if ex.text != "hi there" {
		t.Errorf("text = %q, want %q", ex.text, "hi there")
	}
	if ex.toolName != "" {
		t.Errorf("unexpected tool: %q", ex.toolName)
	}
}

// TestCursorExtractThinking confirms thinking (field 25 -> nested field 1) decodes.
func TestCursorExtractThinking(t *testing.T) {
	think := cFieldLen(cfThinkText, "reasoning...")
	inner := cFieldLen(cfThinking, think)
	outer := cFieldLen(cfRespResponse, inner)
	ex := cExtractFromResponse(outer)
	if ex.thinking != "reasoning..." {
		t.Errorf("thinking = %q", ex.thinking)
	}
}

// TestCursorChecksumShape sanity-checks the Jyh-cipher checksum: it must end with
// the machine id and carry a non-empty url-safe-base64 prefix.
func TestCursorChecksumShape(t *testing.T) {
	machine := "abc123machine"
	cs := cursorChecksum(machine)
	if len(cs) <= len(machine) || cs[len(cs)-len(machine):] != machine {
		t.Errorf("checksum %q must end with machine id %q", cs, machine)
	}
	prefix := cs[:len(cs)-len(machine)]
	if prefix == "" {
		t.Errorf("checksum prefix is empty")
	}
	for _, c := range prefix {
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			t.Errorf("checksum prefix has non-url-safe char %q", c)
		}
	}
}

// TestCursorHeadersComplete verifies the required Cursor headers are present.
func TestCursorHeadersComplete(t *testing.T) {
	h := buildCursorHeaders("tok::secrettoken", "machine-xyz", true)
	for _, k := range []string{"authorization", "x-cursor-checksum", "x-client-key", "x-session-id", "content-type", "x-ghost-mode"} {
		if h[k] == "" {
			t.Errorf("missing header %q", k)
		}
	}
	if h["authorization"] != "Bearer secrettoken" {
		t.Errorf("authorization = %q, want stripped token", h["authorization"])
	}
	if h["content-type"] != "application/connect+proto" {
		t.Errorf("content-type = %q", h["content-type"])
	}
}

// TestCursorBuildRequestDecodes ensures a built chat request is a valid top-level
// frame (field 1 = the StreamUnifiedChatRequest) that decodes and contains the model.
func TestCursorBuildRequestDecodes(t *testing.T) {
	msgs := []cursorMsg{{Role: "user", Content: "hello"}}
	payload := cBuildChatRequest(msgs, "gpt-5", nil, "")
	top := cDecodeMessage(payload)
	reqField, ok := top[cfRequest]
	if !ok || len(reqField) == 0 {
		t.Fatalf("top-level field %d (request) missing", cfRequest)
	}
	inner := cDecodeMessage(reqField[0].value)
	if _, ok := inner[cfMessages]; !ok {
		t.Errorf("request missing messages field %d", cfMessages)
	}
	mf, ok := inner[cfModel]
	if !ok || len(mf) == 0 {
		t.Fatalf("request missing model field %d", cfModel)
	}
	model := cDecodeMessage(mf[0].value)
	if nm, ok := model[cfModelName]; !ok || string(nm[0].value) != "gpt-5" {
		t.Errorf("model name not gpt-5: %+v", model[cfModelName])
	}
}
