package proxy

// ============================================================================
// Gemini JSON-Schema sanitization.
//
// Gemini's functionDeclarations.parameters accepts only a SUBSET of JSON Schema
// (an OpenAPI-3.0 "Schema" object). A request whose tool schema carries keywords
// outside that subset — which Claude Code, OpenAI SDKs, and most MCP tools emit by
// default ($schema, additionalProperties, $ref, oneOf/anyOf/allOf, const, format
// on the wrong types, etc.) — is rejected with HTTP 400, and the WHOLE request
// fails. So tool calling to any Gemini provider is dead-on-arrival unless we strip
// the unsupported keywords first.
//
// This mirrors 9router's cleanJSONSchemaForAntigravity: recursively drop the
// unsupported constraint keywords, convert const->enum, coerce a type array to a
// single type, infer a missing object type, and prune required[] to declared
// properties. We intentionally keep it conservative — we only REMOVE/normalize, we
// never invent constraints — so a valid (if looser) schema always survives.
// ============================================================================

// geminiUnsupportedSchemaKeys are JSON-Schema keywords Gemini's function-calling
// schema validator rejects. Recursively deleted from every schema node.
var geminiUnsupportedSchemaKeys = map[string]bool{
	"$schema":               true,
	"$id":                   true,
	"$ref":                  true,
	"$defs":                 true,
	"$comment":              true,
	"definitions":           true,
	"additionalProperties":  true,
	"patternProperties":     true,
	"propertyNames":         true,
	"unevaluatedProperties": true,
	"dependencies":          true,
	"dependentSchemas":      true,
	"dependentRequired":     true,
	"if":                    true,
	"then":                  true,
	"else":                  true,
	"not":                   true,
	"allOf":                 true,
	"oneOf":                 true,
	// anyOf is handled specially (Gemini DOES support a limited anyOf, but the
	// safest portable behavior is to flatten it — see sanitizeGeminiSchema).
	"patternProperty":  true,
	"pattern":          true,
	"minLength":        true,
	"maxLength":        true,
	"minItems":         true,
	"maxItems":         true,
	"uniqueItems":      true,
	"minProperties":    true,
	"maxProperties":    true,
	"exclusiveMinimum": true,
	"exclusiveMaximum": true,
	"multipleOf":       true,
	// Gemini's function-calling schema only accepts a narrow set of `format`
	// values (date-time/enum for strings, int32/int64 for integers, float/double
	// for numbers) and 400s on anything else — and stock tools emit "uri",
	// "uuid", "email", "hostname", etc. Stripping it entirely (as 9router does)
	// is the portable, no-400 choice; it only loses a soft hint.
	"format":           true,
	"default":          true,
	"examples":         true,
	"example":          true,
	"title":            true,
	"readOnly":         true,
	"writeOnly":        true,
	"deprecated":       true,
	"contentEncoding":  true,
	"contentMediaType": true,
}

// sanitizeGeminiToolSchema returns a Gemini-safe copy of a tool's input schema.
// It accepts whatever the client sent (typically a map[string]interface{}) and
// returns a cleaned map, or nil when there's nothing usable — callers should then
// omit the parameters field (Gemini treats a missing schema as "no parameters").
func sanitizeGeminiToolSchema(schema interface{}) map[string]interface{} {
	m, ok := toStringMap(schema)
	if !ok {
		return nil
	}
	cleaned := sanitizeGeminiSchema(m, 0)
	cm, _ := cleaned.(map[string]interface{})
	return cm
}

// maxGeminiSchemaDepth bounds recursion so a pathological/cyclic schema can't blow
// the stack. JSON Schemas this deep are not real tool definitions.
const maxGeminiSchemaDepth = 64

