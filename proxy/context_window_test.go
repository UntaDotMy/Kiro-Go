package proxy

import "testing"

// TestGetContextWindowSize verifies models are classified into the correct
// context window. This drives the input-token count that clients use to decide
// when to compact; misclassifying opus-4.8 (1M) as 200K under-reports tokens by
// ~5x and prevents timely compaction. The fix version-parses (>=4.6 → 1M)
// instead of matching a fixed 4.6/4.7 substring list, so new minors and majors
// are future-proof.
func TestGetContextWindowSize(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"claude-opus-4.6", 1_000_000},
		{"claude-opus-4-6", 1_000_000},
		{"claude-opus-4.7", 1_000_000},
		{"claude-opus-4.8", 1_000_000}, // the bug: previously fell to 200K
		{"claude-opus-4-8", 1_000_000},
		{"claude-opus-4.9", 1_000_000},
		{"claude-opus-4.10", 1_000_000}, // double-digit minor
		{"claude-sonnet-4.6", 1_000_000},
		{"claude-opus-5.0", 1_000_000}, // future major
		{"claude-sonnet-5-1", 1_000_000},
		{"claude-opus-4.8-thinking", 1_000_000},
		{"CLAUDE-OPUS-4.8", 1_000_000}, // case-insensitive
		{"claude-opus-4.5", 200_000},
		{"claude-sonnet-4.5", 200_000},
		{"claude-sonnet-4", 200_000}, // no minor → not large
		{"claude-haiku-4.5", 200_000},
		{"claude-3-5-sonnet", 200_000}, // legacy 3.x shape
		{"unknown-model", 200_000},
		{"", 200_000},
	}
	for _, c := range cases {
		if got := getContextWindowSize(c.model); got != c.want {
			t.Errorf("getContextWindowSize(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}

// TestParseClaudeVersion covers the regexp-free version extractor that
// isLargeContextModel relies on.
func TestParseClaudeVersion(t *testing.T) {
	cases := []struct {
		in              string
		major, minor    int
		ok              bool
	}{
		{"claude-opus-4.8", 4, 8, true},
		{"claude-opus-4-8", 4, 8, true},
		{"claude-sonnet-4.10", 4, 10, true},
		{"claude-opus-10.2", 10, 2, true},
		{"claude-opus-4.8-thinking", 4, 8, true}, // trailing suffix ignored
		{"claude-sonnet-4", 0, 0, false},         // no minor
		{"claude-3-5-sonnet", 0, 0, false},       // family not in second position
		{"gpt-4o", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		maj, min, ok := parseClaudeVersion(c.in)
		if ok != c.ok || (ok && (maj != c.major || min != c.minor)) {
			t.Errorf("parseClaudeVersion(%q) = (%d,%d,%v), want (%d,%d,%v)",
				c.in, maj, min, ok, c.major, c.minor, c.ok)
		}
	}
}
