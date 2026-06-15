package proxy

import "testing"

func TestNormalizeReasoningEffort(t *testing.T) {
	cases := map[string]string{
		"minimal":   effortMinimal,
		"MINIMAL":   effortMinimal,
		" none ":    effortMinimal,
		"off":       effortMinimal,
		"low":       effortLow,
		"medium":    effortMedium,
		"med":       effortMedium,
		"high":      effortHigh,
		"xhigh":     effortXHigh,
		"x-high":    effortXHigh,
		"extrahigh": effortXHigh,
		"max":       effortMax,
		"maximum":   effortMax,
		"":          effortUnset,
		"banana":    effortUnset,
		"  HIGH  ":  effortHigh,
		"  XHIGH ":  effortXHigh,
	}
	for in, want := range cases {
		if got := normalizeReasoningEffort(in); got != want {
			t.Errorf("normalizeReasoningEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEffortEngagesThinking(t *testing.T) {
	cases := []struct {
		effort        string
		wantEngage    bool
		wantSpecified bool
	}{
		{effortUnset, false, false},
		{effortMinimal, false, true},
		{effortLow, true, true},
		{effortMedium, true, true},
		{effortHigh, true, true},
		{effortXHigh, true, true},
		{effortMax, true, true},
	}
	for _, c := range cases {
		engage, specified := effortEngagesThinking(c.effort)
		if engage != c.wantEngage || specified != c.wantSpecified {
			t.Errorf("effortEngagesThinking(%q) = (%v,%v), want (%v,%v)",
				c.effort, engage, specified, c.wantEngage, c.wantSpecified)
		}
	}
}

// TestResolveThinkingWithEffort pins the core contract: explicit effort
// overrides the base decision, unset/unknown leaves it untouched.
func TestResolveThinkingWithEffort(t *testing.T) {
	cases := []struct {
		name string
		base bool
		raw  string
		want bool
	}{
		{"unset keeps base on", true, "", true},
		{"unset keeps base off", false, "", false},
		{"unknown keeps base on", true, "banana", true},
		{"minimal forces off even if base on", true, "minimal", false},
		{"minimal stays off", false, "minimal", false},
		{"low forces on even if base off", false, "low", true},
		{"medium forces on", false, "medium", true},
		{"high forces on", false, "high", true},
		{"max forces on", false, "max", true},
		{"high keeps on", true, "high", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveThinkingWithEffort(c.base, c.raw); got != c.want {
				t.Fatalf("resolveThinkingWithEffort(%v, %q) = %v, want %v", c.base, c.raw, got, c.want)
			}
		})
	}
}

// effortSchema builds a ModelInfo-style schema fragment advertising the given
// effort enum, mirroring what ListAvailableModels returns.
func effortSchema(levels ...string) map[string]interface{} {
	enum := make([]interface{}, len(levels))
	for i, l := range levels {
		enum[i] = l
	}
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"output_config": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"effort": map[string]interface{}{
						"type": "string",
						"enum": enum,
					},
				},
			},
		},
	}
}

func TestModelEffortLevels(t *testing.T) {
	// Opus-tier: full enum including xhigh.
	got := modelEffortLevels(effortSchema("low", "medium", "high", "xhigh", "max"))
	want := []string{"low", "medium", "high", "xhigh", "max"}
	if len(got) != len(want) {
		t.Fatalf("opus levels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("opus levels = %v, want %v", got, want)
		}
	}

	// Sonnet-tier: no xhigh.
	if got := modelEffortLevels(effortSchema("low", "medium", "high", "max")); len(got) != 4 {
		t.Fatalf("sonnet levels = %v, want 4 entries", got)
	}

	// Model with no schema -> nil.
	if got := modelEffortLevels(nil); got != nil {
		t.Fatalf("nil schema = %v, want nil", got)
	}
	// Schema present but no effort field -> nil.
	if got := modelEffortLevels(map[string]interface{}{"type": "object"}); got != nil {
		t.Fatalf("schema without effort = %v, want nil", got)
	}
}

// TestBuildEffortSchemaRoundTrip verifies buildEffortSchema is the inverse of
// modelEffortLevels — the boot-seed rehydration path persists a level list and
// must reconstruct a schema that modelEffortLevels reads back identically.
func TestBuildEffortSchemaRoundTrip(t *testing.T) {
	for _, levels := range [][]string{
		{"low", "medium", "high", "xhigh", "max"},
		{"low", "medium", "high", "max"},
		{"low"},
	} {
		schema := buildEffortSchema(levels)
		got := modelEffortLevels(schema)
		if len(got) != len(levels) {
			t.Fatalf("round-trip %v -> %v, length mismatch", levels, got)
		}
		for i := range levels {
			if got[i] != levels[i] {
				t.Fatalf("round-trip %v -> %v, mismatch at %d", levels, got, i)
			}
		}
	}

	// Empty / nil input -> no schema (model has no effort support).
	if buildEffortSchema(nil) != nil {
		t.Fatal("buildEffortSchema(nil) should return nil")
	}
	if buildEffortSchema([]string{}) != nil {
		t.Fatal("buildEffortSchema([]) should return nil")
	}
	if buildEffortSchema([]string{"", "  "}) != nil {
		t.Fatal("buildEffortSchema of blank-only levels should return nil")
	}
}

// TestResolveModelEffort pins the gating + clamp-down contract.
func TestResolveModelEffort(t *testing.T) {
	opus := []string{"low", "medium", "high", "xhigh", "max"}
	sonnet := []string{"low", "medium", "high", "max"}

	cases := []struct {
		name      string
		raw       string
		levels    []string
		wantLevel string
		wantOK    bool
	}{
		{"exact high on opus", "high", opus, "high", true},
		{"exact xhigh on opus", "xhigh", opus, "xhigh", true},
		{"max on opus", "max", opus, "max", true},
		{"xhigh clamps to high on sonnet", "xhigh", sonnet, "high", true},
		{"unset -> no field", "", opus, "", false},
		{"minimal -> no field (thinking path)", "minimal", opus, "", false},
		{"none -> no field", "none", opus, "", false},
		{"unknown -> no field", "banana", opus, "", false},
		{"no supported levels -> no field", "high", nil, "", false},
		{"low on sonnet", "low", sonnet, "low", true},
		{"x-high alias clamps on sonnet", "x-high", sonnet, "high", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			level, ok := resolveModelEffort(c.raw, c.levels)
			if level != c.wantLevel || ok != c.wantOK {
				t.Fatalf("resolveModelEffort(%q, %v) = (%q,%v), want (%q,%v)",
					c.raw, c.levels, level, ok, c.wantLevel, c.wantOK)
			}
		})
	}
}

func TestBuildEffortRequestFields(t *testing.T) {
	if got := buildEffortRequestFields(""); got != nil {
		t.Fatalf("empty level = %v, want nil", got)
	}
	got := buildEffortRequestFields("high")
	oc, ok := got["output_config"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing output_config in %v", got)
	}
	if oc["effort"] != "high" {
		t.Fatalf("effort = %v, want high", oc["effort"])
	}
}
