package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
)

// importRequest is the wire shape POST /admin/api/import accepts. It tolerates
// three input forms that real users paste in:
//
//  1. The native Kiro-Go export (apiExportAccounts) — top-level
//     {"version", "exportedAt", "accounts":[{"credentials":{...}, ...}]}.
//  2. A raw array of credential objects.
//  3. A single credential object.
//
// The handler normalises (1) and (3) into (2) before processing, so the rest
// of the pipeline only deals with []importCredential.
//
// IDs from the source file are deliberately ignored. We always allocate a new
// account ID so re-importing on a different host can't collide with locally
// minted IDs and tests can run against the same fixture without state leak.
type importRequest struct {
	Version    string                  `json:"version,omitempty"`
	ExportedAt int64                   `json:"exportedAt,omitempty"`
	Accounts   []importExportedAccount `json:"accounts,omitempty"`
}

// importExportedAccount mirrors the export shape from apiExportAccounts. Only
// the fields we actually consume on import are listed; unknown keys are
// dropped silently so future export-side additions don't break old proxies.
type importExportedAccount struct {
	Email       string                    `json:"email,omitempty"`
	Nickname    string                    `json:"nickname,omitempty"`
	UserId      string                    `json:"userId,omitempty"`
	MachineId   string                    `json:"machineId,omitempty"`
	Idp         string                    `json:"idp,omitempty"`
	Credentials importExportedCredentials `json:"credentials,omitempty"`
}

type importExportedCredentials struct {
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
	Region       string `json:"region,omitempty"`
	AuthMethod   string `json:"authMethod,omitempty"`
	Provider     string `json:"provider,omitempty"`
}

// importCredential is the post-normalisation flat shape every code path below
// works with. Form (2) and (3) of the request body decode straight into this.
type importCredential struct {
	RefreshToken string `json:"refreshToken"`
	AccessToken  string `json:"accessToken,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
	Region       string `json:"region,omitempty"`
	AuthMethod   string `json:"authMethod,omitempty"`
	Provider     string `json:"provider,omitempty"`

	// Optional metadata the caller may include from an export. We only persist
	// these when we actually create a new account; on skip the existing
	// account is left untouched (per the agreed merge strategy).
	Email     string `json:"email,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	UserId    string `json:"userId,omitempty"`
	MachineId string `json:"machineId,omitempty"`
}

