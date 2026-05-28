package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
)

// apiGetModelGroups returns the current model->group mapping. The admin
// UI uses this to render the group routing rules table.
func (h *Handler) apiGetModelGroups(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"groups": config.GetModelGroups(),
	})
}

// apiUpdateModelGroups patches the model->group mapping. Body shape:
//
//	{ "groups": { "claude-opus-4-7": "premium", "claude-haiku-4-5": "free" } }
//
// The request is the complete desired state — every key not present in
// the body is removed. This is intentional so the admin UI can render a
// table, let the operator add/remove rows, and POST the whole table back.
// Atomic on disk via config.ReplaceModelGroups: one cfgLock, one Save,
// so a partial failure can't leave a hybrid map.
func (h *Handler) apiUpdateModelGroups(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Groups map[string]string `json:"groups"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.Groups == nil {
		req.Groups = map[string]string{}
	}
	if err := config.ReplaceModelGroups(req.Groups); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
