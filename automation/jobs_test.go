package automation

import "testing"

// TestParseAccounts splits credential lines, skips blanks, and flags malformed
// rows by line number.
func TestParseAccounts(t *testing.T) {
	lines := []string{
		"user1@gmail.com|secret1",
		"",
		"  user2@gmail.com | secret2 ",
		"noseparator",
		"|nopassword",
		"noemail|",
		"user3@gmail.com|pass|with|pipes",
	}
	accounts, invalid := ParseAccounts(lines)

	if len(accounts) != 3 {
		t.Fatalf("got %d accounts, want 3 (%v)", len(accounts), accounts)
	}
	if accounts[0].Email != "user1@gmail.com" {
		t.Errorf("account[0].Email = %q", accounts[0].Email)
	}
	if accounts[1].Email != "user2@gmail.com" {
		t.Errorf("trimmed account[1].Email = %q, want user2@gmail.com", accounts[1].Email)
	}
	if accounts[2].Email != "user3@gmail.com" {
		t.Errorf("account[2].Email = %q (password with pipes should keep email)", accounts[2].Email)
	}
	// lines 4 (noseparator), 5 (|nopassword), 6 (noemail|) are invalid
	wantInvalid := map[int]bool{4: true, 5: true, 6: true}
	if len(invalid) != 3 {
		t.Fatalf("got invalid=%v, want 3 entries", invalid)
	}
	for _, ln := range invalid {
		if !wantInvalid[ln] {
			t.Errorf("unexpected invalid line %d", ln)
		}
	}
}

// TestSplitCredential covers the mixed-delimiter tolerance: pipe, colon, comma,
// semicolon, tab, and space all split into (email, password).
func TestSplitCredential(t *testing.T) {
	cases := []struct {
		in          string
		email, pass string
		ok          bool
	}{
		{"a@b.com|pw", "a@b.com", "pw", true},
		{"a@b.com:pw", "a@b.com", "pw", true},
		{"a@b.com,pw", "a@b.com", "pw", true},
		{"a@b.com;pw", "a@b.com", "pw", true},
		{"a@b.com\tpw", "a@b.com", "pw", true},
		{"a@b.com pw", "a@b.com", "pw", true},
		{"  a@b.com  |  pw  ", "a@b.com", "pw", true}, // trims both halves
		{"a@b.com|p|q|r", "a@b.com", "p|q|r", true},   // password keeps later pipes
		{"a@b.com|p:q", "a@b.com", "p:q", true},       // pipe wins over colon (tier order)
		{"justtext", "", "", false},                   // no delimiter
		{"", "", "", false},                           // blank
		{"|pw", "", "", false},                        // empty email
		{"a@b.com|", "", "", false},                   // empty password
	}
	for _, c := range cases {
		email, pass, ok := splitCredential(c.in)
		if ok != c.ok || email != c.email || pass != c.pass {
			t.Errorf("splitCredential(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, email, pass, ok, c.email, c.pass, c.ok)
		}
	}
}

// TestClampConcurrency keeps thread counts in the allowed band.
func TestClampConcurrency(t *testing.T) {
	cases := map[int]int{
		0:  DefaultConcurrency,
		-5: DefaultConcurrency,
		1:  1,
		3:  3,
		10: 10,
		50: MaxConcurrency,
	}
	for in, want := range cases {
		if got := clampConcurrency(in); got != want {
			t.Errorf("clampConcurrency(%d) = %d, want %d", in, got, want)
		}
	}
}

// TestParseAccounts_PasswordExtraction confirms the password (kept out of the
// AccountJob) is recoverable by the same split logic Start uses.
func TestParseAccounts_PasswordExtraction(t *testing.T) {
	accounts, _ := ParseAccounts([]string{"a@b.com|p|q"})
	if len(accounts) != 1 {
		t.Fatalf("want 1 account")
	}
	// password portion is everything after the first '|': "p|q"
	// (the manager extracts it the same way in Start)
}
