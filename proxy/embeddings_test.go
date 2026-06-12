package proxy

import (
	"encoding/json"
	"testing"
)

// TestRewriteEmbeddingsModel confirms the model field is de-prefixed while every
// other request field is preserved verbatim.
func TestRewriteEmbeddingsModel(t *testing.T) {
	body := []byte(`{"model":"deepinfra/BAAI/bge-m3","input":["hello","world"],"encoding_format":"float"}`)
	out := rewriteEmbeddingsModel(body, "BAAI/bge-m3")

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("rewritten body not valid JSON: %v", err)
	}
	if m["model"] != "BAAI/bge-m3" {
		t.Errorf("model = %v, want BAAI/bge-m3", m["model"])
	}
	if m["encoding_format"] != "float" {
		t.Errorf("encoding_format lost: %v", m["encoding_format"])
	}
	in, ok := m["input"].([]interface{})
	if !ok || len(in) != 2 {
		t.Errorf("input array lost: %v", m["input"])
	}
}

// TestRewriteEmbeddingsModelBadJSON returns the input unchanged on parse failure.
func TestRewriteEmbeddingsModelBadJSON(t *testing.T) {
	body := []byte(`not json`)
	if out := rewriteEmbeddingsModel(body, "x"); string(out) != "not json" {
		t.Errorf("expected passthrough on bad JSON, got %q", out)
	}
}

// TestEmbeddingsURL confirms the /embeddings endpoint derives correctly for both a
// full embeddings URL (voyage/jina catalog rows) and a bare chat base.
func TestEmbeddingsURL(t *testing.T) {
	cases := []struct{ base, want string }{
		{"https://api.voyageai.com/v1/embeddings", "https://api.voyageai.com/v1/embeddings"},
		{"https://api.deepinfra.com/v1/openai/chat/completions", "https://api.deepinfra.com/v1/openai/embeddings"},
		{"https://api.openai.com/v1/chat/completions", "https://api.openai.com/v1/embeddings"},
	}
	for _, c := range cases {
		ps := providerSettings{dialect: DialectOpenAI, baseURL: c.base}
		if got := ps.embeddingsURL(); got != c.want {
			t.Errorf("embeddingsURL(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}
