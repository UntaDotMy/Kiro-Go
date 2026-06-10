# TODO — incomplete / follow-up work

Tracks what is **not** finished from the multi-provider + web-search + CLAUDE.md
goal so the next session (or reviewer) has an exact pickup point. Items are
grouped by area; each says what's done, what's missing, and where to look.

## 1. Web search — native-first, Kiro fallback

**Done (code + unit tests):**
- Provider-native classification: `nativeWebSearchKind` in `proxy/websearch_native.go`
  recognizes DashScope/Qwen (`enable_search`), Gemini (`google_search`), and
  real Anthropic (`web_search_20250305`). Anthropic-*compatible* hosts
  (glm/kimi/minimax) are deliberately NOT treated as native.
- Native injection into the outbound body: `injectNativeWebSearch`, wired in
  `genericProvider.buildRequest` (`proxy/provider_generic.go`).
- Emulation fallback: `handleClaudeWebSearch` now runs generation on the
  request's OWN backend (`runProviderCollect` in `proxy/websearch_loop.go`) and
  only uses a Kiro account for the MCP search side-call
  (`firstUsableKiroAccount`). Gate in `handler.go` is `shouldEmulateWebSearch`.
- No-Kiro path: if a provider is non-native and no Kiro account exists, the tool
  is dropped and the model answers — **never a 404**.

**Incomplete / not verified:**
- [ ] **Live verification** against a real DashScope-intl key
      (`https://dashscope-intl.aliyuncs.com/compatible-mode/v1`), a Gemini key,
      and a direct Anthropic key. Unit tests cover the body shaping; nobody has
      confirmed the upstreams accept the injected fields end-to-end.
- [ ] **Native citation surfacing.** When a provider runs search natively, the
      grounded *text* answer flows back fine, but provider-native citation
      metadata is NOT mapped to Anthropic `web_search_tool_result` blocks:
      - Gemini returns `groundingMetadata` (search queries + source URIs) — not
        parsed in `parseGeminiSSE`.
      - DashScope returns search/citation info in its response — not parsed in
        `parseOpenAISSE`.
      Result: Claude Code gets the answer but won't render native citation
      chips for the native path (the Kiro emulation path DOES splice citation
      blocks). Decide whether to map these or leave as text-only.
- [ ] **`search_options` for DashScope** (forced_search, enable_citation,
      enable_source) was intentionally left at provider defaults because the
      exact sub-schema wasn't confirmed from primary docs. Revisit if we want
      forced search or citation tuning.
- [ ] **`web_fetch` / `web_extractor`**: only `web_search` is handled. Clients
      that send a `web_fetch` tool still get it dropped. DashScope Responses API
      pairs `web_search` + `web_extractor`; not implemented.

## 2. CLAUDE.md / system-prompt preservation

**Done (code + tests):**
- `applySystemPromptFilters` (`proxy/translator.go`) no longer drops the WHOLE
  system prompt when Claude Code is detected. It preserves `<system-reminder>`
  blocks that carry genuine user/project memory (CLAUDE.md / AGENTS.md) via
  `extractUserMemoryReminders` + `reminderCarriesUserMemory`, dropping only the
  harness boilerplate.
- `stripEnvNoiseLines` now keeps memory-carrying reminders even when
  `FilterEnvNoise` is on.
- Applies to BOTH paths: Kiro (`buildClaudeSystemPrompt`) and non-Kiro
  (`OpenAIToKiro`).
- Tests: `proxy/claudemd_preserve_test.go`.

**Incomplete / not verified:**
- [ ] **Live confirmation** that Claude Code's *current* build embeds CLAUDE.md
      inside `<system-reminder>` (verified against the harness reminder in this
      session's own context, but Claude Code versions drift). If a future
      version moves memory into a plain `# Project instructions` heading instead
      of a reminder block, `reminderCarriesUserMemory` won't catch it — the
      heading-based markers in the classifier cover the common case but should
      be re-checked against a live capture.
- [ ] **Marker coverage**: `reminderCarriesUserMemory` matches the English
      Claude Code memory framing. Localized or AGENTS.md-only setups should be
      spot-checked.

## 3. Cache & context — INVESTIGATION INCOMPLETE

The user asked us to "check cache and context" and noted both Kiro and non-Kiro
feel off. This was being traced when work paused. **No code change made yet.**

**What we know:**
- `promptCacheTracker` (`proxy/cache_tracker.go`) is a LOCAL ESTIMATOR keyed per
  Kiro account; it fingerprints cacheable blocks and reports
  `cache_read`/`cache_creation`. `reconcileCacheUsage` caps the estimate to the
  authoritative `input_tokens` so the emitted usage can't exceed the real total.
- Context window: `contextWindowForModel` + the `OnContextUsage` callback derive
  input tokens from the model's `contextUsagePercentage` when upstream omits a
  hard count (`resolveInputTokens` precedence).

**Open questions to resolve (then explain to the user):**
- [ ] **Non-Kiro cache accounting.** `cacheProfile` is built and passed into the
      Claude response path regardless of backend. For a non-Kiro provider the
      upstream returns its OWN usage (or none); confirm we are not emitting a
      Kiro-estimated `cache_read`/`cache_creation` for a DashScope response that
      never cached. Check `buildClaudeUsageMap` callers on the generic path.
- [ ] **Context window per non-Kiro model.** `contextWindowForModel` is
      Kiro-centric; verify it returns a sane window for dashscope/qwen/gemini
      models so the `%`-derived token fallback isn't wildly wrong.
- [ ] Write the user-facing explanation of how cache + context are computed and
      what is real vs. estimated per backend.

## Housekeeping
- [ ] `nul'` — stray Windows device-name file in the repo root. Untracked,
      harmless, NOT part of this work; left alone (could not remove via the
      device path). Should be deleted out-of-band.
- [ ] `go test -race` not run (no cgo/C toolchain in this env). Concurrency
      paths are covered by explicit accounting tests; run race on CI.