// sanitizeGeminiSchema recursively cleans one schema node. Returns the cleaned
// value (a map for object/array schemas, passed through for leaves).
func sanitizeGeminiSchema(v interface{}, depth int) interface{} {
	if depth > maxGeminiSchemaDepth {
		return map[string]interface{}{"type": "string"}
	}
	node, ok := toStringMap(v)
	if !ok {
		return v
	}

	out := make(map[string]interface{}, len(node))
	for k, val := range node {
		if geminiUnsupportedSchemaKeys[k] {
			continue
		}
		switch k {
		case "properties":
			// Recurse into each property's schema.
			if props, ok := toStringMap(val); ok {
				cleanedProps := make(map[string]interface{}, len(props))
				for pk, pv := range props {
					cleanedProps[pk] = sanitizeGeminiSchema(pv, depth+1)
				}
				out["properties"] = cleanedProps
			}
		case "items":
			// items may be a schema or (rarely) an array of schemas; Gemini wants a
			// single schema, so take the first when it's an array.
			if arr, ok := val.([]interface{}); ok {
				if len(arr) > 0 {
					out["items"] = sanitizeGeminiSchema(arr[0], depth+1)
				}
			} else {
				out["items"] = sanitizeGeminiSchema(val, depth+1)
			}
		case "anyOf":
			// Flatten anyOf to its first viable branch — Gemini's support is
			// inconsistent and a bare anyOf at the parameters root is rejected.
			if arr, ok := val.([]interface{}); ok && len(arr) > 0 {
				if first := sanitizeGeminiSchema(arr[0], depth+1); first != nil {
					if fm, ok := first.(map[string]interface{}); ok {
						for fk, fv := range fm {
							if _, exists := out[fk]; !exists {
								out[fk] = fv
							}
						}
					}
				}
			}
		case "const":
			// const X -> enum:[X] (Gemini has no const).
			out["enum"] = []interface{}{val}
		case "type":
			out["type"] = normalizeGeminiType(val)
		default:
			out[k] = val
		}
	}

	// Infer object type when properties are present but type was dropped/absent —
	// Gemini requires a type, and a property bag with no type is meaningless.
	if _, hasType := out["type"]; !hasType {
		if _, hasProps := out["properties"]; hasProps {
			out["type"] = "object"
		}
	}

	// Prune required[] to properties that actually survived, so we never declare a
	// required field that isn't in properties (Gemini 400s on that mismatch).
	if req, ok := out["required"].([]interface{}); ok {
		if props, ok := out["properties"].(map[string]interface{}); ok {
			kept := make([]interface{}, 0, len(req))
			for _, r := range req {
				if name, ok := r.(string); ok {
					if _, exists := props[name]; exists {
						kept = append(kept, name)
					}
				}
			}
			if len(kept) > 0 {
				out["required"] = kept
			} else {
				delete(out, "required")
			}
		} else {
			// required with no properties is invalid; drop it.
			delete(out, "required")
		}
	}

	return out
}

// normalizeGeminiType coerces a JSON-Schema "type" into the single string Gemini
// expects. A type array like ["string","null"] becomes the first non-null member.
func normalizeGeminiType(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		return t
	case []interface{}:
		for _, e := range t {
			if s, ok := e.(string); ok && s != "null" {
				return s
			}
		}
		if len(t) > 0 {
			if s, ok := t[0].(string); ok {
				return s
			}
		}
	}
	return v
}

// toStringMap normalizes a decoded-JSON value into map[string]interface{} when it
// is one (handles both the direct type and the rare map[interface{}]… shape).
func toStringMap(v interface{}) (map[string]interface{}, bool) {
	if m, ok := v.(map[string]interface{}); ok {
		return m, true
	}
	return nil, false
}

// geminiParametersOrEmpty returns a sanitized parameters object for a tool, or a
// minimal empty-object schema when the tool declares no usable parameters (Gemini
// accepts an empty object but rejects a missing/!object parameters field on a
// declared function). Centralizes the Claude/OpenAI tool-decl conversion.
func geminiParametersOrEmpty(schema interface{}) map[string]interface{} {
	if cleaned := sanitizeGeminiToolSchema(schema); cleaned != nil {
		if _, ok := cleaned["type"]; !ok {
			cleaned["type"] = "object"
		}
		return cleaned
	}
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
}
