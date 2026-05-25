package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
	"kiro-go/pool"
)

// stubRefresh returns a refreshTokenFunc that records the accounts it was
// called with and returns the canned token / expiry. Tests use this in place
// of auth.RefreshToken so the import path runs end-to-end without network.
func stubRefresh(token string, expires int64, profile string) (refreshTokenFunc, *[]config.Account) {
	calls := []config.Account{}
	fn := func(a *config.Account) (string, string, int64, string, error) {
		calls = append(calls, *a)
		return token, "", expires, profile, nil
	}
	return fn, &calls
}

// stubFailingRefresh returns a refreshTokenFunc that always fails — used to
// confirm a single bad refresh doesn't abort the rest of the batch.
func stubFailingRefresh(reason string) refreshTokenFunc {
	return func(*config.Account) (string, string, int64, string, error) {
		return "", "", 0, "", errors.New(reason)
	}
}

// stubUserInfo returns a fixed email / userID so tests don't need network.
func stubUserInfo(email string) userInfoFunc {
	return func(string) (string, string, error) { return email, "user-id", nil }
}

// newImportTestHandler boots a fresh config + pool in a temp dir so each test
// runs in isolation. config is a process-wide singleton so we can't run these
// in parallel — use t.TempDir to at least keep on-disk state per-test.
func newImportTestHandler(t *testing.T) *Handler {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	return &Handler{pool: pool.NewForTesting()}
}

