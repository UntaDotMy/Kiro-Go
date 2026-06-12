package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kiro-go/auth"
	"kiro-go/config"
)

// 9router interop: import a 9router LOGICAL JSON backup (the shape returned by
// 9router's GET /api/settings/database — exportDb() in src/lib/db/index.js) and
// export a Kiro-Go config in that same shape so a 9router instance can consume
// it. This is the portable JSON envelope, NOT 9router's raw .sqlite file backup.
//
// 9router (github.com/decolua/9router) stores every account/credential in a
// `providerConnections` table, custom OpenAI/Anthropic-compatible endpoints in
// `providerNodes`, inbound client keys in `apiKeys`, and named fallback chains
// in `combos`. The envelope carries no version field — importDb() wipes and
// replaces, tolerating missing keys — so we detect it structurally and emit all
// keys (even empty) on export for maximum compatibility.
//
// Mapping summary (see nineRouterProviderToBackend / backendToNineRouter):
//   providerConnections[] <-> config.Account   (backend from provider slug;
//                                                tokens/apiKey; providerSpecificData
//                                                -> Codex/Qoder/Kiro fields)
//   providerNodes[]       <-> config.ProviderConfig (dialect from type, prefix->alias)
//   apiKeys[]             <-> config.APIKey      (inbound client keys)
//   combos/pricing/...    -> preserved as empty on export; ignored on import

// nineRouterSchemaVersion is stamped onto our export as an EXTRA top-level field.
// 9router's importDb() only reads known keys, so this is safely ignored on
// re-import while letting our own importer recognise a Kiro-Go-produced file.
const nineRouterSchemaVersion = 1

// nineRouterBackup is the top-level logical-backup envelope. Unknown/loosely
// typed sections (settings, combos, pricing, ...) are kept as json.RawMessage so
// an import round-trips them untouched and an export can re-emit them verbatim.
type nineRouterBackup struct {
	// Our own marker (extra key; ignored by 9router's importer).
	SchemaVersion int    `json:"schemaVersion,omitempty"`
	ExportedBy    string `json:"exportedBy,omitempty"`
	ExportedAt    string `json:"exportedAt,omitempty"`

	Settings            json.RawMessage        `json:"settings,omitempty"`
	ProviderConnections []nineRouterConnection `json:"providerConnections"`
	ProviderNodes       []nineRouterNode       `json:"providerNodes"`
	ProxyPools          []json.RawMessage      `json:"proxyPools"`
	APIKeys             []nineRouterAPIKey     `json:"apiKeys"`
	Combos              []json.RawMessage      `json:"combos"`
	ModelAliases        json.RawMessage        `json:"modelAliases,omitempty"`
	CustomModels        []json.RawMessage      `json:"customModels"`
	MitmAlias           json.RawMessage        `json:"mitmAlias,omitempty"`
	Pricing             json.RawMessage        `json:"pricing,omitempty"`
}

// nineRouterConnection mirrors a providerConnections row (connectionsRepo.js).
// 9router promotes a handful of SQLite columns and flattens everything else from
// a `data` JSON blob; on the wire all of it appears as sibling fields, so we
// decode the known ones explicitly and keep providerSpecificData as a nested
// object. Runtime-only fields (rateLimitedUntil, modelLock_*, lastError, ...) are
// intentionally NOT modeled — they are reset on import.
type nineRouterConnection struct {
	ID       string `json:"id,omitempty"`
	Provider string `json:"provider"`
	AuthType string `json:"authType,omitempty"` // oauth | apikey | access_token | cookie
	Name     string `json:"name,omitempty"`
	Email    string `json:"email,omitempty"`
	Priority int    `json:"priority,omitempty"`
	IsActive *bool  `json:"isActive,omitempty"`

	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	IDToken      string `json:"idToken,omitempty"`
	ExpiresAt    any    `json:"expiresAt,omitempty"` // ISO-8601 string OR epoch number
	APIKey       string `json:"apiKey,omitempty"`
	DisplayName  string `json:"displayName,omitempty"`
	DefaultModel string `json:"defaultModel,omitempty"`

	ProviderSpecificData map[string]any `json:"providerSpecificData,omitempty"`
}

