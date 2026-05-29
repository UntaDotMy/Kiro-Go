package pool

import "testing"

// TestModelLookupKeysDerivesFutureMinors pins the routing-side
// forwards-compat: the pool's account-has-model filter must accept any
// claude-<family>-N{-,.}N input without a hardcoded table entry per
// release. If a client requests claude-opus-4-8, the pool should look
// up both the dashed and dotted forms — so the per-account model list
// (which ListAvailableModels returns in dotted form) still matches.
func TestModelLookupKeysDerivesFutureMinors(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"claude-opus-4-7", []string{"claude-opus-4-7", "claude-opus-4.7"}},
		{"claude-opus-4.7", []string{"claude-opus-4.7", "claude-opus-4-7"}},
		{"claude-opus-4-8", []string{"claude-opus-4-8", "claude-opus-4.8"}},
		{"claude-opus-4-10", []string{"claude-opus-4-10", "claude-opus-4.10"}}, // double-digit minor
		{"claude-sonnet-10-2", []string{"claude-sonnet-10-2", "claude-sonnet-10.2"}},
		{"claude-sonnet-5-0", []string{"claude-sonnet-5-0", "claude-sonnet-5.0"}},
		{"claude-haiku-4-9", []string{"claude-haiku-4-9", "claude-haiku-4.9"}},
		{"claude-sonnet-4-20250514", []string{"claude-sonnet-4-20250514"}}, // dated, no twin
		{"claude-sonnet-4", []string{"claude-sonnet-4"}},                   // bare family — no twin
		{"gpt-4o", []string{"gpt-4o"}},                                     // non-claude — no twin
		{"", nil},
	}
	for _, c := range cases {
		got := modelLookupKeys(c.in)
		if len(got) != len(c.want) {
			t.Errorf("modelLookupKeys(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("modelLookupKeys(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}
