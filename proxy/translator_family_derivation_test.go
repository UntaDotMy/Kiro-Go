package proxy

import "testing"

// TestClaudeFamilyDottedIDDerivesFutureMinors is the forwards-compat
// pin: a client sending claude-opus-4-8 (or 4-9, 5-0, 5-1, etc.) must
// resolve to Kiro's dotted upstream form WITHOUT requiring a hardcoded
// table entry per release. Same for sonnet and haiku. If this test
// regresses, every "new Anthropic minor version" event becomes a code
// change instead of a config change.
func TestClaudeFamilyDottedIDDerivesFutureMinors(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Existing versions still work — sanity bookend.
		{"opus-4-7 dashed", "claude-opus-4-7", "claude-opus-4.7"},
		{"opus-4.7 dotted passthrough", "claude-opus-4.7", "claude-opus-4.7"},
		{"sonnet-4-5 dashed", "claude-sonnet-4-5", "claude-sonnet-4.5"},
		{"haiku-4-5 dashed", "claude-haiku-4-5", "claude-haiku-4.5"},
		// The whole point: future minors with no hardcoded entry.
		{"opus-4-8 future minor", "claude-opus-4-8", "claude-opus-4.8"},
		{"opus-4.9 future dotted", "claude-opus-4.9", "claude-opus-4.9"},
		{"sonnet-5-0 future major-rev", "claude-sonnet-5-0", "claude-sonnet-5.0"},
		{"haiku-5-1 future", "claude-haiku-5-1", "claude-haiku-5.1"},
		// Two-digit minors / majors must work — the original len==3 check
		// silently rejected these. Pin the relaxed shape so a future
		// claude-opus-4-10 routes correctly.
		{"opus-4-10 double-digit minor", "claude-opus-4-10", "claude-opus-4.10"},
		{"opus-4.10 dotted double-digit minor", "claude-opus-4.10", "claude-opus-4.10"},
		{"sonnet-10-2 double-digit major", "claude-sonnet-10-2", "claude-sonnet-10.2"},
		// Non-derivable shapes return "" so the caller falls through to
		// the explicit modelMapOrdered table or the claude-* passthrough.
		{"bare family rejected", "claude-sonnet-4", ""},
		{"dated suffix rejected", "claude-sonnet-4-20250514", ""},
		{"legacy 3-5 rejected", "claude-3-5-sonnet", ""},
		{"non-claude rejected", "gpt-4o", ""},
		{"empty rejected", "", ""},
		// Junk shapes — three-or-more digit chunks should be rejected so
		// we don't accidentally translate something like
		// claude-opus-100-200 (no such model exists; better to fall
		// through and let the caller's passthrough handle it).
		{"three-digit major rejected", "claude-opus-100-2", ""},
		{"three-digit minor rejected", "claude-opus-4-100", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := claudeFamilyDottedID(c.in)
			if got != c.want {
				t.Fatalf("claudeFamilyDottedID(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestParseModelAndThinkingFutureMinor confirms the derivation reaches
// the public ParseModelAndThinking entry point, which is what the
// request handlers actually call. A future opus-4.8 client must resolve
// to "claude-opus-4.8" upstream id without any code change.
func TestParseModelAndThinkingFutureMinor(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"claude-opus-4-8", "claude-opus-4.8"},
		{"claude-opus-4-8-thinking", "claude-opus-4.8"},
		{"claude-sonnet-5-0", "claude-sonnet-5.0"},
		{"claude-haiku-4-9", "claude-haiku-4.9"},
		{"claude-opus-4-10", "claude-opus-4.10"}, // double-digit minor
		{"claude-sonnet-10-2", "claude-sonnet-10.2"},
	}
	for _, c := range cases {
		got, _ := ParseModelAndThinking(c.in, "-thinking")
		if got != c.want {
			t.Errorf("ParseModelAndThinking(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseModelAndThinkingLegacyPaths pins that removing the family-
// version rows from modelMapOrdered did NOT regress the explicit legacy
// mappings (claude-3-*, gpt-*, the dated sonnet-4 alias, and the bare
// claude-sonnet-4). Without this, a future cleanup could accidentally
// drop one of these and we'd silently break old clients.
func TestParseModelAndThinkingLegacyPaths(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"claude-sonnet-4-20250514", "claude-sonnet-4"},
		{"claude-sonnet-4", "claude-sonnet-4"},
		{"claude-3-5-sonnet", "claude-sonnet-4.5"},
		{"claude-3-opus", "claude-sonnet-4.5"},
		{"claude-3-sonnet", "claude-sonnet-4"},
		{"claude-3-haiku", "claude-haiku-4.5"},
		{"gpt-4-turbo", "claude-sonnet-4.5"},
		{"gpt-4o", "claude-sonnet-4.5"},
		{"gpt-4", "claude-sonnet-4.5"},
		{"gpt-3.5-turbo", "claude-sonnet-4.5"},
	}
	for _, c := range cases {
		got, _ := ParseModelAndThinking(c.in, "-thinking")
		if got != c.want {
			t.Errorf("ParseModelAndThinking(%q) = %q, want %q (legacy mapping must not regress)", c.in, got, c.want)
		}
	}
}
