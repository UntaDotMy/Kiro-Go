package proxy

import (
	"context"
	"kiro-go/auth"
	"kiro-go/config"
	"time"
)

// xaiProvider serves xAI (Grok) accounts. xAI supports BOTH a pasted API key and
// the OAuth authorization-code flow (see auth/xai_oauth.go); both end up as plain
// OpenAI /v1/chat/completions at api.x.ai. This bespoke provider exists only to
// renew OAuth access tokens — an api-key account (no RefreshToken, ExpiresAt 0)
// never triggers refresh, so it behaves exactly like the generic OpenAI provider.
// Inference + model-listing DELEGATE to the shared generic OpenAI provider.
type xaiProvider struct{}

var xaiInference = &genericProvider{dialect: DialectOpenAI}

func init() {
	RegisterProvider(xaiProvider{})
}

func (xaiProvider) Name() string { return "xai" }

func (xaiProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// api-key account (or OAuth account without a refresh token): nothing to renew.
	if acct.RefreshToken == "" {
		return TokenSet{AccessToken: acct.AccessToken, ExpiresAt: acct.ExpiresAt}, nil
	}
	t, err := auth.RefreshXaiToken(ctx, acct.RefreshToken)
	if err != nil {
		return TokenSet{}, err
	}
	ts := TokenSet{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken}
	if ts.RefreshToken == "" {
		ts.RefreshToken = acct.RefreshToken
	}
	if t.ExpiresIn > 0 {
		ts.ExpiresAt = time.Now().Unix() + int64(t.ExpiresIn)
	}
	return ts, nil
}

func (xaiProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	return xaiInference.ListModels(acct)
}

func (xaiProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	return xaiInference.Call(ctx, acct, nr, cb)
}
