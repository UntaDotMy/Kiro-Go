package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
)

// apiListAPIKeys returns all configured API keys with their counters and
// limits. The full secret is included so the dashboard can re-display it on
// demand. Production deployments may want to mask all but the last 4 chars
// — current admin panel is password-gated so we surface them in full.
func (h *Handler) apiListAPIKeys(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"keys": config.GetAPIKeys(),
	})
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
// Use enabled=false to revoke without deleting (preserves history).
func (h *Handler) apiUpdateAPIKey(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Name           *string   `json:"name,omitempty"`
		Enabled        *bool     `json:"enabled,omitempty"`
		Models         *[]string `json:"models,omitempty"`
		DailyReqLimit  *int      `json:"dailyReqLimit,omitempty"`
		DailyTokLimit  *int      `json:"dailyTokLimit,omitempty"`
		DailyCredLimit *float64  `json:"dailyCredLimit,omitempty"`
		ExpiresAt      *int64    `json:"expiresAt,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if !config.UpdateAPIKey(id, req.Name, req.Enabled, req.Models, req.DailyReqLimit, req.DailyTokLimit, req.DailyCredLimit, req.ExpiresAt) {
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
