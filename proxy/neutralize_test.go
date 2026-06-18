package proxy

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
)

func enableFiltersForNeutralize(t *testing.T) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config init: %v", err)
	}
	if err := config.UpdatePromptFilterConfig(true, false, false, nil); err != nil {
		t.Fatalf("enable filter: %v", err)
	}
}

const neutralizeHarnessSample = `You are Claude Code, Anthropic's official CLI for Claude.

# Tone and style
Be concise.

# Doing tasks
Do software engineering tasks.

x-anthropic-billing-header: cc_version=2.1.38; cch=00000;`

func TestNeutralizeHarnessKeepsContractPerBackend(t *testing.T) {
	enableFiltersForNeutralize(t)

	cases := []struct {
		backend       string
		wantBrand     string
		stripIdentity bool
	}{
		{"kiro", "", true},
		{"codebuddy", "CodeBuddy", false},
		{"qoder", "Qoder", false},
		{"groq", "the assistant", false},
	}
	for _, c := range cases {
		out := neutralizeHarness(neutralizeHarnessSample, c.backend)
		if out == "" {
			t.Fatalf("%s: harness dropped entirely", c.backend)
		}
		if !strings.Contains(out, "Do software engineering tasks.") {
			t.Errorf("%s: harness instructions lost:\n%s", c.backend, out)
		}
		if strings.Contains(strings.ToLower(out), "claude") || strings.Contains(strings.ToLower(out), "anthropic") {
			t.Errorf("%s: brand token survived:\n%s", c.backend, out)
		}
		if strings.Contains(out, "x-anthropic-billing-header") {
			t.Errorf("%s: billing header survived (Bedrock 400 risk):\n%s", c.backend, out)
		}
		if c.stripIdentity {
			if strings.Contains(out, "You are ") {
				t.Errorf("%s: identity line should be stripped, not rebranded:\n%s", c.backend, out)
			}
			continue
		}
		if !strings.Contains(out, c.wantBrand) {
			t.Errorf("%s: expected rebrand to %q:\n%s", c.backend, c.wantBrand, out)
		}
	}
}

func TestNeutralizeProviderBodyNeutralizesHarnessButKeepsContext(t *testing.T) {
	enableFiltersForNeutralize(t)

	in := map[string]interface{}{
		"model": "claude-sonnet-4.6", // must NOT be rewritten
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "system",
				"content": neutralizeHarnessSample,
			},
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "Read CLAUDE.md in /home/me/claude_core then ask Claude about Anthropic."},
				},
			},
		},
	}
	raw, _ := json.Marshal(in)
	out := neutralizeProviderBody(raw, "codebuddy")

	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if got["model"] != "claude-sonnet-4.6" {
		t.Errorf("model id altered: got %q", got["model"])
	}

	msgs := got["messages"].([]interface{})
	sys := msgs[0].(map[string]interface{})["content"].(string)
	if !strings.Contains(sys, "Do software engineering tasks.") {
		t.Errorf("CodeBuddy harness instructions lost:\n%s", sys)
	}
	// The harness IDENTITY phrases are gone from the system prompt.
	if strings.Contains(sys, "Claude Code") || strings.Contains(sys, "Anthropic's official CLI") {
		t.Errorf("harness identity phrase survived in system prompt:\n%s", sys)
	}

	// CONTEXT must be preserved verbatim: a working dir like "claude_core", a
	// filename like "CLAUDE.md", and bare brand words in the user's own message
	// are NOT fingerprints — rewriting them is the reported bug (the model could
	// no longer find the real directory). Blanket brand substitution is forbidden.
	userText := msgs[1].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})["text"].(string)
	want := "Read CLAUDE.md in /home/me/claude_core then ask Claude about Anthropic."
	if userText != want {
		t.Errorf("user context was corrupted by brand rewrite:\n got:  %q\n want: %q", userText, want)
	}
}

