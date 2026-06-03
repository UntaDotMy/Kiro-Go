package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"kiro-go/stats"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// apiRevealAPIKey returns the full secret for one key. The list endpoint
// masks every key by design so an admin XSS / accidental screenshare
// doesn't leak the secret; this endpoint is the explicit "copy key"
// affordance — the operator clicks a button per row, the dashboard fetches
// the full secret over the same admin-authed channel, copies it to the
// clipboard, and discards it from memory. The endpoint requires the
// admin password (enforced by the routing layer that wraps every
// /admin/api/* path), so the security boundary is unchanged: anyone who
// could already authenticate can already see the secrets in config.json.
func (h *Handler) apiRevealAPIKey(w http.ResponseWriter, r *http.Request, id string) {
	for _, k := range config.GetAPIKeys() {
		if k.ID == id {
			json.NewEncoder(w).Encode(map[string]string{"key": k.Key})
			return
		}
	}
	w.WriteHeader(404)
	json.NewEncoder(w).Encode(map[string]string{"error": "Key not found"})
}

// apiListAPIKeys returns all configured API keys with their counters and
// limits. Secrets are masked to the last 4 characters of each key (e.g.
// "sk-kg-...abcd") so an admin XSS / accidental screenshare cannot leak
// the full secret. The full secret is returned only by apiCreateAPIKey,
// once, at creation. To rotate a lost key, delete it and create a new one.
func (h *Handler) apiListAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys := config.GetAPIKeys()
	for i := range keys {
		keys[i].Key = maskAPIKeySecret(keys[i].Key)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"keys": keys,
	})
}

// maskAPIKeySecret turns "sk-kg-abcdef...wxyz" into "sk-kg-...wxyz". For very
// short keys (< 8 chars total) we mask everything to avoid leaking the
// majority of the secret.
func maskAPIKeySecret(s string) string {
	if len(s) <= 8 {
		return "********"
	}
	prefix := "sk-kg-"
	suffix := s[len(s)-4:]
	if len(s) > len(prefix) && s[:len(prefix)] == prefix {
		return prefix + "..." + suffix
	}
	return "..." + suffix
}

// apiCreateAPIKey creates a new key. Body fields:
//
//	name:           label
//	models:         optional whitelist
//	dailyReqLimit:  0 = unlimited
//	dailyTokLimit:  0 = unlimited
//	dailyCredLimit: 0 = unlimited
//	expiresAt:      Unix seconds, 0 = no expiry
func (h *Handler) apiCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name           string   `json:"name"`
		Models         []string `json:"models,omitempty"`
		DailyReqLimit  int      `json:"dailyReqLimit"`
		DailyTokLimit  int      `json:"dailyTokLimit"`
		DailyCredLimit float64  `json:"dailyCredLimit"`
		ExpiresAt      int64    `json:"expiresAt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	key, err := config.AddAPIKey(req.Name, req.Models, req.DailyReqLimit, req.DailyTokLimit, req.DailyCredLimit, req.ExpiresAt)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(key)
}

// apiUpdateAPIKey patches a key's metadata. Any field omitted is left alone.
// Use enabled=false to revoke without deleting (preserves history). All
// fields documented on APIKey are patchable here.
func (h *Handler) apiUpdateAPIKey(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Name              *string   `json:"name,omitempty"`
		Enabled           *bool     `json:"enabled,omitempty"`
		Models            *[]string `json:"models,omitempty"`
		ExpiresAt         *int64    `json:"expiresAt,omitempty"`
		LazyExpirySeconds *int64    `json:"lazyExpirySeconds,omitempty"`
		ResetPeriod       *string   `json:"resetPeriod,omitempty"`
		ResetTZ           *string   `json:"resetTZ,omitempty"`
		DailyReqLimit     *int      `json:"dailyReqLimit,omitempty"`
		DailyTokLimit     *int      `json:"dailyTokLimit,omitempty"`
		DailyCredLimit    *float64  `json:"dailyCredLimit,omitempty"`
		MinuteReqLimit    *int      `json:"minuteReqLimit,omitempty"`
		HourReqLimit      *int      `json:"hourReqLimit,omitempty"`
		LifetimeReqLimit  *int      `json:"lifetimeReqLimit,omitempty"`
		LifetimeTokLimit  *int      `json:"lifetimeTokLimit,omitempty"`
		LifetimeCredLimit *float64  `json:"lifetimeCredLimit,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	opts := config.UpdateAPIKeyOptions{
		Name: req.Name, Enabled: req.Enabled, Models: req.Models,
		ExpiresAt: req.ExpiresAt, LazyExpirySeconds: req.LazyExpirySeconds,
		ResetPeriod: req.ResetPeriod, ResetTZ: req.ResetTZ,
		DailyReqLimit: req.DailyReqLimit, DailyTokLimit: req.DailyTokLimit, DailyCredLimit: req.DailyCredLimit,
		MinuteReqLimit: req.MinuteReqLimit, HourReqLimit: req.HourReqLimit,
		LifetimeReqLimit: req.LifetimeReqLimit, LifetimeTokLimit: req.LifetimeTokLimit, LifetimeCredLimit: req.LifetimeCredLimit,
	}
	if !config.UpdateAPIKey(id, opts) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Key not found"})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiDeleteAPIKey permanently removes a key. To preserve history, prefer
