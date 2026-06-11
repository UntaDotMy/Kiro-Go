package proxy

import (
	"encoding/binary"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

// Cursor Connect-RPC protobuf wire encoder/decoder, ported from 9router's
// open-sse/utils/cursorProtobuf.js. Cursor's StreamUnifiedChatWithTools endpoint
// speaks protobuf (not JSON) framed in Connect-RPC envelopes. We hand-roll the
// minimal wire format (varint + length-delimited fields) with the exact field
// numbers Cursor's proto uses — including the reverse-engineered "unknown" fields
// that the server requires. This is byte-compatible with the JS implementation.

const (
	cwireVarint = 0
	cwireLen    = 2
	cwireFixed64 = 1
	cwireFixed32 = 5
)

const (
	cRoleUser      = 1
	cRoleAssistant = 2
	cUnifiedChat   = 1
	cUnifiedAgent  = 2
	cThinkUnspec   = 0
	cThinkMedium   = 1
	cThinkHigh     = 2
	cToolMCP       = 19
)

// Cursor proto field numbers (subset we use), mirroring FIELD in cursorProtobuf.js.
const (
	cfRequest = 1

	// StreamUnifiedChatRequest
	cfMessages       = 1
	cfUnknown2       = 2
	cfInstruction    = 3
	cfUnknown4       = 4
	cfModel          = 5
	cfWebTool        = 8
	cfUnknown13      = 13
	cfCursorSetting  = 15
	cfUnknown19      = 19
	cfConversationID = 23
	cfMetadata       = 26
	cfIsAgentic      = 27
	cfSupportedTools = 29
	cfMessageIDs     = 30
	cfMCPTools       = 34
	cfLargeContext   = 35
	cfUnknown38      = 38
	cfUnifiedMode    = 46
	cfUnknown47      = 47
	cfDisableTools   = 48
	cfThinkingLevel  = 49
	cfUnknown51      = 51
	cfUnknown53      = 53
	cfUnifiedModeNm  = 54

	// ConversationMessage
	cfMsgContent  = 1
	cfMsgRole     = 2
	cfMsgID       = 13
	cfMsgIsAgentic = 29
	cfMsgUnifMode = 47
	cfMsgSuppTools = 51

	// Model
	cfModelName  = 1
	cfModelEmpty = 4

	// Instruction
	cfInstrText = 1

	// CursorSetting
	cfSetPath  = 1
	cfSetU3    = 3
	cfSetU6    = 6
	cfSetU8    = 8
	cfSetU9    = 9
	cfSet6F1   = 1
	cfSet6F2   = 2

	// Metadata
	cfMetaPlatform = 1
	cfMetaArch     = 2
	cfMetaVersion  = 3
	cfMetaCwd      = 4
	cfMetaTS       = 5

	// MessageId
	cfMsgidID   = 1
	cfMsgidRole = 3

	// MCPTool
	cfMcpToolName   = 1
	cfMcpToolDesc   = 2
	cfMcpToolParams = 3
	cfMcpToolServer = 4

	// Response: StreamUnifiedChatResponseWithTools
	cfRespToolCall = 1
	cfRespResponse = 2

	// ClientSideToolV2Call (response)
	cfToolID       = 3
	cfToolName     = 9
	cfToolRawArgs  = 10
	cfToolMcpParam = 27

	// MCPParams
	cfMcpToolsList = 1
	cfMcpNestName  = 1
	cfMcpNestParam = 3

	// StreamUnifiedChatResponse
	cfRespText = 1
	cfThinking = 25
	cfThinkText = 1
)

// --- primitive encoding ---

func cEncodeVarint(v uint64) []byte {
	var b []byte
	for v >= 0x80 {
		b = append(b, byte(v&0x7f)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// cField encodes one field (varint or length-delimited). For LEN, value is a string
// or []byte.
func cFieldVarint(fieldNum int, v uint64) []byte {
	tag := uint64(fieldNum<<3) | cwireVarint
	return append(cEncodeVarint(tag), cEncodeVarint(v)...)
}

func cFieldLen(fieldNum int, value interface{}) []byte {
	var data []byte
	switch t := value.(type) {
	case string:
		data = []byte(t)
	case []byte:
		data = t
	case nil:
		data = nil
	}
	tag := uint64(fieldNum<<3) | cwireLen
	out := cEncodeVarint(tag)
	out = append(out, cEncodeVarint(uint64(len(data)))...)
	return append(out, data...)
}

// --- message encoding ---

func cEncodeInstruction(text string) []byte {
	if text == "" {
		return nil
	}
	return cFieldLen(cfInstrText, text)
}

func cEncodeModel(name string) []byte {
	out := cFieldLen(cfModelName, name)
	return append(out, cFieldLen(cfModelEmpty, []byte{})...)
}

func cEncodeCursorSetting() []byte {
	u6 := append(cFieldLen(cfSet6F1, []byte{}), cFieldLen(cfSet6F2, []byte{})...)
	out := cFieldLen(cfSetPath, `cursor\aisettings`)
	out = append(out, cFieldLen(cfSetU3, []byte{})...)
	out = append(out, cFieldLen(cfSetU6, u6)...)
	out = append(out, cFieldVarint(cfSetU8, 1)...)
	out = append(out, cFieldVarint(cfSetU9, 1)...)
	return out
}

func cEncodeMetadata() []byte {
	out := cFieldLen(cfMetaPlatform, "linux")
	out = append(out, cFieldLen(cfMetaArch, "x64")...)
	out = append(out, cFieldLen(cfMetaVersion, "v20.0.0")...)
	out = append(out, cFieldLen(cfMetaCwd, "/")...)
	out = append(out, cFieldLen(cfMetaTS, cursorNowISO())...)
	return out
}

func cEncodeMessageID(id string, role int) []byte {
	out := cFieldLen(cfMsgidID, id)
	return append(out, cFieldVarint(cfMsgidRole, uint64(role))...)
}

func cEncodeMcpTool(name, desc string, schema interface{}) []byte {
	var out []byte
	if name != "" {
		out = append(out, cFieldLen(cfMcpToolName, name)...)
	}
	if desc != "" {
		out = append(out, cFieldLen(cfMcpToolDesc, desc)...)
	}
	if schema != nil {
		if m, ok := schema.(map[string]interface{}); !ok || len(m) > 0 {
			if b, err := json.Marshal(schema); err == nil {
				out = append(out, cFieldLen(cfMcpToolParams, string(b))...)
			}
		}
	}
	return append(out, cFieldLen(cfMcpToolServer, "custom")...)
}

// cEncodeMessage encodes one ConversationMessage (text only — tool results are
// omitted in this port; Cursor still accepts plain chat/agent turns).
func cEncodeMessage(content string, role, msgID string, isLast, hasTools bool) []byte {
	_ = isLast
	out := cFieldLen(cfMsgContent, content)
	r := cRoleAssistant
	if role == "user" {
		r = cRoleUser
	}
	out = append(cFieldVarintPrefixContent(out), cFieldVarint(cfMsgRole, uint64(r))...)
	out = append(out, cFieldLen(cfMsgID, msgID)...)
	agentic := uint64(0)
	mode := uint64(cUnifiedChat)
	if hasTools {
		agentic = 1
		mode = cUnifiedAgent
	}
	out = append(out, cFieldVarint(cfMsgIsAgentic, agentic)...)
	out = append(out, cFieldVarint(cfMsgUnifMode, mode)...)
	return out
}

// cFieldVarintPrefixContent is a no-op helper kept for readability of the message
// builder above (content field already appended). It returns its input unchanged.
func cFieldVarintPrefixContent(b []byte) []byte { return b }

// cursorMsg is a normalized chat turn for the encoder.
type cursorMsg struct {
	Role    string
	Content string
}

// cursorTool is a normalized tool definition for the encoder.
type cursorTool struct {
	Name        string
	Description string
	Schema      interface{}
}

// cEncodeRequest builds the inner StreamUnifiedChatRequest.
func cEncodeRequest(messages []cursorMsg, modelName string, tools []cursorTool, reasoningEffort string) []byte {
	hasTools := len(tools) > 0
	isAgentic := hasTools

	type fm struct {
		content, role, id string
		isLast            bool
	}
	var formatted []fm
	type mid struct {
		id   string
		role int
	}
	var msgIDs []mid

	for i, m := range messages {
		id := uuid.New().String()
		formatted = append(formatted, fm{m.Content, m.Role, id, i == len(messages)-1})
		r := cRoleAssistant
		if m.Role == "user" {
			r = cRoleUser
		}
		msgIDs = append(msgIDs, mid{id, r})
	}

	thinking := uint64(cThinkUnspec)
	switch reasoningEffort {
	case "medium":
		thinking = cThinkMedium
	case "high":
		thinking = cThinkHigh
	}

	var out []byte
	for _, f := range formatted {
		out = append(out, cFieldLen(cfMessages, cEncodeMessage(f.content, f.role, f.id, f.isLast, hasTools))...)
	}
	out = append(out, cFieldVarint(cfUnknown2, 1)...)
	out = append(out, cFieldLen(cfInstruction, cEncodeInstruction(""))...)
	out = append(out, cFieldVarint(cfUnknown4, 1)...)
	out = append(out, cFieldLen(cfModel, cEncodeModel(modelName))...)
	out = append(out, cFieldLen(cfWebTool, "")...)
	out = append(out, cFieldVarint(cfUnknown13, 1)...)
	out = append(out, cFieldLen(cfCursorSetting, cEncodeCursorSetting())...)
	out = append(out, cFieldVarint(cfUnknown19, 1)...)
	out = append(out, cFieldLen(cfConversationID, uuid.New().String())...)
	out = append(out, cFieldLen(cfMetadata, cEncodeMetadata())...)
	out = append(out, cFieldVarint(cfIsAgentic, boolToU64(isAgentic))...)
	if isAgentic {
		out = append(out, cFieldLen(cfSupportedTools, cEncodeVarint(1))...)
	}
	for _, mi := range msgIDs {
		out = append(out, cFieldLen(cfMessageIDs, cEncodeMessageID(mi.id, mi.role))...)
	}
	for _, t := range tools {
		out = append(out, cFieldLen(cfMCPTools, cEncodeMcpTool(t.Name, t.Description, t.Schema))...)
	}
	out = append(out, cFieldVarint(cfLargeContext, 0)...)
	out = append(out, cFieldVarint(cfUnknown38, 0)...)
	if isAgentic {
		out = append(out, cFieldVarint(cfUnifiedMode, cUnifiedAgent)...)
	} else {
		out = append(out, cFieldVarint(cfUnifiedMode, cUnifiedChat)...)
	}
	out = append(out, cFieldLen(cfUnknown47, "")...)
	out = append(out, cFieldVarint(cfDisableTools, boolToU64(!isAgentic))...)
	out = append(out, cFieldVarint(cfThinkingLevel, thinking)...)
	out = append(out, cFieldVarint(cfUnknown51, 0)...)
	out = append(out, cFieldVarint(cfUnknown53, 1)...)
	if isAgentic {
		out = append(out, cFieldLen(cfUnifiedModeNm, "Agent")...)
	} else {
		out = append(out, cFieldLen(cfUnifiedModeNm, "Ask")...)
	}
	return out
}

// cBuildChatRequest wraps the request in the top-level field 1.
func cBuildChatRequest(messages []cursorMsg, modelName string, tools []cursorTool, reasoningEffort string) []byte {
	return cFieldLen(cfRequest, cEncodeRequest(messages, modelName, tools, reasoningEffort))
}

// cWrapConnectFrame prepends the 5-byte Connect-RPC frame header (flags + 4-byte
// big-endian length). Requests are never compressed (Cursor doesn't accept it).
func cWrapConnectFrame(payload []byte) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = 0x00
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

func boolToU64(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// --- decoding ---

type cDecodedField struct {
	wireType int
	value    []byte // for LEN; varints stored as encoded bytes in `varint`
	varint   uint64
}

func cDecodeVarint(buf []byte, off int) (uint64, int) {
	var result uint64
	var shift uint
	pos := off
	for pos < len(buf) {
		b := buf[pos]
		result |= uint64(b&0x7f) << shift
		pos++
		if b&0x80 == 0 {
			break
		}
		shift += 7
	}
	return result, pos
}

// cDecodeMessage parses a protobuf message into field number -> occurrences.
func cDecodeMessage(data []byte) map[int][]cDecodedField {
	fields := map[int][]cDecodedField{}
	pos := 0
	for pos < len(data) {
		tag, p1 := cDecodeVarint(data, pos)
		if p1 > len(data) {
			break
		}
		fieldNum := int(tag >> 3)
		wireType := int(tag & 0x07)
		var f cDecodedField
		f.wireType = wireType
		pos = p1
		switch wireType {
		case cwireVarint:
			f.varint, pos = cDecodeVarint(data, pos)
		case cwireLen:
			length, p2 := cDecodeVarint(data, pos)
			end := p2 + int(length)
			if end > len(data) {
				return fields
			}
			f.value = data[p2:end]
			pos = end
		case cwireFixed64:
			if pos+8 > len(data) {
				return fields
			}
			f.value = data[pos : pos+8]
			pos += 8
		case cwireFixed32:
			if pos+4 > len(data) {
				return fields
			}
			f.value = data[pos : pos+4]
			pos += 4
		default:
			return fields
		}
		fields[fieldNum] = append(fields[fieldNum], f)
	}
	return fields
}

// cursorExtract is the decoded content of one response frame.
type cursorExtract struct {
	text     string
	thinking string
	toolName string
	toolID   string
	toolArgs string
}

// cExtractFromResponse decodes one StreamUnifiedChatResponseWithTools payload into
// text / thinking / tool-call, mirroring extractTextFromResponse.
func cExtractFromResponse(payload []byte) cursorExtract {
	var out cursorExtract
	fields := cDecodeMessage(payload)

	// Field 1: tool call.
	if tc, ok := fields[cfRespToolCall]; ok && len(tc) > 0 {
		call := cDecodeMessage(tc[0].value)
		if id, ok := call[cfToolID]; ok && len(id) > 0 {
			full := string(id[0].value)
			out.toolID = strings.SplitN(full, "\n", 2)[0]
		}
		if nm, ok := call[cfToolName]; ok && len(nm) > 0 {
			out.toolName = string(nm[0].value)
		}
		if mp, ok := call[cfToolMcpParam]; ok && len(mp) > 0 {
			params := cDecodeMessage(mp[0].value)
			if tl, ok := params[cfMcpToolsList]; ok && len(tl) > 0 {
				tool := cDecodeMessage(tl[0].value)
				if n, ok := tool[cfMcpNestName]; ok && len(n) > 0 {
					out.toolName = string(n[0].value)
				}
				if p, ok := tool[cfMcpNestParam]; ok && len(p) > 0 {
					out.toolArgs = string(p[0].value)
				}
			}
		}
		if out.toolArgs == "" {
			if ra, ok := call[cfToolRawArgs]; ok && len(ra) > 0 {
				out.toolArgs = string(ra[0].value)
			}
		}
		return out
	}

	// Field 2: response (text + thinking).
	if rf, ok := fields[cfRespResponse]; ok && len(rf) > 0 {
		nested := cDecodeMessage(rf[0].value)
		if t, ok := nested[cfRespText]; ok && len(t) > 0 {
			out.text = string(t[0].value)
		}
		if th, ok := nested[cfThinking]; ok && len(th) > 0 {
			tm := cDecodeMessage(th[0].value)
			if tt, ok := tm[cfThinkText]; ok && len(tt) > 0 {
				out.thinking = string(tt[0].value)
			}
		}
	}
	return out
}