// importResultEntry is one row in the import response. The four statuses keep
// the UI side simple: anything other than "imported" is a no-op on disk, and
// "imported" carries the freshly-allocated account ID so the front-end can
// follow up with autoRefreshNewAccount(id).
type importResultEntry struct {
	Status    string `json:"status"` // "imported" | "skipped" | "failed" | "invalid"
	AccountID string `json:"accountId,omitempty"`
	Email     string `json:"email,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type importResponse struct {
	Success  bool                `json:"success"`
	Imported int                 `json:"imported"`
	Skipped  int                 `json:"skipped"`
	Failed   int                 `json:"failed"`
	Results  []importResultEntry `json:"results"`
}

// apiImportAccounts is POST /admin/api/import. It accepts the three input
// forms documented on importRequest and returns a per-row result table.
//
// Merge strategy (locked-in by spec):
//   - skip when an existing account already has the same refreshToken;
//   - never modify the existing account on skip (no metadata refresh);
//   - always re-derive the accessToken via auth.RefreshToken so a new account
//     starts with a verified-working token instead of trusting whatever was
//     in the file.
//
// Failures on individual rows do NOT abort the batch — each row reports its
// own outcome and Save() is called per successful AddAccount, so a midway
// crash leaves the previously-imported rows persisted. This matches the
// pattern apiImportSsoToken / the existing UI loop already use.
func (h *Handler) apiImportAccounts(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	defer body.Close()

	creds, err := decodeImportBody(body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if len(creds) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no accounts in request"})
		return
	}

	// Snapshot existing refreshTokens once so a 100-account import is O(N+M)
	// instead of O(N×M) GetAccounts() calls.
	existing := buildRefreshTokenIndex(config.GetAccounts())

	resp := importResponse{Results: make([]importResultEntry, 0, len(creds))}
	importedAny := false

	for _, c := range creds {
		entry, created := h.importOneAccount(c, existing, auth.RefreshToken, auth.GetUserInfo)
		switch entry.Status {
		case "imported":
			resp.Imported++
			importedAny = true
			if created != "" {
				existing[c.RefreshToken] = created
			}
		case "skipped":
			resp.Skipped++
		default:
			resp.Failed++
		}
		resp.Results = append(resp.Results, entry)
	}

	if importedAny {
		// Single Reload at the end so the SWRR scheduler sees the full new
		// set rather than rebuilding N times mid-loop.
		h.pool.Reload()
	}

	resp.Success = resp.Failed == 0
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// decodeImportBody accepts the three documented body shapes and produces a
// flat []importCredential. Unknown fields are silently ignored.
func decodeImportBody(body io.Reader) ([]importCredential, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty request body")
	}

	// Try array form first — `[{"refreshToken":"..."}, ...]`.
	if first := firstNonSpace(raw); first == '[' {
		var arr []importCredential
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, fmt.Errorf("invalid JSON array: %w", err)
		}
		return arr, nil
	}

	// Object form: either the export envelope (has "accounts") or a single
	// credential. Decode into a permissive struct that carries both shapes
	// and disambiguate after.
	var probe struct {
		Accounts     []importExportedAccount `json:"accounts"`
		RefreshToken string                  `json:"refreshToken"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	if len(probe.Accounts) > 0 {
		out := make([]importCredential, 0, len(probe.Accounts))
		for _, a := range probe.Accounts {
			out = append(out, importCredential{
				RefreshToken: a.Credentials.RefreshToken,
				AccessToken:  a.Credentials.AccessToken,
				ClientID:     a.Credentials.ClientID,
				ClientSecret: a.Credentials.ClientSecret,
				Region:       a.Credentials.Region,
				AuthMethod:   a.Credentials.AuthMethod,
				Provider:     mergeProvider(a.Credentials.Provider, a.Idp),
				Email:        a.Email,
				Nickname:     a.Nickname,
				UserId:       a.UserId,
				MachineId:    a.MachineId,
			})
		}
		return out, nil
	}

	// Single credential object (refreshToken at the top level).
	if probe.RefreshToken == "" {
		return nil, fmt.Errorf("expected accounts array, credentials array, or single credentials object")
	}
	var single importCredential
	if err := json.Unmarshal(raw, &single); err != nil {
		return nil, fmt.Errorf("invalid credentials object: %w", err)
	}
	return []importCredential{single}, nil
}

// refreshTokenFunc is the signature of auth.RefreshToken, factored out so
// tests can stub the network call without spinning up a fake AWS endpoint.
// Returns: accessToken, newRefreshToken, expiresAt, profileArn, error.
type refreshTokenFunc func(account *config.Account) (string, string, int64, string, error)

// userInfoFunc is the signature of auth.GetUserInfo, factored out for the
// same reason. Returns email, userId, error; the second return is unused
// here but kept so the stub matches the real signature exactly.
type userInfoFunc func(accessToken string) (string, string, error)

