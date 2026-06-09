package proxy

import (
	"context"
	"encoding/json"
	"errors"
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

	// Built-in data-driven (api-key) providers.
	for _, bp := range builtinProviders {
		out = append(out, providerCatalogEntry{
			ID:       bp.ID,
			Alias:    bp.Alias,
			Name:     bp.Name,
			Dialect:  string(bp.Dialect),
			AuthType: "apikey",
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
		// Custom (bring-your-own) provider fields. When backend == "custom" the
		// caller supplies the dialect + base URL directly and we register a
		// ProviderConfig on the fly — no separate provider-registration step. This
		// is the "add any OpenAI-compatible endpoint" path: it does NOT reuse the
		// Kiro account schema.
		Dialect string `json:"dialect"` // openai | anthropic | gemini (custom only)
		Alias   string `json:"alias"`   // optional routing prefix (custom only)
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
	if strings.TrimSpace(req.APIKey) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "apiKey is required"})
		return
	}

	// "custom" backend: register a ProviderConfig on the fly from the supplied
	// base URL + dialect, then point the account at it. This is the
	// bring-your-own OpenAI-compatible endpoint flow — base URL is an API BASE
	// (e.g. https://api.example.com/v1), NOT a full /chat/completions URL; the
	// generic provider derives /chat/completions and /models from it.
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
		if !strings.HasPrefix(strings.ToLower(base), "https://") {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "baseURL must use https://"})
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			name = "Custom Provider"
		}
		// Derive a stable id from the name (slug); ensure it doesn't collide with a
		// built-in.
		id := slugifyProviderID(name)
		if id == "" {
			id = "custom-" + auth.GenerateAccountID()[:8]
		}
		if _, clash := resolveBuiltinProvider(id); clash {
			id = id + "-" + auth.GenerateAccountID()[:6]
		}
		pc := config.ProviderConfig{
			ID:          id,
			Alias:       strings.ToLower(strings.TrimSpace(req.Alias)),
			Name:        name,
			Dialect:     dialect,
			BaseURL:     base,
			FetchModels: true,
		}
		if err := config.AddProvider(pc); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		backend = id // the account points at the freshly-registered provider
	} else {
		// Named (built-in or previously-registered custom) provider.
		_, builtinOK := resolveBuiltinProvider(backend)
		_, customOK := config.GetProviderConfig(backend)
		if !builtinOK && !customOK {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "unknown provider backend: " + backend})
			return
		}
		// SSRF guard on the optional per-account base URL override.
		if bu := strings.TrimSpace(req.BaseURL); bu != "" && !strings.HasPrefix(strings.ToLower(bu), "https://") {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "baseURL must use https://"})
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

	// For a "custom" add the base URL is already baked into the ProviderConfig,
	// so don't also stamp it as a per-account override (that would double-apply).
	baseOverride := ""
	if req.Backend != "" && strings.ToLower(strings.TrimSpace(req.Backend)) != "custom" {
		baseOverride = strings.TrimSpace(req.BaseURL)
	}

	acct := config.Account{
		ID:              auth.GenerateAccountID(),
		Backend:         backend,
		APIKey:          strings.TrimSpace(req.APIKey),
		Nickname:        name,
		BaseURLOverride: baseOverride,
		Weight:          req.Weight,
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
	// later models-refresh / first request will populate it. This is what makes
	// "add your OpenAI-compatible endpoint and it gets the models" work on add.
	models := []string{}
	if prov, ok := ProviderForBackend(backend).(*genericProvider); ok {
		if ids, ferr := prov.FetchModelsForAccount(r.Context(), &acct); ferr == nil && len(ids) > 0 {
			models = ids
			h.pool.SetModelList(acct.ID, ids)
			logger.Infof("[Providers] %s account %s: fetched %d models", backend, acct.ID, len(ids))
		} else if ferr != nil {
			logger.Warnf("[Providers] %s account %s: model fetch failed (account still added): %v", backend, acct.ID, ferr)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"id":         acct.ID,
		"backend":    backend,
		"modelCount": len(models),
		"models":     models,
	})
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
	// SSRF guard: restrict custom-provider base URLs to https so they can't be
	// pointed at file://, plaintext, or internal endpoints (see security review).
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(pc.BaseURL)), "https://") {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "baseURL must use https://"})
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
	qoderSessionsMu.Lock()
	delete(qoderSessions, req.SessionID)
	qoderSessionsMu.Unlock()
	logger.Infof("[Qoder] Added account %s (%s)", acct.ID, name)
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "completed", "id": acct.ID})
}
