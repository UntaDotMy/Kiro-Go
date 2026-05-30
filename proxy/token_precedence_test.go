package proxy

import "testing"

// TestResolveInputTokensPrecedence locks in the token-count precedence the
// response finalization paths depend on. CLIs (Claude Code, opencode, Cline,
// Codex) read the usage we return verbatim, so an exact upstream count must
// always win over the coarser context-derived value and the local estimate.
func TestResolveInputTokensPrecedence(t *testing.T) {
	cases := []struct {
		name      string
		upstream  int
		ctxDerive int
		estimate  int
		want      int
	}{
		{"upstream wins over everything", 1234, 1300, 1500, 1234},
		{"upstream wins even when smaller", 10, 9000, 9000, 10},
		{"context-derived used when no upstream", 0, 1300, 1500, 1300},
		{"estimate only when upstream and context absent", 0, 0, 1500, 1500},
		{"zero when nothing available", 0, 0, 0, 0},
		{"negative upstream treated as absent", -5, 1300, 1500, 1300},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveInputTokens(tc.upstream, tc.ctxDerive, tc.estimate); got != tc.want {
				t.Fatalf("resolveInputTokens(%d,%d,%d) = %d, want %d",
					tc.upstream, tc.ctxDerive, tc.estimate, got, tc.want)
			}
		})
	}
}
