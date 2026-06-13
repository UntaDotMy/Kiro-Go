package proxy

import (
	"encoding/json"
	"os"
	"testing"
)

// Story s9: the Gemini schema sanitizer must PRESERVE union (anyOf) semantics and
// the constraints Gemini's Schema proto supports, instead of flattening anyOf to
// its first branch and stripping every constraint. Genuinely-unsupported keywords
// are still removed; KIRO_GEMINI_STRICT_SCHEMA=1 restores the old aggressive strip.

func mustSanitizeGemini(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	var schema map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cleaned := sanitizeGeminiToolSchema(schema)
	if cleaned == nil {
		t.Fatal("sanitizeGeminiToolSchema returned nil")
	}
	return cleaned
}

// TestGeminiPreservesNestedAnyOf: an anyOf inside a property must be kept as a
// union (both branches, sanitized), not flattened to the first branch.
func TestGeminiPreservesNestedAnyOf(t *testing.T) {
	os.Unsetenv("KIRO_GEMINI_STRICT_SCHEMA")
	cleaned := mustSanitizeGemini(t, `{
		"type": "object",
		"properties": {
			"value": {
				"anyOf": [
					{"type": "string"},
					{"type": "integer"}
				]
			}
		}
	}`)
	props := cleaned["properties"].(map[string]interface{})
	value := props["value"].(map[string]interface{})
	branches, ok := value["anyOf"].([]interface{})
	if !ok {
		t.Fatalf("nested anyOf should be preserved as a union, got %v", value)
	}
	if len(branches) != 2 {
		t.Fatalf("anyOf should keep both branches, got %d", len(branches))
	}
	// Each branch's type should survive.
	b0 := branches[0].(map[string]interface{})
	b1 := branches[1].(map[string]interface{})
	if b0["type"] != "string" || b1["type"] != "integer" {
		t.Fatalf("anyOf branches lost their types: %v / %v", b0, b1)
	}
}

// TestGeminiFlattensRootAnyOf: at the parameters ROOT, anyOf is still flattened
// (Gemini requires an object root, not a bare union).
func TestGeminiFlattensRootAnyOf(t *testing.T) {
	os.Unsetenv("KIRO_GEMINI_STRICT_SCHEMA")
	cleaned := mustSanitizeGemini(t, `{
		"anyOf": [
			{"type": "object", "properties": {"a": {"type": "string"}}},
			{"type": "object", "properties": {"b": {"type": "string"}}}
		]
	}`)
	if _, present := cleaned["anyOf"]; present {
		t.Errorf("root-level anyOf should be flattened (Gemini needs an object root), got %v", cleaned)
	}
	if cleaned["type"] != "object" {
		t.Errorf("flattened root should have type object, got %v", cleaned["type"])
	}
}

// TestGeminiPreservesSupportedConstraints: minItems/maxItems/minimum/maximum etc.
// are retained (they're part of the Gemini Schema proto).
func TestGeminiPreservesSupportedConstraints(t *testing.T) {
	os.Unsetenv("KIRO_GEMINI_STRICT_SCHEMA")
	cleaned := mustSanitizeGemini(t, `{
		"type": "object",
		"properties": {
			"tags": {"type": "array", "items": {"type": "string"}, "minItems": 1, "maxItems": 5},
			"age": {"type": "integer", "minimum": 0, "maximum": 120}
		}
	}`)
	props := cleaned["properties"].(map[string]interface{})
	tags := props["tags"].(map[string]interface{})
	if _, ok := tags["minItems"]; !ok {
		t.Error("minItems should be preserved")
	}
	if _, ok := tags["maxItems"]; !ok {
		t.Error("maxItems should be preserved")
	}
	age := props["age"].(map[string]interface{})
	if _, ok := age["minimum"]; !ok {
		t.Error("minimum should be preserved")
	}
	if _, ok := age["maximum"]; !ok {
		t.Error("maximum should be preserved")
	}
}

// TestGeminiStillStripsTrulyUnsupported: exclusiveMinimum/multipleOf/uniqueItems
// and structural composition are dropped even in default mode (they'd 400).
func TestGeminiStillStripsTrulyUnsupported(t *testing.T) {
	os.Unsetenv("KIRO_GEMINI_STRICT_SCHEMA")
	cleaned := mustSanitizeGemini(t, `{
		"type": "object",
		"properties": {
			"n": {"type": "number", "exclusiveMinimum": 0, "multipleOf": 2},
			"arr": {"type": "array", "items": {"type": "string"}, "uniqueItems": true}
		},
		"allOf": [{"required": ["n"]}]
	}`)
	if _, present := cleaned["allOf"]; present {
		t.Error("allOf must be stripped")
	}
	props := cleaned["properties"].(map[string]interface{})
	n := props["n"].(map[string]interface{})
	for _, k := range []string{"exclusiveMinimum", "multipleOf"} {
		if _, present := n[k]; present {
			t.Errorf("%s has no Gemini equivalent and must be stripped", k)
		}
	}
	arr := props["arr"].(map[string]interface{})
	if _, present := arr["uniqueItems"]; present {
		t.Error("uniqueItems must be stripped")
	}
}

// TestGeminiFormatFiltered: an allowed format survives, a disallowed one is dropped.
func TestGeminiFormatFiltered(t *testing.T) {
	os.Unsetenv("KIRO_GEMINI_STRICT_SCHEMA")
	cleaned := mustSanitizeGemini(t, `{
		"type": "object",
		"properties": {
			"when": {"type": "string", "format": "date-time"},
			"link": {"type": "string", "format": "uri"}
		}
	}`)
	props := cleaned["properties"].(map[string]interface{})
	when := props["when"].(map[string]interface{})
	if when["format"] != "date-time" {
		t.Errorf("allowed format date-time should survive, got %v", when["format"])
	}
	link := props["link"].(map[string]interface{})
	if _, present := link["format"]; present {
		t.Error("unsupported format 'uri' should be dropped")
	}
}

// TestGeminiStrictModeRestoresAggressiveStrip: with KIRO_GEMINI_STRICT_SCHEMA=1,
// constraints are stripped and anyOf is flattened (the old behavior).
func TestGeminiStrictModeRestoresAggressiveStrip(t *testing.T) {
	os.Setenv("KIRO_GEMINI_STRICT_SCHEMA", "1")
	defer os.Unsetenv("KIRO_GEMINI_STRICT_SCHEMA")
	cleaned := mustSanitizeGemini(t, `{
		"type": "object",
		"properties": {
			"value": {"anyOf": [{"type": "string"}, {"type": "integer"}]},
			"city": {"type": "string", "minLength": 1, "pattern": "^[A-Z]"}
		}
	}`)
	props := cleaned["properties"].(map[string]interface{})
	value := props["value"].(map[string]interface{})
	if _, present := value["anyOf"]; present {
		t.Error("strict mode should flatten anyOf")
	}
	city := props["city"].(map[string]interface{})
	for _, k := range []string{"minLength", "pattern"} {
		if _, present := city[k]; present {
			t.Errorf("strict mode should strip constraint %s", k)
		}
	}
}
