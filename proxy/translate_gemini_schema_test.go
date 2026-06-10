package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSanitizeGeminiToolSchemaStripsUnsupported is the regression guard for the
// blocking bug: Gemini's function-calling schema validator REJECTS stock JSON
// Schema keywords that Claude Code, OpenAI SDKs, and MCP tools emit by default
// ($schema, additionalProperties, $ref, oneOf/allOf/not, pattern, format-ish
// constraints). An unsanitized schema makes Gemini 400 the WHOLE request, so tool
// calling to any Gemini provider is dead on arrival. This pins that the
// unsupported keywords are removed while the usable structure survives.
func TestSanitizeGeminiToolSchemaStripsUnsupported(t *testing.T) {
	raw := `{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"city": {"type": "string", "minLength": 1, "maxLength": 100, "pattern": "^[A-Z]"},
			"unit": {"type": "string", "enum": ["c", "f"], "default": "c"},
			"opts": {
				"type": "object",
				"additionalProperties": true,
				"properties": {"verbose": {"type": "boolean"}}
			}
		},
		"required": ["city", "ghost"],
		"oneOf": [{"required": ["city"]}]
	}`
	var schema map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	cleaned := sanitizeGeminiToolSchema(schema)
	if cleaned == nil {
		t.Fatal("sanitizeGeminiToolSchema returned nil for a valid object schema")
	}

	// Top-level unsupported keys gone.
	for _, k := range []string{"$schema", "additionalProperties", "oneOf"} {
		if _, present := cleaned[k]; present {
			t.Errorf("unsupported key %q survived sanitization", k)
		}
	}
	if cleaned["type"] != "object" {
		t.Errorf("type = %v, want object", cleaned["type"])
	}

	// Property-level constraints stripped, but the property + its type survive.
	props := cleaned["properties"].(map[string]interface{})
	city := props["city"].(map[string]interface{})
	if city["type"] != "string" {
		t.Errorf("city.type = %v, want string", city["type"])
	}
	for _, k := range []string{"minLength", "maxLength", "pattern"} {
		if _, present := city[k]; present {
			t.Errorf("city.%s should be stripped", k)
		}
	}
	// enum is supported and must survive; default is not.
	unit := props["unit"].(map[string]interface{})
	if _, present := unit["enum"]; !present {
		t.Errorf("unit.enum should survive")
	}
	if _, present := unit["default"]; present {
		t.Errorf("unit.default should be stripped")
	}
	// Nested object cleaned recursively.
	opts := props["opts"].(map[string]interface{})
	if _, present := opts["additionalProperties"]; present {
		t.Errorf("nested additionalProperties should be stripped")
	}

	// required[] pruned to declared properties: "ghost" isn't a property, drop it;
	// "city" is, keep it.
	req, _ := cleaned["required"].([]interface{})
	if len(req) != 1 || req[0] != "city" {
		t.Errorf("required = %v, want [city] (ghost pruned)", req)
	}
}

// TestSanitizeGeminiConstAndTypeArray covers const->enum conversion and type-array
// coercion (["string","null"] -> "string").
func TestSanitizeGeminiConstAndTypeArray(t *testing.T) {
	raw := `{
		"type": "object",
		"properties": {
			"mode": {"const": "fast"},
			"name": {"type": ["string", "null"]}
		}
	}`
	var schema map[string]interface{}
	json.Unmarshal([]byte(raw), &schema)
	cleaned := sanitizeGeminiToolSchema(schema)
	props := cleaned["properties"].(map[string]interface{})

	mode := props["mode"].(map[string]interface{})
	enum, ok := mode["enum"].([]interface{})
	if !ok || len(enum) != 1 || enum[0] != "fast" {
		t.Errorf("const should become enum:[fast], got %v", mode)
	}
	if _, present := mode["const"]; present {
		t.Errorf("const should be removed after conversion")
	}

	name := props["name"].(map[string]interface{})
	if name["type"] != "string" {
		t.Errorf("type array should coerce to 'string', got %v", name["type"])
	}
}

// TestGeminiToolsEmptyParamsGetObjectSchema verifies a tool with no/empty
// parameters still produces a valid empty-object schema (Gemini rejects a declared
// function whose parameters isn't an object).
func TestGeminiToolsEmptyParamsGetObjectSchema(t *testing.T) {
	tools := []ClaudeTool{{Name: "noop", Description: "does nothing"}}
	decls := claudeToolsToGemini(tools)
	if len(decls) != 1 {
		t.Fatalf("expected 1 functionDeclarations group, got %d", len(decls))
	}
	fns := decls[0]["functionDeclarations"].([]map[string]interface{})
	params := fns[0]["parameters"].(map[string]interface{})
	if params["type"] != "object" {
		t.Errorf("empty-param tool should get type:object, got %v", params)
	}
}

// TestBuildGeminiBodyToolSchemaSanitizedEndToEnd confirms the sanitizer is wired
// into the actual body builder (not just callable in isolation).
func TestBuildGeminiBodyToolSchemaSanitizedEndToEnd(t *testing.T) {
	nr := &NormalizedRequest{
		Model: "gemini-2.5-flash",
		Claude: &ClaudeRequest{
			MaxTokens: 128,
			Messages:  []ClaudeMessage{{Role: "user", Content: "hi"}},
			Tools: []ClaudeTool{{
				Name:        "search",
				Description: "search",
				InputSchema: map[string]interface{}{
					"$schema":              "http://json-schema.org/draft-07/schema#",
					"type":                 "object",
					"additionalProperties": false,
					"properties":           map[string]interface{}{"q": map[string]interface{}{"type": "string"}},
				},
			}},
		},
	}
	raw, err := buildGeminiBody(nr, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("buildGeminiBody: %v", err)
	}
	// The serialized body must NOT contain the unsupported keywords anywhere.
	s := string(raw)
	for _, bad := range []string{"$schema", "additionalProperties"} {
		if strings.Contains(s, bad) {
			t.Errorf("Gemini body still contains unsupported keyword %q: %s", bad, s)
		}
	}
}
