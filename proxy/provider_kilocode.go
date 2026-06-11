package proxy

import (
	"context"
	"kiro-go/config"
)

// kilocodeProvider serves Kilo Code accounts authenticated via the custom
// device-auth flow (see auth/kilocode_oauth.go). After auth, Kilo Code is an
// ordinary OpenAI-compatible endpoint reached at api.kilo.ai with a Bearer token
// plus a per-account X-Kilocode-OrganizationID header (carried in
// Account.ExtraHeaders), so this provider DELEGATES inference + model-listing to
// the shared generic OpenAI provider. There is NO refresh token — the issued token
// is long-lived and re-login is required when it lapses, so RefreshToken is a no-op
// that reports the current credential.
type kilocodeProvider struct{}

var kilocodeInference = &genericProvider{dialect: DialectOpenAI}

func init() {
	RegisterProvider(kilocodeProvider{})
}

func (kilocodeProvider) Name() string { return "kilocode" }

// RefreshToken is a no-op: Kilo Code issues no refresh token. Report the current
// access token as never-expiring so the scheduler doesn't churn on it.
func (kilocodeProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	return TokenSet{AccessToken: acct.AccessToken, ExpiresAt: 0}, nil
}

func (kilocodeProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	return kilocodeInference.ListModels(acct)
}

func (kilocodeProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	return kilocodeInference.Call(ctx, acct, nr, cb)
}
