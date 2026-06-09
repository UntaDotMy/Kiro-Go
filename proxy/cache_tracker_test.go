package proxy

import (
	"strings"
	"testing"
	"time"
)

func TestPromptCacheTrackerComputeAndUpdate(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	longSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": longSystem,
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
				},
			},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
	}

	profile := tracker.BuildClaudeProfile(req, 120)
	if profile == nil {
		t.Fatalf("expected cache profile to be built")
	}

	first := tracker.Compute("acct-1", profile)
	if first.CacheCreationInputTokens <= 0 {
		t.Fatalf("expected first request to create cache tokens, got %+v", first)
	}
	if first.CacheReadInputTokens != 0 {
		t.Fatalf("expected first request to have zero cache reads, got %+v", first)
	}

	tracker.Update("acct-1", profile)
	second := tracker.Compute("acct-1", profile)
	if second.CacheReadInputTokens <= 0 {
		t.Fatalf("expected repeated request to read cache tokens, got %+v", second)
	}
	if second.CacheCreationInputTokens != 0 {
		t.Fatalf("expected repeated request to avoid cache creation, got %+v", second)
	}
}

func TestBuildClaudeUsageMapIncludesCacheFields(t *testing.T) {
	usage := promptCacheUsage{
		CacheCreationInputTokens:   30,
		CacheReadInputTokens:       20,
		CacheCreation5mInputTokens: 10,
		CacheCreation1hInputTokens: 20,
	}

	m := buildClaudeUsageMap(100, 50, usage, true)

	if got := m["input_tokens"]; got != 50 {
		t.Fatalf("expected billed input tokens 50, got %#v", got)
	}
	if got := m["cache_creation_input_tokens"]; got != 30 {
		t.Fatalf("expected cache creation tokens 30, got %#v", got)
	}
	if got := m["cache_read_input_tokens"]; got != 20 {
		t.Fatalf("expected cache read tokens 20, got %#v", got)
	}
	creation, ok := m["cache_creation"].(map[string]int)
	if !ok {
		t.Fatalf("expected typed cache creation map, got %#v", m["cache_creation"])
	}
	if creation["ephemeral_5m_input_tokens"] != 10 || creation["ephemeral_1h_input_tokens"] != 20 {
		t.Fatalf("unexpected ttl breakdown: %#v", creation)
	}
}

// TestPromptCacheStableAcrossBillingHeaderDrift verifies that Claude Code's
// per-request "x-anthropic-billing-header: cc_version=...; cch=...;" system
// block (whose content drifts on every request) does not break cache hits.
// The tracker should ignore that metadata when fingerprinting cached prefixes.
func TestPromptCacheStableAcrossBillingHeaderDrift(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	mainSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	build := func(billingHdr string) *ClaudeRequest {
		return &ClaudeRequest{
			Model: "claude-sonnet-4.5",
			System: []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": billingHdr,
				},
				map[string]interface{}{
					"type": "text",
					"text": mainSystem,
					"cache_control": map[string]interface{}{
						"type": "ephemeral",
					},
				},
			},
			Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
		}
	}

	req1 := build("x-anthropic-billing-header: cc_version=2.1.87.1; cch=aaaa;")
	profile1 := tracker.BuildClaudeProfile(req1, 2048)
	if profile1 == nil {
		t.Fatalf("profile1 should be built")
	}
	first := tracker.Compute("acct-1", profile1)
	if first.CacheReadInputTokens != 0 {
		t.Fatalf("expected no cache read on first request, got %+v", first)
	}
	tracker.Update("acct-1", profile1)

	req2 := build("x-anthropic-billing-header: cc_version=2.1.87.42; cch=bbbb; padding=xxyyzz;")
	profile2 := tracker.BuildClaudeProfile(req2, 2048)
	if profile2 == nil {
		t.Fatalf("profile2 should be built")
	}
	second := tracker.Compute("acct-1", profile2)
	if second.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read after billing header drift, got %+v", second)
	}
}

func TestPromptCacheStableWhenBillingHeaderAppearsOrDisappears(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	mainSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	build := func(includeBilling bool) *ClaudeRequest {
		system := []interface{}{}
		if includeBilling {
			system = append(system, map[string]interface{}{
				"type": "text",
				"text": "x-anthropic-billing-header: cc_version=2.1.87.1; cch=aaaa;",
			})
		}
		system = append(system, map[string]interface{}{
			"type": "text",
			"text": mainSystem,
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
			},
		})
		return &ClaudeRequest{
			Model:    "claude-sonnet-4.5",
			System:   system,
			Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
		}
	}

	withBilling := tracker.BuildClaudeProfile(build(true), 2048)
	if withBilling == nil {
		t.Fatalf("profile with billing header should be built")
	}
	tracker.Update("acct-1", withBilling)

	withoutBilling := tracker.BuildClaudeProfile(build(false), 2048)
	if withoutBilling == nil {
		t.Fatalf("profile without billing header should be built")
	}
	result := tracker.Compute("acct-1", withoutBilling)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read when billing header disappears, got %+v", result)
	}
}

