## Problem
After PR #47, the background refresh still logged every 5 minutes for a
custom-account reseller backend:

```
INFO [rcodebuddycn] /models fetch failed (HTTP 404 from https://dpc-tcb.chicross.cn/api/v2/models); using static catalog of 5 models (advisory)
```

The static-catalog skip added in #47 was meant to kill exactly this noise,
but it never fired for `rcodebuddycn`.

## Root cause
`backendShipsStaticCatalog()` only checked:
1. builtin catalog entries with a non-empty `Models` field, and
2. user `ProviderConfig`s with `Models` AND NOT `FetchModels`.

It missed the **self-contained custom-account** path: a user-added account
whose `backend` id is its own routing prefix and whose pinned model list
lives INLINE on the account as `CustomModels` (no shared `ProviderConfig`,
no builtin catalog row). That's exactly the shape of a reseller endpoint
like `rcodebuddycn -> dpc-tcb.chicross.cn`:

```json
{
  "backend": "rcodebuddycn",
  "baseURLOverride": "https://dpc-tcb.chicross.cn/api/v2",
  "customDialect": "openai",
  "customModels": ["glm-5.2", "deepseek-v4-pro", "deepseek-v4-flash", "minimax-m3", "kimi-k2.7"]
}
```

So `backendShipsStaticCatalog("rcodebuddycn")` returned false → the
fast-path in `refreshModelsCache` was never entered → the live
`FetchModelsForAccount` ran, 404'd, and re-logged the advisory fallback
every tick.

## Fix
Extend `backendShipsStaticCatalog` to also return true when
`config.GetCustomAccountByBackend(backend)` yields an account with a
non-empty `CustomModels`. Custom accounts have no `FetchModels` toggle, so
a pinned `CustomModels` list IS the static-only signal. The sibling lookup
in `GetCustomAccountByBackend` covers bulk-added keys whose inline fields
live on the first-added sibling.

## Tests
- `TestBackendShipsStaticCatalogCustomAccount` — the rcodebuddycn shape
  (5-model custom account) is now flagged static-only.
- `TestBackendShipsStaticCatalogCustomAccountNoModels` — a custom account
  with NO `CustomModels` is NOT flagged (it needs the live fetch).
- Existing `TestBackendShipsStaticCatalog` (builtins) still passes.
- `go build`, `go vet`, `go test ./...` all green.

## Files
- `proxy/provider_catalog.go` — extend `backendShipsStaticCatalog`
- `proxy/codebuddy_catalog_test.go` — two new tests
