package config

import "testing"

// TestIsModelAllowedForAPIKey pins the per-key allowlist contract:
//
//   - empty Models = allow everything (default)
//   - dotted/dashed aliases match interchangeably
//   - mismatched ids reject
//
// The first two cover the "default is all" expectation; the alias rule
// matters because the same model can come in as claude-opus-4.7 or
// claude-opus-4-7 depending on which client sent the request.
func TestIsModelAllowedForAPIKey(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		model   string
		want    bool
	}{
		{"empty allowlist allows everything", nil, "claude-opus-4-7", true},
		{"empty allowlist allows custom model", nil, "future-model-not-yet-released", true},
		{"explicit dashed match", []string{"claude-opus-4-7"}, "claude-opus-4-7", true},
		{"dashed allows dotted variant", []string{"claude-opus-4-7"}, "claude-opus-4.7", true},
		{"dotted allows dashed variant", []string{"claude-opus-4.7"}, "claude-opus-4-7", true},
		{"unrelated model rejected", []string{"claude-opus-4-7"}, "claude-haiku-4-5", false},
		{"case-insensitive match", []string{"Claude-Opus-4-7"}, "claude-opus-4-7", true},
		{"whitespace tolerated", []string{"  claude-opus-4-7  "}, "claude-opus-4-7", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := IsModelAllowedForAPIKey(APIKey{Models: c.allowed}, c.model)
			if got != c.want {
				t.Fatalf("IsModelAllowedForAPIKey(allow=%v, model=%q) = %v, want %v", c.allowed, c.model, got, c.want)
			}
		})
	}
}
