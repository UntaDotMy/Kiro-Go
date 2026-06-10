package proxy

import (
	"testing"

	"kiro-go/config"
	"kiro-go/pool"
)

// TestNoModelsProvidersShipStaticCatalog pins that the providers WITHOUT a working
// GET /models endpoint carry a non-empty static catalog, so "fetch models on add"
// shows a real count instead of 0. These ids are surfaced via resolveProviderSettings
// and fall back through FetchModelsForAccount / ListModels when the live fetch 404s.
func TestNoModelsProvidersShipStaticCatalog(t *testing.T) {
	// Providers confirmed (by live probe) to lack a /models endpoint and for which
	// we have a sourced static catalog.
	for _, backend := range []string{"perplexity", "iflow", "alicode", "alicode-intl"} {
		bp, ok := resolveBuiltinProvider(backend)
		if !ok {
			t.Fatalf("builtin provider %q not found", backend)
		}
		if len(bp.Models) == 0 {
			t.Errorf("provider %q has no static catalog; add flow would show 0 models", backend)
		}
	}

	// resolveProviderSettings must surface the static catalog so the fallback path
	// can read it.
	acct := &config.Account{Backend: "perplexity", APIKey: "sk-test"}
	ps, ok := resolveProviderSettings(acct)
	if !ok {
		t.Fatalf("resolveProviderSettings(perplexity) failed")
	}
	if len(ps.models) == 0 {
		t.Errorf("perplexity providerSettings.models empty; static fallback won't fire")
	}
	// Sanity: the well-known sonar id is present.
	var foundSonar bool
	for _, m := range ps.models {
		if m == "sonar" {
			foundSonar = true
		}
	}
	if !foundSonar {
		t.Errorf("perplexity static catalog missing 'sonar': %v", ps.models)
	}
}

// TestAdvisoryModelListDoesNotGateRouting is the core correctness guard for the
// static-catalog design: an advisory (display-only) model list must NOT turn into
// a strict routing filter. A model that's missing from the best-effort static
// catalog must still route (the upstream validates the id at call time), exactly
// as it does for an account with no cached list at all. Only a STRICT list (from a
// live /models fetch) gates routing.
func TestAdvisoryModelListDoesNotGateRouting(t *testing.T) {
	p := pool.NewForTesting()
	const acctID = "acct-advisory"

	// Seed an advisory list that intentionally OMITS "brand-new-model".
	p.SetAdvisoryModelList(acctID, []string{"sonar", "sonar-pro"})

	// GetModelList surfaces the advisory ids (for the dashboard count + /v1/models).
	got := p.GetModelList(acctID)
	if len(got) != 2 {
		t.Fatalf("GetModelList = %v, want 2 advisory ids", got)
	}

	// But routing must remain optimistic: a model NOT in the advisory list is still
	// considered (HasModelForTesting returns true), because the static catalog is a
	// guess, not an authoritative filter.
	if !p.HasModelForTesting(acctID, "brand-new-model") {
		t.Errorf("advisory list wrongly GATED routing: 'brand-new-model' shed even though advisory lists are display-only")
	}
	if !p.HasModelForTesting(acctID, "sonar") {
		t.Errorf("advisory-listed model 'sonar' should also route")
	}

	// A STRICT list (live fetch) DOES gate: an unlisted model is rejected.
	const strictID = "acct-strict"
	p.SetModelList(strictID, []string{"llama-3.3-70b"})
	if p.HasModelForTesting(strictID, "some-other-model") {
		t.Errorf("strict list should gate routing: 'some-other-model' must be shed")
	}
	if !p.HasModelForTesting(strictID, "llama-3.3-70b") {
		t.Errorf("strict-listed model should route")
	}
}

// TestSetModelListSupersedesAdvisory verifies a later live /models fetch replaces
// the advisory list (and re-imposes strict gating), so an account doesn't keep a
// stale static guess after a real catalog arrives.
func TestSetModelListSupersedesAdvisory(t *testing.T) {
	p := pool.NewForTesting()
	const acctID = "acct-supersede"

	p.SetAdvisoryModelList(acctID, []string{"static-a", "static-b"})
	// Unlisted routes (advisory).
	if !p.HasModelForTesting(acctID, "anything") {
		t.Fatalf("advisory should not gate before live fetch")
	}

	// A live fetch arrives -> strict list, advisory dropped.
	p.SetModelList(acctID, []string{"real-1", "real-2"})
	if p.HasModelForTesting(acctID, "static-a") {
		t.Errorf("old advisory id should no longer route after strict list set")
	}
	if !p.HasModelForTesting(acctID, "real-1") {
		t.Errorf("strict-listed 'real-1' should route")
	}
	got := p.GetModelList(acctID)
	if len(got) != 2 || (got[0] != "real-1" && got[0] != "real-2") {
		t.Errorf("GetModelList after live fetch = %v, want the 2 real ids", got)
	}
}
