package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// TestServeHTTPModelsRouteFiltersByKey is the end-to-end regression test for
// the bug where GET /v1/models returned the full catalog even when the calling
// API key restricted Models. The root cause was that the /v1/models route did
// NOT call validateApiKey, so the matched key never landed in the request
// context and handleModels' per-key filter was silently skipped. This test
// drives the FULL routing layer (ServeHTTP) with a real Authorization header —
// the path the earlier unit test bypassed by injecting the key directly.
func TestServeHTTPModelsRouteFiltersByKey(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config init: %v", err)
	}

	// Create a key restricted to a single model.
	key, err := config.AddAPIKey("restricted", []string{"claude-opus-4-7"}, 0, 0, 0, 0)
	if err != nil {
		t.Fatalf("add api key: %v", err)
	}

	h := &Handler{startTime: 0}

	// Call /v1/models through the real router with the restricted key.
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key.Key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	models := decodeModelsResponse(t, rec)
	if !containsModelID(models, "claude-opus-4-7") {
		t.Fatalf("restricted key should still see its allowed model, got %v", modelIDs(models))
	}
	if containsModelID(models, "claude-haiku-4-5") {
		t.Fatalf("BUG: /v1/models returned a disallowed model through the router; got %v", modelIDs(models))
	}
	// gpt-* alias is not on the allowlist either — must be filtered out.
	if containsModelID(models, "gpt-4o") {
		t.Fatalf("disallowed alias gpt-4o leaked through the per-key filter; got %v", modelIDs(models))
	}
}

// TestServeHTTPModelsRouteRejectsBadKey confirms /v1/models now enforces auth
// like the other /v1 routes (it previously skipped validation entirely).
func TestServeHTTPModelsRouteRejectsBadKey(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config init: %v", err)
	}
	if _, err := config.AddAPIKey("k", nil, 0, 0, 0, 0); err != nil {
		t.Fatalf("add api key: %v", err)
	}
	// Ensure auth is required for this assertion.
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-kg-totally-wrong")
	rec := httptest.NewRecorder()
	(&Handler{startTime: 0}).ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("expected 401 for an invalid key on /v1/models, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestValidateApiKeyStashesKeyWhenAuthOptional pins the second fix: even when
// RequireApiKey is OFF, a recognized key presented by the caller is still
// matched and stashed, so its per-key restrictions (model whitelist, quotas)
// take effect. Auth being optional only lets an absent/unknown key through.
func TestValidateApiKeyStashesKeyWhenAuthOptional(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config init: %v", err)
	}
	key, err := config.AddAPIKey("optional-auth", []string{"claude-opus-4-7"}, 0, 0, 0, 0)
	if err != nil {
		t.Fatalf("add api key: %v", err)
	}
	// Turn auth requirement OFF.
	off := false
	if err := config.UpdateSettingsPartial(nil, &off, nil); err != nil {
		t.Fatalf("disable require-api-key: %v", err)
	}
	t.Cleanup(func() { on := true; _ = config.UpdateSettingsPartial(nil, &on, nil) })

	h := &Handler{startTime: 0}
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key.Key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	models := decodeModelsResponse(t, rec)
	if containsModelID(models, "claude-haiku-4-5") {
		t.Fatalf("a presented restricted key must filter even when auth is optional; got %v", modelIDs(models))
	}
	if !containsModelID(models, "claude-opus-4-7") {
		t.Fatalf("allowed model missing; got %v", modelIDs(models))
	}
}

// guard against an unused import if decode helpers move.
var _ = json.Marshal
