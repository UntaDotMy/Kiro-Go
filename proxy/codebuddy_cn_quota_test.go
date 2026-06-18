package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

// cnTestAccount is a minimal codebuddy-cn account whose ck_ key resolves the
// billing/checkin calls. ProxyURL is empty so requests go through the default
// rest client (initialized below) straight to the httptest server.
func cnTestAccount() *config.Account {
	return &config.Account{
		ID:          "cn-test",
		Backend:     "codebuddy-cn",
		APIKey:      "ck_test_key",
		AccessToken: "fallback_token",
	}
}

// init the shared rest client so GetRestClientForProxy("") returns a usable
// client (nil otherwise) when tests dial the httptest server.
func ensureRestClient() { InitKiroHttpClient("") }

// TestCodeBuddyCNQuota_PostsEmptyBody proves PART A (BUG #2): the api-key
// billing call must POST an empty {} body, NOT the 101-year ProductCode body
// that codeBuddyUsageBody(false) produces (which scope-filters the ck_ key's
// packages to empty → quota 0).
func TestCodeBuddyCNQuota_PostsEmptyBody(t *testing.T) {
	ensureRestClient()
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":{"Response":{"Data":{"Accounts":[]}}}}`))
	}))
	defer srv.Close()

	if _, err := fetchCodeBuddyCNQuotaAt(context.Background(), cnTestAccount(), srv.URL); err != nil {
		t.Fatalf("quota fetch error: %v", err)
	}
	if strings.TrimSpace(gotBody) != "{}" {
		t.Errorf("quota body = %q, want %q", gotBody, "{}")
	}
}

// TestDailyCheckinCodeBuddyCN_Success: HTTP 200, code 0, data populated.
func TestDailyCheckinCodeBuddyCN_Success(t *testing.T) {
	ensureRestClient()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0,"data":{"credit":150,"streak_days":3,"is_streak_day":true}}`))
	}))
	defer srv.Close()

	res, err := dailyCheckinCodeBuddyCNAt(context.Background(), cnTestAccount(), srv.URL)
	if err != nil {
		t.Fatalf("checkin error: %v", err)
	}
	if !res.Success || res.Already {
		t.Errorf("got Success=%v Already=%v, want true/false", res.Success, res.Already)
	}
	if res.Credit != 150 || res.StreakDays != 3 || !res.IsStreakDay {
		t.Errorf("got credit=%v streak=%d isStreak=%v, want 150/3/true", res.Credit, res.StreakDays, res.IsStreakDay)
	}
}

// TestDailyCheckinCodeBuddyCN_AlreadyCode10001: HTTP 200 with business code
// 10001 ("today already checked in") is a success-already, not an error.
func TestDailyCheckinCodeBuddyCN_AlreadyCode10001(t *testing.T) {
	ensureRestClient()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":10001,"msg":"今天已签到"}`))
	}))
	defer srv.Close()

	res, err := dailyCheckinCodeBuddyCNAt(context.Background(), cnTestAccount(), srv.URL)
	if err != nil {
		t.Fatalf("checkin error: %v", err)
	}
	if !res.Success || !res.Already {
		t.Errorf("got Success=%v Already=%v, want true/true", res.Success, res.Already)
	}
}

// TestDailyCheckinCodeBuddyCN_Already400: the documented already-signed case is
// HTTP 400 + code 10001. The 400 body MUST be parsed (api_client.py:184,
// 200-217) so it is reported as already, NOT an error.
func TestDailyCheckinCodeBuddyCN_Already400(t *testing.T) {
	ensureRestClient()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"code":10001,"msg":"今天已签到"}`))
	}))
	defer srv.Close()

	res, err := dailyCheckinCodeBuddyCNAt(context.Background(), cnTestAccount(), srv.URL)
	if err != nil {
		t.Fatalf("checkin returned error for already-signed 400: %v", err)
	}
	if !res.Success || !res.Already {
		t.Errorf("got Success=%v Already=%v, want true/true", res.Success, res.Already)
	}
}

// TestDailyCheckinCodeBuddyCN_OtherError: a non-already business code is an error.
func TestDailyCheckinCodeBuddyCN_OtherError(t *testing.T) {
	ensureRestClient()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":11140,"msg":"risk"}`))
	}))
	defer srv.Close()

	if _, err := dailyCheckinCodeBuddyCNAt(context.Background(), cnTestAccount(), srv.URL); err == nil {
		t.Errorf("expected error for code 11140, got nil")
	}
}

// TestCheckinStatusCodeBuddyCN_Parse: the status envelope is decoded into the
// typed struct.
func TestCheckinStatusCodeBuddyCN_Parse(t *testing.T) {
	ensureRestClient()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0,"data":{"active":true,"today_checked_in":false,"streak_days":5,"daily_credit":100}}`))
	}))
	defer srv.Close()

	st, err := checkinStatusCodeBuddyCNAt(context.Background(), cnTestAccount(), srv.URL)
	if err != nil {
		t.Fatalf("checkin-status error: %v", err)
	}
	if !st.Active || st.TodayCheckedIn || st.StreakDays != 5 || st.DailyCredit != 100 {
		t.Errorf("got active=%v today=%v streak=%d daily=%v, want true/false/5/100",
			st.Active, st.TodayCheckedIn, st.StreakDays, st.DailyCredit)
	}
}
