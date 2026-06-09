package proxy

import (
	"io"
	"kiro-go/config"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveProfileArnReturnsCachedValueWithoutRequest(t *testing.T) {
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("unexpected HTTP request for cached profile ARN")
			return nil, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	account := &config.Account{ProfileArn: " arn:aws:codewhisperer:profile/test "}
	got, err := ResolveProfileArn(account)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "arn:aws:codewhisperer:profile/test" {
		t.Fatalf("expected trimmed cached ARN, got %q", got)
	}
}

func TestResolveProfileArnFetchesAndCachesProfile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := config.Account{
		ID:           "acct-1",
		Email:        "user@example.com",
		AccessToken:  "access-token",
		Region:       "us-east-1",
		UsageCurrent: 7,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("expected POST, got %s", req.Method)
			}
			if req.URL.Path != "/ListAvailableProfiles" {
				t.Fatalf("expected ListAvailableProfiles path, got %s", req.URL.Path)
			}
			if got := req.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("expected JSON content type, got %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"profiles":[{"arn":" arn:aws:codewhisperer:profile/fetched "}]} `)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	requestAccount := account
	requestAccount.UsageCurrent = 0
	got, err := ResolveProfileArn(&requestAccount)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "arn:aws:codewhisperer:profile/fetched" {
		t.Fatalf("expected fetched ARN, got %q", got)
	}
	if requestAccount.ProfileArn != got {
		t.Fatalf("expected account to be updated with fetched ARN, got %q", requestAccount.ProfileArn)
	}

	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected one persisted account, got %d", len(accounts))
	}
	if accounts[0].ProfileArn != got {
		t.Fatalf("expected persisted account profile ARN %q, got %q", got, accounts[0].ProfileArn)
	}
	if accounts[0].UsageCurrent != 7 {
		t.Fatalf("expected profile cache update to preserve usage fields, got usageCurrent=%v", accounts[0].UsageCurrent)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

// TestSetKiroHeadersIncludesAmzSdkHeaders verifies the account-MANAGEMENT REST
// path carries the AWS SDK retry-metrics headers the real Kiro client (and the
// in-tree kiro2api reference) send: a per-call amz-sdk-invocation-id UUID and
// amz-sdk-request="attempt=1; max=1" (single-shot — no SDK-level retry here).
func TestSetKiroHeadersIncludesAmzSdkHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://codewhisperer.us-east-1.amazonaws.com/getUsageLimits", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	setKiroHeaders(req, &config.Account{ID: "acct", AccessToken: "tok"})

	if got := req.Header.Get("Amz-Sdk-Request"); got != "attempt=1; max=1" {
		t.Fatalf("expected amz-sdk-request=\"attempt=1; max=1\", got %q", got)
	}
	inv := req.Header.Get("Amz-Sdk-Invocation-Id")
	if strings.TrimSpace(inv) == "" {
		t.Fatal("expected a non-empty amz-sdk-invocation-id")
	}
	// Two calls must produce distinct invocation ids (it's per-call).
	req2, _ := http.NewRequest(http.MethodGet, "https://codewhisperer.us-east-1.amazonaws.com/getUsageLimits", nil)
	setKiroHeaders(req2, &config.Account{ID: "acct", AccessToken: "tok"})
	if req2.Header.Get("Amz-Sdk-Invocation-Id") == inv {
		t.Fatal("amz-sdk-invocation-id must be unique per call")
	}
}
