package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

var errEmptyCodexToken = errors.New("codex token is empty")

func nowUnixSeconds() int64 { return time.Now().Unix() }

// isValidBaseURLScheme reports whether a base URL uses an allowed scheme. Both
// https and http are accepted: not every endpoint an operator targets serves
// TLS (self-hosted gateways, LAN/on-prem boxes, localhost, a sidecar reached
// over plain HTTP). We still reject schemeless input and non-http schemes
// (file://, ftp://, etc.) so the SSRF guard keeps its teeth — the requirement is
// "http or https", not "anything".
func isValidBaseURLScheme(base string) bool {
	l := strings.ToLower(strings.TrimSpace(base))
	return strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://")
}

const baseURLSchemeError = "baseURL must use http:// or https://"

// providerCatalogEntry is the dashboard-facing description of an addable
// provider backend.
type providerCatalogEntry struct {
	ID       string `json:"id"`
	Alias    string `json:"alias,omitempty"`
	Name     string `json:"name"`
	Dialect  string `json:"dialect"`
	AuthType string `json:"authType"` // "apikey" | "oauth"
	BaseURL  string `json:"baseURL,omitempty"`
	Custom   bool   `json:"custom,omitempty"` // user-defined (vs built-in)
}

// apiGetProviderCatalog GET /admin/api/providers/catalog — returns the list of
// provider backends a user can add an account for, so the dashboard can render a
// picker. Built-in api-key providers + the OAuth backends (codex, qoder) +
// user-defined custom providers.
func (h *Handler) apiGetProviderCatalog(w http.ResponseWriter, r *http.Request) {
	var out []providerCatalogEntry

	// Built-in data-driven providers. Most are api-key; an OAuth-flagged one (qwen)
	// is surfaced as authType "oauth" so the dashboard routes it to its device-login
	// connect flow instead of a paste-a-key form.
	for _, bp := range builtinProviders {
		authType := "apikey"
		if bp.OAuth {
			authType = "oauth"
		}
		out = append(out, providerCatalogEntry{
			ID:       bp.ID,
			Alias:    bp.Alias,
			Name:     bp.Name,
			Dialect:  string(bp.Dialect),
			AuthType: authType,
			BaseURL:  bp.BaseURL,
		})
	}
	// OAuth backends (handled by their own connect flows; listed so the UI can
	// surface them). These are added in later phases.
	out = append(out,
		providerCatalogEntry{ID: "codex", Alias: "cx", Name: "OpenAI Codex", Dialect: string(DialectCodex), AuthType: "oauth"},
		providerCatalogEntry{ID: "qoder", Alias: "qd", Name: "Qoder", Dialect: "openai", AuthType: "oauth"},
	)
	// User-defined custom providers.
	for _, pc := range config.GetProviders() {
		out = append(out, providerCatalogEntry{
			ID:       pc.ID,
			Alias:    pc.Alias,
			Name:     firstNonEmpty(pc.Name, pc.ID),
			Dialect:  pc.Dialect,
			AuthType: "apikey",
			BaseURL:  pc.BaseURL,
			Custom:   true,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{"providers": out})
}

// apiAddProviderAccount POST /admin/api/providers/account — add an api-key
// provider account. Body: {backend, apiKey, name?, baseURL?, weight?}. The Kiro
// add flow (/accounts) is untouched; this is the separate path for non-Kiro
// backends, mirroring 9router's generic "connect with API key" form.
func (h *Handler) apiAddProviderAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Backend string `json:"backend"`
		APIKey  string `json:"apiKey"`
		Name    string `json:"name"`
		BaseURL string `json:"baseURL"`
		Weight  int    `json:"weight"`
		// TokenLimit is an optional per-account lifetime token cap. 0 = unlimited.
		TokenLimit int `json:"tokenLimit"`
		// Custom (bring-your-own) provider fields. When backend == "custom" the
		// caller supplies the dialect + base URL directly and we register a
		// ProviderConfig on the fly — no separate provider-registration step. This
		// is the "add any OpenAI-compatible endpoint" path: it does NOT reuse the
		// Kiro account schema.
		Dialect string   `json:"dialect"` // openai | anthropic | gemini (custom only)
		Alias   string   `json:"alias"`   // optional routing prefix (custom only)
		Models  []string `json:"models"`  // optional static model list (custom only)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	backend := strings.ToLower(strings.TrimSpace(req.Backend))
	if backend == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "backend is required"})
		return
	}
	if backend == "kiro" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "use /admin/api/accounts to add Kiro accounts"})
		return
	}
	if backend == "codex" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "use /admin/api/auth/codex/start (OAuth) or /auth/codex/token to add Codex accounts"})
		return
	}
	if backend == "qoder" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "use /admin/api/auth/qoder/start (device login) to add Qoder accounts"})
		return
	}
	if backend == "qwen" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Qwen uses OAuth; add via /admin/api/auth/qwen/start (device login)"})
		return
	}
	if strings.TrimSpace(req.APIKey) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "apiKey is required"})
		return
	}

	// "custom" backend: a bring-your-own OpenAI/Anthropic/Gemini endpoint. Per
	// the operator's choice we do NOT register a reusable Config.Providers[]
	// entry — everything (dialect, base URL, optional pinned models) is stored
	// INLINE on the account, and the account's Backend id is its own routing
	// prefix. Base URL is an API BASE (e.g. https://api.example.com/v1), not a
	// full /chat/completions URL; the generic provider derives the endpoints.
	var customDialect string
	var customModels []string
	if backend == "custom" {
		dialect := strings.ToLower(strings.TrimSpace(req.Dialect))
		if dialect == "" {
			dialect = "openai"
		}
		switch dialect {
		case "openai", "anthropic", "gemini":
		default:
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "dialect must be one of: openai, anthropic, gemini"})
			return
		}
		base := strings.TrimSpace(req.BaseURL)
		if base == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "baseURL is required for a custom provider (e.g. https://api.example.com/v1)"})
			return
		}
		if !isValidBaseURLScheme(base) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": baseURLSchemeError})
			return
		}
		// Routing id/prefix: prefer the operator-supplied alias, else a slug of
		// the name, else a generated id. Must not collide with a built-in id, a
		// registered ProviderConfig, or an existing custom account's backend.
		id := slugifyProviderID(firstNonEmpty(strings.TrimSpace(req.Alias), strings.TrimSpace(req.Name)))
		if id == "" {
			id = "custom-" + auth.GenerateAccountID()[:8]
		}
		id = ensureUniqueBackendID(id)
		backend = id
		customDialect = dialect
		customModels = sanitizeModelList(req.Models)
		// Stash base + models inline on the account below.
		req.BaseURL = base
	} else {
		// Named (built-in or previously-registered custom) provider.
		_, builtinOK := resolveBuiltinProvider(backend)
		_, customOK := config.GetProviderConfig(backend)
		_, inlineOK := config.GetCustomAccountByBackend(backend)
		if !builtinOK && !customOK && !inlineOK {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "unknown provider backend: " + backend})
			return
		}
		// SSRF guard on the optional per-account base URL override.
		if bu := strings.TrimSpace(req.BaseURL); bu != "" && !isValidBaseURLScheme(bu) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": baseURLSchemeError})
			return
		}
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		if bp, ok := resolveBuiltinProvider(backend); ok {
			name = bp.Name
		} else if pc, ok := config.GetProviderConfig(backend); ok {
			name = firstNonEmpty(pc.Name, backend)
		} else {
			name = backend
		}
	}

	// For a custom (inline) account the base URL goes in BaseURLOverride and is
	// paired with CustomDialect; for a named provider it's an optional override.
	baseOverride := strings.TrimSpace(req.BaseURL)

	acct := config.Account{
		ID:              auth.GenerateAccountID(),
		Backend:         backend,
		APIKey:          strings.TrimSpace(req.APIKey),
		Nickname:        name,
		BaseURLOverride: baseOverride,
		CustomDialect:   customDialect,
		CustomModels:    customModels,
		Weight:          req.Weight,
		TokenLimit:      req.TokenLimit,
		Enabled:         true,
	}
	if err := config.AddAccount(acct); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	logger.Infof("[Providers] Added %s account %s (%s)", backend, acct.ID, name)

	// Fetch the live model catalog so the response can report what the endpoint
	// offers (and seed the pool's per-account model filter). Best-effort: a
	// failure here doesn't fail the add — the account is created either way and a
	// later models-refresh / first request will populate it.
	//
	// Fallback order: live /models fetch → the account's pinned CustomModels (a
	// custom account can pin a list when its endpoint has no /models). An empty
	// final list is harmless — the pool treats "no cached models" as "serves any
	// model", so routing still works and the upstream validates the id at call
	// time. We surface modelSource so the dashboard can explain a 0 count.
	models := []string{}
	modelSource := "none"
	advisory := false
	if prov, ok := ProviderForBackend(backend).(*genericProvider); ok {
		if ids, adv, ferr := prov.FetchModelsForAccount(r.Context(), &acct); ferr == nil && len(ids) > 0 {
			models = ids
			advisory = adv
			if adv {
				modelSource = "static" // no /models endpoint; hardcoded catalog
				logger.Infof("[Providers] %s account %s: %d models from static catalog (advisory)", backend, acct.ID, len(ids))
			} else {
				modelSource = "fetched"
				logger.Infof("[Providers] %s account %s: fetched %d models", backend, acct.ID, len(ids))
			}
		} else if ferr != nil {
			logger.Warnf("[Providers] %s account %s: model fetch failed (account still added): %v", backend, acct.ID, ferr)
		}
	}
	// Fall back to the account's pinned models when the live fetch found nothing.
	if len(models) == 0 && len(customModels) > 0 {
		models = append(models, customModels...)
		modelSource = "static"
		advisory = true // pinned custom list is best-effort, not a live catalog
	}
	if len(models) > 0 {
		// Advisory (static) catalogs are DISPLAY-ONLY so a model missing from the
		// hardcoded guess is never shed; a live fetch seeds the strict routing gate.
		if advisory {
			h.pool.SetAdvisoryModelList(acct.ID, models)
		} else {
			h.pool.SetModelList(acct.ID, models)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"id":          acct.ID,
		"backend":     backend,
		"modelCount":  len(models),
		"models":      models,
		"modelSource": modelSource,
	})
}

