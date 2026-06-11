# TODO — status after native-search citations + cache/context fixes

Pickup point for the next session/reviewer. Each item says what's **done** (with
code + tests), what's **verified by research** (primary sources, not guessed),
and what genuinely **needs a live API key or CI** that this environment lacks.

Research note: the open questions in the previous revision were closed with
primary-source research (gemini-cli source, Anthropic docs, Alibaba Model Studio
docs, 9router source, ~6 Kiro-gateway reimplementations), NOT model training
knowledge. Findings are cited inline below.

---

## 1. Web search — native-first, Kiro fallback

### Done (code + unit tests)
- Provider-native classification + body injection (unchanged from prior work):
  `nativeWebSearchKind`, `injectNativeWebSearch` in `proxy/websearch_native.go`.
- **NEW — native citation surfacing for Gemini.** `parseGeminiSSE`
  (`proxy/translate_gemini.go`) now parses
  `candidates[].groundingMetadata.groundingChunks[].web.{uri,title}` and
  `webSearchQueries[]`, accumulating the last non-empty occurrence across the
  stream, and emits them via the new `OnWebSearchResults` callback. The Claude
  handlers (`handleClaudeStream` / `handleClaudeNonStream`) splice them into
  native `server_tool_use` + `web_search_tool_result` blocks
  (`spliceNativeCitationBlocks` / `buildClaudeWebSearchContentBlocks` in
  `proxy/websearch_blocks.go`), so a Claude client renders citation chips for the
  native path — not just the Kiro emulation path.
  - Block shape verified against Anthropic's web-search-tool docs
    (`server_tool_use` → `web_search_tool_result` → `web_search_result{title,url,page_age}`).
  - Gemini grounding shape verified against gemini-cli
    `packages/core/src/tools/web-search.ts` (which qwen-code forks).
  - Tests: `proxy/native_websearch_cache_test.go`
    (`TestParseGeminiSSE_GroundingCitations`, `TestSpliceNativeCitationBlocks_*`).

