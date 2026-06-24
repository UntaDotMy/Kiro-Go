## Summary

Three changes that improve correctness for non-Kiro providers and add
truncation diagnostics.

### 1. Skip background model refresh for static-catalog providers
Providers that ship a fixed model list and have no working `/models`
endpoint (e.g. `codebuddy-cn`) no longer burn a network round-trip every
refresh cycle on a fetch that always 404s.

- New `backendShipsStaticCatalog()` in `proxy/provider_catalog.go`:
  returns true for builtins with a non-empty `Models` field, and for user
  `ProviderConfig`s that have `Models` AND NOT `FetchModels`.
- Fast-path in `refreshModelsCache()` (`proxy/handler.go`): for
  static-only backends, seed the cached catalog directly from the static
  `Models` list — no upstream call. Quota refresh is unaffected (still
  runs via the independent `refreshAllAccounts()` path).
- `FetchModels` in a user `ProviderConfig` is honoured: when set, the
  backend is treated as live-fetch even if it also lists `Models`.

### 2. Model metadata dictionary (verified, not assumed)
New `proxy/model_metadata.go` with two static dictionaries consulted as a
fallback when a model is absent from the live Kiro cache:

- `modelContextWindow` — precise per-model context windows, and
- `modelEffortDict` — graded `reasoning_effort` levels.

Every entry is sourced from official docs, cited inline:

- **GLM / Z.ai** (`docs.z.ai/guides/llm` spec tables): GLM-4.5=128K,
  GLM-4.6/4.7/5/5.1/5-Turbo=200K, **GLM-5.2=1M** (was wrongly 200K).
  GLM uses a **binary** `thinking` toggle — NOT a graded effort enum, so
  it is deliberately not listed in the effort dict (a graded request now
  engages the thinking path silently instead of logging a false warning).
- **xAI Grok** (`docs.x.ai/docs/guides/reasoning`): effort enum
  `{none,low,medium,high}` → listed `{low,medium,high}`.
- **OpenAI** (`platform.openai.com/docs/guides/reasoning`):
  model-dependent `{none,minimal,low,medium,high,xhigh}` → conservative
  graded set `{low,medium,high}`.
- **DeepSeek** (`api-docs.deepseek.com`): always-on `reasoning_content`,
  no effort knob → NOT listed (was wrongly listed before).
- **Gemini**: `thinkingConfig.thinkingLevel` (Gemini-specific enum), not
  OpenAI's scale → listed as a reasoning-model marker with a documented
  proxy rank.
- **MiniMax / Kimi / MiMo**: unverified → NOT listed rather than invented.
- **Anthropic Claude**: mirrors the live `output_config.effort` schema
  already observed in `reasoning_effort.go`.

Wiring (`proxy/handler.go`):
- `effortLevelsForModel()` falls back to the static dict ONLY on a cache
  miss (a live cache entry — even with an empty schema — is authoritative).
- `contextWindowForModel()` and `buildModelInfo()` fall back to the static
  dict before the Claude-version parse / 200K default, so `/v1/models`
  advertises real windows for non-Kiro providers (e.g. `cbcn/glm-5.2`
  now advertises 1M instead of the flat 200K).
- `stripRoutingPrefix()` de-prefixes client ids (`cbcn/glm-5.2`,
  `or/anthropic/claude-...`) to the upstream model id for matching.

`proxy/context_window.go` family table also corrected: GLM-5.2=1M listed
before the bare `glm-5` prefix so longest-prefix match picks 1M.

### 3. Truncation diagnostics (root cause NOT yet patched)
Some providers' streamed responses appear truncated mid-stream then arrive
in a burst at completion. Rather than blindly patch the SSE parsers
without evidence, this adds capture so the next reproduction yields a raw
upstream stream to analyse.

- New `boundedBuffer` ring type in `proxy/outbound_dump.go` (preserves the
  tail, 8 MiB cap) with tests.
- `provider_generic.go` `Call()` tees the raw upstream SSE via
  `io.TeeReader` into the debug capture dir for ALL generic providers when
  debug capture is enabled (`CODEBUDDY_CN_DUMP=<dir>` env var or the admin
  Debug Mode toggle). The captured `<backend>-resp-*.json` is what's needed
  to confirm whether truncation is provider-side (gzip bursts, chunked
  batching) or parser-side.

## Tests
- `go build`, `go vet`, `go test ./...` all pass locally.
- New: `TestBackendShipsStaticCatalog`, `TestStaticEffortLevelsForModel`,
  `TestStaticContextWindowForModel`,
  `TestEffortLevelsForModelFallsBackToStaticDictWhenNotCached`,
  `TestBoundedBufferPreservesTail`, `TestBoundedBufferZeroCapDefaults`.
- Updated `TestContextWindowForModel` to the verified GLM-5.2 = 1M window.
- CI runs `go test -race -count=1` (race not runnable locally — no gcc —
  but the suite is pure Go with no new shared-mutable state).

## Files
- `proxy/model_metadata.go` (new) — dictionaries + prefix stripping
- `proxy/model_metadata_test.go` (new)
- `proxy/outbound_dump.go` — `boundedBuffer` for raw capture
- `proxy/outbound_dump_test.go` (new)
- `proxy/provider_catalog.go` — `backendShipsStaticCatalog()`
- `proxy/provider_generic.go` — raw SSE tee capture
- `proxy/handler.go` — static-catalog fast-path + dict fallback wiring
- `proxy/context_window.go` — corrected GLM windows
- `proxy/codebuddy_catalog_test.go`, `proxy/context_window_test.go` — test updates
