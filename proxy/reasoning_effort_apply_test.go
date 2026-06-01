package proxy

import "testing"

// newHandlerWithModelCache builds a bare Handler with a pre-seeded model cache
// so effort gating can be exercised without a live upstream fetch.
func newHandlerWithModelCache(models []ModelInfo) *Handler {
	h := &Handler{}
	h.cachedModels = models
	return h
}

func payloadForModel(modelID string) *KiroPayload {
	p := &KiroPayload{}
	p.ConversationState.CurrentMessage.UserInputMessage.ModelID = modelID
	return p
}

// effortField pulls output_config.effort out of a payload, "" if absent.
func effortField(p *KiroPayload) string {
	if p.AdditionalModelRequestFields == nil {
		return ""
	}
	oc, ok := p.AdditionalModelRequestFields["output_config"].(map[string]interface{})
	if !ok {
		return ""
	}
	s, _ := oc["effort"].(string)
	return s
}

func TestApplyReasoningEffortNativeForwarding(t *testing.T) {
	opusSchema := effortSchema("low", "medium", "high", "xhigh", "max")
	sonnetSchema := effortSchema("low", "medium", "high", "max")

	h := newHandlerWithModelCache([]ModelInfo{
		{ModelId: "claude-opus-4.6", AdditionalModelRequestFieldsSchema: opusSchema},
		{ModelId: "claude-sonnet-4.6", AdditionalModelRequestFieldsSchema: sonnetSchema},
		{ModelId: "claude-haiku-4.5"}, // no effort schema
	})

	cases := []struct {
		name    string
		model   string
		raw     string
		want    string // expected output_config.effort, "" = field omitted
		wantRet string
	}{
		{"opus high -> high", "claude-opus-4.6", "high", "high", "high"},
		{"opus xhigh -> xhigh", "claude-opus-4.6", "xhigh", "xhigh", "xhigh"},
		{"sonnet xhigh clamps to high", "claude-sonnet-4.6", "xhigh", "high", "high"},
		{"sonnet max -> max", "claude-sonnet-4.6", "max", "max", "max"},
		{"haiku unsupported -> omitted", "claude-haiku-4.5", "high", "", ""},
		{"opus minimal -> omitted (thinking path)", "claude-opus-4.6", "minimal", "", ""},
		{"opus unset -> omitted", "claude-opus-4.6", "", "", ""},
		{"unknown model -> omitted", "claude-mystery-9", "high", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := payloadForModel(c.model)
			ret := h.applyReasoningEffort(p, c.raw)
			if got := effortField(p); got != c.want {
				t.Fatalf("output_config.effort = %q, want %q", got, c.want)
			}
			if ret != c.wantRet {
				t.Fatalf("applyReasoningEffort return = %q, want %q", ret, c.wantRet)
			}
		})
	}
}

// TestApplyReasoningEffortPreservesOtherFields ensures setting effort does not
// clobber any other additionalModelRequestFields keys already present.
func TestApplyReasoningEffortPreservesOtherFields(t *testing.T) {
	h := newHandlerWithModelCache([]ModelInfo{
		{ModelId: "claude-opus-4.6", AdditionalModelRequestFieldsSchema: effortSchema("low", "high", "max")},
	})
	p := payloadForModel("claude-opus-4.6")
	p.AdditionalModelRequestFields = map[string]interface{}{
		"some_other_key": "keep-me",
	}
	h.applyReasoningEffort(p, "high")

	if effortField(p) != "high" {
		t.Fatalf("effort not set, got %v", p.AdditionalModelRequestFields)
	}
	if p.AdditionalModelRequestFields["some_other_key"] != "keep-me" {
		t.Fatalf("clobbered existing key: %v", p.AdditionalModelRequestFields)
	}
}