// maxBulkProviderKeys caps how many api keys one bulk-add request may carry.
// Unlike the Kiro refresh-token import (which calls AWS auth per row), this path
// does NO per-key network call — keys are stored verbatim and the model catalog
// is fetched ONCE for the whole batch — so the only cost is config size. 1000 is
// far above any realistic paste while still bounding a malformed/oversized body.
const maxBulkProviderKeys = 1000

// apiAddProviderAccountsBulk POST /admin/api/providers/account/bulk — add MANY
// api-key accounts for one provider in a single request. Body:
// {backend, apiKeys:[...], name?, baseURL?, weight?, dialect?, alias?, models?}.
//
// Every key in apiKeys becomes its own config.Account on the SAME backend, so the
// pool fans requests out across all of them (see the "fast" strategy). This is the
// bulk twin of apiAddProviderAccount: same validation and same backend resolution
// (including the "custom" bring-your-own endpoint), but it registers a custom
// provider ONCE (so all keys share one routing id instead of N inline ids),
// dedupes against existing keys + within the paste, persists all rows in a single
// save, reloads the pool once, and fetches the model catalog once for the batch.
func (h *Handler) apiAddProviderAccountsBulk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Backend string   `json:"backend"`
		APIKeys []string `json:"apiKeys"`
		Name    string   `json:"name"`
		BaseURL string   `json:"baseURL"`
		Weight  int      `json:"weight"`
		// TokenLimit is an optional per-account lifetime token cap applied to EVERY
		// key in the batch (e.g. each DashScope key is good for 1,000,000 tokens).
		// 0 = unlimited. The pool drops a key once its TotalTokens reaches this, so
		// a 100-key batch naturally stacks onto keys that still have budget.
		TokenLimit int `json:"tokenLimit"`
		// Custom (bring-your-own) provider fields, identical to the single-add path.
		Dialect string   `json:"dialect"`
		Alias   string   `json:"alias"`
		Models  []string `json:"models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	backend := strings.ToLower(strings.TrimSpace(req.Backend))
	if backend == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "backend is required"})
		return
	}
	switch backend {
	case "kiro":
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "use /admin/api/import to bulk-add Kiro accounts (refresh tokens)"})
		return
	case "codex":
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Codex uses OAuth; add via /admin/api/auth/codex/start"})
		return
	case "qoder":
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Qoder needs a device token + user id; bulk-add via /admin/api/import (9router shape)"})
		return
	case "qwen":
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Qwen uses OAuth; add via /admin/api/auth/qwen/start (device login)"})
		return
	}

	// Normalise the pasted keys: split nothing (caller already split), just trim,
	// drop blanks, and de-dupe within the paste so repeating a key doesn't create
	// two accounts.
	seen := map[string]bool{}
	keys := make([]string, 0, len(req.APIKeys))
	for _, k := range req.APIKeys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "no api keys provided"})
		return
	}
	if len(keys) > maxBulkProviderKeys {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("too many keys in one bulk add: %d (max %d)", len(keys), maxBulkProviderKeys),
		})
		return
	}

	// Resolve the backend, mirroring apiAddProviderAccount. For "custom" we register
	// ONE ProviderConfig and point every account at it (so all keys share a single
	// routing prefix), rather than minting N self-contained inline backends.
	var baseOverride string
	if backend == "custom" {
		dialect := strings.ToLower(strings.TrimSpace(req.Dialect))
		if dialect == "" {
			dialect = "openai"
		}
		switch dialect {
		case "openai", "anthropic", "gemini":
		default:
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "dialect must be one of: openai, anthropic, gemini"})
			return
		}
		base := strings.TrimSpace(req.BaseURL)
		if base == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "baseURL is required for a custom provider (e.g. https://api.example.com/v1)"})
			return
		}
		if !isValidBaseURLScheme(base) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": baseURLSchemeError})
			return
		}
		id := slugifyProviderID(firstNonEmpty(strings.TrimSpace(req.Alias), strings.TrimSpace(req.Name)))
		if id == "" {
			id = "custom-" + auth.GenerateAccountID()[:8]
		}
		id = ensureUniqueBackendID(id)
		pc := config.ProviderConfig{
			ID:          id,
			Alias:       slugifyProviderID(firstNonEmpty(strings.TrimSpace(req.Alias), id)),
			Name:        firstNonEmpty(strings.TrimSpace(req.Name), id),
			Dialect:     dialect,
			BaseURL:     base,
			Models:      sanitizeModelList(req.Models),
			FetchModels: true,
		}
		if err := config.AddProvider(pc); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		backend = id
		logger.Infof("[Providers] Bulk-add registered custom provider %s (%s -> %s)", id, dialect, base)
	} else {
		_, builtinOK := resolveBuiltinProvider(backend)
		_, customOK := config.GetProviderConfig(backend)
		_, inlineOK := config.GetCustomAccountByBackend(backend)
		if !builtinOK && !customOK && !inlineOK {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "unknown provider backend: " + backend})
			return
		}
		if bu := strings.TrimSpace(req.BaseURL); bu != "" {
			if !isValidBaseURLScheme(bu) {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": baseURLSchemeError})
				return
			}
			baseOverride = bu
		}
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		if bp, ok := resolveBuiltinProvider(backend); ok {
			name = bp.Name
		} else if pc, ok := config.GetProviderConfig(backend); ok {
			name = firstNonEmpty(pc.Name, backend)
		} else {
			name = backend
		}
	}

	// Dedupe against api keys already stored on this backend so a re-paste is
	// idempotent (matches the 9router import's per-backend|ak| dedupe intent).
	existingKeys := map[string]bool{}
	for _, a := range config.GetAccounts() {
		if config.GetAccountBackend(&a) == backend && strings.TrimSpace(a.APIKey) != "" {
			existingKeys[strings.TrimSpace(a.APIKey)] = true
		}
	}

	batch := make([]config.Account, 0, len(keys))
	ids := make([]string, 0, len(keys))
	skipped := 0
	for i, key := range keys {
		if existingKeys[key] {
			skipped++
			continue
		}
		nm := name
		if len(keys) > 1 {
			nm = fmt.Sprintf("%s #%d", name, i+1)
		}
		acct := config.Account{
			ID:              auth.GenerateAccountID(),
			Backend:         backend,
			APIKey:          key,
			Nickname:        nm,
			BaseURLOverride: baseOverride,
			Weight:          req.Weight,
			TokenLimit:      req.TokenLimit,
			Enabled:         true,
		}
		batch = append(batch, acct)
		ids = append(ids, acct.ID)
	}

	if len(batch) == 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "backend": backend, "added": 0, "skipped": skipped,
			"modelCount": 0, "message": "all keys already present",
		})
		return
	}

	if _, err := config.AddAccounts(batch); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	logger.Infof("[Providers] Bulk-added %d %s accounts (%d skipped as duplicates)", len(batch), backend, skipped)

	// Fetch the model catalog ONCE for the whole batch (every account shares the
	// same backend + endpoint, so the catalog is identical) and seed it onto all
	// new account ids. Best-effort: a failure here doesn't fail the add.
	models := []string{}
	modelSource := "none"
	advisory := false
	probe := batch[0]
	if prov, ok := ProviderForBackend(backend).(*genericProvider); ok {
		if fetched, adv, ferr := prov.FetchModelsForAccount(r.Context(), &probe); ferr == nil && len(fetched) > 0 {
			models = fetched
			advisory = adv
			if adv {
				modelSource = "static"
			} else {
				modelSource = "fetched"
			}
		} else if ferr != nil {
			logger.Warnf("[Providers] bulk %s: model fetch failed (accounts still added): %v", backend, ferr)
		}
	}
	if len(models) == 0 {
		if clean := sanitizeModelList(req.Models); len(clean) > 0 {
			models = clean
			modelSource = "static"
			advisory = true
		}
	}
	if len(models) > 0 {
		for _, id := range ids {
			// Advisory (static / pinned) catalogs are display-only; a live fetch
			// seeds the strict per-account routing filter. See SetAdvisoryModelList.
			if advisory {
				h.pool.SetAdvisoryModelList(id, models)
			} else {
				h.pool.SetModelList(id, models)
			}
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"backend":     backend,
		"added":       len(batch),
		"skipped":     skipped,
		"modelCount":  len(models),
		"modelSource": modelSource,
	})
}

// ensureUniqueBackendID returns a routing id derived from base that does not
// collide with a reserved bespoke backend (kiro/codex/qoder), a built-in
// provider, a registered ProviderConfig, or an existing self-contained custom
// account's backend. Loops until a free id is found (a 6-char random suffix
// makes a second collision astronomically unlikely, but we never return a taken
// id — a duplicate backend would strand the second account, unreachable).
func ensureUniqueBackendID(base string) string {
	taken := func(candidate string) bool {
		switch candidate {
		case "kiro", "codex", "qoder":
			return true // reserved bespoke backends
		}
		if _, ok := resolveBuiltinProvider(candidate); ok {
			return true
		}
		if _, ok := config.GetProviderConfig(candidate); ok {
			return true
		}
		if _, ok := config.GetCustomAccountByBackend(candidate); ok {
			return true
		}
		return false
	}
	id := base
	for taken(id) {
		id = base + "-" + auth.GenerateAccountID()[:6]
	}
	return id
}

// sanitizeModelList trims, de-dupes, and drops empty entries from a
// user-supplied model id list (custom-provider "Models" field).
func sanitizeModelList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" || seen[strings.ToLower(m)] {
			continue
		}
		seen[strings.ToLower(m)] = true
		out = append(out, m)
	}
	return out
}

// slugifyProviderID turns a display name into a lowercase, dash-separated id
// safe to use as a provider backend key and routing prefix. Non-alphanumeric
// runs collapse to a single dash; leading/trailing dashes are trimmed.
func slugifyProviderID(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// apiListCustomProviders GET /admin/api/providers — returns the user-defined
// custom provider configs (not the built-in catalog).
func (h *Handler) apiListCustomProviders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{"providers": config.GetProviders()})
}

// apiAddCustomProvider POST /admin/api/providers — register a custom
// OpenAI-/Anthropic-/Gemini-compatible provider (base URL + dialect) so accounts
// can target it via Backend == its id.
func (h *Handler) apiAddCustomProvider(w http.ResponseWriter, r *http.Request) {
	var pc config.ProviderConfig
	if err := json.NewDecoder(r.Body).Decode(&pc); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	pc.ID = strings.ToLower(strings.TrimSpace(pc.ID))
	pc.Dialect = strings.ToLower(strings.TrimSpace(pc.Dialect))
	if pc.ID == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "id is required"})
		return
	}
	switch pc.Dialect {
	case "openai", "anthropic", "gemini":
		// ok
	default:
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "dialect must be one of: openai, anthropic, gemini"})
		return
	}
	if strings.TrimSpace(pc.BaseURL) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "baseURL is required"})
		return
	}
	// SSRF guard: restrict custom-provider base URLs to http(s) so they can't be
	// pointed at file://, ftp://, or other non-web schemes (see security review).
	// Both http and https are allowed — self-hosted / LAN / localhost gateways
	// don't always serve TLS.
	if !isValidBaseURLScheme(pc.BaseURL) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": baseURLSchemeError})
		return
	}
	// Don't let a custom provider shadow a built-in backend id.
	if _, ok := resolveBuiltinProvider(pc.ID); ok {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "id collides with a built-in provider: " + pc.ID})
		return
	}
	if err := config.AddProvider(pc); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	logger.Infof("[Providers] Registered custom provider %s (%s -> %s)", pc.ID, pc.Dialect, pc.BaseURL)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": pc.ID})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// seedProviderModelList populates the pool's per-account model filter (and the
// global models cache) right after a non-Kiro account is created, so the
// dashboard shows the real model count immediately instead of "0 models".
// Best-effort: a provider whose ListModels needs the network (generic) is
// handled by the add path's own live fetch; this covers the static-catalog
// providers (codex, qoder) whose ListModels is local and never fails.
func (h *Handler) seedProviderModelList(acct *config.Account) {
	prov := ProviderFor(acct)
	if prov == nil {
		return
	}
	models, err := prov.ListModels(acct)
	if err != nil || len(models) == 0 {
		return
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ModelId)
	}
	h.pool.SetModelList(acct.ID, ids)
	h.modelsCacheMu.Lock()
	h.cachedModels = mergeUniqueModels(h.cachedModels, models)
	h.modelsCacheTime = nowUnixSeconds()
	h.modelsCacheMu.Unlock()
}

// ---- Codex (ChatGPT) OAuth admin endpoints --------------------------------

// apiStartCodexLogin POST /admin/api/auth/codex/start — begins a Codex browser
// OAuth flow and returns {sessionId, authUrl}. The dashboard opens authUrl and
// polls /auth/codex/poll until completion.
func (h *Handler) apiStartCodexLogin(w http.ResponseWriter, r *http.Request) {
	sess, err := auth.StartCodexLogin()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId": sess.ID,
		"authUrl":   sess.AuthURL,
	})
}

// apiPollCodexLogin POST /admin/api/auth/codex/poll {sessionId} — returns the
// status; on "completed" it creates the account and returns its id.
func (h *Handler) apiPollCodexLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	status, tokens, errMsg, found := auth.PollCodexLogin(req.SessionID)
	if !found {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found or expired"})
		return
	}
	switch status {
	case "pending":
		json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
	case "error":
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": errMsg})
	case "completed":
		id, err := h.createCodexAccount(tokens, "")
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "completed", "id": id})
	default:
		json.NewEncoder(w).Encode(map[string]string{"status": status})
	}
}

// apiImportCodexToken POST /admin/api/auth/codex/token {accessToken, name?} —
// adds a Codex account from a pasted access/id token (no browser flow).
func (h *Handler) apiImportCodexToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccessToken string `json:"accessToken"`
		Name        string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	tokens, err := auth.ImportCodexToken(req.AccessToken)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	id, err := h.createCodexAccount(tokens, req.Name)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": id})
}

// createCodexAccount persists a new Codex account from completed tokens and
// reloads the pool. ExpiresAt is derived from ExpiresIn so the exp-based
// ensureValidToken gate refreshes it on schedule.
func (h *Handler) createCodexAccount(t *auth.CodexTokens, name string) (string, error) {
	if t == nil || strings.TrimSpace(t.AccessToken) == "" {
		return "", errEmptyCodexToken
	}
	nm := strings.TrimSpace(name)
	if nm == "" {
		nm = firstNonEmpty(t.Email, "Codex")
	}
	acct := config.Account{
		ID:             auth.GenerateAccountID(),
		Backend:        "codex",
		Email:          t.Email,
		Nickname:       nm,
		AccessToken:    t.AccessToken,
		RefreshToken:   t.RefreshToken,
		IDToken:        t.IDToken,
		CodexAccountID: t.AccountID,
		CodexPlanType:  t.PlanType,
		Enabled:        true,
	}
	if t.ExpiresIn > 0 {
		acct.ExpiresAt = nowUnixSeconds() + int64(t.ExpiresIn)
	}
	if err := config.AddAccount(acct); err != nil {
		return "", err
	}
	h.pool.Reload()
	h.seedProviderModelList(&acct)
	logger.Infof("[Codex] Added account %s (%s, plan=%s)", acct.ID, nm, t.PlanType)
	// Kick off an initial usage poll in the background so routing has the quota
	// windows promptly.
	safeGoArg("codex-usage-initial", acct, func(a config.Account) {
		h.refreshCodexAccountUsage(context.Background(), &a)
	})
	return acct.ID, nil
}

// ---- Qoder device-flow admin endpoints ------------------------------------

// qoderSessions holds in-flight Qoder device logins. Qoder's flow is poll-based
// (no local callback server), so we keep the session's verifier/nonce/machineId
// here until the dashboard polls to completion.
var (
	qoderSessions   = map[string]*auth.QoderSession{}
	qoderSessionsMu sync.Mutex
)

// apiStartQoderLogin POST /admin/api/auth/qoder/start — begins a Qoder device
// login and returns {sessionId, authUrl}. The dashboard opens authUrl and polls.
func (h *Handler) apiStartQoderLogin(w http.ResponseWriter, r *http.Request) {
	sess := auth.StartQoderLogin()
	qoderSessionsMu.Lock()
	qoderSessions[sess.ID] = sess
	// Opportunistic cleanup of expired sessions.
	for id, s := range qoderSessions {
		if time.Now().After(s.ExpiresAt) {
			delete(qoderSessions, id)
		}
	}
	qoderSessionsMu.Unlock()
	json.NewEncoder(w).Encode(map[string]interface{}{"sessionId": sess.ID, "authUrl": sess.AuthURL})
}

// apiPollQoderLogin POST /admin/api/auth/qoder/poll {sessionId} — one poll; on
// success it creates the account and returns its id.
func (h *Handler) apiPollQoderLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	qoderSessionsMu.Lock()
	sess := qoderSessions[req.SessionID]
	qoderSessionsMu.Unlock()
	if sess == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found or expired"})
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "login expired"})
		return
	}

	status, token, err := auth.PollQoderDeviceToken(sess.Nonce, sess.CodeVerifier)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": err.Error()})
		return
	}
	if status == "pending" {
		json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
		return
	}
	// status == "ok"
	name, email, _ := auth.FetchQoderUserInfo(token.AccessToken)
	if name == "" {
		name = firstNonEmpty(email, "Qoder")
	}
	acct := config.Account{
		ID:             auth.GenerateAccountID(),
		Backend:        "qoder",
		Email:          email,
		Nickname:       name,
		AccessToken:    token.AccessToken,
		RefreshToken:   token.RefreshToken,
		ExpiresAt:      token.ExpiresAt,
		QoderUserID:    token.UserID,
		QoderMachineID: sess.MachineID,
		Enabled:        true,
	}
	if err := config.AddAccount(acct); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	h.seedProviderModelList(&acct)
	qoderSessionsMu.Lock()
	delete(qoderSessions, req.SessionID)
	qoderSessionsMu.Unlock()
	logger.Infof("[Qoder] Added account %s (%s)", acct.ID, name)
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "completed", "id": acct.ID})
}

// ---- Qwen (Alibaba) OAuth device-flow admin endpoints ---------------------

// qwenSession is an in-flight Qwen device login. Like Qoder, the flow is
// poll-based (no local callback server): we keep the device code + PKCE verifier
// here until the dashboard polls to completion.
type qwenSession struct {
	DeviceCode   string
	CodeVerifier string
	UserCode     string
	VerifyURL    string
	ExpiresAt    time.Time
}

var (
	qwenSessions   = map[string]*qwenSession{}
	qwenSessionsMu sync.Mutex
)

// apiStartQwenLogin POST /admin/api/auth/qwen/start — begins a Qwen device login
// and returns {sessionId, userCode, verificationUri, verificationUriComplete}.
// The dashboard shows the code + URL and polls until completion.
func (h *Handler) apiStartQwenLogin(w http.ResponseWriter, r *http.Request) {
	da, err := auth.StartQwenDeviceAuth(r.Context())
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	id := auth.GenerateAccountID()
	expiresIn := da.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 300 // device codes are typically valid ~5 min
	}
	sess := &qwenSession{
		DeviceCode:   da.DeviceCode,
		CodeVerifier: da.CodeVerifier,
		UserCode:     da.UserCode,
		VerifyURL:    firstNonEmpty(da.VerificationURIComplete, da.VerificationURI),
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second),
	}
	qwenSessionsMu.Lock()
	qwenSessions[id] = sess
	// Opportunistic cleanup of expired sessions.
	for sid, s := range qwenSessions {
		if time.Now().After(s.ExpiresAt) {
			delete(qwenSessions, sid)
		}
	}
	qwenSessionsMu.Unlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":               id,
		"userCode":                da.UserCode,
		"verificationUri":         da.VerificationURI,
		"verificationUriComplete": da.VerificationURIComplete,
		"interval":                da.Interval,
	})
}

// apiPollQwenLogin POST /admin/api/auth/qwen/poll {sessionId} — one poll; on
// success it creates the Qwen account (with the OAuth tokens + resolved base URL)
// and returns its id.
func (h *Handler) apiPollQwenLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	qwenSessionsMu.Lock()
	sess := qwenSessions[req.SessionID]
	qwenSessionsMu.Unlock()
	if sess == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found or expired"})
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "login expired"})
		return
	}

	status, tokens, err := auth.PollQwenToken(r.Context(), sess.DeviceCode, sess.CodeVerifier)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": err.Error()})
		return
	}
	if status == "pending" {
		json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
		return
	}
	// status == "ok": persist the account. The resource_url selects the
	// OpenAI-compatible base, stored in BaseURLOverride so resolveProviderSettings
	// layers it over the catalog default.
	baseURL := auth.QwenBaseURLFromResource(tokens.ResourceURL)
	acct := config.Account{
		ID:              auth.GenerateAccountID(),
		Backend:         "qwen",
		Nickname:        "Qwen",
		AccessToken:     tokens.AccessToken,
		RefreshToken:    tokens.RefreshToken,
		BaseURLOverride: baseURL,
		Enabled:         true,
	}
	if tokens.ExpiresIn > 0 {
		acct.ExpiresAt = nowUnixSeconds() + int64(tokens.ExpiresIn)
	}
	if err := config.AddAccount(acct); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	qwenSessionsMu.Lock()
	delete(qwenSessions, req.SessionID)
	qwenSessionsMu.Unlock()
	logger.Infof("[Qwen] Added account %s (base=%s)", acct.ID, baseURL)

	// Best-effort: fetch the model catalog now so the dashboard shows a count
	// immediately. A failure doesn't fail the add. Qwen's DashScope base serves a
	// real /models list, so this is the live (strict) path; an advisory fallback
	// would only kick in if that ever changed.
	if ids, advisory, ferr := qwenInference.FetchModelsForAccount(r.Context(), &acct); ferr == nil && len(ids) > 0 {
		if advisory {
			h.pool.SetAdvisoryModelList(acct.ID, ids)
		} else {
			h.pool.SetModelList(acct.ID, ids)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"status": "completed", "id": acct.ID})
}

// ---- CodeBuddy browser-OAuth polling login (CN + international hosts) ----

type codeBuddySession struct {
	State     string
	AuthURL   string
	Backend   string // "codebuddy" (CN) or "codebuddy-ai" (international)
	Host      string // auth host base for poll/refresh
	ExpiresAt time.Time
}

var (
	codeBuddySessions   = map[string]*codeBuddySession{}
	codeBuddySessionsMu sync.Mutex
)

// codeBuddyBackendHost maps a CodeBuddy backend id to its auth host + display
// nickname. Defaults to the China gateway for an unknown/empty id so older callers
// keep working.
func codeBuddyBackendHost(backend string) (host, nickname string) {
	switch backend {
	case "codebuddy-ai":
		return auth.CodeBuddyHostIntl, "CodeBuddy (International)"
	default:
		return auth.CodeBuddyHostCN, "CodeBuddy"
	}
}

// codeBuddyBackendFromPath derives the backend id from the auth route path. The
// international route is /auth/codebuddy-ai/*, the CN route is /auth/codebuddy/*.
func codeBuddyBackendFromPath(path string) string {
	if strings.Contains(path, "/codebuddy-ai/") {
		return "codebuddy-ai"
	}
	return "codebuddy"
}

// apiStartCodeBuddyLogin POST /admin/api/auth/codebuddy/start (CN) and
// /admin/api/auth/codebuddy-ai/start (international) — begins a CodeBuddy browser
// login and returns {sessionId, verificationUri}. The dashboard opens the URL for
// the operator to approve, then polls until completion. The backend is taken from
// the request path so the right host is used for the whole flow.
func (h *Handler) apiStartCodeBuddyLogin(w http.ResponseWriter, r *http.Request) {
	backend := codeBuddyBackendFromPath(r.URL.Path)
	host, _ := codeBuddyBackendHost(backend)
	ca, err := auth.StartCodeBuddyAuth(r.Context(), host)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	id := auth.GenerateAccountID()
	sess := &codeBuddySession{
		State:     ca.State,
		AuthURL:   ca.AuthURL,
		Backend:   backend,
		Host:      host,
		ExpiresAt: time.Now().Add(300 * time.Second), // login window ~5 min
	}
	codeBuddySessionsMu.Lock()
	codeBuddySessions[id] = sess
	for sid, s := range codeBuddySessions {
		if time.Now().After(s.ExpiresAt) {
			delete(codeBuddySessions, sid)
		}
	}
	codeBuddySessionsMu.Unlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":               id,
		"verificationUri":         ca.AuthURL,
		"verificationUriComplete": ca.AuthURL,
		"interval":                5,
	})
}

// apiPollCodeBuddyLogin POST /admin/api/auth/codebuddy{,-ai}/poll {sessionId} —
// one poll; on success it creates the CodeBuddy account with the OAuth tokens and
// returns its id. The host + backend are read from the session captured at start.
func (h *Handler) apiPollCodeBuddyLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	codeBuddySessionsMu.Lock()
	sess := codeBuddySessions[req.SessionID]
	codeBuddySessionsMu.Unlock()
	if sess == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found or expired"})
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "login expired"})
		return
	}

	host := sess.Host
	if host == "" {
		host = auth.CodeBuddyHostCN
	}
	backend := sess.Backend
	if backend == "" {
		backend = "codebuddy"
	}
	_, nickname := codeBuddyBackendHost(backend)

	status, tokens, err := auth.PollCodeBuddyToken(r.Context(), host, sess.State)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": err.Error()})
		return
	}
	if status == "pending" {
		json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
		return
	}
	acct := config.Account{
		ID:           auth.GenerateAccountID(),
		Backend:      backend,
		Nickname:     nickname,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		Enabled:      true,
	}
	if tokens.ExpiresIn > 0 {
		acct.ExpiresAt = nowUnixSeconds() + int64(tokens.ExpiresIn)
	}
	if err := config.AddAccount(acct); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	codeBuddySessionsMu.Lock()
	delete(codeBuddySessions, req.SessionID)
	codeBuddySessionsMu.Unlock()
	logger.Infof("[CodeBuddy] Added account %s (%s)", acct.ID, backend)

	if ids, advisory, ferr := codeBuddyInference.FetchModelsForAccount(r.Context(), &acct); ferr == nil && len(ids) > 0 {
		if advisory {
			h.pool.SetAdvisoryModelList(acct.ID, ids)
		} else {
			h.pool.SetModelList(acct.ID, ids)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"status": "completed", "id": acct.ID})
}

// ---- Kimi Coding (Moonshot) OAuth device login ----

type kimiCodingSession struct {
	DeviceCode string
	UserCode   string
	VerifyURL  string
	ExpiresAt  time.Time
}

var (
	kimiCodingSessions   = map[string]*kimiCodingSession{}
	kimiCodingSessionsMu sync.Mutex
)

// apiStartKimiCodingLogin POST /admin/api/auth/kimi-coding/start — begins a Kimi
// Coding device login and returns {sessionId, userCode, verificationUri, ...}.
func (h *Handler) apiStartKimiCodingLogin(w http.ResponseWriter, r *http.Request) {
	da, err := auth.StartKimiCodingDeviceAuth(r.Context())
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	id := auth.GenerateAccountID()
	expiresIn := da.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 300
	}
	sess := &kimiCodingSession{
		DeviceCode: da.DeviceCode,
		UserCode:   da.UserCode,
		VerifyURL:  firstNonEmpty(da.VerificationURIComplete, da.VerificationURI),
		ExpiresAt:  time.Now().Add(time.Duration(expiresIn) * time.Second),
	}
	kimiCodingSessionsMu.Lock()
	kimiCodingSessions[id] = sess
	for sid, s := range kimiCodingSessions {
		if time.Now().After(s.ExpiresAt) {
			delete(kimiCodingSessions, sid)
		}
	}
	kimiCodingSessionsMu.Unlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":               id,
		"userCode":                da.UserCode,
		"verificationUri":         da.VerificationURI,
		"verificationUriComplete": da.VerificationURIComplete,
		"interval":                da.Interval,
	})
}

// apiPollKimiCodingLogin POST /admin/api/auth/kimi-coding/poll {sessionId} — one poll.
func (h *Handler) apiPollKimiCodingLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	kimiCodingSessionsMu.Lock()
	sess := kimiCodingSessions[req.SessionID]
	kimiCodingSessionsMu.Unlock()
	if sess == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found or expired"})
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "login expired"})
		return
	}

	status, tokens, err := auth.PollKimiCodingToken(r.Context(), sess.DeviceCode)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": err.Error()})
		return
	}
	if status == "pending" {
		json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
		return
	}
	acct := config.Account{
		ID:           auth.GenerateAccountID(),
		Backend:      "kimi-coding",
		Nickname:     "Kimi Coding",
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		Enabled:      true,
	}
	if tokens.ExpiresIn > 0 {
		acct.ExpiresAt = nowUnixSeconds() + int64(tokens.ExpiresIn)
	}
	if err := config.AddAccount(acct); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	kimiCodingSessionsMu.Lock()
	delete(kimiCodingSessions, req.SessionID)
	kimiCodingSessionsMu.Unlock()
	logger.Infof("[KimiCoding] Added account %s", acct.ID)

	if ids, advisory, ferr := kimiCodingInference.FetchModelsForAccount(r.Context(), &acct); ferr == nil && len(ids) > 0 {
		if advisory {
			h.pool.SetAdvisoryModelList(acct.ID, ids)
		} else {
			h.pool.SetModelList(acct.ID, ids)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"status": "completed", "id": acct.ID})
}

// ---- Kilo Code custom device-auth login ----

type kilocodeSession struct {
	Code      string
	VerifyURL string
	ExpiresAt time.Time
}

var (
	kilocodeSessions   = map[string]*kilocodeSession{}
	kilocodeSessionsMu sync.Mutex
)

// apiStartKilocodeLogin POST /admin/api/auth/kilocode/start — begins a Kilo Code
// device login and returns {sessionId, verificationUri}.
func (h *Handler) apiStartKilocodeLogin(w http.ResponseWriter, r *http.Request) {
	da, err := auth.StartKilocodeDeviceAuth(r.Context())
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	id := auth.GenerateAccountID()
	sess := &kilocodeSession{
		Code:      da.Code,
		VerifyURL: da.VerificationURL,
		ExpiresAt: time.Now().Add(time.Duration(da.ExpiresIn) * time.Second),
	}
	kilocodeSessionsMu.Lock()
	kilocodeSessions[id] = sess
	for sid, s := range kilocodeSessions {
		if time.Now().After(s.ExpiresAt) {
			delete(kilocodeSessions, sid)
		}
	}
	kilocodeSessionsMu.Unlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":               id,
		"verificationUri":         da.VerificationURL,
		"verificationUriComplete": da.VerificationURL,
		"interval":                da.Interval,
	})
}

// apiPollKilocodeLogin POST /admin/api/auth/kilocode/poll {sessionId} — one poll.
func (h *Handler) apiPollKilocodeLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	kilocodeSessionsMu.Lock()
	sess := kilocodeSessions[req.SessionID]
	kilocodeSessionsMu.Unlock()
	if sess == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found or expired"})
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "login expired"})
		return
	}

	status, tokens, err := auth.PollKilocodeToken(r.Context(), sess.Code)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": err.Error()})
		return
	}
	if status == "pending" {
		json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
		return
	}
	acct := config.Account{
		ID:          auth.GenerateAccountID(),
		Backend:     "kilocode",
		Nickname:    "Kilo Code",
		AccessToken: tokens.AccessToken,
		Email:       tokens.UserEmail,
		Enabled:     true,
	}
	if tokens.OrgID != "" {
		acct.ExtraHeaders = map[string]string{"X-Kilocode-OrganizationID": tokens.OrgID}
	}
	if err := config.AddAccount(acct); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	kilocodeSessionsMu.Lock()
	delete(kilocodeSessions, req.SessionID)
	kilocodeSessionsMu.Unlock()
	logger.Infof("[Kilocode] Added account %s (org=%s)", acct.ID, tokens.OrgID)

	if ids, advisory, ferr := kilocodeInference.FetchModelsForAccount(r.Context(), &acct); ferr == nil && len(ids) > 0 {
		if advisory {
			h.pool.SetAdvisoryModelList(acct.ID, ids)
		} else {
			h.pool.SetModelList(acct.ID, ids)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"status": "completed", "id": acct.ID})
}

// ---- GitHub Copilot OAuth device login ----

type githubSession struct {
	DeviceCode string
	UserCode   string
	VerifyURL  string
	ExpiresAt  time.Time
}

var (
	githubSessions   = map[string]*githubSession{}
	githubSessionsMu sync.Mutex
)

// apiStartGitHubLogin POST /admin/api/auth/github/start — begins a GitHub Copilot
// device login and returns {sessionId, userCode, verificationUri}.
func (h *Handler) apiStartGitHubLogin(w http.ResponseWriter, r *http.Request) {
	da, err := auth.StartGitHubDeviceAuth(r.Context())
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	id := auth.GenerateAccountID()
	expiresIn := da.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 900 // GitHub device codes are valid ~15 min
	}
	sess := &githubSession{
		DeviceCode: da.DeviceCode,
		UserCode:   da.UserCode,
		VerifyURL:  da.VerificationURI,
		ExpiresAt:  time.Now().Add(time.Duration(expiresIn) * time.Second),
	}
	githubSessionsMu.Lock()
	githubSessions[id] = sess
	for sid, s := range githubSessions {
		if time.Now().After(s.ExpiresAt) {
			delete(githubSessions, sid)
		}
	}
	githubSessionsMu.Unlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":               id,
		"userCode":                da.UserCode,
		"verificationUri":         da.VerificationURI,
		"verificationUriComplete": da.VerificationURI,
		"interval":                da.Interval,
	})
}

// apiPollGitHubLogin POST /admin/api/auth/github/poll {sessionId} — one poll; on
// success it creates the GitHub Copilot account (GitHub token in RefreshToken, minted
// Copilot token in AccessToken) and returns its id.
func (h *Handler) apiPollGitHubLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	githubSessionsMu.Lock()
	sess := githubSessions[req.SessionID]
	githubSessionsMu.Unlock()
	if sess == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found or expired"})
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "login expired"})
		return
	}

	status, tokens, err := auth.PollGitHubToken(r.Context(), sess.DeviceCode)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": err.Error()})
		return
	}
	if status == "pending" {
		json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
		return
	}
	nick := "GitHub Copilot"
	if tokens.GitHubLogin != "" {
		nick = "GitHub Copilot (" + tokens.GitHubLogin + ")"
	}
	acct := config.Account{
		ID:           auth.GenerateAccountID(),
		Backend:      "github",
		Nickname:     nick,
		AccessToken:  tokens.CopilotToken, // short-lived inference bearer
		RefreshToken: tokens.GitHubToken,  // long-lived; re-mints copilot tokens
		ExpiresAt:    tokens.CopilotExpiresAt,
		Enabled:      true,
	}
	if err := config.AddAccount(acct); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	githubSessionsMu.Lock()
	delete(githubSessions, req.SessionID)
	githubSessionsMu.Unlock()
	logger.Infof("[GitHubCopilot] Added account %s (login=%s)", acct.ID, tokens.GitHubLogin)

	if ids, advisory, ferr := githubInference.FetchModelsForAccount(r.Context(), &acct); ferr == nil && len(ids) > 0 {
		if advisory {
			h.pool.SetAdvisoryModelList(acct.ID, ids)
		} else {
			h.pool.SetModelList(acct.ID, ids)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"status": "completed", "id": acct.ID})
}

// ---- Claude Code (Anthropic) OAuth manual-code login ----

type claudeSession struct {
	State        string
	CodeVerifier string
	ExpiresAt    time.Time
}

var (
	claudeSessions   = map[string]*claudeSession{}
	claudeSessionsMu sync.Mutex
)

// apiStartClaudeLogin POST /admin/api/auth/claude/start — returns {sessionId,
// authUrl}. The operator opens authUrl, approves, copies the code#state string the
// Anthropic console shows, and submits it to /auth/claude/complete.
func (h *Handler) apiStartClaudeLogin(w http.ResponseWriter, r *http.Request) {
	st, err := auth.StartClaudeLogin()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	id := auth.GenerateAccountID()
	claudeSessionsMu.Lock()
	claudeSessions[id] = &claudeSession{
		State:        st.State,
		CodeVerifier: st.CodeVerifier,
		ExpiresAt:    time.Now().Add(10 * time.Minute),
	}
	for sid, s := range claudeSessions {
		if time.Now().After(s.ExpiresAt) {
			delete(claudeSessions, sid)
		}
	}
	claudeSessionsMu.Unlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId": id,
		"authUrl":   st.AuthURL,
	})
}

// apiCompleteClaudeLogin POST /admin/api/auth/claude/complete {sessionId, code} —
// exchanges the pasted code for tokens and creates the Claude Code account.
func (h *Handler) apiCompleteClaudeLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
		Code      string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "missing code"})
		return
	}
	claudeSessionsMu.Lock()
	sess := claudeSessions[req.SessionID]
	claudeSessionsMu.Unlock()
	if sess == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found or expired"})
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "login expired"})
		return
	}

	tokens, err := auth.ExchangeClaudeCode(r.Context(), req.Code, sess.CodeVerifier, sess.State)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": err.Error()})
		return
	}
	acct := config.Account{
		ID:           auth.GenerateAccountID(),
		Backend:      "claude-code",
		Nickname:     "Claude Code",
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		Enabled:      true,
	}
	if tokens.ExpiresIn > 0 {
		acct.ExpiresAt = nowUnixSeconds() + int64(tokens.ExpiresIn)
	}
	if err := config.AddAccount(acct); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	claudeSessionsMu.Lock()
	delete(claudeSessions, req.SessionID)
	claudeSessionsMu.Unlock()
	logger.Infof("[ClaudeCode] Added account %s", acct.ID)

	if ids, advisory, ferr := claudeInference.FetchModelsForAccount(r.Context(), &acct); ferr == nil && len(ids) > 0 {
		if advisory {
			h.pool.SetAdvisoryModelList(acct.ID, ids)
		} else {
			h.pool.SetModelList(acct.ID, ids)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"status": "completed", "id": acct.ID})
}

// ---- xAI (Grok) OAuth loopback login ----

// apiStartXaiLogin POST /admin/api/auth/xai/start — begins the xAI browser OAuth
// flow (loopback callback) and returns {sessionId, authUrl}.
func (h *Handler) apiStartXaiLogin(w http.ResponseWriter, r *http.Request) {
	sess, err := auth.StartXaiLogin()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId": sess.ID,
		"authUrl":   sess.AuthURL,
	})
}

// apiPollXaiLogin POST /admin/api/auth/xai/poll {sessionId} — returns status; on
// "completed" it creates the xAI account and returns its id.
func (h *Handler) apiPollXaiLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	status, tokens, errMsg, found := auth.PollXaiLogin(req.SessionID)
	if !found {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found or expired"})
		return
	}
	switch status {
	case "pending":
		json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
	case "error":
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": errMsg})
	case "completed":
		nick := firstNonEmpty(tokens.Email, "xAI (Grok)")
		acct := config.Account{
			ID:           auth.GenerateAccountID(),
			Backend:      "xai",
			Email:        tokens.Email,
			Nickname:     nick,
			AccessToken:  tokens.AccessToken,
			RefreshToken: tokens.RefreshToken,
			Enabled:      true,
		}
		if tokens.ExpiresIn > 0 {
			acct.ExpiresAt = nowUnixSeconds() + int64(tokens.ExpiresIn)
		}
		if err := config.AddAccount(acct); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		h.pool.Reload()
		logger.Infof("[xAI] Added account %s", acct.ID)
		if ids, advisory, ferr := xaiInference.FetchModelsForAccount(r.Context(), &acct); ferr == nil && len(ids) > 0 {
			if advisory {
				h.pool.SetAdvisoryModelList(acct.ID, ids)
			} else {
				h.pool.SetModelList(acct.ID, ids)
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "completed", "id": acct.ID})
	default:
		json.NewEncoder(w).Encode(map[string]string{"status": status})
	}
}

// ---- Cline OAuth manual-code login ----

type clineSession struct {
	ExpiresAt time.Time
}

var (
	clineSessions   = map[string]*clineSession{}
	clineSessionsMu sync.Mutex
)

// apiStartClineLogin POST /admin/api/auth/cline/start — returns {sessionId, authUrl}.
// The operator opens authUrl, approves, copies the returned code, and submits it to
// /auth/cline/complete. Cline carries no PKCE/state, so the session is just a TTL guard.
func (h *Handler) apiStartClineLogin(w http.ResponseWriter, r *http.Request) {
	id := auth.GenerateAccountID()
	clineSessionsMu.Lock()
	clineSessions[id] = &clineSession{ExpiresAt: time.Now().Add(10 * time.Minute)}
	for sid, s := range clineSessions {
		if time.Now().After(s.ExpiresAt) {
			delete(clineSessions, sid)
		}
	}
	clineSessionsMu.Unlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId": id,
		"authUrl":   auth.BuildClineAuthURL(),
	})
}

// apiCompleteClineLogin POST /admin/api/auth/cline/complete {sessionId, code}.
func (h *Handler) apiCompleteClineLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
		Code      string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "missing code"})
		return
	}
	clineSessionsMu.Lock()
	sess := clineSessions[req.SessionID]
	clineSessionsMu.Unlock()
	if sess == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found or expired"})
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "login expired"})
		return
	}

	tokens, err := auth.ExchangeClineCode(r.Context(), req.Code)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": err.Error()})
		return
	}
	acct := config.Account{
		ID:           auth.GenerateAccountID(),
		Backend:      "cline",
		Nickname:     firstNonEmpty(tokens.Email, "Cline"),
		Email:        tokens.Email,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		Enabled:      true,
	}
	if tokens.ExpiresIn > 0 {
		acct.ExpiresAt = nowUnixSeconds() + int64(tokens.ExpiresIn)
	}
	if err := config.AddAccount(acct); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	clineSessionsMu.Lock()
	delete(clineSessions, req.SessionID)
	clineSessionsMu.Unlock()
	logger.Infof("[Cline] Added account %s", acct.ID)

	if ids, advisory, ferr := clineInference.FetchModelsForAccount(r.Context(), &acct); ferr == nil && len(ids) > 0 {
		if advisory {
			h.pool.SetAdvisoryModelList(acct.ID, ids)
		} else {
			h.pool.SetModelList(acct.ID, ids)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"status": "completed", "id": acct.ID})
}

// ---- iFlow OAuth loopback login ----

// apiStartIFlowLogin POST /admin/api/auth/iflow/start — begins the iFlow browser
// OAuth flow (loopback callback) and returns {sessionId, authUrl}.
func (h *Handler) apiStartIFlowLogin(w http.ResponseWriter, r *http.Request) {
	sess, err := auth.StartIFlowLogin()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId": sess.ID,
		"authUrl":   sess.AuthURL,
	})
}

// apiPollIFlowLogin POST /admin/api/auth/iflow/poll {sessionId} — returns status;
// on "completed" it creates the iFlow account (apiKey as the inference credential).
func (h *Handler) apiPollIFlowLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	status, tokens, errMsg, found := auth.PollIFlowLogin(req.SessionID)
	if !found {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found or expired"})
		return
	}
	switch status {
	case "pending":
		json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
	case "error":
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": errMsg})
	case "completed":
		acct := config.Account{
			ID:       auth.GenerateAccountID(),
			Backend:  "iflow",
			Nickname: firstNonEmpty(tokens.Email, "iFlow"),
			Email:    tokens.Email,
			// iFlow inference authenticates with the resolved apiKey (Bearer), so it
			// lives in APIKey; the OAuth tokens are kept for a future re-auth.
			APIKey:       tokens.APIKey,
			AccessToken:  tokens.AccessToken,
			RefreshToken: tokens.RefreshToken,
			Enabled:      true,
		}
		if err := config.AddAccount(acct); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		h.pool.Reload()
		logger.Infof("[iFlow] Added account %s", acct.ID)
		if ids, advisory, ferr := genericOpenAIInference().FetchModelsForAccount(r.Context(), &acct); ferr == nil && len(ids) > 0 {
			if advisory {
				h.pool.SetAdvisoryModelList(acct.ID, ids)
			} else {
				h.pool.SetModelList(acct.ID, ids)
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "completed", "id": acct.ID})
	default:
		json.NewEncoder(w).Encode(map[string]string{"status": status})
	}
}

// ---- GitLab Duo OAuth manual-code login (self-hostable: operator supplies
// baseUrl + clientId + optional clientSecret of the OAuth app on their instance) ----

type gitlabSession struct {
	BaseURL      string
	ClientID     string
	ClientSecret string
	State        string
	CodeVerifier string
	ExpiresAt    time.Time
}

var (
	gitlabSessions   = map[string]*gitlabSession{}
	gitlabSessionsMu sync.Mutex
)

// apiStartGitLabLogin POST /admin/api/auth/gitlab/start {baseUrl?, clientId,
// clientSecret?} — returns {sessionId, authUrl}.
func (h *Handler) apiStartGitLabLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL      string `json:"baseUrl"`
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if strings.TrimSpace(req.ClientID) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "clientId is required (register an OAuth app on your GitLab instance)"})
		return
	}
	st, err := auth.StartGitLabLogin(req.BaseURL, req.ClientID)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	id := auth.GenerateAccountID()
	gitlabSessionsMu.Lock()
	gitlabSessions[id] = &gitlabSession{
		BaseURL:      req.BaseURL,
		ClientID:     req.ClientID,
		ClientSecret: req.ClientSecret,
		State:        st.State,
		CodeVerifier: st.CodeVerifier,
		ExpiresAt:    time.Now().Add(10 * time.Minute),
	}
	for sid, s := range gitlabSessions {
		if time.Now().After(s.ExpiresAt) {
			delete(gitlabSessions, sid)
		}
	}
	gitlabSessionsMu.Unlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId": id,
		"authUrl":   st.AuthURL,
	})
}

// apiCompleteGitLabLogin POST /admin/api/auth/gitlab/complete {sessionId, code} —
// exchanges the pasted code and creates the GitLab Duo account. The instance
// inference URL + OAuth app creds are persisted so token refresh works later.
func (h *Handler) apiCompleteGitLabLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
		Code      string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "missing code"})
		return
	}
	gitlabSessionsMu.Lock()
	sess := gitlabSessions[req.SessionID]
	gitlabSessionsMu.Unlock()
	if sess == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found or expired"})
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "login expired"})
		return
	}

	tokens, err := auth.ExchangeGitLabCode(r.Context(), sess.BaseURL, sess.ClientID, sess.ClientSecret, req.Code, sess.CodeVerifier)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": err.Error()})
		return
	}
	// Resolve the instance inference URL (default gitlab.com).
	base := strings.TrimRight(strings.TrimSpace(sess.BaseURL), "/")
	if base == "" {
		base = "https://gitlab.com"
	} else if !strings.HasPrefix(strings.ToLower(base), "http") {
		base = "https://" + base
	}
	inferenceURL := base + "/api/v4/chat/completions"

	acct := config.Account{
		ID:              auth.GenerateAccountID(),
		Backend:         "gitlab",
		Nickname:        firstNonEmpty(tokens.Username, "GitLab Duo"),
		AccessToken:     tokens.AccessToken,
		RefreshToken:    tokens.RefreshToken,
		ClientID:        sess.ClientID,
		ClientSecret:    sess.ClientSecret,
		BaseURLOverride: inferenceURL,
		Enabled:         true,
	}
	if tokens.ExpiresIn > 0 {
		acct.ExpiresAt = nowUnixSeconds() + int64(tokens.ExpiresIn)
	}
	if err := config.AddAccount(acct); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	gitlabSessionsMu.Lock()
	delete(gitlabSessions, req.SessionID)
	gitlabSessionsMu.Unlock()
	logger.Infof("[GitLab] Added account %s (user=%s)", acct.ID, tokens.Username)

	if ids, advisory, ferr := gitlabInference.FetchModelsForAccount(r.Context(), &acct); ferr == nil && len(ids) > 0 {
		if advisory {
			h.pool.SetAdvisoryModelList(acct.ID, ids)
		} else {
			h.pool.SetModelList(acct.ID, ids)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"status": "completed", "id": acct.ID})
}

// ---- Gemini CLI (Cloud Code Assist) OAuth loopback login ----

// apiStartGeminiCLILogin POST /admin/api/auth/gemini-cli/start — begins the Google
// OAuth flow (loopback callback) and returns {sessionId, authUrl}.
func (h *Handler) apiStartGeminiCLILogin(w http.ResponseWriter, r *http.Request) {
	sess, err := auth.StartGeminiCLILogin()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId": sess.ID,
		"authUrl":   sess.AuthURL,
	})
}

// apiPollGeminiCLILogin POST /admin/api/auth/gemini-cli/poll {sessionId} — returns
// status; on "completed" creates the account (project id stored in ExtraHeaders).
func (h *Handler) apiPollGeminiCLILogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	status, tokens, errMsg, found := auth.PollGeminiCLILogin(req.SessionID)
	if !found {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found or expired"})
		return
	}
	switch status {
	case "pending":
		json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
	case "error":
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": errMsg})
	case "completed":
		acct := config.Account{
			ID:           auth.GenerateAccountID(),
			Backend:      "gemini-cli",
			Nickname:     firstNonEmpty(tokens.Email, "Gemini CLI"),
			Email:        tokens.Email,
			AccessToken:  tokens.AccessToken,
			RefreshToken: tokens.RefreshToken,
			Enabled:      true,
		}
		if tokens.ProjectID != "" {
			acct.ExtraHeaders = map[string]string{"x-goog-project": tokens.ProjectID}
		}
		if tokens.ExpiresIn > 0 {
			acct.ExpiresAt = nowUnixSeconds() + int64(tokens.ExpiresIn)
		}
		if err := config.AddAccount(acct); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		h.pool.Reload()
		logger.Infof("[GeminiCLI] Added account %s (project=%s)", acct.ID, tokens.ProjectID)
		if p := ProviderForBackend("gemini-cli"); p != nil {
			if models, merr := p.ListModels(&acct); merr == nil && len(models) > 0 {
				ids := make([]string, 0, len(models))
				for _, m := range models {
					ids = append(ids, m.ModelId)
				}
				h.pool.SetAdvisoryModelList(acct.ID, ids)
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "completed", "id": acct.ID})
	default:
		json.NewEncoder(w).Encode(map[string]string{"status": status})
	}
}

// ---- Antigravity (Cloud Code Assist) OAuth loopback login ----

// apiStartAntigravityLogin POST /admin/api/auth/antigravity/start.
func (h *Handler) apiStartAntigravityLogin(w http.ResponseWriter, r *http.Request) {
	sess, err := auth.StartAntigravityLogin()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId": sess.ID,
		"authUrl":   sess.AuthURL,
	})
}

// apiPollAntigravityLogin POST /admin/api/auth/antigravity/poll {sessionId}.
func (h *Handler) apiPollAntigravityLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	status, tokens, errMsg, found := auth.PollAntigravityLogin(req.SessionID)
	if !found {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found or expired"})
		return
	}
	switch status {
	case "pending":
		json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
	case "error":
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": errMsg})
	case "completed":
		acct := config.Account{
			ID:           auth.GenerateAccountID(),
			Backend:      "antigravity",
			Nickname:     firstNonEmpty(tokens.Email, "Antigravity"),
			Email:        tokens.Email,
			AccessToken:  tokens.AccessToken,
			RefreshToken: tokens.RefreshToken,
			Enabled:      true,
		}
		if tokens.ProjectID != "" {
			acct.ExtraHeaders = map[string]string{"x-goog-project": tokens.ProjectID}
		}
		if tokens.ExpiresIn > 0 {
			acct.ExpiresAt = nowUnixSeconds() + int64(tokens.ExpiresIn)
		}
		if err := config.AddAccount(acct); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		h.pool.Reload()
		logger.Infof("[Antigravity] Added account %s (project=%s)", acct.ID, tokens.ProjectID)
		if p := ProviderForBackend("antigravity"); p != nil {
			if models, merr := p.ListModels(&acct); merr == nil && len(models) > 0 {
				ids := make([]string, 0, len(models))
				for _, m := range models {
					ids = append(ids, m.ModelId)
				}
				h.pool.SetAdvisoryModelList(acct.ID, ids)
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "completed", "id": acct.ID})
	default:
		json.NewEncoder(w).Encode(map[string]string{"status": status})
	}
}

// ---- Vertex AI Service-Account import (no browser flow) ----

// apiImportVertexServiceAccount POST /admin/api/auth/vertex/import
// {serviceAccountJson, region?, name?} — validates the SA JSON, mints a first
// access token to confirm it works, and creates the Vertex account (SA JSON in
// APIKey, region in Region).
func (h *Handler) apiImportVertexServiceAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ServiceAccountJSON string `json:"serviceAccountJson"`
		Region             string `json:"region"`
		Name               string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	sa, err := auth.ParseVertexServiceAccount(strings.TrimSpace(req.ServiceAccountJSON))
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	// Confirm the SA can actually mint a token before persisting.
	tok, expiresAt, err := auth.VertexAccessToken(r.Context(), req.ServiceAccountJSON)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "service account validation failed: " + err.Error()})
		return
	}
	region := strings.TrimSpace(req.Region)
	if region == "" {
		region = "us-central1"
	}
	acct := config.Account{
		ID:          auth.GenerateAccountID(),
		Backend:     "vertex",
		Nickname:    firstNonEmpty(req.Name, "Vertex AI ("+sa.ProjectID+")"),
		Email:       sa.ClientEmail,
		APIKey:      strings.TrimSpace(req.ServiceAccountJSON), // durable SA credential
		AccessToken: tok,
		ExpiresAt:   expiresAt.Unix(),
		Region:      region,
		Enabled:     true,
	}
	if err := config.AddAccount(acct); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	logger.Infof("[Vertex] Added account %s (project=%s region=%s)", acct.ID, sa.ProjectID, region)
	if p := ProviderForBackend("vertex"); p != nil {
		if models, merr := p.ListModels(&acct); merr == nil && len(models) > 0 {
			ids := make([]string, 0, len(models))
			for _, m := range models {
				ids = append(ids, m.ModelId)
			}
			h.pool.SetAdvisoryModelList(acct.ID, ids)
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": acct.ID})
}

// ---- Cursor IDE token import (token + machine id pasted from the IDE) ----

// apiImportCursorToken POST /admin/api/auth/cursor/import {accessToken, machineId, name?}
// — adds a Cursor account from the token + machine id imported from the Cursor IDE's
// state.vscdb (cursorAuth/accessToken + storage.serviceMachineId).
func (h *Handler) apiImportCursorToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccessToken string `json:"accessToken"`
		MachineID   string `json:"machineId"`
		Name        string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if strings.TrimSpace(req.AccessToken) == "" || strings.TrimSpace(req.MachineID) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "accessToken and machineId are both required (import them from the Cursor IDE)"})
		return
	}
	acct := config.Account{
		ID:          auth.GenerateAccountID(),
		Backend:     "cursor",
		Nickname:    firstNonEmpty(req.Name, "Cursor IDE"),
		AccessToken: strings.TrimSpace(req.AccessToken),
		MachineId:   strings.TrimSpace(req.MachineID),
		Enabled:     true,
	}
	if err := config.AddAccount(acct); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	logger.Infof("[Cursor] Added account %s", acct.ID)
	if p := ProviderForBackend("cursor"); p != nil {
		if models, merr := p.ListModels(&acct); merr == nil && len(models) > 0 {
			ids := make([]string, 0, len(models))
			for _, m := range models {
				ids = append(ids, m.ModelId)
			}
			h.pool.SetAdvisoryModelList(acct.ID, ids)
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": acct.ID})
}

// apiImportCookieProvider POST /admin/api/auth/cookie/import {backend, cookie, name?}
// — adds a cookie-authenticated web-subscription account (grok-web or
// perplexity-web). The cookie value is stored in APIKey; the bespoke provider sends
// it as the appropriate Cookie header.
func (h *Handler) apiImportCookieProvider(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Backend string `json:"backend"`
		Cookie  string `json:"cookie"`
		Name    string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	backend := strings.ToLower(strings.TrimSpace(req.Backend))
	if backend != "grok-web" && backend != "perplexity-web" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "backend must be grok-web or perplexity-web"})
		return
	}
	if strings.TrimSpace(req.Cookie) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "cookie value is required"})
		return
	}
	bp, _ := resolveBuiltinProvider(backend)
	acct := config.Account{
		ID:       auth.GenerateAccountID(),
		Backend:  backend,
		Nickname: firstNonEmpty(req.Name, bp.Name),
		APIKey:   strings.TrimSpace(req.Cookie),
		Enabled:  true,
	}
	if err := config.AddAccount(acct); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	logger.Infof("[%s] Added cookie account %s", backend, acct.ID)
	if p := ProviderForBackend(backend); p != nil {
		if models, merr := p.ListModels(&acct); merr == nil && len(models) > 0 {
			ids := make([]string, 0, len(models))
			for _, m := range models {
				ids = append(ids, m.ModelId)
			}
			h.pool.SetAdvisoryModelList(acct.ID, ids)
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": acct.ID})
}
