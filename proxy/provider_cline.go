package proxy

import (
	"context"
	"kiro-go/auth"
	"kiro-go/config"
	"time"
)

// clineProvider serves Cline accounts authenticated via the OAuth code flow (see
// auth/cline_oauth.go). After auth, inference is OpenAI-compatible at api.cline.bot,
// so this provider DELEGATES Call/ListModels to the shared generic OpenAI provider
// and owns only the OAuth token refresh.
type clineProvider struct{}

var clineInference = &genericProvider{dialect: DialectOpenAI}

func init() {
	RegisterProvider(clineProvider{})
}

func (clineProvider) Name() string { return "cline" }

func (clineProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if acct.RefreshToken == "" {
		return TokenSet{AccessToken: acct.AccessToken, ExpiresAt: acct.ExpiresAt}, nil
	}
	t, err := auth.RefreshClineToken(ctx, acct.RefreshToken)
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

func (clineProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	return clineInference.ListModels(acct)
}

func (clineProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	return clineInference.Call(ctx, acct, nr, cb)
}