// PUT enabled=false.
func (h *Handler) apiDeleteAPIKey(w http.ResponseWriter, r *http.Request, id string) {
	if !config.DeleteAPIKey(id) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Key not found"})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// modelStatsCache memoizes the (expensive) ByModel + ByModelEffort full-history
// aggregations for a short window. The admin Overview loads it on login and the
// Analytics tab re-fetches it on every switch, so without a cache each visit ran
// two GROUP-BY scans of the whole stats history. A few-second TTL keeps the
// numbers effectively live while collapsing bursts (overview + analytics in the
// same session) onto a single pair of queries.
var (
	modelStatsCacheMu  sync.Mutex
	modelStatsCacheVal []byte
	modelStatsCacheAt  time.Time
)

const modelStatsCacheTTL = 5 * time.Second

// apiGetModelStats returns the persisted per-model totals (lifetime). Each
// entry includes lastUsed so the dashboard can render "last seen" timestamps,
// plus an "effort" breakdown: per reasoning-effort level (low/medium/high/
// xhigh/max/default), the requests / tokens / credits driven at that level.
// The per-effort entries sum to the model's own totals. Backed by the SQLite
// stats table — survives restarts. Result is cached for modelStatsCacheTTL.
func (h *Handler) apiGetModelStats(w http.ResponseWriter, r *http.Request) {
	modelStatsCacheMu.Lock()
	if modelStatsCacheVal != nil && time.Since(modelStatsCacheAt) < modelStatsCacheTTL {
		payload := modelStatsCacheVal
		modelStatsCacheMu.Unlock()
		w.Write(payload)
		return
	}
	modelStatsCacheMu.Unlock()

	byModel, _ := stats.ByModel()
	byEffort, _ := stats.ByModelEffort()
	out := make(map[string]map[string]interface{}, len(byModel))
	for model, t := range byModel {
		entry := map[string]interface{}{
			"requests": t.Requests,
			"success":  t.Success,
			"failed":   t.Failed,
			"tokens":   t.TokensIn + t.TokensOut,
			"credits":  t.Credits,
			"lastUsed": t.LastAt,
		}
		if levels := byEffort[model]; len(levels) > 0 {
			effort := make(map[string]map[string]interface{}, len(levels))
			for level, et := range levels {
				effort[level] = map[string]interface{}{
					"requests": et.Requests,
					"tokens":   et.TokensIn + et.TokensOut,
					"credits":  et.Credits,
				}
			}
			entry["effort"] = effort
		}
		out[model] = entry
	}
	payload, err := json.Marshal(map[string]interface{}{"models": out})
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"models": out})
		return
	}
	modelStatsCacheMu.Lock()
	modelStatsCacheVal = payload
	modelStatsCacheAt = time.Now()
	modelStatsCacheMu.Unlock()
	w.Write(payload)
}

// apiGetAvailableModels returns the unfiltered model catalog the API key
// editor should show as checkboxes. This is the same source /v1/models
// uses BEFORE the per-key allowlist filter — the canonical Anthropic ids
// (claude-opus-4-7) plus their dotted Kiro aliases (claude-opus-4.7),
// deduplicated. The non-Claude aliases (auto / gpt-4o / gpt-4) are also
// included because the runtime path translates them to Claude models, so
// an operator may legitimately want to include or exclude them.
//
// Cold-start fallback applies: if the per-account model cache is empty
// we serve fallbackAnthropicModels so the form has something to render
// while a background refresh fills the cache.
func (h *Handler) apiGetAvailableModels(w http.ResponseWriter, r *http.Request) {
	thinkingSuffix := config.GetThinkingConfig().Suffix
	h.modelsCacheMu.RLock()
	cached := h.cachedModels
	h.modelsCacheMu.RUnlock()
	if len(cached) == 0 {
		h.triggerModelsRefreshAsync()
	}
	models := buildAnthropicModelsResponse(cached, thinkingSuffix)
	if len(models) == 0 {
		models = fallbackAnthropicModels(thinkingSuffix)
	}
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		if id, ok := m["id"].(string); ok {
			seen[id] = true
		}
	}
	for _, alias := range []string{"auto", "gpt-4o", "gpt-4"} {
		if seen[alias] {
			continue
		}
		seen[alias] = true
		models = append(models, buildModelInfo(alias, "kiro-proxy", true))
	}
	// Surface a flat list of ids — the form only needs the id strings,
	// and the existing /v1/models response shape (full objects with
	// owned_by, supportsImage, etc.) carries fields the checkbox form
	// doesn't render. Keep the payload lean.
	ids := make([]string, 0, len(models))
	for _, m := range models {
		if id, ok := m["id"].(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	json.NewEncoder(w).Encode(map[string]interface{}{"models": ids})
}

// apiGetStatsTotals returns the persisted lifetime global counters loaded
// from SQLite. Used by the dashboard's top stat cards so the numbers reflect
// the true historical total instead of the post-restart in-memory window.
func (h *Handler) apiGetStatsTotals(w http.ResponseWriter, r *http.Request) {
	t, _ := stats.AllTimeTotals("global", "")
	json.NewEncoder(w).Encode(t)
}

// apiGetStatsHistory returns the daily time series for any scope.
//
//	GET /admin/api/stats/history?scope=global&days=28
//	GET /admin/api/stats/history?scope=model&id=claude-sonnet-4.5&days=28
//	GET /admin/api/stats/history?scope=key&id=<key id>&days=28
//
// days defaults to 28; days=0 returns the full history.
func (h *Handler) apiGetStatsHistory(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "global"
	}
	id := r.URL.Query().Get("id")
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days < 0 {
		days = 28
	}
	if days == 0 && r.URL.Query().Get("days") == "" {
		days = 28
	}
	rows, err := stats.History(scope, id, days)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if rows == nil {
		rows = []stats.DailyEntry{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"entries": rows})
}
