package proxy

import (
	"testing"
)

// TestParseModelsListResponse covers the provider response shapes the live
// model fetch must handle: OpenAI {"data":[...]}, a {"models":[...]} shape,
// {"results":[...]}, and a bare array — plus id-field fallbacks and dedup.
func TestParseModelsListResponse(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "openai data shape",
			body: `{"object":"list","data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"}]}`,
			want: []string{"gpt-4o", "gpt-4o-mini"},
		},
		{
			name: "models shape with name fallback",
			body: `{"models":[{"name":"llama-3.3-70b"},{"id":"mixtral-8x7b","name":"Mixtral"}]}`,
			want: []string{"llama-3.3-70b", "mixtral-8x7b"},
		},
		{
			name: "results shape",
			body: `{"results":[{"id":"a"},{"slug":"b"}]}`,
			want: []string{"a", "b"},
		},
		{
			name: "bare array",
			body: `[{"id":"m1"},{"id":"m2"}]`,
			want: []string{"m1", "m2"},
		},
		{
			name: "dedup + skip empty",
			body: `{"data":[{"id":"x"},{"id":"x"},{"id":""},{"foo":"bar"}]}`,
			want: []string{"x"},
		},
		{
			name: "empty",
			body: `{"data":[]}`,
			want: []string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseModelsListResponse([]byte(c.body))
			ids := make([]string, 0, len(got))
			for _, m := range got {
				ids = append(ids, m.ModelId)
			}
			if len(ids) != len(c.want) {
				t.Fatalf("got %v, want %v", ids, c.want)
			}
			for i := range ids {
				if ids[i] != c.want[i] {
					t.Fatalf("got %v, want %v", ids, c.want)
				}
			}
		})
	}
}

// TestProviderSettingsURLDerivation verifies the chat/models URL derivation from
// a base — both when the configured baseURL is a full inference URL (builtin
// catalog) and when it's a bare API base (custom provider).
func TestProviderSettingsURLDerivation(t *testing.T) {
	cases := []struct {
		name      string
		ps        providerSettings
		wantChat  string
		wantModel string
	}{
		{
			name:      "openai builtin full chat url",
			ps:        providerSettings{dialect: DialectOpenAI, baseURL: "https://api.groq.com/openai/v1/chat/completions"},
			wantChat:  "https://api.groq.com/openai/v1/chat/completions",
			wantModel: "https://api.groq.com/openai/v1/models",
		},
		{
			name:      "openai custom bare base",
			ps:        providerSettings{dialect: DialectOpenAI, baseURL: "https://api.example.com/v1"},
			wantChat:  "https://api.example.com/v1/chat/completions",
			wantModel: "https://api.example.com/v1/models",
		},
		{
			name:      "openai custom bare base trailing slash",
			ps:        providerSettings{dialect: DialectOpenAI, baseURL: "https://api.example.com/v1/"},
			wantChat:  "https://api.example.com/v1/chat/completions",
			wantModel: "https://api.example.com/v1/models",
		},
		{
			name:      "anthropic full messages url",
			ps:        providerSettings{dialect: DialectAnthropic, baseURL: "https://api.anthropic.com/v1/messages"},
			wantChat:  "https://api.anthropic.com/v1/messages",
			wantModel: "https://api.anthropic.com/v1/models",
		},
		{
			name:      "anthropic bare base",
			ps:        providerSettings{dialect: DialectAnthropic, baseURL: "https://api.z.ai/api/anthropic/v1"},
			wantChat:  "https://api.z.ai/api/anthropic/v1/messages",
			wantModel: "https://api.z.ai/api/anthropic/v1/models",
		},
		{
			name:      "gemini base is models root",
			ps:        providerSettings{dialect: DialectGemini, baseURL: "https://generativelanguage.googleapis.com/v1beta/models"},
			wantChat:  "https://generativelanguage.googleapis.com/v1beta/models/chat/completions", // unused for gemini; build path uses apiBase()+model
			wantModel: "https://generativelanguage.googleapis.com/v1beta/models",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.ps.dialect != DialectGemini {
				if got := c.ps.chatURL(); got != c.wantChat {
					t.Errorf("chatURL() = %q, want %q", got, c.wantChat)
				}
			}
			if got := c.ps.modelsURL(); got != c.wantModel {
				t.Errorf("modelsURL() = %q, want %q", got, c.wantModel)
			}
		})
	}
}

// TestSlugifyProviderID pins the custom-provider id derivation.
func TestSlugifyProviderID(t *testing.T) {
	cases := map[string]string{
		"My LLM Gateway":   "my-llm-gateway",
		"  Trim  Me  ":     "trim-me",
		"weird!!!chars@@@": "weird-chars",
		"already-slug":     "already-slug",
		"UPPER":            "upper",
		"":                 "",
		"###":              "",
		"a/b/c":            "a-b-c",
	}
	for in, want := range cases {
		if got := slugifyProviderID(in); got != want {
			t.Errorf("slugifyProviderID(%q) = %q, want %q", in, got, want)
		}
	}
}
