package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

// TestLooksLikeNineRouterBackup pins the structural sniff used to auto-route a
// pasted 9router backup through the shared /admin/api/import endpoint.
func TestLooksLikeNineRouterBackup(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"providerConnections present", `{"providerConnections":[{"provider":"kiro"}]}`, true},
		{"providerNodes present", `{"providerNodes":[{"id":"x","type":"openai-compatible","baseUrl":"https://a"}]}`, true},
		{"combos present", `{"combos":[{"name":"smart","models":["kr/x"]}]}`, true},
		{"native kiro export envelope", `{"version":"1","accounts":[{"credentials":{"refreshToken":"r"}}]}`, false},
		{"credential array", `[{"refreshToken":"r"}]`, false},
		{"single credential", `{"refreshToken":"r"}`, false},
		{"garbage", `not json`, false},
	}
	for _, c := range cases {
		if got := looksLikeNineRouterBackup([]byte(c.body)); got != c.want {
			t.Errorf("%s: looksLikeNineRouterBackup = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestNineRouterProviderToBackend covers the slug → backend mapping for the
// bespoke providers, common aliases, built-in catalog ids, and the
// node-resolved compatible slugs (which must NOT resolve here).
func TestNineRouterProviderToBackend(t *testing.T) {
	newImportTestHandler(t) // initialise config so GetProviderConfig is safe
	cases := []struct {
		slug   string
		want   string
		wantOK bool
	}{
		{"kiro", "kiro", true},
		{"codex", "codex", true},
		{"chatgpt", "codex", true},
		{"qoder", "qoder", true},
		{"claude", "anthropic", true},
		{"anthropic", "anthropic", true},
		{"gemini", "gemini", true},
		{"google", "gemini", true},
		{"glm-cn", "glm-cn", true},                // now a built-in catalog id (resolves to itself)
		{"groq", "groq", true},                    // built-in catalog id
		{"or", "openrouter", true},                // built-in alias
		{"codebuddy", "codebuddy", true},          // newly-added built-in
		{"openai-compatible-chat-xyz", "", false}, // resolved by node id, not here
		{"anthropic-compatible-foo", "", false},
		{"totally-unknown", "", false},
	}
	for _, c := range cases {
		got, ok := nineRouterProviderToBackend(c.slug)
		if ok != c.wantOK || got != c.want {
			t.Errorf("nineRouterProviderToBackend(%q) = (%q,%v), want (%q,%v)", c.slug, got, ok, c.want, c.wantOK)
		}
	}
}

// TestImportNineRouterBackup drives a representative multi-provider backup
// (Kiro OAuth, Codex access_token, Qoder, an openai-compatible node + its
// apikey connection, plus an inbound client key) through the importer and
// asserts the resulting accounts, custom provider, and api key.
func TestImportNineRouterBackup(t *testing.T) {
	h := newImportTestHandler(t)

	backup := `{
      "settings": {"comboStrategy":"fallback"},
      "providerConnections": [
        {
          "id":"c-kiro","provider":"kiro","authType":"oauth","email":"me@example.com",
          "name":"me@example.com","priority":1,"isActive":true,
          "accessToken":"kiro-at","refreshToken":"kiro-rt","expiresAt":"2030-01-01T00:00:00.000Z",
          "providerSpecificData":{"profileArn":"arn:aws:cw:profile/X","authMethod":"social","provider":"Google","region":"us-east-1"}
        },
        {
          "id":"c-codex","provider":"codex","authType":"access_token","email":"cx@example.com",
          "name":"ChatGPT","accessToken":"codex-at","isActive":true,
          "providerSpecificData":{"chatgptAccountId":"wsid_123","chatgptPlanType":"pro"}
        },
        {
          "id":"c-qoder","provider":"qoder","authType":"access_token","name":"Qoder",
          "accessToken":"dt-qoder","isActive":true,
          "providerSpecificData":{"userId":"u-9","machineId":"m-9"}
        },
        {
          "id":"c-node","provider":"openai-compatible-chat-xyz","authType":"apikey",
          "name":"My LLM key","apiKey":"sk-mine","isActive":true,
          "providerSpecificData":{"baseUrl":"https://api.example.com/v1"}
        },
        {
          "id":"c-groq","provider":"groq","authType":"apikey","name":"groq key","apiKey":"gsk-1","isActive":false
        }
      ],
      "providerNodes": [
        {"id":"openai-compatible-chat-xyz","type":"openai-compatible","name":"My LLM","prefix":"myllm","apiType":"chat","baseUrl":"https://api.example.com/v1"}
      ],
      "apiKeys": [
        {"id":"k1","key":"sk-9r-abc","name":"default","isActive":true}
      ],
      "combos": [{"name":"smart","models":["kr/x"]}]
    }`

	res, err := h.importNineRouterBackup([]byte(backup))
	if err != nil {
		t.Fatalf("importNineRouterBackup: %v", err)
	}

	if res.Providers != 1 {
		t.Errorf("Providers = %d, want 1 (the openai-compatible node)", res.Providers)
	}
	if res.Accounts != 5 {
		t.Errorf("Accounts = %d, want 5", res.Accounts)
	}
	if res.APIKeys != 1 {
		t.Errorf("APIKeys = %d, want 1", res.APIKeys)
	}
	if !res.Success {
		t.Errorf("Success = false, results=%+v", res.Results)
	}

	// Custom provider registered with derived dialect + alias.
	pc, ok := config.GetProviderConfig("openai-compatible-chat-xyz")
	if !ok {
		t.Fatal("custom provider not registered")
	}
	if pc.Dialect != "openai" || pc.Alias != "myllm" || pc.BaseURL != "https://api.example.com/v1" {
		t.Errorf("custom provider mismatch: %+v", pc)
	}

	accounts := config.GetAccounts()
	byBackend := map[string]config.Account{}
	for _, a := range accounts {
		byBackend[a.Backend] = a
	}

	kiro := byBackend["kiro"]
	if kiro.RefreshToken != "kiro-rt" || kiro.ProfileArn != "arn:aws:cw:profile/X" || kiro.AuthMethod != "social" || kiro.Provider != "Google" {
		t.Errorf("kiro account mismatch: %+v", kiro)
	}
	if kiro.ExpiresAt <= 0 {
		t.Errorf("kiro ExpiresAt should be parsed from ISO date, got %d", kiro.ExpiresAt)
	}

	codex := byBackend["codex"]
	if codex.AccessToken != "codex-at" || codex.CodexAccountID != "wsid_123" || codex.CodexPlanType != "pro" {
		t.Errorf("codex account mismatch: %+v", codex)
	}

	qoder := byBackend["qoder"]
	if qoder.AccessToken != "dt-qoder" || qoder.QoderUserID != "u-9" || qoder.QoderMachineID != "m-9" {
		t.Errorf("qoder account mismatch: %+v", qoder)
	}

	node := byBackend["openai-compatible-chat-xyz"]
	if node.APIKey != "sk-mine" || node.BaseURLOverride != "https://api.example.com/v1" {
		t.Errorf("node-backed account mismatch: %+v", node)
	}

	groq := byBackend["groq"]
	if groq.APIKey != "gsk-1" || groq.Enabled {
		t.Errorf("groq account mismatch (should be disabled): %+v", groq)
	}

	keys := config.GetAPIKeys()
	found := false
	for _, k := range keys {
		if k.Key == "sk-9r-abc" {
			found = true
		}
	}
	if !found {
		t.Errorf("inbound api key sk-9r-abc not imported; keys=%+v", keys)
	}
}

// TestImportNineRouterBackupDedupes confirms a second import of the same backup
// is idempotent (no duplicate accounts or keys).
func TestImportNineRouterBackupDedupes(t *testing.T) {
	h := newImportTestHandler(t)
	backup := `{"providerConnections":[
       {"provider":"kiro","authType":"oauth","email":"a@b.com","refreshToken":"rt-1","accessToken":"at-1"},
       {"provider":"groq","authType":"apikey","name":"g","apiKey":"gsk-1"}
     ],"apiKeys":[{"key":"sk-9r-1","name":"d"}]}`

	if _, err := h.importNineRouterBackup([]byte(backup)); err != nil {
		t.Fatalf("first import: %v", err)
	}
	res2, err := h.importNineRouterBackup([]byte(backup))
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if res2.Accounts != 0 {
		t.Errorf("second import created %d accounts, want 0 (all dupes)", res2.Accounts)
	}
	if res2.APIKeys != 0 {
		t.Errorf("second import created %d api keys, want 0 (dupe)", res2.APIKeys)
	}
	if got := len(config.GetAccounts()); got != 2 {
		t.Errorf("total accounts = %d, want 2 after idempotent re-import", got)
	}
}

// TestExportNineRouterRoundTrip exports a config built from known accounts and
// re-imports it into a fresh handler, asserting credentials survive the trip.
func TestExportNineRouterRoundTrip(t *testing.T) {
	h := newImportTestHandler(t)

	// Seed: a custom provider + accounts across backends.
	_ = config.AddProvider(config.ProviderConfig{ID: "mygw", Alias: "mg", Name: "My GW", Dialect: "openai", BaseURL: "https://gw.example.com/v1"})
	_ = config.AddAccount(config.Account{ID: "a1", Backend: "kiro", Email: "k@x.com", RefreshToken: "rt-k", AccessToken: "at-k", ProfileArn: "arn:p", AuthMethod: "social", Provider: "Google", Enabled: true})
	_ = config.AddAccount(config.Account{ID: "a2", Backend: "codex", Email: "c@x.com", AccessToken: "at-c", CodexAccountID: "wsid", CodexPlanType: "pro", Enabled: true})
	_ = config.AddAccount(config.Account{ID: "a3", Backend: "qoder", AccessToken: "dt-q", QoderUserID: "u", QoderMachineID: "m", Enabled: true})
	_ = config.AddAccount(config.Account{ID: "a4", Backend: "mygw", APIKey: "sk-gw", Enabled: true})

	bk := h.exportNineRouterBackup(nil)

	// Marshal and assert the envelope shape 9router expects.
	raw, err := json.Marshal(bk)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	if !looksLikeNineRouterBackup(raw) {
		t.Fatal("exported backup is not recognised as a 9router backup")
	}
	if bk.SchemaVersion != nineRouterSchemaVersion || bk.ExportedBy != "kiro-go" {
		t.Errorf("export markers missing: version=%d by=%q", bk.SchemaVersion, bk.ExportedBy)
	}
	if len(bk.ProviderConnections) != 4 {
		t.Fatalf("export connections = %d, want 4", len(bk.ProviderConnections))
	}
	if len(bk.ProviderNodes) != 1 {
		t.Errorf("export nodes = %d, want 1 (the custom provider)", len(bk.ProviderNodes))
	}

	// Re-import into a fresh handler.
	h2 := newImportTestHandler(t)
	res, err := h2.importNineRouterBackup(raw)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if res.Accounts != 4 {
		t.Errorf("re-import accounts = %d, want 4; results=%+v", res.Accounts, res.Results)
	}

	got := map[string]config.Account{}
	for _, a := range config.GetAccounts() {
		got[a.Backend] = a
	}
	if got["kiro"].RefreshToken != "rt-k" || got["kiro"].ProfileArn != "arn:p" {
		t.Errorf("kiro lost data on round-trip: %+v", got["kiro"])
	}
	if got["codex"].CodexAccountID != "wsid" || got["codex"].AccessToken != "at-c" {
		t.Errorf("codex lost data on round-trip: %+v", got["codex"])
	}
	if got["qoder"].QoderUserID != "u" {
		t.Errorf("qoder lost data on round-trip: %+v", got["qoder"])
	}
	if got["mygw"].APIKey != "sk-gw" {
		t.Errorf("custom provider account lost key on round-trip: %+v", got["mygw"])
	}
	// The custom provider node round-tripped and re-registered.
	if _, ok := config.GetProviderConfig("mygw"); !ok {
		t.Error("custom provider mygw not re-registered on import")
	}
}

// TestApiImportAutoRoutesNineRouter verifies the shared /admin/api/import
// endpoint detects and processes a 9router backup body.
func TestApiImportAutoRoutesNineRouter(t *testing.T) {
	h := newImportTestHandler(t)
	body := `{"providerConnections":[{"provider":"groq","authType":"apikey","name":"g","apiKey":"gsk-x"}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/import", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	h.apiImportAccounts(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var res nineRouterImportResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	if res.Accounts != 1 {
		t.Errorf("auto-routed import accounts = %d, want 1", res.Accounts)
	}
	accs := config.GetAccounts()
	if len(accs) != 1 || accs[0].Backend != "groq" || accs[0].APIKey != "gsk-x" {
		t.Errorf("account not created from auto-routed body: %+v", accs)
	}
}

// TestExportNineRouterHandlerHeaders checks the export handler emits the
// download headers and valid JSON.
func TestExportNineRouterHandlerHeaders(t *testing.T) {
	h := newImportTestHandler(t)
	_ = config.AddAccount(config.Account{ID: "a1", Backend: "kiro", RefreshToken: "rt", AccessToken: "at", Enabled: true})

	req := httptest.NewRequest(http.MethodPost, "/admin/api/export/9router", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	h.apiExportNineRouter(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "9router-backup.json") {
		t.Errorf("missing download filename, Content-Disposition=%q", cd)
	}
	var bk nineRouterBackup
	if err := json.Unmarshal(w.Body.Bytes(), &bk); err != nil {
		t.Fatalf("export body invalid JSON: %v", err)
	}
	if len(bk.ProviderConnections) != 1 {
		t.Errorf("export connections = %d, want 1", len(bk.ProviderConnections))
	}
}

// TestParseFlexibleUnixSeconds covers the expiry shapes 9router/Kiro emit.
func TestParseFlexibleUnixSeconds(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int64
	}{
		{"nil", nil, 0},
		{"rfc3339", "2021-01-01T00:00:00Z", 1609459200},
		{"epoch seconds number", float64(1609459200), 1609459200},
		{"epoch ms number", float64(1609459200000), 1609459200},
		{"epoch seconds string", "1609459200", 1609459200},
		{"epoch ms string", "1609459200000", 1609459200},
		{"empty string", "", 0},
		{"garbage", "not-a-date", 0},
	}
	for _, c := range cases {
		if got := parseFlexibleUnixSeconds(c.in); got != c.want {
			t.Errorf("%s: parseFlexibleUnixSeconds(%v) = %d, want %d", c.name, c.in, got, c.want)
		}
	}
}
