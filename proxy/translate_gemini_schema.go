package proxy

import (
	"os"

	"kiro-go/logger"
)

// ============================================================================
// Gemini JSON-Schema sanitization.
//
// Gemini's functionDeclarations.parameters accepts an OpenAPI-3.0-style "Schema"
// object — a SUBSET of JSON Schema. A request carrying keywords outside that
// subset ($schema, additionalProperties, $ref, oneOf/allOf/not, if/then/else,
// etc.) is rejected with HTTP 400 and the WHOLE request fails, so tool calling to
// any Gemini provider is dead-on-arrival unless we strip those first.
//
// EARLIER this sanitizer was over-aggressive: it flattened anyOf to its first
// branch (discarding union/alternative semantics) and stripped every numeric /
// string / array constraint (minItems, minLength, pattern, minimum, …). But the
// current Gemini Schema proto DOES support those — flattening and stripping them
// silently weakened tool schemas for no benefit. We now:
//   - PRESERVE anyOf as a real union (each branch recursively sanitized), except
//     at the parameters ROOT where Gemini requires an object type.
//   - RETAIN the constraints Gemini supports (minItems/maxItems, minLength/
//     maxLength, minProperties/maxProperties, pattern, minimum/maximum, nullable,
//     enum, default).
//   - Still STRIP the genuinely-unsupported keywords (structural composition,
//     exclusiveMinimum/Maximum, multipleOf, uniqueItems) and filter `format` to
//     Gemini's allowed values, emitting a debug diagnostic when a constraint is
//     dropped so operators can see what was lost.
//
// Escape hatch: KIRO_GEMINI_STRICT_SCHEMA=1 restores the old aggressive strip
// (drop ALL constraints + flatten anyOf) for an operator whose Gemini-compatible
// endpoint is older/stricter and 400s on the richer schema.
// ============================================================================

// geminiAlwaysStripKeys are JSON-Schema keywords Gemini's function-calling schema
// validator rejects in EVERY mode — structural composition, schema plumbing, and
// the numeric constraints with no Gemini equivalent. Recursively deleted.
var geminiAlwaysStripKeys = map[string]bool{
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
	"patternProperty":       true,
	// No Gemini Schema equivalent — these would 400.
	"exclusiveMinimum": true,
	"exclusiveMaximum": true,
	"multipleOf":       true,
	"uniqueItems":      true,
	// Soft hints Gemini ignores; cheaper to drop than risk a validator quirk.
	"examples":         true,
	"example":          true,
	"readOnly":         true,
	"writeOnly":        true,
	"deprecated":       true,
	"contentEncoding":  true,
	"contentMediaType": true,
}

// geminiConstraintKeys are validation constraints the modern Gemini Schema proto
// SUPPORTS. They are retained by default and only stripped in strict mode
// (KIRO_GEMINI_STRICT_SCHEMA=1). Listed so the strict path knows what to drop and
// the diagnostic knows what it's dropping.
var geminiConstraintKeys = map[string]bool{
	"minLength":     true,
	"maxLength":     true,
	"minItems":      true,
	"maxItems":      true,
	"minProperties": true,
	"maxProperties": true,
	"pattern":       true,
	"minimum":       true,
	"maximum":       true,
	"nullable":      true,
}

// geminiAllowedFormats is the set of `format` values Gemini's function-calling
// schema accepts. Any other format value (uri, uuid, email, hostname, …) makes
// Gemini 400, so we drop those while keeping the supported ones.
var geminiAllowedFormats = map[string]bool{
	"enum":      true,
	"date-time": true,
	"int32":     true,
	"int64":     true,
	"float":     true,
	"double":    true,
}

// geminiStrictSchema reports whether the operator opted into the old aggressive
// strip via KIRO_GEMINI_STRICT_SCHEMA=1.
func geminiStrictSchema() bool {
	return os.Getenv("KIRO_GEMINI_STRICT_SCHEMA") == "1"
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
// value (a map for object/array schemas, passed through for leaves). depth==0 is
// the parameters root, where Gemini requires an object type (anyOf is flattened
// there because a bare union root is rejected).
func sanitizeGeminiSchema(v interface{}, depth int) interface{} {
	if depth > maxGeminiSchemaDepth {
		return map[string]interface{}{"type": "string"}
	}
	node, ok := toStringMap(v)
	if !ok {
		return v
	}
	strict := geminiStrictSchema()

	out := make(map[string]interface{}, len(node))
	for k, val := range node {
		if geminiAlwaysStripKeys[k] {
			continue
		}
		// Supported constraints: retained by default, dropped only in strict mode.
		if geminiConstraintKeys[k] {
			if strict {
				logger.Debugf("[Gemini] Dropping constraint %q (KIRO_GEMINI_STRICT_SCHEMA)", k)
				continue
			}
			out[k] = val
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
			arr, ok := val.([]interface{})
			if !ok || len(arr) == 0 {
				continue
			}
			if strict || depth == 0 {
				// Strict mode, or the parameters ROOT (Gemini needs an object type
				// there, not a bare union): flatten to the first viable branch and
				// merge its keys, preserving the prior conservative behavior.
				if depth == 0 && !strict {
					logger.Debugf("[Gemini] Flattening root-level anyOf to its first branch (Gemini requires an object root)")
				}
				if first := sanitizeGeminiSchema(arr[0], depth+1); first != nil {
					if fm, ok := first.(map[string]interface{}); ok {
						for fk, fv := range fm {
							if _, exists := out[fk]; !exists {
								out[fk] = fv
							}
						}
					}
				}
				continue
			}
			// PRESERVE the union: sanitize each branch and keep anyOf. Gemini's
			// Schema proto supports anyOf in nested property positions.
			branches := make([]interface{}, 0, len(arr))
			for _, b := range arr {
				branches = append(branches, sanitizeGeminiSchema(b, depth+1))
			}
			out["anyOf"] = branches
		case "const":
			// const X -> enum:[X] (Gemini has no const).
			out["enum"] = []interface{}{val}
		case "type":
			out["type"] = normalizeGeminiType(val)
		case "format":
			// Keep only the format values Gemini accepts; drop the rest (uri, uuid,
			// email, …) which would 400 the whole request.
			if s, ok := val.(string); ok && geminiAllowedFormats[s] {
				out["format"] = s
			} else if ok {
				logger.Debugf("[Gemini] Dropping unsupported format %q", s)
			}
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
