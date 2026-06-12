package proxy

import (
	"context"
	"kiro-go/auth"
	"kiro-go/config"
	"time"
)

// codeBuddyProvider serves CodeBuddy (Tencent) accounts authenticated via the
// browser-OAuth polling flow (see auth/codebuddy_oauth.go). After auth, CodeBuddy
// is an ordinary OpenAI-compatible endpoint reached at
// <host>/v1/chat/completions with a Bearer access token, so this provider
// DELEGATES inference + model-listing to the shared generic OpenAI provider and
// only owns the OAuth token refresh.
//
// CodeBuddy has two interchangeable official hosts (per the CLI's product.json):
// the China gateway copilot.tencent.com ("codebuddy") and the international site
// www.codebuddy.ai ("codebuddy-ai"). They share one auth implementation; the
// backend name selects the host so token refresh hits the same gateway the
// account logged in against. The base URL is fixed (from the catalog), so unlike
// qwen there is no per-account resource_url to track.
type codeBuddyProvider struct {
	name string // backend id: "codebuddy" (CN) or "codebuddy-ai" (international)
	host string // auth host base (auth.CodeBuddyHostCN / CodeBuddyHostIntl)
}

// codeBuddyInference handles the actual chat + /models calls for codebuddy accounts.
var codeBuddyInference = &genericProvider{dialect: DialectOpenAI}

func init() {
	RegisterProvider(codeBuddyProvider{name: "codebuddy", host: auth.CodeBuddyHostCN})
	RegisterProvider(codeBuddyProvider{name: "codebuddy-ai", host: auth.CodeBuddyHostIntl})
}

func (p codeBuddyProvider) Name() string { return p.name }

// RefreshToken renews the OAuth access token via the CodeBuddy refresh endpoint on
// this provider's host. An account with no refresh token falls back to its current
// credentials.
func (p codeBuddyProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if acct.RefreshToken == "" {
		return TokenSet{AccessToken: acct.AccessToken, ExpiresAt: acct.ExpiresAt}, nil
	}
	t, err := auth.RefreshCodeBuddyToken(ctx, p.host, acct.RefreshToken)
	if err != nil {
		return TokenSet{}, err
	}
	ts := TokenSet{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
	}
	if ts.RefreshToken == "" {
		ts.RefreshToken = acct.RefreshToken
	}
	if t.ExpiresIn > 0 {
		ts.ExpiresAt = time.Now().Unix() + int64(t.ExpiresIn)
	}
	return ts, nil
}

// ListModels delegates to the generic OpenAI provider. CodeBuddy has no working
// GET /models endpoint, so this falls back to the catalog's advisory list.
func (p codeBuddyProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	return codeBuddyInference.ListModels(acct)
}

// Call delegates to the generic OpenAI provider.
func (p codeBuddyProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	return codeBuddyInference.Call(ctx, acct, nr, cb)
}
