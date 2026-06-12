package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
	"strings"
)

// Embeddings passthrough.
//
// Embeddings are OpenAI-standardized (POST {base}/embeddings -> {data:[{embedding}]}),
// so unlike chat they need no per-dialect translation: we forward the client's
// OpenAI-shaped embeddings request to the resolved provider's /embeddings endpoint
// with the account's credential and return the JSON verbatim. This adds a NON-chat
// inbound endpoint (/v1/embeddings) that routes through the same model->backend
// resolution and account failover as chat. Only OpenAI-dialect providers expose an
// embeddings route; a request to a non-OpenAI backend is rejected cleanly.
//
// Image/TTS/STT are deliberately NOT handled here: each returns provider-specific
// binary formats with no common wire shape, so a passthrough cannot be correct
// across providers without per-provider translation. Embeddings are the one
// non-chat capability with a universal OpenAI-compatible contract.

// embeddingsURL derives {base}/embeddings from an OpenAI-dialect provider's settings.
func (ps providerSettings) embeddingsURL() string {
	return ps.apiBase() + "/embeddings"
}

// callEmbeddings forwards an embeddings request body to the provider and returns the
// upstream status + raw JSON response. Only valid for OpenAI-dialect providers.
func (g *genericProvider) callEmbeddings(ctx context.Context, acct *config.Account, body []byte) (int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ps, ok := resolveProviderSettings(acct)
	if !ok {
		return 0, nil, fmt.Errorf("unknown provider for account %s", acct.ID)
	}
	if ps.dialect != DialectOpenAI {
		return 0, nil, fmt.Errorf("provider %s (dialect %s) has no embeddings endpoint", ps.id, ps.dialect)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", ps.embeddingsURL(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range ps.headers {
		req.Header.Set(k, v)
	}
	g.applyAuthHeader(req, ps, acct)

	resp, err := GetRestClientForProxy(ResolveAccountProxyURL(acct)).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, raw, nil
}

// handleEmbeddings serves POST /v1/embeddings. It resolves the backend from the
// model id (same prefix scheme as chat: "deepinfra/BAAI/bge-..."), then forwards to
// that provider's /embeddings endpoint via the generic OpenAI provider. Defaults to
// any OpenAI-dialect account when the model is unprefixed.
func (h *Handler) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.Model) == "" {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON or missing model")
		return
	}

	matchedKeyID := ""
	if k := matchedAPIKey(r); k != nil {
		matchedKeyID = k.ID
	}

	backend, upstreamModel := ParseModelBackend(req.Model)
	if backend == "kiro" {
		// Kiro (CodeWhisperer) has no embeddings API. Require an explicit non-Kiro
		// provider prefix so we never misroute.
		h.sendOpenAIError(w, 400, "invalid_request_error",
			"embeddings require a provider-prefixed model (e.g. \"deepinfra/BAAI/bge-m3\"); Kiro has no embeddings endpoint")
		return
	}
	if dialectFor(backend) != DialectOpenAI {
		h.sendOpenAIError(w, 400, "invalid_request_error",
			fmt.Sprintf("provider %q does not expose an OpenAI-compatible embeddings endpoint", backend))
		return
	}

	// Rewrite the body's model to the de-prefixed upstream id so the provider sees
	// its own model name.
	rewritten := rewriteEmbeddingsModel(body, upstreamModel)

	gp := &genericProvider{dialect: DialectOpenAI}
	worker := func(account *config.Account) (bool, error) {
		status, raw, callErr := gp.callEmbeddings(r.Context(), account, rewritten)
		if callErr != nil {
			return false, callErr // pre-commit: allow failover to another account
		}
		if status == 429 {
			return false, &QuotaError{Endpoints: []string{backend}}
		}
		if status != 200 {
			// Non-retryable upstream error: commit it (return verbatim) so the client
			// sees the real provider error instead of an opaque failover failure.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			w.Write(raw)
			return true, nil
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write(raw)
		return true, nil
	}

	committed, retryAfter, ferr := h.runWithFailoverBackend(backend, upstreamModel, matchedKeyID, "", worker)
	if committed {
		return
	}
	if retryAfter > 0 {
		setRetryAfter(w, retryAfter)
		h.sendOpenAIError(w, 429, "rate_limit_exceeded", "All accounts are rate limited")
		return
	}
	if ferr == nil {
		h.sendOpenAIError(w, 503, "server_error", "No available accounts for "+backend)
		return
	}
	h.sendOpenAIError(w, 503, "server_error", safeUpstreamError("embeddings failover", ferr))
}

// rewriteEmbeddingsModel replaces the "model" field with the de-prefixed upstream id,
// preserving every other field of the original request.
func rewriteEmbeddingsModel(body []byte, upstreamModel string) []byte {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["model"] = upstreamModel
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
