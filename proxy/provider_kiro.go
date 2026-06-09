package proxy

import (
	"context"
	"kiro-go/auth"
	"kiro-go/config"
)

// kiroProvider is the AWS CodeWhisperer / Amazon Q backend — the original (and
// default) provider. Its three methods are thin adapters over the existing
// package functions, so routing an account through ProviderFor produces
// byte-identical behavior to the pre-abstraction direct calls.
type kiroProvider struct{}

func init() {
	RegisterProvider(kiroProvider{})
}

func (kiroProvider) Name() string { return "kiro" }

// RefreshToken adapts auth.RefreshToken's 5-tuple into a TokenSet. The caller
// (ensureValidToken) unpacks it back exactly as before, preserving the in-place
// account mutation + pool reconciliation semantics.
func (kiroProvider) RefreshToken(ctx context.Context, acct *config.Account) (TokenSet, error) {
	at, rt, exp, arn, err := auth.RefreshToken(acct)
	if err != nil {
		return TokenSet{}, err
	}
	return TokenSet{
		AccessToken:  at,
		RefreshToken: rt,
		ExpiresAt:    exp,
		ProfileArn:   arn,
	}, nil
}

// ListModels delegates to the existing Kiro ListAvailableModels.
func (kiroProvider) ListModels(acct *config.Account) ([]ModelInfo, error) {
	return ListAvailableModels(acct)
}

// Call runs the existing Kiro upstream call. nr.Kiro carries the payload built by
// the handler (ClaudeToKiro / OpenAIToKiro), so this is a verbatim pass-through.
// As a safety net (e.g. a future caller that didn't prebuild), it builds the
// payload from whichever request side is set.
func (kiroProvider) Call(ctx context.Context, acct *config.Account, nr *NormalizedRequest, cb *KiroStreamCallback) error {
	payload := nr.Kiro
	if payload == nil {
		switch {
		case nr.Claude != nil:
			payload = ClaudeToKiro(nr.Claude, nr.Thinking)
		case nr.OpenAI != nil:
			payload = OpenAIToKiro(nr.OpenAI, nr.Thinking)
		default:
			// Nothing to send — preserve the existing "all endpoints failed"
			// style error rather than panicking on a nil payload.
			return CallKiroAPIContext(ctx, acct, &KiroPayload{}, cb)
		}
	}
	return CallKiroAPIContext(ctx, acct, payload, cb)
}
