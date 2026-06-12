package proxy

import (
	"context"
	"kiro-go/auth"
	"kiro-go/config"
	"time"
)

// claudeCodeProvider serves Claude Code accounts authenticated via the Anthropic
// OAuth manual-code flow (see auth/claude_oauth.go). After auth, Claude is the
// Anthropic Messages dialect at api.anthropic.com but authenticated with a Bearer
// OAuth access token (NOT x-api-key) plus the oauth beta header — so it uses a
// dedicated catalog row ("claude-code") with AuthHeader "bearer". Inference +
// model-listing DELEGATE to the shared generic Anthropic provider; this provider
// owns only the OAuth token refresh.
type claudeCodeProvider struct{}

var claudeInference = &genericProvider{dialect: DialectAnthropic}

func init() {
	RegisterProvider(claudeCodeProvider{})
}

func (claudeCodeProvider) Name() string { return "claude-code" }

func (claudeCodeProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if acct.RefreshToken == "" {
		return TokenSet{AccessToken: acct.AccessToken, ExpiresAt: acct.ExpiresAt}, nil
	}
	t, err := auth.RefreshClaudeToken(ctx, acct.RefreshToken)
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

func (claudeCodeProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	return claudeInference.ListModels(acct)
}

func (claudeCodeProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	return claudeInference.Call(ctx, acct, nr, cb)
}