func TestCanonicalCacheValueIgnoresPositionKeys(t *testing.T) {
	first := canonicalizeCacheValue(stripCachePositionKeys(map[string]interface{}{
		"kind":         "system",
		"system_index": 0,
		"block": map[string]interface{}{
			"type": "text",
			"text": "stable",
		},
	}))
	second := canonicalizeCacheValue(stripCachePositionKeys(map[string]interface{}{
		"kind":         "system",
		"system_index": 1,
		"block": map[string]interface{}{
			"type": "text",
			"text": "stable",
		},
	}))
	if first != second {
		t.Fatalf("expected position keys to be ignored, got %q vs %q", first, second)
	}
}

func TestCanonicalCacheValuePreservesSemanticPositionKeys(t *testing.T) {
	first := canonicalizeCacheValue(map[string]interface{}{
		"kind": "system",
		"block": map[string]interface{}{
			"type":        "text",
			"text":        "stable",
			"block_index": 1,
		},
	})
	second := canonicalizeCacheValue(map[string]interface{}{
		"kind": "system",
		"block": map[string]interface{}{
			"type":        "text",
			"text":        "stable",
			"block_index": 2,
		},
	})
	if first == second {
		t.Fatalf("expected semantic block_index fields to remain fingerprinted")
	}
}

// TestPromptCacheImplicitBreakpointAtMessageEnd verifies that once any
// explicit cache_control breakpoint has been seen, subsequent message-end
// boundaries act as implicit breakpoints. This allows multi-turn conversations
// to hit earlier stored prefix fingerprints even when the newest messages
// lack explicit cache_control.
func TestPromptCacheImplicitBreakpointAtMessageEnd(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	systemText := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	baseSystem := []interface{}{
		map[string]interface{}{
			"type": "text",
			"text": systemText,
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
			},
		},
	}

	// Round 1: single user message.
	req1 := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   baseSystem,
		Messages: []ClaudeMessage{{Role: "user", Content: "question one"}},
	}
	profile1 := tracker.BuildClaudeProfile(req1, 2048)
	if profile1 == nil {
		t.Fatalf("profile1 should be built")
	}
	tracker.Update("acct-1", profile1)

	// Round 2: conversation continues with new messages. The latest user
	// message has no explicit cache_control; it should still hit the stored
	// prefix via the implicit message-end breakpoint.
	req2 := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: baseSystem,
		Messages: []ClaudeMessage{
			{Role: "user", Content: "question one"},
			{Role: "assistant", Content: "answer one"},
			{Role: "user", Content: "follow-up question"},
		},
	}
	profile2 := tracker.BuildClaudeProfile(req2, 4096)
	if profile2 == nil {
		t.Fatalf("profile2 should be built")
	}
	result := tracker.Compute("acct-1", profile2)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read via implicit message-end breakpoint, got %+v", result)
	}
}