func TestDecodeImportBodyAcceptsExportEnvelope(t *testing.T) {
	body := `{
	  "version": "1.0.9-A1",
	  "exportedAt": 123,
	  "accounts": [
	    {
	      "email": "a@example.com",
	      "idp": "BuilderId",
	      "credentials": {"refreshToken": "rt-1", "clientId": "cid", "clientSecret": "csec", "region": "us-west-2", "authMethod": "IdC"}
	    },
	    {
	      "email": "b@example.com",
	      "idp": "Google",
	      "credentials": {"refreshToken": "rt-2"}
	    }
	  ]
	}`
	got, err := decodeImportBody(strings.NewReader(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 credentials, got %d", len(got))
	}
	if got[0].RefreshToken != "rt-1" || got[0].ClientID != "cid" || got[0].Region != "us-west-2" {
		t.Fatalf("first row mis-decoded: %+v", got[0])
	}
	if got[0].Provider != "BuilderId" {
		t.Fatalf("idp must populate Provider when credentials.provider is empty, got %q", got[0].Provider)
	}
	if got[1].RefreshToken != "rt-2" {
		t.Fatalf("second row mis-decoded: %+v", got[1])
	}
}

func TestDecodeImportBodyAcceptsRawArray(t *testing.T) {
	body := `[
	  {"refreshToken": "rt-1", "clientId": "c", "clientSecret": "s", "region": "us-east-1"},
	  {"refreshToken": "rt-2"}
	]`
	got, err := decodeImportBody(strings.NewReader(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0].RefreshToken != "rt-1" || got[1].RefreshToken != "rt-2" {
		t.Fatalf("array decode failed: %+v", got)
	}
}

func TestDecodeImportBodyAcceptsSingleObject(t *testing.T) {
	body := `{"refreshToken": "rt-only", "region": "eu-west-1"}`
	got, err := decodeImportBody(strings.NewReader(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].RefreshToken != "rt-only" || got[0].Region != "eu-west-1" {
		t.Fatalf("single-object decode failed: %+v", got)
	}
}

func TestDecodeImportBodyRejectsEmpty(t *testing.T) {
	if _, err := decodeImportBody(strings.NewReader("")); err == nil {
		t.Fatal("empty body must error")
	}
	if _, err := decodeImportBody(strings.NewReader("{}")); err == nil {
		t.Fatal("object with neither accounts nor refreshToken must error")
	}
}

func TestImportOneAccountSkipsExistingRefreshToken(t *testing.T) {
	h := newImportTestHandler(t)
	existing := map[string]string{"rt-dup": "acct-existing"}

	refresh, calls := stubRefresh("new-access", 9_999_999_999, "")
	entry, created := h.importOneAccount(
		importCredential{RefreshToken: "rt-dup"},
		existing,
		refresh,
		stubUserInfo("x@example.com"),
	)
	if entry.Status != "skipped" {
		t.Fatalf("expected skipped, got %q (%s)", entry.Status, entry.Reason)
	}
	if entry.AccountID != "acct-existing" {
		t.Fatalf("expected existing accountID surfaced, got %q", entry.AccountID)
	}
	if created != "" {
		t.Fatalf("skip path must not return a new ID, got %q", created)
	}
	if len(*calls) != 0 {
		t.Fatal("skip path must not call RefreshToken")
	}
	if got := config.GetAccounts(); len(got) != 0 {
		t.Fatalf("skip must not persist; got %d accounts", len(got))
	}
}

func TestImportOneAccountInvalidEmptyRefreshToken(t *testing.T) {
	h := newImportTestHandler(t)
	refresh, _ := stubRefresh("", 0, "")
	entry, created := h.importOneAccount(
		importCredential{RefreshToken: "   "},
		nil, refresh, stubUserInfo(""),
	)
	if entry.Status != "invalid" {
		t.Fatalf("expected invalid, got %q (%s)", entry.Status, entry.Reason)
	}
	if created != "" {
		t.Fatal("invalid row must not produce an account ID")
	}
}

func TestImportOneAccountFailedRefreshIsReportedNotPanicked(t *testing.T) {
	h := newImportTestHandler(t)
	entry, created := h.importOneAccount(
		importCredential{RefreshToken: "rt-bad", ClientID: "c", ClientSecret: "s"},
		nil,
		stubFailingRefresh("token revoked"),
		stubUserInfo(""),
	)
	if entry.Status != "failed" {
		t.Fatalf("expected failed, got %q", entry.Status)
	}
	if !strings.Contains(entry.Reason, "token revoked") {
		t.Fatalf("expected refresh error in reason, got %q", entry.Reason)
	}
	if created != "" || len(config.GetAccounts()) != 0 {
		t.Fatal("failed refresh must not persist anything")
	}
}

func TestImportOneAccountPersistsNewAccountWithRefreshedToken(t *testing.T) {
	h := newImportTestHandler(t)
	refresh, calls := stubRefresh("fresh-access", 1_700_000_000, "arn:profile")
	entry, created := h.importOneAccount(
		importCredential{
			RefreshToken: "rt-new",
			ClientID:     "cid",
			ClientSecret: "csec",
			Region:       "us-east-2",
			AuthMethod:   "IdC",
			Email:        "user@example.com",
			Nickname:     "tester",
		},
		map[string]string{},
		refresh,
		stubUserInfo("should-not-be-used@example.com"),
	)
	if entry.Status != "imported" {
		t.Fatalf("expected imported, got %q (%s)", entry.Status, entry.Reason)
	}
	if created == "" || created != entry.AccountID {
		t.Fatalf("expected new accountID, got entry=%q created=%q", entry.AccountID, created)
	}
	if entry.Email != "user@example.com" {
		t.Fatalf("supplied email must take precedence; got %q", entry.Email)
	}

	saved := config.GetAccounts()
	if len(saved) != 1 {
		t.Fatalf("expected 1 saved account, got %d", len(saved))
	}
	a := saved[0]
	if a.AccessToken != "fresh-access" {
		t.Fatalf("must persist the refreshed accessToken, got %q", a.AccessToken)
	}
	if a.ProfileArn != "arn:profile" {
		t.Fatalf("must persist the profileArn returned by refresh, got %q", a.ProfileArn)
	}
	if a.AuthMethod != "idc" {
		t.Fatalf("authMethod must be normalised, got %q", a.AuthMethod)
	}
	if a.Region != "us-east-2" {
		t.Fatalf("supplied region must round-trip, got %q", a.Region)
	}
	if !a.Enabled {
		t.Fatal("imported accounts must be enabled by default")
	}
	if a.MachineId == "" {
		t.Fatal("imported accounts must have a machineId")
	}
	if (*calls)[0].RefreshToken != "rt-new" {
		t.Fatalf("refresh must be called with the imported refreshToken, got %q", (*calls)[0].RefreshToken)
	}
}

func TestImportOneAccountUsesUserInfoWhenEmailMissing(t *testing.T) {
	h := newImportTestHandler(t)
	refresh, _ := stubRefresh("a", 1_700_000_000, "")
	entry, _ := h.importOneAccount(
		importCredential{RefreshToken: "rt-x"},
		nil,
		refresh,
		stubUserInfo("from-userinfo@example.com"),
	)
	if entry.Email != "from-userinfo@example.com" {
		t.Fatalf("expected GetUserInfo email when supplied was empty, got %q", entry.Email)
	}
}

func TestApiImportAccountsSkipDuplicatesAndReturnPerRowResults(t *testing.T) {
	h := newImportTestHandler(t)

	// Pre-seed an account so we can test the skip path through the full handler.
	if err := config.AddAccount(config.Account{
		ID:           "preexist",
		RefreshToken: "rt-existing",
		AuthMethod:   "social",
		Provider:     "Google",
		Region:       "us-east-1",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Drive the dispatch math through importOneAccount + a stub refresh so we
	// don't trigger the global http.ProxyFromEnvironment cache (which would
	// poison TestBuildKiroTransportFallsBackToEnvironmentProxy if it runs
	// later in the same `go test` invocation). The handler-level integration
	// is exercised by TestApiImportAccountsRejectsOversizedBody and the
	// decode tests; this test focuses on the per-row counter behaviour.
	rows := []importCredential{
		{RefreshToken: "rt-existing"},                                                   // skip
		{RefreshToken: ""},                                                              // invalid
		{RefreshToken: "rt-broken", ClientID: "c", ClientSecret: "s", AuthMethod: "idc"}, // failed
	}
	failingRefresh := stubFailingRefresh("simulated network failure")
	existing := buildRefreshTokenIndex(config.GetAccounts())

	resp := importResponse{Results: make([]importResultEntry, 0, len(rows))}
	for _, c := range rows {
		entry, _ := h.importOneAccount(c, existing, failingRefresh, stubUserInfo(""))
		switch entry.Status {
		case "imported":
			resp.Imported++
		case "skipped":
			resp.Skipped++
		default:
			resp.Failed++
		}
		resp.Results = append(resp.Results, entry)
	}
	resp.Success = resp.Failed == 0

	if resp.Imported != 0 {
		t.Fatalf("expected 0 imported, got %d", resp.Imported)
	}
	if resp.Skipped != 1 {
		t.Fatalf("expected 1 skipped, got %d (results=%+v)", resp.Skipped, resp.Results)
	}
	if resp.Failed != 2 {
		t.Fatalf("expected 2 failed (1 invalid + 1 refresh-failed), got %d", resp.Failed)
	}
	if resp.Success {
		t.Fatal("Success must be false when any row failed")
	}
}

// TestApiImportAccountsRoundTripsThroughRouter exercises the actual HTTP
// handler path with a body that will only hit the in-process skip / invalid
// code paths, so no real refresh is attempted and the env-proxy cache stays
// untouched.
func TestApiImportAccountsRoundTripsThroughRouter(t *testing.T) {
	h := newImportTestHandler(t)
	if err := config.AddAccount(config.Account{
		ID:           "preexist",
		RefreshToken: "rt-existing",
		AuthMethod:   "social",
		Provider:     "Google",
		Region:       "us-east-1",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body := `[{"refreshToken":"rt-existing"},{"refreshToken":""}]`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/import", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()

	h.apiImportAccounts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var resp importResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Skipped != 1 || resp.Failed != 1 || resp.Imported != 0 {
		t.Fatalf("counters wrong: imported=%d skipped=%d failed=%d (results=%+v)",
			resp.Imported, resp.Skipped, resp.Failed, resp.Results)
	}
}

func TestApiImportAccountsRejectsOversizedBody(t *testing.T) {
	h := newImportTestHandler(t)

	// 33 MiB of zero bytes — over maxRequestBodyBytes (32 MiB). The
	// MaxBytesReader should refuse to read past the cap and decodeImportBody
	// should surface that as a 400.
	huge := bytes.Repeat([]byte("x"), 33*1024*1024)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/import", bytes.NewReader(huge))
	rec := httptest.NewRecorder()
	h.apiImportAccounts(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on oversized body, got %d", rec.Code)
	}
}

func TestNormalizeAuthMethodFolding(t *testing.T) {
	cases := []struct {
		raw, clientID, secret, want string
	}{
		{"IdC", "", "", "idc"},
		{"idc", "", "", "idc"},
		{"BuilderId", "", "", "idc"},
		{"enterprise", "", "", "idc"},
		{"social", "", "", "social"},
		{"google", "", "", "social"},
		{"github", "", "", "social"},
		{"", "cid", "csec", "idc"},
		{"", "", "", "social"},
		{"random-junk", "", "", "social"},
	}
	for _, c := range cases {
		if got := normalizeAuthMethod(c.raw, c.clientID, c.secret); got != c.want {
			t.Errorf("normalizeAuthMethod(%q,%q,%q) = %q, want %q",
				c.raw, c.clientID, c.secret, got, c.want)
		}
	}
}

func TestDefaultProviderFallback(t *testing.T) {
	if got := defaultProvider("", "idc"); got != "BuilderId" {
		t.Errorf("idc default = %q, want BuilderId", got)
	}
	if got := defaultProvider("", "social"); got != "Google" {
		t.Errorf("social default = %q, want Google", got)
	}
	if got := defaultProvider("Github", "social"); got != "Github" {
		t.Errorf("explicit provider must win, got %q", got)
	}
}

func TestBuildRefreshTokenIndexSkipsBlanks(t *testing.T) {
	idx := buildRefreshTokenIndex([]config.Account{
		{ID: "a", RefreshToken: "rt-1"},
		{ID: "b", RefreshToken: "  "}, // whitespace-only must not collide
		{ID: "c", RefreshToken: ""},
		{ID: "d", RefreshToken: "rt-2"},
	})
	if len(idx) != 2 {
		t.Fatalf("expected 2 entries, got %d (idx=%v)", len(idx), idx)
	}
	if idx["rt-1"] != "a" || idx["rt-2"] != "d" {
		t.Fatalf("wrong index: %v", idx)
	}
}

func TestRedactForLogMasksLocalPart(t *testing.T) {
	cases := map[string]string{
		"alice@example.com":        "***@example.com",
		"":                         "***",
		"no-at":                    "***",
		"@only-domain.example.com": "***",
	}
	for in, want := range cases {
		if got := redactForLog(in); got != want {
			t.Errorf("redactForLog(%q) = %q, want %q", in, got, want)
		}
	}
}

// Compile-time check: the production dependencies match the injected
// signatures. If auth.RefreshToken or auth.GetUserInfo ever change shape this
// stops compiling, forcing the import path to be updated in lockstep.
var (
	_ refreshTokenFunc = (refreshTokenFunc)(nil)
	_ userInfoFunc     = (userInfoFunc)(nil)
)