func TestNeutralizeProviderBodyNonModeratedKeepsUserContent(t *testing.T) {
	enableFiltersForNeutralize(t)

	in := map[string]interface{}{
		"model": "llama-3.3-70b",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": neutralizeHarnessSample},
			map[string]interface{}{"role": "user", "content": "Tell me about Claude models."},
		},
	}
	raw, _ := json.Marshal(in)
	out := neutralizeProviderBody(raw, "groq")

	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	msgs := got["messages"].([]interface{})
	user := msgs[1].(map[string]interface{})["content"].(string)
	// Non-moderated providers must not rewrite the user's own content.
	if user != "Tell me about Claude models." {
		t.Errorf("user content was altered on a non-moderated provider: %q", user)
	}
	// But the harness system prompt is still neutralized.
	sys := msgs[0].(map[string]interface{})["content"].(string)
	if strings.Contains(strings.ToLower(sys), "anthropic") {
		t.Errorf("harness brand token survived: %s", sys)
	}
}

func TestNeutralizeProviderBodyInvalidJSONFailsOpen(t *testing.T) {
	raw := []byte("not json {{{")
	if got := neutralizeProviderBody(raw, "codebuddy"); string(got) != string(raw) {
		t.Errorf("invalid JSON should pass through unchanged: got %q", string(got))
	}
}

const moderationCatalogSample = `- adversarial-security-review: Stress-test code and configuration from an attacker's perspective using a structured red-team / blue-team / adjudicator pass, beyond a checklist scan. Use when a change touches auth, secrets, agent/hook config, or anything an attacker would target - first think like the attacker (enumerate concrete exploit paths), then like the defender. Use when the user says "security review", "threat model this", "can this be exploited". (Tools: Read, Grep, Glob, Bash)
- security-and-compliance-auditor: Performs threat modeling, exploitability analysis, and remediation quality, or when a vulnerability needs reproduction. (Tools: Read, Grep, Glob, Bash)
- deep-research: fan-out web searches, fetch sources, adversarially verify claims, synthesize a cited report.`

func TestSoftenModerationVocabularyClearsTriggersKeepsMeaning(t *testing.T) {
	got := softenModerationVocabulary(moderationCatalogSample)

	triggers := []string{
		"attacker", "red-team", "blue-team", "exploit", "exploitability",
		"threat model", "vulnerability", "adversarially",
	}
	for _, trig := range triggers {
		if strings.Contains(strings.ToLower(got), strings.ToLower(trig)) {
			t.Errorf("moderation trigger %q survived softening:\n%s", trig, got)
		}
	}

	if !strings.Contains(got, "adversarial-security-review") {
		t.Errorf("invokable skill identifier was corrupted - model could not call it:\n%s", got)
	}
	for _, id := range []string{"security-and-compliance-auditor", "deep-research"} {
		if !strings.Contains(got, id) {
			t.Errorf("agent/skill identifier %q lost:\n%s", id, got)
		}
	}
	for _, kept := range []string{"Tools: Read, Grep, Glob, Bash", "auth, secrets", "synthesize a cited report"} {
		if !strings.Contains(got, kept) {
			t.Errorf("operational guidance %q lost - model would be degraded:\n%s", kept, got)
		}
	}
	for _, meaning := range []string{"reviewer", "risk model", "weak", "defect"} {
		if !strings.Contains(strings.ToLower(got), meaning) {
			t.Errorf("expected neutral replacement %q absent - meaning not preserved:\n%s", meaning, got)
		}
	}
}