// importOneAccount handles dedupe + token refresh + persistence for a single
// row. Returns the user-visible result entry and the freshly-minted account
// ID (empty when no row was created).
//
// refresh and getUserInfo are injected so tests can run without making real
// network calls. In production both come from the auth package.
func (h *Handler) importOneAccount(
	c importCredential,
	existing map[string]string,
	refresh refreshTokenFunc,
	getUserInfo userInfoFunc,
) (importResultEntry, string) {
	rt := strings.TrimSpace(c.RefreshToken)
	if rt == "" {
		return importResultEntry{Status: "invalid", Reason: "missing refreshToken"}, ""
	}

	if existingID, ok := existing[rt]; ok {
		return importResultEntry{
			Status:    "skipped",
			AccountID: existingID,
			Reason:    "refreshToken already imported",
		}, ""
	}

	authMethod := normalizeAuthMethod(c.AuthMethod, c.ClientID, c.ClientSecret)
	provider := defaultProvider(c.Provider, authMethod)
	region := c.Region
	if region == "" {
		region = "us-east-1"
	}

	temp := &config.Account{
		RefreshToken: rt,
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		AuthMethod:   authMethod,
		Region:       region,
	}
	accessToken, newRT, expiresAt, profileArn, err := refresh(temp)
	if err != nil {
		return importResultEntry{
			Status: "failed",
			Reason: "token refresh failed: " + err.Error(),
		}, ""
	}
	if newRT != "" {
		rt = newRT
	}

	// Best-effort enrich with the email; failure here doesn't fail the row,
	// since the account is otherwise functional.
	email := strings.TrimSpace(c.Email)
	if email == "" && getUserInfo != nil {
		if got, _, gerr := getUserInfo(accessToken); gerr == nil {
			email = got
		}
	}

	machineID := strings.TrimSpace(c.MachineId)
	if machineID == "" {
		machineID = config.GenerateMachineId()
	}

	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		Nickname:     c.Nickname,
		UserId:       c.UserId,
		AccessToken:  accessToken,
		RefreshToken: rt,
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		AuthMethod:   authMethod,
		Provider:     provider,
		Region:       region,
		ExpiresAt:    expiresAt,
		Enabled:      true,
		MachineId:    machineID,
		ProfileArn:   profileArn,
	}

	if err := config.AddAccount(account); err != nil {
		return importResultEntry{
			Status: "failed",
			Reason: "save failed: " + err.Error(),
		}, ""
	}

	logger.Infof("import: added account id=%s email=%s authMethod=%s region=%s",
		account.ID, redactForLog(account.Email), account.AuthMethod, account.Region)

	return importResultEntry{
		Status:    "imported",
		AccountID: account.ID,
		Email:     email,
	}, account.ID
}

// buildRefreshTokenIndex maps refreshToken → accountID for fast O(1) dedupe.
// We trim whitespace to match what importOneAccount checks against; any other
// canonicalisation (case, base64 normalisation) would silently merge tokens
// that AWS treats as distinct, so we deliberately do not normalise further.
func buildRefreshTokenIndex(accounts []config.Account) map[string]string {
	idx := make(map[string]string, len(accounts))
	for _, a := range accounts {
		rt := strings.TrimSpace(a.RefreshToken)
		if rt == "" {
			continue
		}
		idx[rt] = a.ID
	}
	return idx
}

// normalizeAuthMethod folds the half-dozen casings users paste into the two
// values the rest of the codebase recognises ("idc" or "social"). When the
// caller didn't supply one, presence of clientId+clientSecret is a strong
// signal for IdC OIDC.
func normalizeAuthMethod(raw, clientID, clientSecret string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "idc", "builderid", "enterprise":
		return "idc"
	case "social", "google", "github":
		return "social"
	}
	if clientID != "" && clientSecret != "" {
		return "idc"
	}
	return "social"
}

// defaultProvider keeps Provider populated even when the file omitted it.
// Display strings ("BuilderId", "Google") match what the dashboard expects.
func defaultProvider(raw, authMethod string) string {
	if v := strings.TrimSpace(raw); v != "" {
		return v
	}
	if authMethod == "idc" {
		return "BuilderId"
	}
	return "Google"
}

// mergeProvider prefers the credentials.provider field but falls back to the
// account-level idp when the credentials block didn't carry one. Both fields
// are present in the native export shape.
func mergeProvider(credProvider, idp string) string {
	if v := strings.TrimSpace(credProvider); v != "" {
		return v
	}
	return strings.TrimSpace(idp)
}

// firstNonSpace returns the first non-whitespace byte of raw, or 0 if raw is
// empty / all whitespace. Used to disambiguate JSON array vs object without
// allocating a full scanner.
func firstNonSpace(raw []byte) byte {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return b
		}
	}
	return 0
}

// redactForLog returns the email with its local-part masked. We log the
// domain because that's the operator-facing signal ("which IdC tenant did I
// just import?") without leaking the user identifier verbatim.
func redactForLog(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 0 {
		return "***"
	}
	return "***" + email[at:]
}