// nineRouterNode mirrors a providerNodes row (nodesRepo.js): a user-defined
// OpenAI/Anthropic-compatible endpoint.
type nineRouterNode struct {
	ID      string `json:"id"`
	Type    string `json:"type"` // openai-compatible | anthropic-compatible | custom-embedding
	Name    string `json:"name,omitempty"`
	Prefix  string `json:"prefix,omitempty"`
	APIType string `json:"apiType,omitempty"` // chat | responses (openai-compatible)
	BaseURL string `json:"baseUrl,omitempty"`
}

// nineRouterAPIKey mirrors an apiKeys row (apiKeysRepo.js): a client-facing key
// used to authenticate INTO the router (not an upstream provider key).
type nineRouterAPIKey struct {
	ID        string `json:"id,omitempty"`
	Key       string `json:"key"`
	Name      string `json:"name,omitempty"`
	MachineID string `json:"machineId,omitempty"`
	IsActive  *bool  `json:"isActive,omitempty"`
}

// looksLikeNineRouterBackup reports whether a raw JSON body is a 9router logical
// backup envelope, so the shared /admin/api/import endpoint can auto-route it.
// We require the distinctive providerConnections key (Kiro-Go's native export
// uses "accounts"); presence of providerNodes/combos is corroborating.
func looksLikeNineRouterBackup(raw []byte) bool {
	if firstNonSpace(raw) != '{' {
		return false
	}
	var probe struct {
		ProviderConnections json.RawMessage `json:"providerConnections"`
		ProviderNodes       json.RawMessage `json:"providerNodes"`
		Combos              json.RawMessage `json:"combos"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return len(probe.ProviderConnections) > 0 || len(probe.ProviderNodes) > 0 || len(probe.Combos) > 0
}

// nineRouterImportResult is the per-section outcome the dashboard renders.
type nineRouterImportResult struct {
	Success          bool                `json:"success"`
	Providers        int                 `json:"providers"`        // custom ProviderConfigs registered
	Accounts         int                 `json:"accounts"`         // accounts imported
	APIKeys          int                 `json:"apiKeys"`          // inbound client keys imported
	Skipped          int                 `json:"skipped"`          // duplicates / inactive-empty rows skipped
	Failed           int                 `json:"failed"`           // rows that errored
	Results          []importResultEntry `json:"results"`          // per-account detail
	UnsupportedSlugs []string            `json:"unsupportedSlugs"` // provider slugs we couldn't map
}

// importNineRouterBackup applies a 9router logical backup to the Kiro-Go config.
// Order matters: providerNodes register custom ProviderConfigs FIRST so the
// accounts that reference them resolve, then connections become accounts, then
// inbound apiKeys. Every section is best-effort per row; a bad row reports its
// own failure without aborting the batch. The pool is reloaded once at the end.
func (h *Handler) importNineRouterBackup(raw []byte) (nineRouterImportResult, error) {
	var bk nineRouterBackup
	if err := json.Unmarshal(raw, &bk); err != nil {
		return nineRouterImportResult{}, fmt.Errorf("invalid 9router backup JSON: %w", err)
	}

	res := nineRouterImportResult{Results: make([]importResultEntry, 0, len(bk.ProviderConnections))}

	// 1) providerNodes -> custom ProviderConfig registry. nodeIDToBackend maps a
	// 9router node id to the Kiro-Go backend id the referencing connections use.
	nodeIDToBackend := make(map[string]string, len(bk.ProviderNodes))
	for _, n := range bk.ProviderNodes {
		backend, ok := h.registerNineRouterNode(n)
		if !ok {
			continue
		}
		nodeIDToBackend[strings.TrimSpace(n.ID)] = backend
		res.Providers++
	}

	// 2) providerConnections -> accounts.
	existing := buildNineRouterDedupeIndex(config.GetAccounts())
	unsupported := map[string]bool{}
	importedAny := false
	for _, conn := range bk.ProviderConnections {
		entry, status := h.importNineRouterConnection(conn, nodeIDToBackend, existing)
		switch status {
		case "imported":
			res.Accounts++
			importedAny = true
		case "skipped":
			res.Skipped++
		case "unsupported":
			res.Skipped++
			if slug := strings.ToLower(strings.TrimSpace(conn.Provider)); slug != "" {
				unsupported[slug] = true
			}
		default: // failed / invalid
			res.Failed++
		}
		res.Results = append(res.Results, entry)
	}

	// 3) inbound client apiKeys.
	for _, k := range bk.APIKeys {
		if h.importNineRouterAPIKey(k) {
			res.APIKeys++
		} else {
			res.Skipped++
		}
	}

	if importedAny {
		h.pool.Reload()
	}

	for slug := range unsupported {
		res.UnsupportedSlugs = append(res.UnsupportedSlugs, slug)
	}
	res.Success = res.Failed == 0
	return res, nil
}

// registerNineRouterNode turns a providerNodes row into a config.ProviderConfig
// and registers it. Returns the Kiro-Go backend id and true on success. The node
// id is preserved as the backend id so connections referencing it resolve.
func (h *Handler) registerNineRouterNode(n nineRouterNode) (string, bool) {
	id := strings.ToLower(strings.TrimSpace(n.ID))
	base := strings.TrimSpace(n.BaseURL)
	if id == "" || base == "" {
		return "", false
	}
	// SSRF guard parity with apiAddCustomProvider: http(s) only (not file://, etc.).
	if !isValidBaseURLScheme(base) {
		return "", false
	}
	var dialect string
	switch strings.ToLower(strings.TrimSpace(n.Type)) {
	case "anthropic-compatible":
		dialect = "anthropic"
	case "openai-compatible":
		dialect = "openai"
	default:
		// custom-embedding and unknown node types have no inference dialect.
		return "", false
	}
	// Don't shadow a built-in backend id.
	if _, clash := resolveBuiltinProvider(id); clash {
		return "", false
	}
	pc := config.ProviderConfig{
		ID:          id,
		Alias:       strings.ToLower(strings.TrimSpace(n.Prefix)),
		Name:        firstNonEmpty(n.Name, id),
		Dialect:     dialect,
		BaseURL:     base,
		FetchModels: true,
	}
	if err := config.AddProvider(pc); err != nil {
		return "", false
	}
	return id, true
}

// importNineRouterConnection maps one providerConnections row to a Kiro-Go
// account and persists it. Returns (resultEntry, status) where status is one of
// "imported" | "skipped" | "unsupported" | "failed" | "invalid".
func (h *Handler) importNineRouterConnection(
	conn nineRouterConnection,
	nodeIDToBackend map[string]string,
	existing map[string]string,
) (importResultEntry, string) {
	slug := strings.ToLower(strings.TrimSpace(conn.Provider))
	if slug == "" {
		return importResultEntry{Status: "invalid", Reason: "missing provider"}, "invalid"
	}

	// Resolve the Kiro-Go backend. A node-referenced custom provider takes
	// precedence (its id was registered in step 1); otherwise map the slug.
	backend, ok := nodeIDToBackend[slug]
	if !ok {
		backend, ok = nineRouterProviderToBackend(slug)
	}
	if !ok {
		return importResultEntry{Status: "skipped", Reason: "unsupported provider slug: " + slug, Email: conn.Email}, "unsupported"
	}

	name := firstNonEmpty(conn.DisplayName, conn.Name, conn.Email)
	enabled := conn.IsActive == nil || *conn.IsActive

	acct := config.Account{
		ID:       auth.GenerateAccountID(),
		Backend:  backend,
		Email:    conn.Email,
		Nickname: name,
		Weight:   conn.Priority,
		Enabled:  enabled,
	}

	// Credentials by auth type / what's present.
	acct.AccessToken = strings.TrimSpace(conn.AccessToken)
	acct.RefreshToken = strings.TrimSpace(conn.RefreshToken)
	acct.IDToken = strings.TrimSpace(conn.IDToken)
	acct.APIKey = strings.TrimSpace(conn.APIKey)
	if exp := parseFlexibleUnixSeconds(conn.ExpiresAt); exp > 0 {
		acct.ExpiresAt = exp
	}

	// provider-specific data → typed account fields.
	applyNineRouterProviderSpecific(&acct, backend, conn.ProviderSpecificData)

	// Dedupe key per backend: refreshToken, else apiKey, else accessToken, else email.
	dk := nineRouterDedupeKey(acct)
	if dk != "" {
		if existingID, dup := existing[dk]; dup {
			return importResultEntry{Status: "skipped", AccountID: existingID, Reason: "already imported", Email: conn.Email}, "skipped"
		}
	}

	// A row with no usable credential at all is not importable.
	if acct.AccessToken == "" && acct.RefreshToken == "" && acct.APIKey == "" {
		return importResultEntry{Status: "invalid", Reason: "no credentials in connection", Email: conn.Email}, "invalid"
	}

	if err := config.AddAccount(acct); err != nil {
		return importResultEntry{Status: "failed", Reason: "save failed: " + err.Error(), Email: conn.Email}, "failed"
	}
	if dk != "" {
		existing[dk] = acct.ID
	}
	return importResultEntry{Status: "imported", AccountID: acct.ID, Email: conn.Email}, "imported"
}

// importNineRouterAPIKey imports one inbound client key. 9router keys are
// HMAC/machine-derived, so importing the literal value onto a different host
// won't validate under 9router's own scheme — but as a Kiro-Go inbound key the
// stored literal IS the credential, so we persist it verbatim (deduped by value).
func (h *Handler) importNineRouterAPIKey(k nineRouterAPIKey) bool {
	key := strings.TrimSpace(k.Key)
	if key == "" {
		return false
	}
	for _, existing := range config.GetAPIKeys() {
		if existing.Key == key {
			return false
		}
	}
	enabled := k.IsActive == nil || *k.IsActive
	rec := config.APIKey{
		ID:        firstNonEmpty(strings.TrimSpace(k.ID), generateImportedKeyID()),
		Name:      firstNonEmpty(k.Name, "9router import"),
		Key:       key,
		Enabled:   enabled,
		CreatedAt: time.Now().Unix(),
	}
	return config.AddRawAPIKey(rec) == nil
}

// applyNineRouterProviderSpecific copies provider-specific data fields onto the
// typed account columns Kiro-Go uses, per backend.
func applyNineRouterProviderSpecific(acct *config.Account, backend string, psd map[string]any) {
	if psd == nil {
		psd = map[string]any{}
	}
	getStr := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := psd[k]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		return ""
	}
	switch backend {
	case "kiro":
		acct.ProfileArn = firstNonEmpty(acct.ProfileArn, getStr("profileArn"))
		acct.AuthMethod = normalizeAuthMethod(getStr("authMethod"), acct.ClientID, acct.ClientSecret)
		acct.Provider = defaultProvider(getStr("provider", "idp"), acct.AuthMethod)
		if acct.Region == "" {
			acct.Region = firstNonEmpty(getStr("region"), "us-east-1")
		}
		if acct.MachineId == "" {
			acct.MachineId = firstNonEmpty(getStr("machineId"), config.GenerateMachineId())
		}
	case "codex":
		acct.CodexAccountID = firstNonEmpty(acct.CodexAccountID, getStr("chatgptAccountId", "accountId", "workspaceId"))
		acct.CodexPlanType = firstNonEmpty(acct.CodexPlanType, getStr("chatgptPlanType", "planType"))
		// If only an id_token was provided, decode identity from it.
		if acct.IDToken == "" {
			acct.IDToken = getStr("idToken")
		}
		if acct.IDToken != "" && acct.CodexAccountID == "" {
			id, plan, email := auth.DecodeCodexIDToken(acct.IDToken)
			acct.CodexAccountID = firstNonEmpty(acct.CodexAccountID, id)
			acct.CodexPlanType = firstNonEmpty(acct.CodexPlanType, plan)
			acct.Email = firstNonEmpty(acct.Email, email)
		}
	case "qoder":
		acct.QoderUserID = firstNonEmpty(acct.QoderUserID, getStr("userId", "uid"))
		acct.QoderMachineID = firstNonEmpty(acct.QoderMachineID, getStr("machineId"), config.GenerateMachineId())
	default:
		// Custom/api-key providers: a baseUrl in providerSpecificData becomes a
		// per-account override (the ProviderConfig already carries the default).
		if bu := getStr("baseUrl", "baseURL"); bu != "" && isValidBaseURLScheme(bu) {
			acct.BaseURLOverride = bu
		}
	}
}

// nineRouterDedupeKey returns the per-backend identity used to skip duplicate
// imports. Prefers the most stable credential available.
func nineRouterDedupeKey(a config.Account) string {
	switch {
	case a.RefreshToken != "":
		return a.Backend + "|rt|" + a.RefreshToken
	case a.APIKey != "":
		return a.Backend + "|ak|" + a.APIKey
	case a.AccessToken != "":
		return a.Backend + "|at|" + a.AccessToken
	case a.Email != "":
		return a.Backend + "|em|" + strings.ToLower(a.Email)
	}
	return ""
}

// buildNineRouterDedupeIndex builds the dedupe map from existing accounts so a
// re-import is idempotent across all backends (not just Kiro refresh tokens).
func buildNineRouterDedupeIndex(accounts []config.Account) map[string]string {
	idx := make(map[string]string, len(accounts)*2)
	for _, a := range accounts {
		if k := nineRouterDedupeKey(a); k != "" {
			idx[k] = a.ID
		}
	}
	return idx
}

// ---- slug <-> backend mapping ---------------------------------------------

// nineRouterProviderToBackend maps a 9router provider slug to a Kiro-Go backend
// id. Bespoke backends (kiro/codex/qoder) and the bundled api-key providers map
// by name; a handful of 9router aliases are normalised. openai-/anthropic-
// compatible-* node slugs are handled by the caller via the node id map, so they
// are NOT resolved here. Returns (backend, ok).
func nineRouterProviderToBackend(slug string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(slug))
	switch s {
	case "kiro", "kiro-import":
		return "kiro", true
	case "codex", "codex-import", "chatgpt":
		return "codex", true
	case "qoder":
		return "qoder", true
	case "claude", "anthropic":
		return "anthropic", true
	case "google", "gemini", "gemini-cli":
		return "gemini", true
	}
	// openai-/anthropic-compatible nodes are resolved by id upstream; if one slips
	// through without a matching node, we can't reconstruct its base URL.
	if strings.HasPrefix(s, "openai-compatible") || strings.HasPrefix(s, "anthropic-compatible") {
		return "", false
	}
	// Direct match against the built-in catalog id or alias.
	if _, ok := resolveBuiltinProvider(s); ok {
		return s, true
	}
	if id, ok := builtinByAlias[s]; ok {
		return id, true
	}
	// A previously-registered custom provider id.
	if _, ok := config.GetProviderConfig(s); ok {
		return s, true
	}
	return "", false
}

// backendToNineRouter maps a Kiro-Go backend id to the 9router provider slug used
// on export. Custom provider ids pass through unchanged (their providerNode is
// emitted alongside). Returns the slug; never empty for a non-empty backend.
func backendToNineRouter(backend string) string {
	b := strings.ToLower(strings.TrimSpace(backend))
	switch b {
	case "", "kiro":
		return "kiro"
	case "codex":
		return "codex"
	case "qoder":
		return "qoder"
	case "anthropic":
		return "claude"
	case "gemini":
		return "google"
	}
	return b
}

// ---- export ---------------------------------------------------------------

// exportNineRouterBackup renders the current Kiro-Go config as a 9router logical
// backup. Every top-level key 9router's importDb() reads is emitted (empty when
// we have no equivalent) for maximum compatibility, plus our own schemaVersion/
// exportedBy markers (ignored by 9router). Only the given account IDs are
// exported when ids is non-empty.
func (h *Handler) exportNineRouterBackup(ids []string) nineRouterBackup {
	idSet := map[string]bool{}
	for _, id := range ids {
		idSet[id] = true
	}

	accounts := config.GetAccounts()
	providers := config.GetProviders()

	// Index custom providers so each exported connection for a custom backend can
	// emit its providerNode exactly once.
	customByID := map[string]config.ProviderConfig{}
	for _, p := range providers {
		customByID[strings.ToLower(strings.TrimSpace(p.ID))] = p
	}

	bk := nineRouterBackup{
		SchemaVersion:       nineRouterSchemaVersion,
		ExportedBy:          "kiro-go",
		ExportedAt:          time.Now().UTC().Format(time.RFC3339),
		ProviderConnections: []nineRouterConnection{},
		ProviderNodes:       []nineRouterNode{},
		ProxyPools:          []json.RawMessage{},
		APIKeys:             []nineRouterAPIKey{},
		Combos:              []json.RawMessage{},
		CustomModels:        []json.RawMessage{},
		Settings:            json.RawMessage(`{"comboStrategy":"fallback","rtkEnabled":true}`),
		ModelAliases:        json.RawMessage(`{}`),
		MitmAlias:           json.RawMessage(`{}`),
		Pricing:             json.RawMessage(`{}`),
	}

	emittedNodes := map[string]bool{}
	for _, a := range accounts {
		if len(idSet) > 0 && !idSet[a.ID] {
			continue
		}
		backend := config.GetAccountBackend(&a)
		conn := h.accountToNineRouterConnection(a, backend, customByID)
		bk.ProviderConnections = append(bk.ProviderConnections, conn)

		// Emit a providerNode for a custom backend the first time we see it.
		if pc, isCustom := customByID[backend]; isCustom && !emittedNodes[backend] {
			emittedNodes[backend] = true
			bk.ProviderNodes = append(bk.ProviderNodes, providerConfigToNineRouterNode(pc))
		}
	}

	// Inbound client keys.
	for _, k := range config.GetAPIKeys() {
		active := k.Enabled
		bk.APIKeys = append(bk.APIKeys, nineRouterAPIKey{
			ID:       k.ID,
			Key:      k.Key,
			Name:     k.Name,
			IsActive: &active,
		})
	}

	return bk
}

// accountToNineRouterConnection renders one account as a providerConnections row.
func (h *Handler) accountToNineRouterConnection(a config.Account, backend string, customByID map[string]config.ProviderConfig) nineRouterConnection {
	active := a.Enabled
	conn := nineRouterConnection{
		ID:                   firstNonEmpty(a.ID, auth.GenerateAccountID()),
		Provider:             backendToNineRouter(backend),
		Name:                 firstNonEmpty(a.Nickname, a.Email),
		Email:                a.Email,
		Priority:             a.Weight,
		IsActive:             &active,
		AccessToken:          a.AccessToken,
		RefreshToken:         a.RefreshToken,
		IDToken:              a.IDToken,
		APIKey:               a.APIKey,
		DisplayName:          a.Nickname,
		ProviderSpecificData: map[string]any{},
	}
	if a.ExpiresAt > 0 {
		conn.ExpiresAt = time.Unix(a.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}

	// authType: oauth when a refresh token exists, access_token when only an
	// access token, apikey for key-based providers.
	switch {
	case a.APIKey != "":
		conn.AuthType = "apikey"
	case a.RefreshToken != "":
		conn.AuthType = "oauth"
	case a.AccessToken != "":
		conn.AuthType = "access_token"
	}

	// If this is a custom backend, the connection.provider must be the node id so
	// 9router resolves it against the emitted providerNode.
	if pc, ok := customByID[backend]; ok {
		conn.Provider = pc.ID
		conn.ProviderSpecificData["baseUrl"] = firstNonEmpty(a.BaseURLOverride, pc.BaseURL)
	}

	switch backend {
	case "kiro":
		if a.ProfileArn != "" {
			conn.ProviderSpecificData["profileArn"] = a.ProfileArn
		}
		if a.AuthMethod != "" {
			conn.ProviderSpecificData["authMethod"] = a.AuthMethod
		}
		if a.Provider != "" {
			conn.ProviderSpecificData["provider"] = a.Provider
		}
		if a.Region != "" {
			conn.ProviderSpecificData["region"] = a.Region
		}
	case "codex":
		if a.CodexAccountID != "" {
			conn.ProviderSpecificData["chatgptAccountId"] = a.CodexAccountID
		}
		if a.CodexPlanType != "" {
			conn.ProviderSpecificData["chatgptPlanType"] = a.CodexPlanType
		}
	case "qoder":
		if a.QoderUserID != "" {
			conn.ProviderSpecificData["userId"] = a.QoderUserID
		}
		if a.QoderMachineID != "" {
			conn.ProviderSpecificData["machineId"] = a.QoderMachineID
		}
	}
	if len(conn.ProviderSpecificData) == 0 {
		conn.ProviderSpecificData = nil
	}
	return conn
}

// providerConfigToNineRouterNode renders a custom ProviderConfig as a
// providerNodes row.
func providerConfigToNineRouterNode(pc config.ProviderConfig) nineRouterNode {
	nodeType := "openai-compatible"
	apiType := "chat"
	if strings.EqualFold(pc.Dialect, "anthropic") {
		nodeType = "anthropic-compatible"
		apiType = ""
	}
	return nineRouterNode{
		ID:      pc.ID,
		Type:    nodeType,
		Name:    firstNonEmpty(pc.Name, pc.ID),
		Prefix:  firstNonEmpty(pc.Alias, pc.ID),
		APIType: apiType,
		BaseURL: pc.BaseURL,
	}
}

// ---- helpers ---------------------------------------------------------------

// parseFlexibleUnixSeconds accepts the several shapes 9router/Kiro emit for an
// expiry: an RFC3339 string, a numeric string (epoch seconds or ms), a JSON
// number (epoch seconds or ms), and returns Unix SECONDS (0 if unparseable).
func parseFlexibleUnixSeconds(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case float64:
		return normalizeEpoch(int64(t))
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return normalizeEpoch(n)
		}
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		if isAllDigitsStr(s) {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return normalizeEpoch(n)
			}
		}
		for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04:05.000Z"} {
			if parsed, err := time.Parse(layout, s); err == nil {
				return parsed.Unix()
			}
		}
	}
	return 0
}

// normalizeEpoch converts a possibly-millisecond epoch to seconds. Anything past
// ~year 33658 in seconds (1e12) is treated as milliseconds.
func normalizeEpoch(n int64) int64 {
	if n <= 0 {
		return 0
	}
	if n > 1_000_000_000_000 { // > 1e12 → milliseconds
		return n / 1000
	}
	return n
}

func isAllDigitsStr(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// generateImportedKeyID returns a non-empty id for an imported inbound key when
// the source omitted one.
func generateImportedKeyID() string {
	return "imp-" + auth.GenerateAccountID()[:12]
}

// ---- admin handlers --------------------------------------------------------

// apiImportNineRouter POST /admin/api/import/9router — explicit endpoint for a
// 9router logical backup. (The shared /admin/api/import also auto-detects this
// shape; this route is for callers that want to be unambiguous.)
func (h *Handler) apiImportNineRouter(w http.ResponseWriter, r *http.Request) {
	limited := http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	defer limited.Close()
	raw, err := io.ReadAll(limited)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "read body: " + err.Error()})
		return
	}
	raw = trimUTF8BOM(raw)
	if !looksLikeNineRouterBackup(raw) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "not a 9router backup (missing providerConnections/providerNodes/combos)"})
		return
	}
	res, ierr := h.importNineRouterBackup(raw)
	w.Header().Set("Content-Type", "application/json")
	if ierr != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": ierr.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(res)
}

// apiExportNineRouter POST /admin/api/export/9router — emit the current config as
// a 9router logical backup. Optional body {"ids":[...]} restricts to those
// account IDs. The download contains plaintext upstream credentials (matching
// 9router's own export), so it is gated behind the admin auth like every other
// /admin/api route.
func (h *Handler) apiExportNineRouter(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // empty/invalid body => export all
	}
	bk := h.exportNineRouterBackup(req.IDs)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\"9router-backup.json\"")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(bk)
}

// trimUTF8BOM strips a leading UTF-8 BOM if present (files saved by some Windows
// editors carry one and would break json.Unmarshal).
func trimUTF8BOM(raw []byte) []byte {
	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		return raw[3:]
	}
	return raw
}
