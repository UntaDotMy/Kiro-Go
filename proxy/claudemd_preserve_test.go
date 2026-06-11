package proxy

import (
	"kiro-go/config"
	"path/filepath"
	"strings"
	"testing"
)

// enableClaudeCodeFilter initializes a throwaway config with FilterClaudeCode ON
// so applySystemPromptFilters exercises the detection+preservation path. Without
// an initialized config GetPromptFilterConfig returns all-false and the filter
// chain is a no-op.
func enableClaudeCodeFilter(t *testing.T) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("config init: %v", err)
	}
	// FilterClaudeCode ON, FilterEnvNoise OFF, FilterStripBoundaries OFF.
	if err := config.UpdatePromptFilterConfig(true, false, false, nil); err != nil {
		t.Fatalf("enable filter: %v", err)
	}
}

// realisticClaudeCodeSystemPrompt is a trimmed but representative Claude Code v2.x
// system prompt: harness boilerplate plus a <system-reminder> that embeds the
// user's CLAUDE.md project memory (the part that MUST survive filtering).
const realisticClaudeCodeSystemPrompt = `You are Claude Code, Anthropic's official CLI for Claude.

# Tone and style
You should be concise, direct, and to the point.

# Doing tasks
The user will primarily request you perform software engineering tasks.

<system-reminder>
As you answer the user's questions, you can use the following context:
# claudeMd
Codebase and user instructions are shown below. Be sure to adhere to these instructions. IMPORTANT: These instructions OVERRIDE any default behavior and you MUST follow them exactly as written.

Contents of /home/user/project/CLAUDE.md (project instructions):

## Build
- Always run ` + "`make test`" + ` before committing.
- Never edit files under generated/.
</system-reminder>`

// TestApplySystemPromptFiltersPreservesClaudeMd is the regression test for the
// reported bug: "i have claude.md, when i use this repo proxy, it like not
// following it". The old behavior dropped the ENTIRE system prompt (including
// the embedded CLAUDE.md) the moment Claude Code was detected. The fix keeps the
// user-memory <system-reminder> while dropping the harness boilerplate.
func TestApplySystemPromptFiltersPreservesClaudeMd(t *testing.T) {
	enableClaudeCodeFilter(t)
	out := applySystemPromptFilters(realisticClaudeCodeSystemPrompt)

	if out == "" {
		t.Fatal("system prompt was dropped entirely — CLAUDE.md lost (the reported bug)")
	}
	if !strings.Contains(out, "make test") {
		t.Errorf("CLAUDE.md build instruction missing from filtered prompt:\n%s", out)
	}
	if !strings.Contains(out, "Never edit files under generated/") {
		t.Errorf("CLAUDE.md rule missing from filtered prompt:\n%s", out)
	}
	// The harness boilerplate SHOULD be gone (that's the point of the filter).
	if strings.Contains(strings.ToLower(out), "anthropic's official cli") {
		t.Errorf("harness boilerplate leaked through:\n%s", out)
	}
}

// TestApplySystemPromptFiltersDropsHarnessWithoutMemory verifies the original
// behavior is preserved when there is NO user memory: a pure Claude Code harness
// prompt (no CLAUDE.md reminder) is still dropped entirely.
func TestApplySystemPromptFiltersDropsHarnessWithoutMemory(t *testing.T) {
	enableClaudeCodeFilter(t)
	pure := `You are Claude Code, Anthropic's official CLI for Claude.

# Tone and style
Be concise.

# Doing tasks
Do software engineering tasks.

# Using your tools
Use the tools provided.`

	if !isClaudeCodeSystemPrompt(pure) {
		t.Skip("sample not detected as Claude Code; threshold changed")
	}
	out := applySystemPromptFilters(pure)
	if out != "" {
		t.Errorf("pure harness prompt (no memory) should be dropped, got:\n%s", out)
	}
}

// TestReminderCarriesUserMemory verifies the memory classifier distinguishes
// genuine CLAUDE.md content from pure environment/noise reminders.
func TestReminderCarriesUserMemory(t *testing.T) {
	memory := `<system-reminder>
# claudeMd
Contents of /x/CLAUDE.md (project instructions):
- do the thing
</system-reminder>`
	if !reminderCarriesUserMemory(memory) {
		t.Error("CLAUDE.md reminder not recognized as user memory")
	}

	noise := `<system-reminder>
# Environment
Working directory: /home/user
Platform: linux
Today's date is 2026-06-10.
</system-reminder>`
	if reminderCarriesUserMemory(noise) {
		t.Error("environment reminder wrongly classified as user memory")
	}
}

