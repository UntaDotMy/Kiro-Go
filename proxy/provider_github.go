package proxy

import (
	"context"
	"kiro-go/auth"
	"kiro-go/config"
)

// githubCopilotProvider serves GitHub Copilot accounts authenticated via the OAuth
// device flow (see auth/github_oauth.go). GitHub Copilot is a TWO-TOKEN provider:
// the long-lived GitHub access token (persisted in Account.RefreshToken) is
// exchanged for a short-lived Copilot bearer token (Account.AccessToken) that
// authenticates inference at api.githubcopilot.com. After auth, inference is
// OpenAI-compatible, so this provider DELEGATES Call/ListModels to the shared
// generic OpenAI provider and owns only the Copilot-token minting on refresh.
type githubCopilotProvider struct{}

var githubInference = &genericProvider{dialect: DialectOpenAI}

func init() {
	RegisterProvider(githubCopilotProvider{})
}

func (githubCopilotProvider) Name() string { return "github" }

// RefreshToken re-mints the short-lived Copilot token from the stored GitHub access
// token (kept in RefreshToken). The GitHub token itself is preserved so future
// refreshes keep working.
func (githubCopilotProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if acct.RefreshToken == "" {
		return TokenSet{AccessToken: acct.AccessToken, ExpiresAt: acct.ExpiresAt}, nil
	}
	copTok, copExp, err := auth.MintGitHubCopilotToken(ctx, acct.RefreshToken)
	if err != nil {
		return TokenSet{}, err
	}
	return TokenSet{
		AccessToken:  copTok,
		RefreshToken: acct.RefreshToken, // preserve the GitHub token
		ExpiresAt:    copExp,
	}, nil
}

func (githubCopilotProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	return githubInference.ListModels(acct)
}

func (githubCopilotProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	return githubInference.Call(ctx, acct, nr, cb)
}