### Verified by research — deliberate non-implementation
- **DashScope / Qwen OpenAI-compatible mode does NOT return search sources.**
  This is an Alibaba Model Studio platform limitation, confirmed by the official
  web-search doc ("The OpenAI-compatible protocol does not support returning
  search sources in responses") and corroborated by CherryHQ/cherry-studio
  #10628. So on the compatible-mode endpoint the grounded *text* answer flows
  back (model cites inline) but there is no structured source list to map —
  **text-only is correct, not a bug.** We do NOT synthesize fake sources by
  regexing URLs out of prose.
  - To get structured DashScope citations we'd have to add a **native DashScope
    mode** branch (`POST /api/v1/services/aigc/text-generation/generation` with
    `search_options.enable_source:true` → `output.search_info.search_results[]`).
    That's a separate request/response envelope; deferred as a follow-up, see §1
    backlog.
- **qwen-code itself removed its built-in `web_search` tool** (docs:
  "Breaking Change: Built-in web_search Tool Removed") and now does web search
  exclusively via **MCP servers** (Alibaba Bailian WebSearch MCP, Tavily, GLM
  WebSearch Prime) — the same MCP architecture Kiro-Go already uses with Kiro's
  `/mcp` endpoint. So the robust cross-provider search path is MCP-based search
  (already implemented + splices citations), and `enable_search` is a lightweight
  grounding bonus on top.

### Backlog (needs a live key or a new subsystem)
- [ ] **Live verification** of the Gemini grounding path against a real Gemini
      key — specifically to confirm whether `groundingMetadata` arrives on every
      SSE chunk or only the final one. The parser is written to be correct either
      way (captures the last non-empty occurrence), but a live capture should
      confirm. **Needs a key; not runnable here.**
- [ ] **DashScope native-mode branch** for structured citations (see above).
      Bigger change (new envelope + usage-key mapping). Until then DashScope
      search is text-only by design.
- [ ] **Live verification** against real DashScope-intl / direct-Anthropic keys
      that the injected `enable_search` / hosted `web_search_20250305` fields are
      accepted end-to-end. Unit tests cover body shaping only. **Needs keys.**

## 1b. web_fetch / web_extractor

### Verified by research — current behavior is the safe one
- Kiro's MCP endpoint (`q.<region>.amazonaws.com/mcp`) exposes **only
  `web_search`** — no `web_fetch` / `fetch` / `read_url` tool. Verified across ~6
  independent Kiro-gateway reimplementations (jwadow/kiro-gateway,
  NguyenSiTrung/CLIProxyAPI, MarshuMax/reverse_proxy, etc.); all show
  `tools/list` = `{web_search}` only.
- A client-sent `web_fetch` (hosted, type-stamped) tool is therefore **cleanly
  dropped** by `isAnthropicServerTool` (`proxy/translator.go`) before reaching
  upstream — it never 400s, and because the tool is removed from the catalog the
  model never emits a fetch call, so there is nothing to error on. The model
  answers from training/other tools. This is the spec-safe graceful behavior.
- [ ] **Not implemented (by design): a real web_fetch fetcher.** Implementing a
      genuine `web_fetch_tool_result` would require our own HTTP fetcher
      (readability extraction, PDF→base64) — a medium-risk new network capability
      with SSRF surface that MUST ship behind a config flag + allowlist + private-
      IP/metadata-endpoint blocking. Deliberately left for a scoped follow-up; do
      NOT route web_fetch through Kiro's `web_search` and pretend the snippet is
      page content.

---

## 2. CLAUDE.md / system-prompt preservation

### Done (code + tests)
- `applySystemPromptFilters` preserves user/project memory `<system-reminder>`
  blocks (unchanged core from prior work).
- **NEW — broadened marker coverage** in `reminderCarriesUserMemory`
  (`proxy/translator.go`): now recognizes AGENTS.md-only setups, heading-based
  embeds (`# Project instructions` with no "Contents of" header), other harness
  memory filenames (GEMINI.md / QWEN.md / copilot-instructions.md /
  CLAUDE.local.md), global-instruction framing, and several localized framings
  (zh/es/fr/de/ja/pt). False-positive bias is intentional: keeping a little extra
  system text is harmless; dropping a user's CLAUDE.md is not.
  - Tests: `proxy/claudemd_preserve_test.go`
    (`TestReminderCarriesUserMemory_ExtendedMarkers`,
    `TestExtractUserMemoryReminders_AgentsMdOnly`).

### Backlog (needs a live capture)
- [ ] Spot-check against a live capture from a *future* Claude Code build if it
      moves memory out of `<system-reminder>` blocks entirely. The heading-based
      markers now cover the common non-reminder case, but framing can drift.

---

## 3. Cache & context — RESOLVED (was "investigation incomplete")

The previous revision flagged this as unresolved ("both Kiro and non-Kiro feel
off"). Root cause and fix below, modeled on 9router's honest-passthrough
approach (verified against decolua/9router `usageTracking.js` + translators).

### Done (code + tests)
- **Cross-backend cache rule enforced.** New `resolveResponseCache`
  (`proxy/cache_tracker.go`) + `isKiro` gating in both Claude handlers:
  - **Kiro backend:** the local `promptCache` estimator stays authoritative
    (Kiro's upstream reports no cache split) — unchanged behavior.
  - **Any non-Kiro backend:** the Kiro estimate is **never** emitted. We pass
    through the provider's **real** reported cache, or emit no cache fields at
    all. This fixes the bug where a DashScope/Gemini response that never cached
    could still carry a Kiro-estimated `cache_read`/`cache_creation`, making a
    client's running context tally drift upward every turn.
- **Real upstream cache passthrough** via the new `OnCacheUsage` callback:
  - OpenAI-compatible: `usage.prompt_tokens_details.cached_tokens` (DeepSeek
    fallback `usage.prompt_cache_hit_tokens`) — `proxy/translate_openai.go`.
  - Gemini: `usageMetadata.cachedContentTokenCount` — `proxy/translate_gemini.go`.
  - Anthropic-compatible: `usage.cache_read_input_tokens` /
    `cache_creation_input_tokens` — `proxy/translate_anthropic.go`.
  - Emitted only when > 0 (no zero stubs).
- **Per-family context window** for non-Kiro models. New
  `familyContextWindowFor` (`proxy/context_window.go`) + hook in
  `getContextWindowSize` (`proxy/kiro.go`): a Gemini-2.5 (1M) / qwen-turbo (1M) /
  qwen-plus (128K) model now advertises a sane window instead of the flat 200K
  Claude default, so the client's `used / window` gauge and compaction timing are
  correct. Live `tokenLimits.maxInputTokens` still wins when the provider's
  `/models` reports one; the table is fallback-only.
  - Tests: `TestFamilyContextWindowFor`, `TestGetContextWindowSize_NonKiroFamilies`,
    `TestResolveResponseCache_*`, `TestParse{OpenAI,Gemini,Anthropic}SSE_*` in
    `proxy/native_websearch_cache_test.go`.

### How cache + context are computed (user-facing explanation)

- **Context window (the denominator of the usage gauge):** resolved live-first.
  For a Kiro/Claude model we read Kiro's advertised `tokenLimits.maxInputTokens`;
  if absent, we version-parse the Claude id (Opus/Sonnet/Haiku ≥ 4.6 → 1M, else
  200K). For a non-Kiro model we use the provider's live `/models` window if it
  reports one, else a documented per-family fallback (Gemini 2.5 ≈ 1.05M, qwen-
  turbo 1M, qwen-plus/qwen2.5 128K, qwen-max 32K, glm/kimi/deepseek 128K),
  else 200K. These fallbacks are family-level doc values, not live-confirmed per
  exact id — a live `/models` value always overrides them.

- **Input tokens (the numerator):** exact upstream count from the event stream
  wins; if the provider sent none we derive it from the model's
  `contextUsagePercentage × window`; only as a last resort do we estimate locally
  from the request. (`resolveInputTokens` precedence — unchanged.)

- **Cache (`cache_read` / `cache_creation`):**
  - On **Kiro**, these are a **local estimate**. Kiro's upstream does not return a
    cache split, so a per-account prompt-cache tracker fingerprints cacheable
    blocks and estimates the read/creation split, capped so
    `billed + cache_read + cache_creation` never exceeds the real input total.
  - On **every non-Kiro provider**, these are **only ever the provider's own
    reported numbers** (OpenAI `cached_tokens`, Gemini `cachedContentTokenCount`,
    Anthropic `cache_*_input_tokens`). If the provider reports nothing, we emit
    nothing — we never show a Kiro-style estimate for a provider that didn't
    cache. So a cache number on a non-Kiro response is real, or it's absent.

### Backlog (needs a live key)
- [ ] Confirm the exact per-id context windows against each provider's live
      `/models` (the table is a documented-family fallback; prefer live values).
- [ ] Confirm Gemini `cachedContentTokenCount` and DashScope
      `prompt_tokens_details.cached_tokens` populate as expected against real
      keys (only meaningful when explicit context caching is in use).

---

## Housekeeping
- [x] Stray `nul'` device-name file — not present in the tree (already gone).
- [x] Accidental `qwen-code/` clone left by a research step — removed.
- [ ] `go test -race` — **still not runnable here**: needs a C toolchain
      (CGO/gcc) which this Windows/go1.26 env lacks (`cgo: C compiler "gcc" not
      found`). All concurrency paths are covered by the non-race unit tests; run
      `-race` on CI where a C compiler is present.