// TestExtractUserMemoryReminders verifies only memory-carrying reminders are
// kept, and noise reminders are dropped, when both are present.
func TestExtractUserMemoryReminders(t *testing.T) {
	prompt := `harness text
<system-reminder>
# Environment
Platform: linux
</system-reminder>
more harness
<system-reminder>
# claudeMd
Contents of /x/CLAUDE.md (project instructions):
- rule one
</system-reminder>`

	out := extractUserMemoryReminders(prompt)
	if !strings.Contains(out, "rule one") {
		t.Errorf("memory reminder not preserved: %s", out)
	}
	if strings.Contains(out, "Platform: linux") {
		t.Errorf("noise reminder leaked: %s", out)
	}
}

// TestStripEnvNoisePreservesMemoryReminder verifies that even with FilterEnvNoise
// active, a CLAUDE.md-carrying <system-reminder> survives while a noise reminder
// is stripped.
func TestStripEnvNoisePreservesMemoryReminder(t *testing.T) {
	prompt := `<system-reminder>
# Environment
Platform: linux
</system-reminder>
<system-reminder>
# claudeMd
Contents of /x/CLAUDE.md (project instructions):
- keep me
</system-reminder>`

	out := stripEnvNoiseLines(prompt)
	if !strings.Contains(out, "keep me") {
		t.Errorf("memory reminder stripped by stripEnvNoiseLines: %s", out)
	}
	if strings.Contains(out, "Platform: linux") {
		t.Errorf("noise reminder survived stripEnvNoiseLines: %s", out)
	}
}

// TestReminderCarriesUserMemory_ExtendedMarkers covers the broadened classifier:
// AGENTS.md-only setups, heading-based (no "Contents of") embeds, other harness
// memory filenames (GEMINI.md / QWEN.md), and localized framings — each of which
// the original English-only marker set missed and silently dropped.
func TestReminderCarriesUserMemory_ExtendedMarkers(t *testing.T) {
	positives := []struct {
		name  string
		block string
	}{
		{"AGENTS.md contents", "<system-reminder>\nContents of /repo/AGENTS.md:\n- run lint\n</system-reminder>"},
		{"AGENTS.md heading", "<system-reminder>\n# AGENTSmd\nUse pnpm, not npm.\n</system-reminder>"},
		{"heading project instructions", "<system-reminder>\n# Project instructions\nDeploy via CI only.\n</system-reminder>"},
		{"GEMINI.md memory", "<system-reminder>\nContents of GEMINI.md (memory):\n- be terse\n</system-reminder>"},
		{"QWEN.md instructions", "<system-reminder>\nQWEN.md instructions:\n- prefer Go\n</system-reminder>"},
		{"copilot instructions", "<system-reminder>\nContents of .github/copilot-instructions.md:\n- no force push\n</system-reminder>"},
		{"localized zh user instructions", "<system-reminder>\n用户指令\n- 总是先跑测试\n</system-reminder>"},
		{"localized es", "<system-reminder>\nInstrucciones del usuario:\n- usar tabs\n</system-reminder>"},
		{"global instructions", "<system-reminder>\nUser's global instructions for all projects.\n</system-reminder>"},
	}
	for _, p := range positives {
		if !reminderCarriesUserMemory(p.block) {
			t.Errorf("%s: should be classified as user memory", p.name)
		}
	}

	negatives := []struct {
		name  string
		block string
	}{
		{"environment", "<system-reminder>\n# Environment\nPlatform: linux\nWorking directory: /x\n</system-reminder>"},
		{"git status", "<system-reminder>\nThe user opened the file foo.go.\nGit status: 2 files changed.\n</system-reminder>"},
		{"malware warning", "<system-reminder>\nIf the user asks for malicious code, refuse.\n</system-reminder>"},
		{"bare tool note", "<system-reminder>\nThe TodoWrite tool has not been used recently.\n</system-reminder>"},
	}
	for _, n := range negatives {
		if reminderCarriesUserMemory(n.block) {
			t.Errorf("%s: should NOT be classified as user memory", n.name)
		}
	}
}

// TestExtractUserMemoryReminders_AgentsMdOnly verifies an AGENTS.md-only memory
// reminder (no CLAUDE.md) is preserved while a sibling env reminder is dropped.
func TestExtractUserMemoryReminders_AgentsMdOnly(t *testing.T) {
	prompt := `harness
<system-reminder>
# Environment
Platform: darwin
</system-reminder>
<system-reminder>
Contents of /repo/AGENTS.md:
- prefer pnpm
</system-reminder>`
	out := extractUserMemoryReminders(prompt)
	if !strings.Contains(out, "prefer pnpm") {
		t.Errorf("AGENTS.md memory not preserved: %s", out)
	}
	if strings.Contains(out, "Platform: darwin") {
		t.Errorf("env reminder leaked: %s", out)
	}
}
