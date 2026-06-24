## Problem
Every request to a GLM or DeepSeek model with a graded effort (e.g.
`xhigh`) logged a WARN:

```
WARN [Effort] Requested "xhigh" for model "rcodebuddycn/glm-5.2" but it advertises no effort support (or the model cache has not been populated yet) — sending WITHOUT graded effort, falling back to thinking on/off
```

…every single request, forever. The message reads like a transient
post-restart cache-miss, but for GLM/DeepSeek it's the **steady state**.

## Root cause
GLM and DeepSeek have **no graded `reasoning_effort` knob** (verified from
official docs in PR #47):

- **GLM** (`docs.z.ai/guides/capabilities/thinking-mode`): binary
  `thinking:{type:enabled|disabled}` toggle.
- **DeepSeek** (`api-docs.deepseek.com/guides/reasoning_model`): always-on
  CoT via `reasoning_content`.

So the static effort dict (`modelEffortDict` in `model_metadata.go`)
deliberately has **no** GLM/DeepSeek entry. When `applyReasoningEffort`
can't map the requested level, `len(levels)==0` and it fires the WARN.

But this is the **intended path**, not a bug: `resolveThinkingWithEffort`
runs *before* `applyReasoningEffort` and correctly engages thinking on/off
for `low/medium/high/xhigh/max`. The request is handled correctly — the
warning is pure noise.

## Fix
Add a data-driven set of known binary-thinking / no-graded-knob model
families:

- `modelNoEffortKnob` — `{glm-*, glm-4*, deepseek-r*, deepseek-reasoner,
  deepseek-v*, deepseek}` (longest-prefix matched, lower-cased).
- `modelIsKnownNoKnob(upstreamModel)` — the classifier.

In `applyReasoningEffort`, when a graded request can't be mapped AND
`len(levels)==0`:
- **known no-knob model** → `logger.Debugf` (silent; thinking path
  engaged correctly).
- **genuinely unrecognized model** → keep the `Warnf` (real transient
  post-restart cache-miss case still surfaces).

The de-prefixed upstream id is passed to the classifier
(`stripRoutingPrefix(modelID)`), so `rcodebuddycn/glm-5.2` resolves to
`glm-5.2` → matches `glm-5`.

## Behavior after fix
- `xhigh` on `rcodebuddycn/glm-5.2`: thinking engages (unchanged), no
  WARN. Stats `effort=none` is correct — no graded level is forwarded
  because GLM has no `output_config.effort` field.
- `xhigh` on a truly unknown model (e.g. a brand-new model before the
  first models refresh): WARN still fires.

## Tests
- `TestModelIsKnownNoKnob` — GLM/DeepSeek (incl. prefixed
  `rcodebuddycn/glm-5.2`, `cbcn/deepseek-v4-pro`) classify as no-knob;
  grok/gpt-5/claude/gemini/gpt-4o/unknown do NOT.
- Existing effort tests unchanged.
- `go build`, `go vet`, `go test ./...` all green.

## Files
- `proxy/model_metadata.go` — `modelNoEffortKnob` + `modelIsKnownNoKnob`
- `proxy/handler.go` — `applyReasoningEffort` Debug-vs-Warn split
- `proxy/model_metadata_test.go` — `TestModelIsKnownNoKnob`
