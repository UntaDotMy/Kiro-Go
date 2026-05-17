package proxy

import (
	"sync"
	"time"
)

// modelStat is the per-model running counter aggregated across all accounts
// and API keys. Stored in-memory (process lifetime) and surfaced via the
// /admin/api/modelstats endpoint for the dashboard's per-model usage card.
//
// Reset on restart by design — these are visibility metrics, not billing.
// (Billing-grade per-model usage would require a persistence layer with
// time-series storage, which is out of scope.)
type modelStat struct {
	Requests  int64
	Tokens    int64
	Credits   float64
	LastUsed  int64
}

var (
	modelStatsMu sync.RWMutex
	modelStats   = map[string]*modelStat{}
)

// recordModelUsage increments the per-model running counters. Called from
// every successful proxy request after the upstream response completes.
// The model parameter is the canonical id the client requested (the
// dashboard groups by this).
func recordModelUsage(model string, tokens int, credits float64) {
	if model == "" {
		return
	}
	modelStatsMu.Lock()
	defer modelStatsMu.Unlock()
	s, ok := modelStats[model]
	if !ok {
		s = &modelStat{}
		modelStats[model] = s
	}
	s.Requests++
	s.Tokens += int64(tokens)
	s.Credits += credits
	s.LastUsed = time.Now().Unix()
}

// snapshotModelStats returns a copy of the current per-model counters
// suitable for serialization to the dashboard. Sorted-stable map iteration
// is the dashboard's responsibility.
func snapshotModelStats() map[string]map[string]interface{} {
	modelStatsMu.RLock()
	defer modelStatsMu.RUnlock()
	out := make(map[string]map[string]interface{}, len(modelStats))
	for model, s := range modelStats {
		out[model] = map[string]interface{}{
			"requests": s.Requests,
			"tokens":   s.Tokens,
			"credits":  s.Credits,
			"lastUsed": s.LastUsed,
		}
	}
	return out
}

// resetModelStats clears the counters. Used by the dashboard's "Reset stats"
// button so per-model totals reset alongside the global counters.
func resetModelStats() {
	modelStatsMu.Lock()
	defer modelStatsMu.Unlock()
	modelStats = map[string]*modelStat{}
}
