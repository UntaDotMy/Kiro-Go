package proxy

import (
	"errors"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Replaces the endpoint chain with httptest URLs for the duration of a
// test, then restores the original list. Builds exactly len(urls) synthetic
// endpoints so a test can drive an N-endpoint failover chain regardless of how
// many endpoints the production region chain currently declares — the tests
// here exercise the failover MECHANISM, not the specific production host set.
func swapKiroEndpoints(t *testing.T, urls []string) {
	t.Helper()
	if len(urls) == 0 {
		t.Fatalf("swapKiroEndpoints requires at least one url")
	}
	// Seed each synthetic endpoint from the real chain's entry at the same index
	// (cycling if the test asks for more than the chain declares) so Origin /
	// AmzTarget stay realistic; only the URL is swapped to the httptest server.
	defaults := kiroEndpointsForRegion("us-east-1")
	override := make([]kiroEndpoint, len(urls))
	for i, u := range urls {
		base := defaults[i%len(defaults)]
		base.URL = u
		override[i] = base
	}
	prev := kiroEndpointsOverride
	kiroEndpointsOverride = override
	t.Cleanup(func() {
		kiroEndpointsOverride = prev
	})
}

// swapKiroHttpClientForTest installs http.Clients (both streaming and REST)
// whose transports do NOT reference http.ProxyFromEnvironment, so the test
// does not trigger the once-cached global env-proxy resolution. This keeps
// TestBuildKiroTransportFallsBackToEnvironmentProxy (which uses t.Setenv to
// influence ProxyFromEnvironment) deterministic regardless of test ordering.
// See https://pkg.go.dev/net/http#ProxyFromEnvironment for the caching contract.
//
// We must swap BOTH stores: CallKiroAPI uses kiroHttpStore for streaming, but
// it also calls ResolveProfileArn → listAvailableProfiles which uses
// kiroRestHttpStore. Either path can fire the env-proxy cache.
func swapKiroHttpClientForTest(t *testing.T) {
	t.Helper()
	prevStream := kiroHttpStore.Load()
	prevRest := kiroRestHttpStore.Load()
	mkClient := func() *http.Client {
		return &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{Proxy: nil},
		}
	}
	kiroHttpStore.Store(mkClient())
	kiroRestHttpStore.Store(mkClient())
	t.Cleanup(func() {
		if prevStream != nil {
			kiroHttpStore.Store(prevStream)
		}
		if prevRest != nil {
			kiroRestHttpStore.Store(prevRest)
		}
	})
}

func newCountingServer(status int, retryAfter string) (*httptest.Server, *int32) {
	var mu sync.Mutex
	hits := int32(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
	}))
	return srv, &hits
}

// TestCallKiroAPIFailsOverAcrossEndpointsOn429 exercises the new behavior:
// when the first endpoint returns 429, we should try the next endpoint instead
// of bailing out. We assert all three endpoints in the chain are hit.
func TestCallKiroAPIFailsOverAcrossEndpointsOn429(t *testing.T) {
	swapKiroHttpClientForTest(t)
	s1, h1 := newCountingServer(http.StatusTooManyRequests, "10")
	defer s1.Close()
	s2, h2 := newCountingServer(http.StatusTooManyRequests, "20")
	defer s2.Close()
	s3, h3 := newCountingServer(http.StatusTooManyRequests, "30")
	defer s3.Close()

	swapKiroEndpoints(t, []string{s1.URL, s2.URL, s3.URL})

	acct := &config.Account{ID: "test", Email: "t@example.com", AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "hi"

	err := CallKiroAPI(acct, payload, &KiroStreamCallback{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var qe *QuotaError
	if !errors.As(err, &qe) {
		t.Fatalf("expected *QuotaError, got %T: %v", err, err)
	}
	if *h1 != 1 || *h2 != 1 || *h3 != 1 {
		t.Fatalf("expected each endpoint hit once, got hits=(%d,%d,%d)", *h1, *h2, *h3)
	}
	// Min retry-after across the chain is 10s (s1).
	if qe.RetryAfter != 10*time.Second {
		t.Fatalf("expected min RetryAfter=10s, got %s", qe.RetryAfter)
	}
}

// If the first endpoint 429s but the second responds 200, CallKiroAPI must
// not return a QuotaError — it should consume the success path and never
// touch the third endpoint.
func TestCallKiroAPIRecoversWhenSecondEndpointSucceeds(t *testing.T) {
	swapKiroHttpClientForTest(t)
	s1, h1 := newCountingServer(http.StatusTooManyRequests, "10")
	defer s1.Close()
	// s2 returns a minimal valid empty body. parseEventStream finishes cleanly
	// when there are no events and calls OnComplete.
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
	}))
	defer s2.Close()
	s3, h3 := newCountingServer(http.StatusTooManyRequests, "30")
	defer s3.Close()

	swapKiroEndpoints(t, []string{s1.URL, s2.URL, s3.URL})

	acct := &config.Account{ID: "test2", Email: "t2@example.com", AccessToken: "x", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "hi"

	// Provide all callbacks parseEventStream may invoke so we don't panic on
	// nil dereference when the upstream body is empty.
	cb := &KiroStreamCallback{
		OnText:         func(text string, isThinking bool) {},
		OnToolUse:      func(KiroToolUse) {},
		OnComplete:     func(in, out int) {},
		OnCredits:      func(c float64) {},
		OnContextUsage: func(p float64) {},
		OnStopReason:   func(r string) {},
	}
	err := CallKiroAPI(acct, payload, cb)
	// Whatever parseEventStream returns is fine — the key invariant is that
	// it is NOT a *QuotaError, since the chain found a non-throttled surface.
	var qe *QuotaError
	if errors.As(err, &qe) {
		t.Fatalf("expected non-QuotaError after recovery, got QuotaError on %v", qe.Endpoints)
	}
	if *h1 != 1 {
		t.Fatalf("expected first endpoint hit once, got %d", *h1)
	}
	if *h3 != 0 {
		t.Fatalf("expected third endpoint NOT to be hit after second succeeded, got %d", *h3)
	}
}