func TestReconcileCacheUsage(t *testing.T) {
	assertInvariant := func(t *testing.T, inputTokens int, result promptCacheUsage) {
		t.Helper()
		billed := billedClaudeInputTokens(inputTokens, result)
		total := billed + result.CacheCreationInputTokens + result.CacheReadInputTokens
		if inputTokens <= 0 {
			if total != 0 {
				t.Fatalf("invariant: expected total 0 for non-positive input, got billed=%d creation=%d read=%d total=%d",
					billed, result.CacheCreationInputTokens, result.CacheReadInputTokens, total)
			}
			return
		}
		if total != inputTokens {
			t.Fatalf("invariant: billed(%d) + creation(%d) + read(%d) = %d, want %d",
				billed, result.CacheCreationInputTokens, result.CacheReadInputTokens, total, inputTokens)
		}
	}

	t.Run("no_overshoot", func(t *testing.T) {
		input := 1000
		usage := promptCacheUsage{
			CacheCreationInputTokens:   200,
			CacheReadInputTokens:       300,
			CacheCreation5mInputTokens: 100,
			CacheCreation1hInputTokens: 100,
		}
		result := reconcileCacheUsage(input, usage)

		if result.CacheCreationInputTokens != 200 {
			t.Fatalf("creation: got %d, want 200", result.CacheCreationInputTokens)
		}
		if result.CacheReadInputTokens != 300 {
			t.Fatalf("read: got %d, want 300", result.CacheReadInputTokens)
		}
		if result.CacheCreation5mInputTokens != 100 {
			t.Fatalf("c5: got %d, want 100", result.CacheCreation5mInputTokens)
		}
		if result.CacheCreation1hInputTokens != 100 {
			t.Fatalf("c1: got %d, want 100", result.CacheCreation1hInputTokens)
		}
		assertInvariant(t, input, result)
	})

	t.Run("overshoot_live_bug", func(t *testing.T) {
		input := 322495
		usage := promptCacheUsage{
			CacheCreationInputTokens:   444208,
			CacheReadInputTokens:       0,
			CacheCreation5mInputTokens: 222104,
			CacheCreation1hInputTokens: 222104,
		}
		result := reconcileCacheUsage(input, usage)

		if result.CacheCreationInputTokens != 322495 {
			t.Fatalf("creation: got %d, want 322495", result.CacheCreationInputTokens)
		}
		if result.CacheReadInputTokens != 0 {
			t.Fatalf("read: got %d, want 0", result.CacheReadInputTokens)
		}
		if result.CacheCreation5mInputTokens+result.CacheCreation1hInputTokens != 322495 {
			t.Fatalf("c5+c1: got %d+%d=%d, want sum 322495",
				result.CacheCreation5mInputTokens, result.CacheCreation1hInputTokens,
				result.CacheCreation5mInputTokens+result.CacheCreation1hInputTokens)
		}
		assertInvariant(t, input, result)
	})

	t.Run("read_overshoot", func(t *testing.T) {
		input := 500
		usage := promptCacheUsage{
			CacheCreationInputTokens: 0,
			CacheReadInputTokens:     800,
		}
		result := reconcileCacheUsage(input, usage)

		if result.CacheCreationInputTokens != 0 {
			t.Fatalf("creation: got %d, want 0", result.CacheCreationInputTokens)
		}
		if result.CacheReadInputTokens != 500 {
			t.Fatalf("read: got %d, want 500", result.CacheReadInputTokens)
		}
		assertInvariant(t, input, result)
	})

	t.Run("both_overshoot", func(t *testing.T) {
		input := 1000
		usage := promptCacheUsage{
			CacheCreationInputTokens:   600,
			CacheReadInputTokens:       600,
			CacheCreation5mInputTokens: 300,
			CacheCreation1hInputTokens: 300,
		}
		result := reconcileCacheUsage(input, usage)

		// read=600 fits within input, creation capped to remaining space (1000-600=400)
		if result.CacheReadInputTokens != 600 {
			t.Fatalf("read: got %d, want 600", result.CacheReadInputTokens)
		}
		if result.CacheCreationInputTokens != 400 {
			t.Fatalf("creation: got %d, want 400", result.CacheCreationInputTokens)
		}
		assertInvariant(t, input, result)
	})

	t.Run("zero_input", func(t *testing.T) {
		input := 0
		usage := promptCacheUsage{
			CacheCreationInputTokens: 100,
		}
		result := reconcileCacheUsage(input, usage)

		if result.CacheCreationInputTokens != 0 || result.CacheReadInputTokens != 0 ||
			result.CacheCreation5mInputTokens != 0 || result.CacheCreation1hInputTokens != 0 {
			t.Fatalf("expected all zeros, got %+v", result)
		}
		assertInvariant(t, input, result)
	})

	t.Run("negative_input", func(t *testing.T) {
		input := -5
		usage := promptCacheUsage{
			CacheCreationInputTokens: 100,
		}
		result := reconcileCacheUsage(input, usage)

		if result.CacheCreationInputTokens != 0 || result.CacheReadInputTokens != 0 ||
			result.CacheCreation5mInputTokens != 0 || result.CacheCreation1hInputTokens != 0 {
			t.Fatalf("expected all zeros, got %+v", result)
		}
		assertInvariant(t, input, result)
	})

	t.Run("creation_breakdown_rescale", func(t *testing.T) {
		input := 1000
		usage := promptCacheUsage{
			CacheCreationInputTokens:   800,
			CacheReadInputTokens:       400,
			CacheCreation5mInputTokens: 600,
			CacheCreation1hInputTokens: 200,
		}
		result := reconcileCacheUsage(input, usage)

		// read=400 fits, maxCreation=1000-400=600, creation capped from 800→600
		if result.CacheReadInputTokens != 400 {
			t.Fatalf("read: got %d, want 400", result.CacheReadInputTokens)
		}
		if result.CacheCreationInputTokens != 600 {
			t.Fatalf("creation: got %d, want 600", result.CacheCreationInputTokens)
		}
		// c5 = 600*600/800 = 450, c1 = 600-450 = 150
		if result.CacheCreation5mInputTokens != 450 {
			t.Fatalf("c5: got %d, want 450", result.CacheCreation5mInputTokens)
		}
		if result.CacheCreation1hInputTokens != 150 {
			t.Fatalf("c1: got %d, want 150", result.CacheCreation1hInputTokens)
		}
		assertInvariant(t, input, result)
	})
}
