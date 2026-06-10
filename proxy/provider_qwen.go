package proxy

import (
	"context"
	"kiro-go/auth"
	"kiro-go/config"
	"time"
)

// qwenProvider serves Qwen (Alibaba) accounts authenticated via the qwen-code
// OAuth device flow (see auth/qwen_oauth.go). After auth, Qwen is an ordinary
// OpenAI-compatible endpoint reached at {resource_url}/v1/chat/completions with a
// Bearer access token, so this provider DELEGATES inference + model-listing to the
// shared generic OpenAI provider and only owns the OAuth token refresh (which the
// generic provider treats as a no-op). The per-account base URL (from the token
// response's resource_url) lives in Account.BaseURLOverride, which
// resolveProviderSettings already layers over the catalog default.
type qwenProvider struct{}

// qwenInference is a stateless generic OpenAI provider instance used to handle the
// actual chat + /models calls for qwen accounts. genericProvider holds only its
// dialect, so a dedicated instance is safe and avoids a registry lookup.
var qwenInference = &genericProvider{dialect: DialectOpenAI}

func init() {
	RegisterProvider(qwenProvider{})
}

func (qwenProvider) Name() string { return "qwen" }

// RefreshToken renews the OAuth access token via the device-flow refresh grant.
// The refreshed resource_url is returned in Extra["baseURLOverride"] so the
// caller can repoint the account if Qwen moved its endpoint. An account with no
// refresh token (shouldn't happen post-login) falls back to its current creds.
func (qwenProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if acct.RefreshToken == "" {
		// No refresh token — return current creds unchanged (never-expire view) so a
		// misconfigured account degrades gracefully rather than hard-failing.
		return TokenSet{AccessToken: acct.AccessToken, ExpiresAt: acct.ExpiresAt}, nil
	}
	t, err := auth.RefreshQwenToken(ctx, acct.RefreshToken)
	if err != nil {
		return TokenSet{}, err
	}
	ts := TokenSet{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
	}
	if t.ExpiresIn > 0 {
		ts.ExpiresAt = time.Now().Unix() + int64(t.ExpiresIn)
	}
	if base := auth.QwenBaseURLFromResource(t.ResourceURL); base != "" {
		ts.Extra = map[string]string{"baseURLOverride": base}
	}
	return ts, nil
}

// ListModels delegates to the generic OpenAI provider, which fetches
// {base}/models with the account's Bearer access token.
func (qwenProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	return qwenInference.ListModels(acct)
}

// Call delegates to the generic OpenAI provider: build the OpenAI chat body, POST
// to {base}/chat/completions with the Bearer access token, stream the SSE back.
func (qwenProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	return qwenInference.Call(ctx, acct, nr, cb)
}
