package proxy

import "testing"

// codeBuddyQuotaKind returns which CodeBuddy quota sync (if any) applies to a backend.
// "" = none, "intl" = codebuddy/codebuddy-ai (SyncCodeBuddyQuota), "cn" = codebuddy-cn (SyncCodeBuddyCNQuota).
func codeBuddyQuotaKind(backend string) string {
	switch backend {
	case "codebuddy", "codebuddy-ai":
		return "intl"
	case "codebuddy-cn":
		return "cn"
	default:
		return ""
	}
}

func TestCodeBuddyQuotaKind(t *testing.T) {
	tests := []struct {
		backend string
		want    string
	}{
		{"codebuddy", "intl"},
		{"codebuddy-ai", "intl"},
		{"codebuddy-cn", "cn"},
		{"kiro", ""},
		{"groq", ""},
		{"codex", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := codeBuddyQuotaKind(tt.backend)
		if got != tt.want {
			t.Errorf("codeBuddyQuotaKind(%q) = %q, want %q", tt.backend, got, tt.want)
		}
	}
}
