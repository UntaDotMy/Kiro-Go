package proxy

import (
	"context"
	"kiro-go/auth"
	"kiro-go/config"
	"strings"
	"time"
)

// gitlabProvider serves GitLab Duo accounts authenticated via the OAuth
// authorization-code + PKCE flow (see auth/gitlab_oauth.go). GitLab is
// self-hostable, so the account carries the instance inference URL in
// BaseURLOverride (e.g. https://gitlab.example.com/api/v4/chat/completions) and the
// OAuth app credentials in ClientID/ClientSecret. After auth, inference is
// OpenAI-compatible, so this provider DELEGATES Call/ListModels to the shared
// generic OpenAI provider and owns only the OAuth token refresh.
type gitlabProvider struct{}

var gitlabInference = &genericProvider{dialect: DialectOpenAI}

func init() {
	RegisterProvider(gitlabProvider{})
}

func (gitlabProvider) Name() string { return "gitlab" }

// gitlabInstanceRoot derives the instance root (e.g. https://gitlab.com) from the
// account's inference base URL by trimming the OpenAI-compatible suffix. Falls back
// to gitlab.com when unset.
func gitlabInstanceRoot(acct *config.Account) string {
	base := strings.TrimRight(strings.TrimSpace(acct.BaseURLOverride), "/")
	base = strings.TrimSuffix(base, "/api/v4/chat/completions")
	base = strings.TrimSuffix(base, "/chat/completions")
	if base == "" {
		return "https://gitlab.com"
	}
	return base
}

func (gitlabProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if acct.RefreshToken == "" {
		return TokenSet{AccessToken: acct.AccessToken, ExpiresAt: acct.ExpiresAt}, nil
	}
	t, err := auth.RefreshGitLabToken(ctx, gitlabInstanceRoot(acct), acct.ClientID, acct.ClientSecret, acct.RefreshToken)
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

func (gitlabProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	return gitlabInference.ListModels(acct)
}

func (gitlabProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	return gitlabInference.Call(ctx, acct, nr, cb)
}