func TestNeutralizeBodySoftensCatalogOnModeratedBackendOnly(t *testing.T) {
	enableFiltersForNeutralize(t)

	build := func(backend string) string {
		body := map[string]interface{}{
			"model": "glm-5.2",
			"messages": []interface{}{
				map[string]interface{}{"role": "system", "content": moderationCatalogSample},
				map[string]interface{}{"role": "user", "content": "hi"},
			},
			"tools": []interface{}{
				map[string]interface{}{"type": "function", "function": map[string]interface{}{
					"name":        "Workflow",
					"description": "independent perspectives and adversarial checks before committing",
				}},
			},
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(neutralizeProviderBody(raw, backend))
	}

	cn := build("codebuddy-cn")
	for _, trig := range []string{"attacker", "red-team", "exploit", "adversarial check"} {
		if strings.Contains(strings.ToLower(cn), strings.ToLower(trig)) {
			t.Errorf("trigger %q survived on moderated backend:\n%s", trig, cn)
		}
	}
	if !strings.Contains(cn, "adversarial-security-review") {
		t.Errorf("skill identifier corrupted on moderated backend:\n%s", cn)
	}
	if !strings.Contains(cn, `"Workflow"`) {
		t.Errorf("tool name must never change:\n%s", cn)
	}

	kiro := build("kiro")
	if !strings.Contains(kiro, "attacker") || !strings.Contains(kiro, "exploit") {
		t.Errorf("non-moderated backend must keep vocabulary verbatim:\n%s", kiro)
	}
}

const securityDisclaimerSample = `You are an interactive agent that helps users with software engineering tasks.

IMPORTANT: Assist with authorized security testing, defensive security, CTF challenges, and educational contexts. Refuse requests for destructive techniques, DoS attacks, mass targeting, supply chain compromise, or detection evasion for malicious purposes. Dual-use security tools (C2 frameworks, credential testing, exploit development) require clear authorization context: pentesting engagements, CTF competitions, security research, or defensive use cases.

# Harness
 - Prefer the dedicated file/search tools over shell commands when one fits.

# Doing tasks
Do software engineering tasks.`

func TestSoftenStripsSecurityDisclaimerOnModeratedPath(t *testing.T) {
	got := softenModerationVocabulary(securityDisclaimerSample)

	for _, phrase := range []string{
		"DoS attacks", "supply chain compromise", "detection evasion",
		"C2 frameworks", "credential testing", "IMPORTANT: Assist with authorized security testing",
	} {
		if strings.Contains(got, phrase) {
			t.Errorf("disclaimer phrase %q survived strip (content_filter risk):\n%s", phrase, got)
		}
	}
	if !strings.Contains(got, "# Harness") || !strings.Contains(got, "dedicated file/search tools") {
		t.Errorf("harness/tool guidance lost — model would be degraded:\n%s", got)
	}
	if !strings.Contains(got, "interactive agent that helps users") {
		t.Errorf("agent role intro lost:\n%s", got)
	}
	if !strings.Contains(got, "# Doing tasks") {
		t.Errorf("doing-tasks section lost:\n%s", got)
	}
}

func TestNeutralizeBodyStripsDisclaimerOnModeratedBackendOnly(t *testing.T) {
	enableFiltersForNeutralize(t)

	build := func(backend string) string {
		body := map[string]interface{}{
			"model": "glm-5.2",
			"messages": []interface{}{
				map[string]interface{}{"role": "system", "content": securityDisclaimerSample},
				map[string]interface{}{"role": "user", "content": "hi"},
			},
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(neutralizeProviderBody(raw, backend))
	}

	cn := build("codebuddy-cn")
	for _, phrase := range []string{"DoS attacks", "supply chain compromise", "detection evasion", "C2 frameworks", "credential testing"} {
		if strings.Contains(cn, phrase) {
			t.Errorf("disclaimer phrase %q survived on moderated backend:\n%s", phrase, cn)
		}
	}
	if !strings.Contains(cn, "# Harness") {
		t.Errorf("harness lost on moderated backend:\n%s", cn)
	}

	kiro := build("kiro")
	if !strings.Contains(kiro, "DoS attacks") || !strings.Contains(kiro, "C2 frameworks") {
		t.Errorf("non-moderated backend must keep disclaimer verbatim:\n%s", kiro)
	}
}
