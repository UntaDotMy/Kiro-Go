package proxy

import (
	"context"
	"kiro-go/auth"
	"kiro-go/config"
	"time"
)

// kimiCodingProvider serves Kimi Coding (Moonshot) accounts authenticated via the
// OAuth device flow (see auth/kimi_coding_oauth.go). After auth, Kimi Coding is an
// Anthropic-compatible /v1/messages endpoint reached with a Bearer access token, so
// this provider DELEGATES inference + model-listing to the shared generic Anthropic
// provider and only owns the OAuth token refresh.
type kimiCodingProvider struct{}

// kimiCodingInference handles the actual chat + /models calls. Kimi Coding speaks the
// Anthropic Messages dialect.
var kimiCodingInference = &genericProvider{dialect: DialectAnthropic}

func init() {
	RegisterProvider(kimiCodingProvider{})
}

func (kimiCodingProvider) Name() string { return "kimi-coding" }

func (kimiCodingProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if acct.RefreshToken == "" {
		return TokenSet{AccessToken: acct.AccessToken, ExpiresAt: acct.ExpiresAt}, nil
	}
	t, err := auth.RefreshKimiCodingToken(ctx, acct.RefreshToken)
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

func (kimiCodingProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	return kimiCodingInference.ListModels(acct)
}

func (kimiCodingProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	return kimiCodingInference.Call(ctx, acct, nr, cb)
}
