package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"kiro-go/stats"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const tokenRefreshSkewSeconds int64 = 120

// backgroundTokenRefreshSkewSeconds is the proactive refresh window used by the
// periodic background sweep. It MUST exceed the background tick interval (5 min)
// or an idle account's token can expire in the gap between ticks: the on-request
// path uses the tighter tokenRefreshSkewSeconds (120s), but an account that
// serves no requests relies entirely on the sweep. With a 5-min tick and a
// 120s window, a token expiring 3 min after a tick is missed, and by the next
// tick it's already dead. We refresh anything expiring within 11 min so every
// token is renewed at least one full tick before it can lapse. The on-request
// fast path keeps the tighter 120s window so active accounts don't refresh more
// than necessary.
const backgroundTokenRefreshSkewSeconds int64 = 11 * 60

// refreshFanoutConcurrency caps the number of in-flight upstream calls when
// fan-outing background refreshes (token + RefreshAccountInfo, model list)
// across accounts. Sequential refresh extended the stale-token window to
// O(N * upstreamLatency); fully unbounded fan-out would burst N parallel
// auth/usage calls at boot. 8 in-flight is a comfortable middle ground:
// for typical pools (10–30 accounts) it cuts wall time to <= ~ceil(N/8) *
// per-call latency, while staying well below any reasonable upstream rate
// limit on the auth and usage endpoints (those are per-account, not per-
// proxy, so concurrency between accounts doesn't compound).
const refreshFanoutConcurrency = 8

// maxRequestBodyBytes caps every JSON request body the proxy will read into
// memory. 32 MiB is more than any reasonable Claude / OpenAI / Responses
// payload (largest practical conversations top out around 5–10 MiB even with
// long histories) while still defending the server from a malicious client
// streaming an unbounded body. Wraped via http.MaxBytesReader at every
// io.ReadAll(r.Body) call site.
const maxRequestBodyBytes int64 = 32 * 1024 * 1024

// Handler HTTP 处理器
type Handler struct {
	pool *pool.AccountPool
	// 运行时统计 (使用原子操作)
	totalRequests   int64
	successRequests int64
	failedRequests  int64
	totalTokens     int64
	totalCredits    float64 // float64 需要用锁保护
	creditsMu       sync.RWMutex
	startTime       int64
	stopRefresh     chan struct{}
	stopStatsSaver  chan struct{}
	// stopDashboardPusher terminates the periodic dashboard snapshot pusher.
	// That pusher broadcasts the live snapshot ~1s while >=1 dashboard is
	// connected, so purely-live fields (inflight, cooldown countdown, paced
	// rate, AIMD limit) update in realtime instead of only on request
	// completion. Closed in Stop().
	stopDashboardPusher chan struct{}
	// 模型缓存
	cachedModels    []ModelInfo
	modelsCacheMu   sync.RWMutex
	modelsCacheTime int64
	// modelsRefreshing collapses concurrent refreshModelsCache fan-outs into
	// a single in-flight call. Set with atomic CAS at the entry point and
	// cleared in defer so a panic/error path doesn't strand the gate. Without
	// this gate, a burst of /v1/models requests during cold start each kicked
	// off their own N-account fan-out.
	modelsRefreshing atomic.Bool
	promptCache      *promptCacheTracker
	// tokenRefreshLocks holds one mutex per account id so a slow synchronous
	// token refresh on one account no longer serializes the refresh-check path
	// for every other account (the prior single Handler-wide mutex did). The
	// fast path in ensureValidToken stays lock-free; only the rare near-expiry
	// refresh acquires the per-account lock. Keyed by account id, created on
	// demand via sync.Map.
	tokenRefreshLocks sync.Map // accountID -> *sync.Mutex
	// Per-account debounce for lazy quota refresh after a request completes.
	refreshDebounceMu sync.Mutex
	refreshScheduled  map[string]bool
	// dashboardHub fans realtime status updates to subscribed admin
	// dashboards (see proxy/dashboard_ws.go). Initialized in NewHandler;
	// nil-safe because broadcastDashboardUpdate guards against nil.
	dashboardHub *dashboardHub
	// globalRL is an opt-in global token-bucket rate limiter (disabled until
	// config.GlobalRateLimitPerMinute > 0). Backstops the per-key limits with a
	// single proxy-wide request cap. See proxy/global_ratelimit.go.
	globalRL globalRateLimiter
}

type thinkingStreamSource int

const (
	thinkingSourceUnknown thinkingStreamSource = iota
	thinkingSourceReasoningEvent
	thinkingSourceTagBlock
)

func allowReasoningSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceTagBlock {
		return false
	}
	*source = thinkingSourceReasoningEvent
	return true
}

func allowTagSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceReasoningEvent {
		return false
	}
	if *source == thinkingSourceUnknown {
		*source = thinkingSourceTagBlock
	}
	return *source == thinkingSourceTagBlock
}

func validateClaudeRequestShape(req *ClaudeRequest) string {
	if strings.TrimSpace(req.Model) == "" {
		return "model is required"
	}
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		return msg
	}

	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		lastRole = role
		if role != "user" {
			continue
		}

		text, images, toolResults := extractClaudeUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" || len(toolResults) > 0 {
			hasUserContext = true
		}
	}

	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	return ""
}

func validateClaudeThinkingConfig(thinking *ClaudeThinkingConfig, maxTokens int) string {
	if thinking == nil {
		return ""
	}

	kind := strings.ToLower(strings.TrimSpace(thinking.Type))
	switch kind {
	case "enabled":
		if maxTokens == 0 {
			return "thinking.type enabled cannot be used with max_tokens=0"
		}
		if thinking.BudgetTokens <= 0 {
			return "thinking.budget_tokens is required when thinking.type is enabled"
		}
		if thinking.BudgetTokens < 1024 {
			return "thinking.budget_tokens must be at least 1024"
		}
		if maxTokens > 0 && thinking.BudgetTokens >= maxTokens {
			return "thinking.budget_tokens must be less than max_tokens"
		}
	case "adaptive":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is adaptive"
		}
	case "disabled":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is disabled"
		}
	default:
		return "thinking.type must be one of: enabled, adaptive, disabled"
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	if display != "" && display != "summarized" && display != "omitted" {
		return "thinking.display must be one of: summarized, omitted"
	}
	if kind == "disabled" && display != "" {
		return "thinking.display is not supported when thinking.type is disabled"
	}

	return ""
}

// applyAdaptiveThinkingDefault mutates req so adaptive thinking is enabled
// when the client did not specify any thinking config. Kiro upstream uses
// adaptive thinking by default for Claude 4-family models, so flipping this
// on lets Claude Code's /model panel surface the "thinking" indicator and
// routes reasoning content through the response builder.
//
// When the client explicitly sent thinking.type="enabled" with a
// budget_tokens value, log a debug line noting that the budget is dropped.
// Kiro's native thinking knob (additionalModelRequestFields.thinking) accepts
// only {type: adaptive|disabled} — there is no budget_tokens on the wire, so a
// requested budget can't be honored. (Reasoning EFFORT, by contrast, IS a
// native wire field — output_config.effort — and is forwarded separately for
// models that advertise support; see reasoning_effort.go.)
func applyAdaptiveThinkingDefault(req *ClaudeRequest) {
	if req == nil {
		return
	}

	if req.Thinking == nil {
		if !modelSupportsAdaptiveThinking(req.Model) {
			return
		}
		req.Thinking = &ClaudeThinkingConfig{Type: "adaptive"}
		logger.Debugf("[Thinking] Defaulting to adaptive thinking for %s", req.Model)
		return
	}

	kind := strings.ToLower(strings.TrimSpace(req.Thinking.Type))
	if kind == "enabled" && req.Thinking.BudgetTokens > 0 {
		logger.Debugf("[Thinking] Client requested thinking.type=enabled with budget_tokens=%d for %s; Kiro's native thinking field accepts only {type: adaptive|disabled} (no budget_tokens), so adaptive thinking is applied and the budget is dropped.", req.Thinking.BudgetTokens, req.Model)
	}
}

type claudeThinkingResponseOptions struct {
	Format      string
	OmitDisplay bool
}

func resolveClaudeThinkingResponseOptions(thinking *ClaudeThinkingConfig, defaultFormat string) claudeThinkingResponseOptions {
	opts := claudeThinkingResponseOptions{Format: defaultFormat}
	if opts.Format == "" {
		opts.Format = "thinking"
	}
	if thinking == nil {
		return opts
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	switch display {
	case "summarized":
		opts.Format = "thinking"
	case "omitted":
		opts.Format = "thinking"
		opts.OmitDisplay = true
	}

	return opts
}

func validateOpenAIRequestShape(req *OpenAIRequest) string {
	if strings.TrimSpace(req.Model) == "" {
		return "model is required"
	}
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}

	hasNonSystem := false
	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		if role != "system" {
			hasNonSystem = true
			lastRole = role
		}

		if role != "user" {
			continue
		}
		text, images := extractOpenAIUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" {
			hasUserContext = true
		}
	}

	if !hasNonSystem {
		return "at least one non-system message is required"
	}
	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user or tool"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	return ""
}

func NewHandler() *Handler {
	// 启动时应用代理配置
	applyProxyConfig(config.GetProxyURL())

	totalReq, successReq, failedReq, totalTokens, totalCredits := config.GetStats()
	// Prefer the persisted SQLite totals when available — they accumulate
	// across restarts and represent the true historical numbers. Falls back
	// to the legacy config.GetStats() values if the stats DB is empty (first
	// run after upgrade) or unavailable.
	if t, err := stats.AllTimeTotals("global", ""); err == nil && t.Requests > 0 {
		totalReq = t.Requests
		successReq = t.Success
		failedReq = t.Failed
		totalTokens = t.TokensIn + t.TokensOut
		totalCredits = t.Credits
	}
	h := &Handler{
		pool:            pool.GetPool(),
		totalRequests:   int64(totalReq),
		successRequests: int64(successReq),
		failedRequests:  int64(failedReq),
		totalTokens:     int64(totalTokens),
		totalCredits:    totalCredits,
		startTime:           time.Now().Unix(),
		stopRefresh:         make(chan struct{}),
		stopStatsSaver:      make(chan struct{}),
		stopDashboardPusher: make(chan struct{}),
		promptCache:         newPromptCacheTracker(defaultPromptCacheTTL),
		dashboardHub:        newDashboardHub(),
	}
	// Configure the opt-in global rate limiter from persisted config (0 = off).
	h.globalRL.Configure(config.GetGlobalRateLimitPerMinute())
	// Seed the in-memory model cache from the persisted last-known-good
	// catalog so /v1/models serves real ids immediately after a restart,
	// before the first live upstream fetch completes. The background
	// refresh (backgroundRefresh -> refreshModelsCache) overwrites this
	// with fresh data within ~10s of boot.
	if known := config.GetKnownModels(); len(known) > 0 {
		seeded := make([]ModelInfo, 0, len(known))
		for _, id := range known {
			seeded = append(seeded, ModelInfo{ModelId: id})
		}
		h.cachedModels = seeded
		h.modelsCacheTime = time.Now().Unix()
	}
	// 启动后台刷新
	safeGo("backgroundRefresh", h.backgroundRefresh)
	// 启动后台统计保存 (每30秒保存一次)
	safeGo("backgroundStatsSaver", h.backgroundStatsSaver)
	// Periodic sweep of the admin brute-force map so a distributed attack from
	// many distinct single-attempt IPs can't grow it without bound.
	h.startAdminFailSweeper()
	// Periodic dashboard snapshot pusher so live-only fields (inflight, cooldown
	// countdown, paced rate, AIMD limit) update in realtime while a dashboard is
	// open, not just on request completion. Gated by hasSubscribers() so it's a
	// cheap no-op when nobody is watching.
	safeGo("dashboardPusher", h.dashboardPusher)
	return h
}

// Stop signals the background goroutines to terminate. Called from main.go
// during graceful shutdown so the proxy doesn't leak goroutines or partial
// config writes.
func (h *Handler) Stop() {
	// closeOnce closes a stop channel exactly once and tolerates a nil channel.
	// Nil-safe because a Handler built as a struct literal in tests (rather than
	// via NewHandler) leaves these channels nil; close(nil) would panic, and the
	// bare select/default form does not guard against that (a receive on a nil
	// channel blocks, so the default arm runs and closes nil). Production always
	// goes through NewHandler, so this only matters for test ergonomics.
	closeOnce := func(ch chan struct{}) {
		if ch == nil {
			return
		}
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
	closeOnce(h.stopRefresh)
	closeOnce(h.stopStatsSaver)
	closeOnce(h.stopDashboardPusher)
	// One last stats flush so the latest counters survive the restart.
	h.saveStats()
}

// backgroundRefresh runs a periodic global refresh of every enabled account's
// quota and the model list. Default cadence is short (5 minutes) so the
// dashboard's per-account quota numbers are reasonably fresh; per-request
// lazy refresh (triggerAccountRefresh) provides finer granularity for
// accounts that are actively used.
func (h *Handler) backgroundRefresh() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	// Initial refresh after a small delay so the proxy can start serving
	// immediately without waiting on upstream account info.
	time.Sleep(10 * time.Second)
	h.refreshModelsCache()
	h.refreshAllAccounts()

	for {
		select {
		case <-ticker.C:
			h.refreshModelsCache()
			h.refreshAllAccounts()
		case <-h.stopRefresh:
			return
		}
	}
}

// triggerAccountRefresh schedules a debounced background quota refresh for a
// single account. Called from request-completion paths so an account that
// just served a request gets a fresh usageCurrent within ~30 seconds, while
// idle accounts only refresh on the global ticker.
func (h *Handler) triggerAccountRefresh(accountID string) {
	if accountID == "" {
		return
	}
	h.refreshDebounceMu.Lock()
	if h.refreshScheduled == nil {
		h.refreshScheduled = make(map[string]bool)
	}
	if h.refreshScheduled[accountID] {
		h.refreshDebounceMu.Unlock()
		return
	}
	h.refreshScheduled[accountID] = true
	h.refreshDebounceMu.Unlock()

	safeGoArg("triggerAccountRefresh", accountID, func(id string) {
		// Debounce: coalesce bursts of requests into a single refresh.
		// 3 seconds is short enough that the dashboard's verified `quotaUsed`
		// number catches up almost in real-time, while still coalescing
		// rapid-fire requests onto a single Kiro account-info refresh.
		// Was 30 s before A20 — that was too slow for a "live" dashboard UX.
		select {
		case <-time.After(3 * time.Second):
		case <-h.stopRefresh:
			h.refreshDebounceMu.Lock()
			delete(h.refreshScheduled, id)
			h.refreshDebounceMu.Unlock()
			return
		}

		h.refreshDebounceMu.Lock()
		delete(h.refreshScheduled, id)
		h.refreshDebounceMu.Unlock()

		acc := h.pool.GetByID(id)
		if acc == nil || !acc.Enabled || acc.AccessToken == "" {
			return
		}
		info, err := RefreshAccountInfo(acc)
		if err != nil {
			logger.Debugf("[LazyRefresh] Failed to refresh %s: %v", acc.Email, err)
			return
		}
		flipped, _ := config.UpdateAccountInfo(acc.ID, *info)
		if flipped {
			// Auto-disable / auto-recover took effect — re-pick weights so the
			// scheduler stops (or resumes) routing to this account immediately.
			h.pool.Reload()
			logger.Infof("[AutoDisable] Account %s enabled-state flipped on refresh (current=%.1f/%.1f trial=%.1f/%.1f)",
				redactForLog(acc.Email), info.UsageCurrent, info.UsageLimit, info.TrialUsageCurrent, info.TrialUsageLimit)
		}
		// Credit / quota numbers just changed — push to dashboards.
		h.broadcastDashboardUpdate()
	})
}

// refreshAllAccounts 刷新所有账户信息
//
// Per-account work (token refresh + RefreshAccountInfo) is independent, so
// we fan out across accounts with a bounded worker pool. Sequential refresh
// took O(N * upstreamLatency) wall time which extended the
// stale-quota / stale-token window for the whole pool. Concurrency is
// capped at refreshFanoutConcurrency to avoid hammering the upstream auth /
// usage endpoints during boot or on the periodic background tick.
func (h *Handler) refreshAllAccounts() {
	accounts := config.GetAccounts()
	if len(accounts) == 0 {
		return
	}

	sem := make(chan struct{}, refreshFanoutConcurrency)
	var wg sync.WaitGroup
	// anyFlipped records whether any account's Enabled state changed during the
	// sweep (auto-disable / auto-recover). Set from worker goroutines, so it must
	// be atomic. We Reload the pool unconditionally below, but keep the signal for
	// future use / clarity.
	var anyFlipped atomic.Bool
	for i := range accounts {
		account := &accounts[i]
		if !account.Enabled || account.AccessToken == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(account *config.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf("[BackgroundRefresh] worker panic for %s: %v", account.Email, r)
				}
			}()

			// 检查 token 是否需要刷新
			// Use the wider background skew (not the on-request 120s) so an idle
			// account that serves no traffic still gets its token renewed a full
			// tick before expiry. Otherwise a token expiring between two 5-min
			// ticks would lapse and the next request would 401 before failover.
			if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-backgroundTokenRefreshSkewSeconds {
				newAccessToken, newRefreshToken, newExpiresAt, profileArn, err := auth.RefreshToken(account)
				if err != nil {
					logger.Warnf("[BackgroundRefresh] Token refresh failed for %s: %v", account.Email, err)
					return
				}
				account.AccessToken = newAccessToken
				if newRefreshToken != "" {
					account.RefreshToken = newRefreshToken
				}
				account.ExpiresAt = newExpiresAt
				config.UpdateAccountToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
				h.pool.UpdateToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
				if profileArn != "" {
					account.ProfileArn = profileArn
					config.UpdateAccountProfileArn(account.ID, profileArn)
				}
			}

			// 刷新账户信息
			info, err := RefreshAccountInfo(account)
			if err != nil {
				logger.Warnf("[BackgroundRefresh] Failed to refresh %s: %v", account.Email, err)
				return
			}

			// Apply in memory WITHOUT a per-account disk write; the whole sweep
			// is flushed once after the WaitGroup below. The prior per-account
			// config.UpdateAccountInfo rewrote the entire config.json (marshal +
			// fsync + rename) under the write lock once per account per tick.
			flipped, _ := config.UpdateAccountInfoNoSave(account.ID, *info)
			if flipped {
				anyFlipped.Store(true)
			}
			logger.Infof("[BackgroundRefresh] Refreshed %s: %s %.1f/%.1f", account.Email, info.SubscriptionType, info.UsageCurrent, info.UsageLimit)
		}(account)
	}
	wg.Wait()
	// Single batched persist for the whole sweep instead of one write per account.
	if err := config.FlushConfig(); err != nil {
		logger.Warnf("[BackgroundRefresh] Failed to persist refreshed account info: %v", err)
	}
	h.pool.Reload()
}

// apiKeyCtxKey is the request-context key under which a matched APIKey is
// stashed by validateApiKey, so success-path handlers can call ConsumeAPIKey
// to debit per-key counters and surface a 429 when daily limits are hit.
type apiKeyCtxKey struct{}

// validateApiKey checks Authorization / X-Api-Key headers against (in order):
//
//  1. The legacy single-key (config.ApiKey) when set
//  2. Any enabled, non-expired key in config.APIKeys
//
// On match, the APIKey is stashed on r.Context() (when from the multi-list)
// for downstream handlers to consume via ConsumeMatchedAPIKey. Returns true
// when the request is authorised, false otherwise. When RequireApiKey is off
// or no keys are configured, returns true unconditionally.
func (h *Handler) validateApiKey(r *http.Request) bool {
	required := config.IsApiKeyRequired()

	authHeader := r.Header.Get("Authorization")
	apiKeyHeader := r.Header.Get("X-Api-Key")

	var providedKey string
	if strings.HasPrefix(authHeader, "Bearer ") {
		providedKey = strings.TrimPrefix(authHeader, "Bearer ")
	} else if apiKeyHeader != "" {
		providedKey = apiKeyHeader
	}

	// Multi-key path: walk configured keys and constant-time compare each.
	// (After A12, the legacy single key is auto-migrated into APIKeys[0],
	// so we no longer need a separate legacy fast-path. Going through the
	// multi-key loop ensures the matched key gets stashed in the request
	// context for per-key statistics tracking.)
	//
	// We attempt this match even when RequireApiKey is OFF: if the caller
	// presented a recognized key, we still stash it so per-key features
	// (model whitelist, quotas, stats) take effect. Auth being optional only
	// means an UNrecognized/absent key is still allowed through — it does not
	// mean a presented key's restrictions should be ignored.
	keys := config.GetAPIKeys()
	for i := range keys {
		k := keys[i]
		if !k.Enabled {
			continue
		}
		if k.ExpiresAt > 0 && time.Now().Unix() > k.ExpiresAt {
			continue
		}
		// Lazy expiry: a key with LazyExpirySeconds expires that many seconds
		// after its FIRST use. Enforced here at auth time so a lazy-expired key
		// stops authenticating on EVERY route — not just the inference routes
		// that call enforceAPIKeyLimit. Without this, an expired-lazy key could
		// still reach /v1/models and /v1/stats until an operator flipped Enabled.
		if k.LazyExpirySeconds > 0 && k.FirstUsedAt > 0 &&
			time.Now().Unix() > k.FirstUsedAt+k.LazyExpirySeconds {
			continue
		}
		if providedKey != "" && subtle.ConstantTimeCompare([]byte(providedKey), []byte(k.Key)) == 1 {
			// Stash the matched key on the request context so downstream
			// handlers can debit per-key counters and filter by whitelist.
			ctx := context.WithValue(r.Context(), apiKeyCtxKey{}, &k)
			*r = *r.WithContext(ctx)
			return true
		}
	}

	// No key matched. When auth is not required, allow the request through
	// (anonymous / legacy-permissive). When it IS required, reject.
	if !required {
		return true
	}

	// Fall back to "no keys configured at all" -> permissive. Catches the
	// pre-A7 state where neither the legacy single key nor any multi-key
	// entries exist.
	legacyKey := config.GetApiKey()
	if legacyKey == "" && len(keys) == 0 {
		return true
	}
	return false
}

// matchedAPIKey returns the APIKey associated with this request (if any), set
// by validateApiKey.
func matchedAPIKey(r *http.Request) *config.APIKey {
	if k, ok := r.Context().Value(apiKeyCtxKey{}).(*config.APIKey); ok {
		return k
	}
	return nil
}

// matchedAPIKeyID returns the id of the key that authenticated this request,
// or "" if unauthenticated / using the legacy permissive fallback.
func matchedAPIKeyID(r *http.Request) string {
	if k, ok := r.Context().Value(apiKeyCtxKey{}).(*config.APIKey); ok && k != nil {
		return k.ID
	}
	return ""
}

// enforceAPIKeyLimit is the pre-flight gate: if the request was authenticated
// by a multi-key entry, run CheckAPIKeyLimit before we burn upstream quota.
// Returns true when the request was rejected (429 already written).
func (h *Handler) enforceAPIKeyLimit(w http.ResponseWriter, r *http.Request, model string) bool {
	k := matchedAPIKey(r)
	if k == nil {
		return false
	}
	rejected, reason := config.CheckAPIKeyLimit(k.ID, model)
	if !rejected {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    "rate_limit_error",
			"message": "API key limit reached: " + reason,
		},
	})
	return true
}

// enforceAPIKeyRateLimit is the model-agnostic gate for metadata routes
// (/v1/models, /v1/stats). It enforces the key's enable/expiry/rate/quota
// limits WITHOUT the model-whitelist dimension, so a disabled, expired, or
// rate-exhausted key can't keep probing these routes, while a model-restricted
// key can still list its filtered catalog. Returns true when the request was
// rejected (429 already written).
func (h *Handler) enforceAPIKeyRateLimit(w http.ResponseWriter, r *http.Request) bool {
	k := matchedAPIKey(r)
	if k == nil {
		return false
	}
	rejected, reason := config.CheckAPIKeyRateLimit(k.ID)
	if !rejected {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    "rate_limit_error",
			"message": "API key limit reached: " + reason,
		},
	})
	return true
}

// enforceGlobalRateLimit applies the opt-in proxy-wide token-bucket limiter
// before any per-key check or upstream call. Returns true when the request was
// rejected (429 + Retry-After already written). The `format` selects the error
// envelope so each API surface gets its native shape. When the limiter is
// disabled (default) this is a cheap no-op that returns false.
func (h *Handler) enforceGlobalRateLimit(w http.ResponseWriter, format string) bool {
	allowed, retryAfter := h.globalRL.allow()
	if allowed {
		return false
	}
	setRetryAfter(w, retryAfter)
	msg := "Global rate limit reached; retry after " + strconv.Itoa(retryAfterSeconds(retryAfter)) + "s"
	switch format {
	case "claude":
		h.sendClaudeError(w, 429, "rate_limit_error", msg)
	case "responses":
		h.sendResponsesError(w, 429, "rate_limit_exceeded", msg)
	default: // openai
		h.sendOpenAIError(w, 429, "rate_limit_error", msg)
	}
	return true
}

// ServeHTTP 路由分发
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Debug-level request trace for fine-grained visibility
	logger.Debugf("[HTTP] %s %s from %s", r.Method, path, r.RemoteAddr)

	// Request correlation id — use the client's X-Request-ID if present,
	// otherwise generate one. Anthropic's docs require this header on every
	// response; LiteLLM and many SDKs surface it in errors for support
	// triage. Echoed in our own error envelopes via setRequestID helper.
	reqID := r.Header.Get("X-Request-ID")
	if reqID == "" {
		reqID = "req_" + uuid.New().String()
	}
	w.Header().Set("X-Request-ID", reqID)
	w.Header().Set("Request-Id", reqID)

	// CORS — scoped per path. The inference API keeps the wildcard SDK callers
	// need; admin/portal/landing/health are same-origin only (no wildcard).
	// See proxy/security.go.
	setCORSHeaders(w, r)

	// Security response headers. HTML UI surfaces (landing/admin/portal) get the
	// full hardening set (CSP, X-Frame-Options, ...); JSON surfaces get the cheap
	// always-safe subset. HSTS only on TLS / trusted-proxy https.
	setSecurityHeaders(w, r, isHTMLSurfacePath(path))

	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}

	// 路由
	switch {
	// API 端点（需要验证 API Key）
	case path == "/v1/messages" || path == "/messages" || path == "/anthropic/v1/messages":
		if !h.validateApiKey(r) {
			h.sendClaudeError(w, 401, "authentication_error", "Invalid or missing API key")
			return
		}
		if h.enforceGlobalRateLimit(w, "claude") {
			return
		}
		h.handleClaudeMessages(w, r)
	case path == "/v1/messages/count_tokens" || path == "/messages/count_tokens":
		if !h.validateApiKey(r) {
			h.sendClaudeError(w, 401, "authentication_error", "Invalid or missing API key")
			return
		}
		h.handleCountTokens(w, r)
	case path == "/v1/chat/completions" || path == "/chat/completions":
		if !h.validateApiKey(r) {
			h.sendOpenAIError(w, 401, "authentication_error", "Invalid or missing API key")
			return
		}
		if h.enforceGlobalRateLimit(w, "openai") {
			return
		}
		h.handleOpenAIChat(w, r)
	case path == "/v1/responses" || path == "/responses" || path == "/openai/v1/responses" || path == "/backend-api/codex/responses":
		// Codex CLI uses /backend-api/codex/responses; the OpenAI Responses
		// API path is /v1/responses. Both routes accept either HTTP POST
		// (SSE streaming) or a WebSocket upgrade — Codex CLI's experimental
		// "responses_websockets" feature flag uses the WS variant.
		if isWebSocketUpgrade(r) {
			// Apply the same global rate-limit backstop as the HTTP path BEFORE
			// the upgrade — otherwise a WS client could open unbounded long-lived
			// streaming connections that never consume a global token. A 429
			// before the handshake cleanly fails the upgrade.
			if h.enforceGlobalRateLimit(w, "responses") {
				return
			}
			h.handleResponsesWebSocket(w, r)
			return
		}
		if !h.validateApiKey(r) {
			h.sendResponsesError(w, 401, "authentication_error", "Invalid or missing API key")
			return
		}
		if h.enforceGlobalRateLimit(w, "responses") {
			return
		}
		h.handleResponses(w, r)
	case path == "/v1/models" || path == "/models":
		// Validate the API key here (like the other /v1/* routes) so the
		// matched key lands in the request context. handleModels reads it via
		// matchedAPIKey to filter the response down to the key's allowed
		// models — without this call the context is empty and the per-key
		// whitelist filter is silently skipped, leaking the full catalog.
		if !h.validateApiKey(r) {
			h.sendOpenAIError(w, 401, "authentication_error", "Invalid or missing API key")
			return
		}
		// Apply the model-agnostic rate/quota gate so a disabled, expired, or
		// rate-exhausted key can't probe this route unmetered.
		if h.enforceAPIKeyRateLimit(w, r) {
			return
		}
		h.handleModels(w, r)
	case path == "/v1/key-status" || path == "/portal/api/key-status":
		// Public customer portal endpoint: authenticated by the presented key
		// itself, its own per-IP rate limit, no oracle, never leaks the raw key.
		// See proxy/portal_handler.go.
		h.handlePortalKeyStatus(w, r)
	case path == "/api/event_logging/batch":
		// Claude Code 遥测端点 - 直接返回 200 OK
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{"status":"ok"}`))

	// 管理端点
	case path == "/admin" || path == "/admin/":
		h.serveAdminPage(w, r)
	case path == "/admin/ws/status":
		// Realtime dashboard push. The WS handshake carries the admin
		// password in Sec-WebSocket-Protocol because browsers can't set
		// custom headers on the upgrade. Auth is verified inside the
		// handler with constant-time compare; this route bypasses the
		// header-based admin middleware on purpose.
		h.handleDashboardWS(w, r)
	case strings.HasPrefix(path, "/admin/api/"):
		h.handleAdminAPI(w, r)
	case strings.HasPrefix(path, "/admin/"):
		h.serveStaticFile(w, r)

	// 客户门户（公开只读）
	case path == "/portal" || path == "/portal/":
		h.servePortalPage(w, r)
	case strings.HasPrefix(path, "/portal/"):
		h.serveJailedStaticFile(w, r, "/portal/")

	// 落地页
	case path == "/":
		h.serveLandingPage(w, r)

	// 健康检查（最小化输出，不泄露版本/uptime 指纹）
	case path == "/health":
		h.handleHealth(w, r)

	// 统计端点（需要 API Key 鉴权）
	case path == "/v1/stats":
		if !h.validateApiKey(r) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
			return
		}
		if h.enforceAPIKeyRateLimit(w, r) {
			return
		}
		h.handleStats(w, r)

	default:
		http.Error(w, "Not Found", 404)
	}
}

// handleHealth 健康检查（不暴露版本/uptime 指纹，仅返回存活状态）。
// 版本与运行时长改由鉴权后的 /admin/api/status 提供，避免未鉴权指纹采集。
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
	})
}

// handleStats 统计数据（需要 API Key 鉴权）
func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "ok",
		"version":         config.Version,
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

// handleModels 模型列表
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	thinkingSuffix := config.GetThinkingConfig().Suffix

	// 尝试用缓存的真实模型列表
	h.modelsCacheMu.RLock()
	cached := h.cachedModels
	h.modelsCacheMu.RUnlock()

	// On cold start (cache empty) we used to block this request on the full
	// per-account fan-out, which made the first /v1/models call take seconds.
	// Now we kick off the refresh in the background (collapsed via the
	// modelsRefreshing gate so concurrent first-hits share one fan-out) and
	// serve fallbackAnthropicModels immediately. Subsequent requests will
	// see the populated cache once the refresh completes.
	if len(cached) == 0 {
		h.triggerModelsRefreshAsync()
	}

	models := buildAnthropicModelsResponse(cached, thinkingSuffix)
	if len(models) == 0 {
		models = fallbackAnthropicModels(thinkingSuffix)
	}

	// Append client aliases (auto / gpt-*) so SDKs that pin to those names
	// still resolve a model. Skip ids that the cached list already produced
	// (Kiro returns "auto" as a real model — re-appending it duplicated the
	// picker entry).
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

	// Per-key model whitelist filter. When the caller authenticated with an
	// API key that has a non-empty Models list, restrict the /v1/models
	// response to entries on that list. Empty list (or no key, e.g.
	// unauthenticated legacy path) returns the full set — preserves the
	// existing "default is everything" behavior. We use the same alias
	// resolver as the request-time pre-flight gate (config.modelIsAllowedBy
	// Whitelist exposed via config.IsModelAllowedForAPIKey) so a key
	// configured with "claude-opus-4.7" still sees "claude-opus-4-7" in the
	// list.
	if k := matchedAPIKey(r); k != nil && len(k.Models) > 0 {
		filtered := make([]map[string]interface{}, 0, len(models))
		for _, m := range models {
			id, _ := m["id"].(string)
			if id == "" {
				continue
			}
			if config.IsModelAllowedForAPIKey(*k, id) {
				filtered = append(filtered, m)
			}
		}
		models = filtered
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   models,
	})
	return
}

func buildAnthropicModelsResponse(cached []ModelInfo, thinkingSuffix string) []map[string]interface{} {
	if len(cached) == 0 {
		return nil
	}

	// We intentionally do NOT emit "<id>-thinking" variants here. The thinking
	// suffix is a response-side parsing flag only. Listing the suffixed
	// variants in /v1/models doubled the picker entries for every model
	// without changing behavior, so they're dropped. (Reasoning effort is
	// forwarded natively per-model via output_config.effort when the client
	// sends reasoning_effort / reasoning.effort — see reasoning_effort.go —
	// and does not need a separate model id.)
	//
	// Per Kiro model we emit two ids:
	//   1. the canonical dashed Anthropic form (e.g. "claude-opus-4-7") — what
	//      Claude Code's picker recognizes;
	//   2. the raw dotted Kiro id (e.g. "claude-opus-4.7") — alias for clients
	//      that ask by the upstream id.
	// Duplicates (same id) collapse, so a Kiro id that's already in dashed
	// form yields a single entry.
	models := make([]map[string]interface{}, 0, len(cached)*2)
	seen := make(map[string]bool, len(cached)*2)
	emit := func(id string, supportsImage bool) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		models = append(models, buildModelInfo(id, "anthropic", supportsImage))
	}
	for _, m := range cached {
		supportsImage := modelSupportsImage(m.InputTypes)
		emit(kiroModelToAnthropicID(m.ModelId), supportsImage)
		emit(m.ModelId, supportsImage)
	}
	_ = thinkingSuffix // signature retained for callers; suffix variants are no longer emitted
	return models
}

func fallbackAnthropicModels(thinkingSuffix string) []map[string]interface{} {
	// Used only when the in-memory per-account model cache is empty (e.g. a
	// /v1/models hit during the very first cold start, before any live fetch
	// or persisted catalog seed). Prefer the persisted last-known-good
	// catalog so we emit the real ids the upstream actually offers; only if
	// nothing has ever been fetched do we fall to the minimal bootstrap
	// pairs below. For every id we emit both the canonical dashed Anthropic
	// form (what Claude Code's picker recognizes) and the dotted Kiro alias.
	out := make([]map[string]interface{}, 0, 8)
	seen := make(map[string]bool, 16)
	emit := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, buildModelInfo(id, "anthropic", true))
	}

	if known := config.GetKnownModels(); len(known) > 0 {
		for _, raw := range known {
			// Dashed canonical form first (Claude Code picker), then the
			// raw upstream id as an alias. canonicalAnthropicModelID maps
			// dotted -> dashed mechanically, so future minors just work.
			emit(kiroModelToAnthropicID(raw))
			emit(strings.ToLower(strings.TrimSpace(raw)))
		}
		_ = thinkingSuffix // suffix variants are no longer emitted
		return out
	}

	// Minimal bootstrap — only reached on a truly fresh install that has
	// never reached upstream. Mirrors the live path's dedup rules: dashed
	// Anthropic id + dotted Kiro id, no dated suffix, no -thinking variant.
	pairs := []struct {
		dashed string
		dotted string
	}{
		{"claude-opus-4-7", "claude-opus-4.7"},
		{"claude-sonnet-4-6", "claude-sonnet-4.6"},
		{"claude-haiku-4-5", "claude-haiku-4.5"},
		{"claude-sonnet-4-5", "claude-sonnet-4.5"},
		{"claude-opus-4-5", "claude-opus-4.5"},
		{"claude-sonnet-4", ""},
	}
	for _, p := range pairs {
		emit(p.dashed)
		emit(p.dotted)
	}
	_ = thinkingSuffix // signature retained; suffix variants are no longer emitted
	return out
}

// kiroModelToAnthropicID converts Kiro's internal dotted model id (e.g.
// "claude-opus-4.7", "claude-sonnet-4.5") to the canonical Anthropic
// dashed form Claude Code recognizes in its model picker. Dated suffixes
// (e.g. "-20251101") are intentionally NOT stripped: the input from
// Kiro's ListAvailableModels is the family id, never the dated form.
//
// The transformation is purely mechanical (every "." -> "-") which makes
// it forwards-compatible: a future claude-opus-4.8 / 4.9 / 5.0 just works
// without a code change. We delegate to canonicalAnthropicModelID so the
// request response shape and this listing path share one definition.
func kiroModelToAnthropicID(kiroID string) string {
	return canonicalAnthropicModelID(strings.ToLower(strings.TrimSpace(kiroID)))
}

func modelSupportsImage(inputTypes []string) bool {
	for _, t := range inputTypes {
		lt := strings.ToLower(t)
		if strings.Contains(lt, "image") || strings.Contains(lt, "vision") {
			return true
		}
	}
	return false
}

func buildModelInfo(id, ownedBy string, supportsImage bool) map[string]interface{} {
	modalities := []string{"text"}
	if supportsImage {
		modalities = append(modalities, "image")
	}
	modalitiesMap := map[string][]string{
		"input":  modalities,
		"output": []string{"text"},
	}

	// Claude Code 2.x reads these capability fields from /v1/models to decide:
	//   - whether to show the model in the picker as "available" (vs grayed out)
	//   - whether to enable the effort / thinking control
	//   - whether to surface it in /model command instead of falling back to a
	//     stale "Opus 4 has been updated to the latest" placeholder
	// The thinking suffix is what tells the upstream proxy to flip Kiro into
	// extended-thinking mode; when it's present in the model id, Claude Code
	// also wants supports_extended_thinking=true on the response so the
	// effort slider activates. We avoid calling config.GetThinkingConfig()
	// inside this function so it stays pure / testable.
	thinkingSuffix := ""
	if c := config.GetThinkingConfigOrEmpty(); c != nil {
		thinkingSuffix = c.Suffix
	}
	supportsExtendedThinking := thinkingSuffix != "" && strings.HasSuffix(id, thinkingSuffix)
	contextWindow := getContextWindowSize(id)
	maxOutputTokens := 8192
	if strings.Contains(strings.ToLower(id), "opus") {
		maxOutputTokens = 32000
	}

	createdAt := modelCreatedAt(id)

	out := map[string]interface{}{
		// Anthropic spec fields (https://docs.claude.com/en/api/models-list)
		"id":           id,
		"type":         "model",
		"display_name": modelDisplayName(id, supportsExtendedThinking),
		"created_at":   createdAt,
		// Legacy OpenAI-shaped fields kept for SDKs that walk both keys.
		"object":   "model",
		"owned_by": ownedBy,
		"created":  modelCreatedUnix(id),
		// Capability flags Claude Code 2.x and many SDKs introspect.
		"supports_image":             supportsImage,
		"supports_vision":            supportsImage,
		"supports_extended_thinking": supportsExtendedThinking,
		"supports_tools":             true,
		"supports_streaming":         true,
		"supports_function_calling":  true,
		"max_tokens":                 maxOutputTokens,
		"max_output_tokens":          maxOutputTokens,
		"context_window":             contextWindow,
		"context_length":             contextWindow,
		"input_modalities":           modalities,
		"output_modalities":          []string{"text"},
		"modalities":                 modalitiesMap,
		"capabilities": map[string]bool{
			"vision":            supportsImage,
			"image":             supportsImage,
			"image_vision":      supportsImage,
			"tools":             true,
			"streaming":         true,
			"function_calling":  true,
			"extended_thinking": supportsExtendedThinking,
			"reasoning":         supportsExtendedThinking,
		},
		"info": map[string]interface{}{
			"meta": map[string]interface{}{
				"capabilities": map[string]bool{
					"vision":            supportsImage,
					"image_vision":      supportsImage,
					"extended_thinking": supportsExtendedThinking,
					"reasoning":         supportsExtendedThinking,
				},
			},
		},
	}
	return out
}

// modelDisplayName builds a human-friendly label from the canonical id so
// Claude Code's picker shows e.g. "Claude Opus 4.7" instead of the raw
// "claude-opus-4-7". Date stamps are stripped for legacy clients still
// asking with the dated form.
func modelDisplayName(id string, withThinking bool) string {
	base := id
	suffix := ""
	if c := config.GetThinkingConfigOrEmpty(); c != nil {
		suffix = c.Suffix
	}
	if suffix != "" {
		base = strings.TrimSuffix(base, suffix)
	}
	// Drop YYYYMMDD date stamp if present — keep the family + version.
	if idx := strings.LastIndex(base, "-"); idx > 0 && len(base)-idx == 9 {
		tail := base[idx+1:]
		allDigits := true
		for _, r := range tail {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			base = base[:idx]
		}
	}
	parts := strings.Split(base, "-")
	cap := func(s string) string {
		if s == "" {
			return s
		}
		return strings.ToUpper(s[:1]) + s[1:]
	}
	pretty := make([]string, 0, len(parts))
	for _, p := range parts {
		switch p {
		case "claude":
			pretty = append(pretty, "Claude")
		case "opus", "sonnet", "haiku":
			pretty = append(pretty, cap(p))
		default:
			// Re-merge the version digits with a dot ("4-7" -> "4.7").
			if len(p) == 1 && p[0] >= '0' && p[0] <= '9' && len(pretty) > 0 {
				pretty[len(pretty)-1] = pretty[len(pretty)-1] + "." + p
				continue
			}
			pretty = append(pretty, p)
		}
	}
	label := strings.Join(pretty, " ")
	if withThinking {
		label += " (Thinking)"
	}
	return label
}

// modelCreatedAt returns an RFC3339 timestamp for the model. Claude Code uses
// this as a tie-breaker when multiple variants share a family. We extract the
// YYYYMMDD suffix when present, otherwise return today's date so the model is
// always treated as current.
func modelCreatedAt(id string) string {
	suffix := ""
	if c := config.GetThinkingConfigOrEmpty(); c != nil {
		suffix = c.Suffix
	}
	base := id
	if suffix != "" {
		base = strings.TrimSuffix(base, suffix)
	}
	if idx := strings.LastIndex(base, "-"); idx > 0 && len(base)-idx == 9 {
		tail := base[idx+1:]
		allDigits := true
		for _, r := range tail {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits && len(tail) == 8 {
			return tail[:4] + "-" + tail[4:6] + "-" + tail[6:8] + "T00:00:00Z"
		}
	}
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}

// modelCreatedUnix returns the OpenAI-shaped Unix-seconds variant of the
// created_at timestamp.
func modelCreatedUnix(id string) int64 {
	t, err := time.Parse(time.RFC3339, modelCreatedAt(id))
	if err != nil {
		return time.Now().Unix()
	}
	return t.Unix()
}

// refreshModelsCache 从 Kiro API 拉取模型列表并缓存
//
// Per-account upstream calls run concurrently with a small worker pool.
// Each goroutine writes its own per-account result into a slice indexed by
// account position; the merge into the global aggregate happens once on
// the main goroutine after all workers finish, so we don't need a mutex
// around mergeUniqueModels (which is not concurrency-safe).
func (h *Handler) refreshModelsCache() {
	accounts := config.GetEnabledAccounts()
	if len(accounts) == 0 {
		return
	}

	type accountResult struct {
		models   []ModelInfo
		modelIDs []string
		account  *config.Account
		err      error
	}
	results := make([]accountResult, len(accounts))
	sem := make(chan struct{}, refreshFanoutConcurrency)
	var wg sync.WaitGroup

	for i := range accounts {
		i := i
		account := &accounts[i]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf("[ModelsCache] refresh worker panic for %s: %v", account.Email, r)
					results[i] = accountResult{account: account, err: fmt.Errorf("worker panic: %v", r)}
				}
			}()
			if err := h.ensureValidToken(account); err != nil {
				results[i] = accountResult{account: account, err: fmt.Errorf("token refresh failed: %w", err)}
				return
			}
			models, err := ListAvailableModels(account)
			if err != nil {
				results[i] = accountResult{account: account, err: err}
				return
			}
			modelIDs := make([]string, 0, len(models))
			for _, m := range models {
				modelIDs = append(modelIDs, m.ModelId)
			}
			results[i] = accountResult{models: models, modelIDs: modelIDs, account: account}
		}()
	}
	wg.Wait()

	aggregated := make([]ModelInfo, 0)
	for i := range results {
		r := &results[i]
		if r.account == nil {
			continue
		}
		if r.err != nil {
			logger.Warnf("[ModelsCache] Failed to refresh for %s: %v", r.account.Email, r.err)
			continue
		}
		// 缓存每账号可用模型，用于路由时过滤
		h.pool.SetModelList(r.account.ID, r.modelIDs)
		aggregated = mergeUniqueModels(aggregated, r.models)
	}

	if len(aggregated) > 0 {
		h.modelsCacheMu.Lock()
		h.cachedModels = aggregated
		h.modelsCacheTime = time.Now().Unix()
		h.modelsCacheMu.Unlock()
		logger.Infof("[ModelsCache] Cached %d models", len(aggregated))

		// Persist the catalog as last-known-good so a restart serves real
		// model ids immediately instead of the hardcoded bootstrap list.
		// Best-effort: a write failure is logged but doesn't affect serving.
		ids := make([]string, 0, len(aggregated))
		for _, m := range aggregated {
			ids = append(ids, m.ModelId)
		}
		if err := config.SetKnownModels(ids); err != nil {
			logger.Warnf("[ModelsCache] Failed to persist known models: %v", err)
		}
	}
}

// triggerModelsRefreshAsync runs refreshModelsCache in the background, collapsing
// concurrent callers via the modelsRefreshing CAS gate. Used on cold-cache /v1/models
// hits so the request can serve fallbackAnthropicModels immediately while the per-
// account fan-out finishes asynchronously. If a refresh is already in flight the
// new caller returns immediately — the inflight goroutine will populate the cache.
func (h *Handler) triggerModelsRefreshAsync() {
	if !h.modelsRefreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer h.modelsRefreshing.Store(false)
		// Recover from any panic in the per-account fan-out so a single
		// upstream surprise can't tear down the whole proxy. The CAS gate
		// is still cleared by the deferred Store above (deferred funcs run
		// during unwind in LIFO order, so this recover sees the panic).
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[ModelsCache] async refresh panic: %v", r)
			}
		}()
		h.refreshModelsCache()
	}()
}

// fetchAndCacheAccountModels 为单个账号拉取并写入模型缓存。
// 同时更新 pool 的路由缓存与全局聚合模型列表。
func (h *Handler) fetchAndCacheAccountModels(account *config.Account) error {
	if err := h.ensureValidToken(account); err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}
	models, err := ListAvailableModels(account)
	if err != nil {
		return err
	}
	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ModelId)
	}
	h.pool.SetModelList(account.ID, modelIDs)

	// 合并到聚合缓存
	h.modelsCacheMu.Lock()
	h.cachedModels = mergeUniqueModels(h.cachedModels, models)
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()

	logger.Infof("[ModelsCache] Refreshed %d models for account %s", len(models), account.Email)
	return nil
}

// apiRefreshAccountModels POST /admin/api/accounts/{id}/models/refresh
// 立即为指定账号拉取并更新模型路由缓存。
func (h *Handler) apiRefreshAccountModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	// 从 pool 取运行时最新 token（与 refreshModelsCache 逻辑一致）
	if latest := h.pool.GetByID(id); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
	}
	if err := h.fetchAndCacheAccountModels(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"count":   len(h.pool.GetModelList(id)),
	})
}

// apiRefreshAllAccountsModels POST /admin/api/accounts/models/refresh
// 直接复用 refreshModelsCache，为所有已启用账号刷新模型路由缓存。
func (h *Handler) apiRefreshAllAccountsModels(w http.ResponseWriter, r *http.Request) {
	h.refreshModelsCache()
	h.modelsCacheMu.RLock()
	cachedLen := len(h.cachedModels)
	h.modelsCacheMu.RUnlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"refreshed": cachedLen,
		"failed":    0,
	})
}

func mergeUniqueModels(existing []ModelInfo, incoming []ModelInfo) []ModelInfo {
	if len(incoming) == 0 {
		return existing
	}

	indexByID := make(map[string]int, len(existing))
	merged := make([]ModelInfo, len(existing))
	copy(merged, existing)
	for i, model := range merged {
		indexByID[strings.ToLower(strings.TrimSpace(model.ModelId))] = i
	}

	for _, model := range incoming {
		key := strings.ToLower(strings.TrimSpace(model.ModelId))
		if key == "" {
			continue
		}
		if idx, ok := indexByID[key]; ok {
			merged[idx] = mergeModelInfo(merged[idx], model)
			continue
		}
		indexByID[key] = len(merged)
		merged = append(merged, model)
	}

	return merged
}

func mergeModelInfo(base ModelInfo, extra ModelInfo) ModelInfo {
	if base.ModelName == "" {
		base.ModelName = extra.ModelName
	}
	if base.Description == "" {
		base.Description = extra.Description
	}
	if base.RateMultiplier == 0 {
		base.RateMultiplier = extra.RateMultiplier
	}
	if base.TokenLimits == nil {
		base.TokenLimits = extra.TokenLimits
	}
	if base.AdditionalModelRequestFieldsSchema == nil {
		base.AdditionalModelRequestFieldsSchema = extra.AdditionalModelRequestFieldsSchema
	}
	base.InputTypes = mergeStringLists(base.InputTypes, extra.InputTypes)
	return base
}

// effortLevelsForModel returns the reasoning-effort levels the given (mapped,
// upstream) model id accepts, read from the cached ListAvailableModels schema.
// Returns nil when the model is unknown to the cache or declares no effort
// field — callers then skip native effort and rely on the thinking on/off path.
func (h *Handler) effortLevelsForModel(modelID string) []string {
	key := strings.ToLower(strings.TrimSpace(modelID))
	if key == "" {
		return nil
	}
	h.modelsCacheMu.RLock()
	defer h.modelsCacheMu.RUnlock()
	for i := range h.cachedModels {
		if strings.ToLower(strings.TrimSpace(h.cachedModels[i].ModelId)) == key {
			return modelEffortLevels(h.cachedModels[i].AdditionalModelRequestFieldsSchema)
		}
	}
	return nil
}

// applyReasoningEffort forwards a graded reasoning-effort value to the Kiro
// upstream NATIVELY when the resolved model supports it, by populating
// payload.AdditionalModelRequestFields with {"output_config":{"effort":LEVEL}}.
//
// It is gated on the model's own advertised schema (effortLevelsForModel): the
// requested level is clamped DOWN to the nearest supported level, and the field
// is omitted entirely for models that don't support effort or for unset/
// "minimal" requests (those are handled by the thinking on/off path instead).
// This is safe against the upstream's HTTP 400 "model does not support
// additional fields" validation because we never send a level the model didn't
// declare. Returns the level actually attached ("" if none) for logging/echo.
func (h *Handler) applyReasoningEffort(payload *KiroPayload, rawEffort string) string {
	if payload == nil {
		return ""
	}
	modelID := payload.ConversationState.CurrentMessage.UserInputMessage.ModelID
	levels := h.effortLevelsForModel(modelID)
	level, ok := resolveModelEffort(rawEffort, levels)
	if !ok {
		return ""
	}
	fields := buildEffortRequestFields(level)
	if fields == nil {
		return ""
	}
	if payload.AdditionalModelRequestFields == nil {
		payload.AdditionalModelRequestFields = fields
	} else {
		// Merge without clobbering any other passthrough keys already set.
		payload.AdditionalModelRequestFields["output_config"] = fields["output_config"]
	}
	// Stash the resolved level so recordSuccess can attribute per-effort
	// analytics without threading it through every handler signature.
	payload.ResolvedEffort = level
	logger.Debugf("[Effort] Forwarding native reasoning effort %q for model %s (requested %q, supported %v)", level, modelID, rawEffort, levels)
	return level
}

func mergeStringLists(base []string, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base)+len(extra))
	merged := make([]string, 0, len(base)+len(extra))
	for _, item := range base {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	for _, item := range extra {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	return merged
}

// handleCountTokens Token 计数（Claude Code 会调用）
func (h *Handler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	applyAdaptiveThinkingDefault(&req)
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)
	_ = actualModel // mapping is performed internally by ClaudeToKiro for the Kiro upstream call
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)

	estimatedTokens := estimateClaudeRequestInputTokens(effectiveReq)
	if estimatedTokens < 1 {
		estimatedTokens = 1
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": estimatedTokens})
}

// handleClaudeMessages Claude API 处理
func (h *Handler) handleClaudeMessages(w http.ResponseWriter, r *http.Request) {
	h.handleClaudeMessagesInternal(w, r)
}

func (h *Handler) handleClaudeMessagesInternal(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	// 读取请求
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON: "+err.Error())
		return
	}
	// Default to adaptive thinking for Claude-family models when the client
	// did not specify any thinking config. Kiro upstream uses adaptive thinking
	// by default; setting thinking.type="adaptive" on the inbound request makes
	// Claude Code's /model panel surface the "thinking" indicator and routes
	// reasoning content through the response builder.
	applyAdaptiveThinkingDefault(&req)
	if msg := validateClaudeRequestShape(&req); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	// Per-key pre-flight gate. If the matched API key is exhausted on any
	// dimension (rate, periodic, lifetime, expiry), reject with 429 BEFORE
	// we burn an upstream Kiro account quota slot.
	if h.enforceAPIKeyLimit(w, r, req.Model) {
		return
	}

	// Snapshot thinking config once at the top of the request so the
	// downstream mapping + response options + adaptive defaults all share a
	// single cfgLock.RLock. Earlier revisions called config.GetThinkingConfig
	// twice in this handler and three times in the OpenAI variant.
	thinkingCfg := config.GetThinkingConfig()

	actualModel, _ := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)

	// 解析模型和 thinking 模式
	_, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)

	// Fold Anthropic's native reasoning-effort knob (output_config.effort, which
	// is what Claude Code's CLAUDE_CODE_EFFORT_LEVEL maps onto) into the thinking
	// decision, mirroring the OpenAI path: an explicit "minimal" turns reasoning
	// off, low/medium/high/xhigh/max turn it on, and an unset value leaves the
	// suffix/thinking-config decision untouched. The graded level is forwarded
	// natively below (and inside the agentic loops via ClaudeToKiro) for models
	// that advertise output_config.effort support.
	claudeEffort := claudeRequestEffort(&req)
	thinking = resolveThinkingWithEffort(thinking, claudeEffort)

	matchedKeyID := ""
	if k := matchedAPIKey(r); k != nil {
		matchedKeyID = k.ID
	}

	// Web-search agentic loop: only when the feature is enabled AND the request
	// actually carries a web_search tool. This path runs the search via Kiro's
	// native MCP endpoint and answers with native citation blocks. Every other
	// request falls through to the original, unchanged handler path below.
	if config.GetWebSearchEnabled() {
		if _, ok := findClaudeWebSearchTool(req.Tools); ok {
			h.handleClaudeWebSearch(w, &req, req.Model, matchedKeyID, thinking)
			return
		}
	}

	// Tool-search agentic loop: only when the feature is enabled AND the request
	// carries a tool_search server tool together with at least one deferred
	// (defer_loading) tool. This withholds the deferred tool schemas from the
	// upstream model, runs the regex/BM25 match proxy-side, and answers with
	// native tool_search_tool_result blocks. A tool_search tool with no deferred
	// tools is inert and falls through to the normal path below.
	if config.GetToolSearchEnabled() {
		if requestHasToolSearch(req.Tools) {
			h.handleClaudeToolSearch(w, &req, req.Model, matchedKeyID, thinking)
			return
		}
	}

	// mapping happens inside ClaudeToKiro; keep req.Model as the original id
	// (e.g. "claude-opus-4-7" or a dated alias the client still has cached)
	// so the response echoes the exact id the client sent.
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)
	thinkingResponseOpts := resolveClaudeThinkingResponseOptions(req.Thinking, thinkingCfg.ClaudeFormat)
	estimatedInputTokens := estimateClaudeRequestInputTokens(effectiveReq)
	cacheProfile := h.promptCache.BuildClaudeProfile(effectiveReq, estimatedInputTokens)

	// 转换请求（与账号无关，可在多账号故障转移之间复用）
	kiroPayload := ClaudeToKiro(&req, thinking)

	// Forward graded reasoning effort natively when the resolved model supports
	// it (output_config.effort), clamped to the model's advertised levels. No-op
	// for models without effort support or an unset/"minimal" request, where the
	// thinking on/off decision above already applied. Sets payload.ResolvedEffort
	// so the per-effort success analytics on this path are populated.
	h.applyReasoningEffort(kiroPayload, claudeEffort)

	// matchedKeyID was resolved above (before the web-search branch) and is
	// reused here for the per-key success debit on the normal path.

	// Run the upstream call with multi-account failover. The worker is
	// invoked once per attempt with a freshly-selected account; a retryable
	// pre-commit failure rotates to a peer (see runWithFailover).
	worker := func(account *config.Account) (bool, error) {
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)
		// CallKiroAPI mutates payload.ProfileArn to the account's own ARN;
		// reset it so each attempt resolves the ARN for ITS account, not the
		// previous failed one.
		kiroPayload.ProfileArn = ""
		if req.Stream {
			return h.handleClaudeStream(r.Context(), w, account, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheUsage, cacheProfile, matchedKeyID)
		}
		return h.handleClaudeNonStream(r.Context(), w, account, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheUsage, cacheProfile, matchedKeyID)
	}

	committed, retryAfter, err := h.runWithFailover(actualModel, matchedKeyID, kiroPayload.ResolvedEffort, worker)
	if committed {
		return // worker already wrote the full response (or a mid-stream error)
	}
	if err == nil {
		// No account available at all.
		if retryAfter > 0 {
			setRetryAfter(w, retryAfter)
			h.sendClaudeError(w, 429, "rate_limit_error", "All accounts are rate limited; retry after "+strconv.Itoa(retryAfterSeconds(retryAfter))+"s")
			return
		}
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}
	// Every attempted account failed pre-commit. Surface the most useful
	// status: 429 + Retry-After when throttled, else the upstream error.
	if retryAfter > 0 {
		setRetryAfter(w, retryAfter)
		h.sendClaudeError(w, 429, "rate_limit_error", "All accounts are rate limited; retry after "+strconv.Itoa(retryAfterSeconds(retryAfter))+"s")
		return
	}
	h.sendClaudeError(w, 503, "api_error", safeUpstreamError("claude messages failover", err))
}

// handleClaudeStream Claude 流式响应. Returns (committed, err). It defers the
// message_start frame until the first upstream byte arrives so that a
// pre-commit failure (auth/429/connection error before any content) returns
// (false, err) and lets the dispatcher fail over to another account without
// the client ever seeing a partial stream. Once any frame is flushed,
// committed=true and a later error is surfaced inline as an SSE error event.
func (h *Handler) handleClaudeStream(ctx context.Context, w http.ResponseWriter, account *config.Account, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheUsage promptCacheUsage, cacheProfile *promptCacheProfile, apiKeyID string) (bool, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, 500, "api_error", "Streaming not supported")
		return true, nil
	}

	// 获取 thinking 输出格式配置
	thinkingFormat := thinkingOpts.Format

	msgID := "msg_" + uuid.New().String()
	var inputTokens, outputTokens int
	var credits float64
	var realInputTokens int
	var toolUses []KiroToolUse
	var nextContentIndex int
	var rawContentBuilder strings.Builder
	var rawThinkingBuilder strings.Builder
	var upstreamStopReason string
	activeBlockIndex := -1
	activeBlockType := ""
	startInputTokens := estimatedInputTokens

	// committed flips to true the moment we write the first byte to the
	// client (message_start). Before that, the dispatcher may fail over.
	committed := false

	// writeMu serializes every byte written to the client. Without it the
	// heartbeat goroutine (below) and the upstream-driven callbacks would race
	// on the ResponseWriter. All SSE frames — content and pings — go through
	// emit/commit so the lock is the single choke point.
	var writeMu sync.Mutex
	emit := func(event string, data interface{}) {
		writeMu.Lock()
		h.sendSSE(w, flusher, event, data)
		writeMu.Unlock()
	}
	// commit writes the SSE response headers + message_start exactly once,
	// on the first upstream signal. Idempotent. committed is flipped only
	// AFTER message_start is on the wire (all under writeMu), so a concurrent
	// heartbeat tick can never emit a ping before message_start.
	commit := func() {
		writeMu.Lock()
		defer writeMu.Unlock()
		if committed {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		h.sendSSE(w, flusher, "message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":            msgID,
				"type":          "message",
				"role":          "assistant",
				"content":       []interface{}{},
				"model":         canonicalAnthropicModelID(model),
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         buildClaudeUsageMap(startInputTokens, 0, cacheUsage, cacheProfile != nil),
			},
		})
		committed = true
	}

	// Downstream heartbeat: once committed, emit an Anthropic `ping` event every
	// streamHeartbeatInterval while the upstream is silent. This is what keeps
	// Claude Code from treating a legitimately quiet generation (a long thinking
	// pause, a slow tool gap) as a dead connection and aborting it client-side.
	// Pings are harmless anywhere in an Anthropic stream. The returned stop is
	// called before the terminal frames are written so no ping can follow
	// message_stop. The tick writes under writeMu and only after commit, so it
	// can never interleave a half-written frame or precede message_start.
	stopHB := startSSEHeartbeat(streamHeartbeatInterval, func() {
		writeMu.Lock()
		if committed {
			// Roll the write deadline forward so a healthy long stream is never
			// cut by the server's static WriteTimeout, then emit the keepalive.
			rollWriteDeadline(w)
			h.sendSSE(w, flusher, "ping", map[string]interface{}{"type": "ping"})
		}
		writeMu.Unlock()
	})
	defer stopHB()

	closeActiveBlock := func() {
		if activeBlockIndex < 0 {
			return
		}
		emit("content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": activeBlockIndex,
		})
		activeBlockIndex = -1
		activeBlockType = ""
	}

	startContentBlock := func(blockType string) {
		if activeBlockType == blockType {
			return
		}
		commit() // ensure message_start precedes any content frame
		closeActiveBlock()

		idx := nextContentIndex
		nextContentIndex++

		if blockType == "thinking" {
			emit("content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]string{
					"type":     "thinking",
					"thinking": "",
				},
			})
		} else {
			emit("content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]string{
					"type": "text",
					"text": "",
				},
			})
		}

		activeBlockIndex = idx
		activeBlockType = blockType
	}

	// Thinking 标签解析状态由 thinkingTextProcessor 内部管理；这里不再
	// 单独保存 textBuffer / inThinkingBlock / dropTagThinking / thinkingSource。

	// 发送文本的辅助函数
	// thinkingState: 0=普通内容, 1=thinking开始, 2=thinking中间, 3=thinking结束
	sendText := func(text string, thinkingState int) {
		if thinkingState == 0 {
			// 普通内容
			if text == "" {
				return
			}
			startContentBlock("text")
			emit("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": activeBlockIndex,
				"delta": map[string]string{"type": "text_delta", "text": text},
			})
			return
		}

		if !thinking {
			return
		}

		switch thinkingFormat {
		case "think":
			var outputText string
			switch thinkingState {
			case 1:
				outputText = "<think>" + text
			case 2:
				outputText = text
			case 3:
				outputText = text + "</think>"
			}
			if outputText == "" {
				return
			}
			startContentBlock("text")
			emit("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": activeBlockIndex,
				"delta": map[string]string{"type": "text_delta", "text": outputText},
			})
		case "reasoning_content":
			if text == "" {
				return
			}
			startContentBlock("text")
			emit("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": activeBlockIndex,
				"delta": map[string]string{"type": "text_delta", "text": text},
			})
		default:
			if thinkingOpts.OmitDisplay {
				if thinkingState == 1 {
					startContentBlock("thinking")
					return
				}
				if thinkingState == 3 {
					if activeBlockType != "thinking" {
						startContentBlock("thinking")
					}
					closeActiveBlock()
				}
				return
			}
			if thinkingState == 3 && text == "" {
				if activeBlockType == "thinking" {
					closeActiveBlock()
				}
				return
			}
			if text != "" {
				startContentBlock("thinking")
				emit("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "thinking_delta", "thinking": text},
				})
			}
			if thinkingState == 3 && activeBlockType == "thinking" {
				closeActiveBlock()
			}
		}
	}

	// 处理文本，解析 <thinking> 标签 — 实现复用 thinkingTextProcessor。
	// 旧版本在 Anthropic 与 OpenAI 路径各自维护一份相同的状态机闭包，
	// 上游修复（例如截断 bug 的 15-rune 收尾）只改一处会导致漂移。
	processor := newThinkingProcessor(thinking, sendText, allowReasoningSource, allowTagSource)

	callback := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if text == "" {
				return
			}
			if isThinking {
				rawThinkingBuilder.WriteString(text)
			} else {
				rawContentBuilder.WriteString(text)
			}
			processor.Process(text, isThinking)
		},
		OnToolUse: func(tu KiroToolUse) {
			commit() // ensure message_start precedes the tool_use block
			// 先刷新缓冲区
			processor.Finalize()
			rawContentBuilder.WriteString(tu.Name)
			if b, err := json.Marshal(tu.Input); err == nil {
				rawContentBuilder.Write(b)
			}

			toolUses = append(toolUses, tu)
			closeActiveBlock()

			idx := nextContentIndex
			nextContentIndex++

			emit("content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]interface{}{
					"type":  "tool_use",
					"id":    tu.ToolUseID,
					"name":  tu.Name,
					"input": map[string]interface{}{},
				},
			})

			inputJSON, _ := json.Marshal(tu.Input)
			emit("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]interface{}{
					"type":         "input_json_delta",
					"partial_json": string(inputJSON),
				},
			})

			emit("content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": idx,
			})
		},
		OnComplete: func(inTok, outTok int) {
			inputTokens = inTok
			outputTokens = outTok
		},
		OnCredits: func(c float64) {
			credits = c
		},
		OnContextUsage: func(pct float64) {
			realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
		},
		OnStopReason: func(r string) { upstreamStopReason = r },
	}

	err := CallKiroAPIContext(ctx, account, payload, callback)
	// Upstream is done (or failed). Stop the heartbeat BEFORE writing any
	// terminal/error frame so a ping can never land after message_stop.
	stopHB()
	if err != nil {
		if !committed {
			// Nothing reached the client yet — let the dispatcher fail over.
			// Cool this account now; the dispatcher counts the global failure
			// once all attempts are exhausted.
			h.recordAttemptError(err, account.ID)
			return false, err
		}
		// Already streaming — we can't fail over. Record the failure (this
		// committed request failed mid-stream) and surface it inline as an
		// SSE error event so the client sees it. safeStreamErrorMessage never
		// leaks raw Go/transport internals (e.g. "stream error: ...; INTERNAL_ERROR;
		// received from peer" or "http2: client connection lost") — it logs the
		// real cause and returns a stable, friendly message. A recognized stream
		// reset / connection-lost is additionally surfaced as the distinct
		// overloaded_error event type so clients can tell it apart from a generic
		// api_error and retry.
		h.handleUpstreamError(err, account.ID, model, apiKeyID, payload.ResolvedEffort)
		eventType := "error"
		emitType := "api_error"
		var sre *ErrUpstreamStreamReset
		if errors.As(err, &sre) {
			eventType = "overloaded_error"
			emitType = "overloaded_error"
		}
		emitError := map[string]string{"type": emitType, "message": safeStreamErrorMessage(err)}
		emit(eventType, map[string]interface{}{
			"type":  eventType,
			"error": emitError,
		})
		return true, nil
	}

	// Success. Ensure message_start was sent even when upstream produced zero
	// content blocks (rare, but the client still needs a well-formed stream).
	commit()

	// 刷新剩余缓冲区
	processor.Finalize()
	closeActiveBlock()

	// Token precedence (most → least accurate): the exact upstream count from
	// the event stream wins; if upstream sent none, fall back to the model's
	// own contextUsagePercentage × window (coarse, rounded to a percentage);
	// only estimate locally as a last resort.
	inputTokens = resolveInputTokens(inputTokens, realInputTokens, estimatedInputTokens)
	outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
	thinkingOutput := rawThinkingBuilder.String()
	if thinking && thinkingOutput == "" && extractedReasoning != "" {
		thinkingOutput = extractedReasoning
	}
	if !thinking {
		thinkingOutput = ""
	}
	if outputTokens <= 0 {
		outputTokens = estimateClaudeOutputTokens(outputContent, thinkingOutput, toolUses)
	}

	// Update per-account pool counters BEFORE recordSuccess so the realtime
	// dashboard broadcast (fired inside recordSuccess) already reflects this
	// request's credits/tokens for the account card — otherwise the per-account
	// numbers would lag one request behind the pushed snapshot.
	h.pool.RecordSuccess(account.ID)
	h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
	h.recordSuccess(model, apiKeyID, payload.ResolvedEffort, inputTokens, outputTokens, credits)
	h.triggerAccountRefresh(account.ID)
	if apiKeyID != "" {
		_, _ = config.ConsumeAPIKey(apiKeyID, inputTokens+outputTokens, credits, model)
	}
	h.promptCache.Update(account.ID, cacheProfile)

	// 发送 message_delta
	stopReason := resolveAnthropicStopReason(upstreamStopReason, len(toolUses) > 0)

	emit("message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason": stopReason,
		},
		"usage": buildClaudeUsageMap(inputTokens, outputTokens, cacheUsage, cacheProfile != nil),
	})

	emit("message_stop", map[string]interface{}{
		"type": "message_stop",
	})
	return true, nil
}

// streamWriteDeadlineWindow is how far ahead the streaming write deadline is
// pushed on each heartbeat tick. It must exceed streamHeartbeatInterval (so a
// healthy stream's deadline is always refreshed before it lapses) yet stay
// finite (so a genuinely stuck downstream write to a dead client eventually
// fails instead of blocking a goroutine + upstream slot forever). This replaces
// the server's fixed 30-minute WriteTimeout for streaming responses: Go sets
// that deadline once at response start and never extends it on Flush, so
// without this a healthy generation running past 30 minutes would be cut
// mid-work.
const streamWriteDeadlineWindow = 2 * time.Minute

// rollWriteDeadline pushes the response write deadline forward by
// streamWriteDeadlineWindow. Best-effort: if the ResponseWriter doesn't support
// deadlines (rare; httptest recorders, some middlewares) the error is ignored
// and the server's static WriteTimeout remains in force.
func rollWriteDeadline(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(streamWriteDeadlineWindow))
}

// startSSEHeartbeat starts a goroutine that calls tick every interval until the
// returned stop func is invoked. stop blocks until the goroutine has exited, so
// callers can rely on no further ticks running once stop returns — that join is
// what guarantees a heartbeat ping can never be written after the terminal SSE
// frames. stop is idempotent. A non-positive interval disables the heartbeat
// entirely (stop is then a no-op).
func startSSEHeartbeat(interval time.Duration, tick func()) func() {
	if interval <= 0 {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		// Recover so a panic inside tick() (e.g. a nil flusher / ResponseWriter
		// edge case) can't crash the whole process — this is a spawned
		// goroutine, which net/http does NOT recover for us.
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[SSEHeartbeat] tick panic recovered: %v", r)
			}
		}()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				tick()
			}
		}
	}()
	return func() {
		once.Do(func() { close(stop) })
		<-done
	}
}

func (h *Handler) sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	// Build the SSE frame in a single allocation and emit it with one Write
	// call. fmt.Fprintf parses the format string and issues multiple writes
	// per frame; on a streaming hot path that adds up. Pre-sizing the slice
	// avoids the implicit grow inside append.
	frame := make([]byte, 0, len(event)+len(jsonData)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, event...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, jsonData...)
	frame = append(frame, "\n\n"...)
	w.Write(frame)
	flusher.Flush()
}

// writeOpenAIDataFrame emits an OpenAI-style "data: <json>\n\n" SSE chunk in
// a single Write, avoiding the format-string parse on the streaming hot path.
// Caller is expected to Flush — sometimes multiple frames are written before
// a flush (e.g. final chunk + [DONE]).
func writeOpenAIDataFrame(w http.ResponseWriter, jsonData []byte) {
	frame := make([]byte, 0, len(jsonData)+10)
	frame = append(frame, "data: "...)
	frame = append(frame, jsonData...)
	frame = append(frame, "\n\n"...)
	w.Write(frame)
}

// openAIDoneFrame is the byte-for-byte termination chunk for an OpenAI stream.
// Pre-built to skip the format string and string conversion on every request.
var openAIDoneFrame = []byte("data: [DONE]\n\n")

// backgroundStatsSaver 后台定时保存统计数据
func (h *Handler) backgroundStatsSaver() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.saveStats()
		case <-h.stopStatsSaver:
			h.saveStats() // 退出前保存一次
			return
		}
	}
}

// saveStats 保存统计到配置文件
func (h *Handler) saveStats() {
	config.UpdateStats(
		int(atomic.LoadInt64(&h.totalRequests)),
		int(atomic.LoadInt64(&h.successRequests)),
		int(atomic.LoadInt64(&h.failedRequests)),
		int(atomic.LoadInt64(&h.totalTokens)),
		h.getCredits(),
	)
}

// getCredits 线程安全获取 credits
func (h *Handler) getCredits() float64 {
	h.creditsMu.RLock()
	defer h.creditsMu.RUnlock()
	return h.totalCredits
}

// addCredits 线程安全增加 credits
func (h *Handler) addCredits(credits float64) {
	h.creditsMu.Lock()
	h.totalCredits += credits
	h.creditsMu.Unlock()
}

// 统计记录 (使用原子操作)
// recordSuccess updates the global counters AND triggers a debounced quota
// refresh for the account that just served the request, so the dashboard
// reflects new usage within ~30 seconds without waiting for the 5-minute
// global tick. accountID is optional (passed by the success-path callers
// that have it). Also records to the persistent SQLite stats so per-day
// rollups survive restarts.
func (h *Handler) recordSuccess(model, apiKeyID, effort string, inputTokens, outputTokens int, credits float64) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.successRequests, 1)
	atomic.AddInt64(&h.totalTokens, int64(inputTokens+outputTokens))
	h.addCredits(credits)
	stats.Record(model, apiKeyID, effort, true, inputTokens, outputTokens, credits)
	// One-line success trace at INFO so operators can verify the counters
	// chain end-to-end (model -> tokens -> credits -> SQLite). Set
	// LOG_LEVEL=warn in production if this is too noisy; the data is also
	// available via /admin/api/status and the Analytics tab. Gated on level
	// so the hot path skips the getCredits() RLock + arg formatting when the
	// operator runs above INFO.
	if logger.GetLevel() <= logger.LevelInfo {
		logger.Infof("[Stats] model=%s key=%s in=%d out=%d credits=%.4f total_req=%d total_tok=%d total_cred=%.2f",
			model,
			apiKeyIDForLog(apiKeyID),
			inputTokens, outputTokens, credits,
			atomic.LoadInt64(&h.totalRequests),
			atomic.LoadInt64(&h.totalTokens),
			h.getCredits(),
		)
	}
	// Realtime push to subscribed dashboards. Best-effort, non-blocking;
	// see broadcastDashboardUpdate for the slow-consumer policy.
	h.broadcastDashboardUpdate()
}

// apiKeyIDForLog returns "-" when no key is associated, so the log line is
// always parseable as space-separated key=value pairs.
func apiKeyIDForLog(id string) string {
	if id == "" {
		return "-"
	}
	return id
}

func (h *Handler) recordFailure(model, apiKeyID, effort string) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.failedRequests, 1)
	stats.Record(model, apiKeyID, effort, false, 0, 0, 0)
	h.broadcastDashboardUpdate()
}

// checkOverageError 检测 402 超额错误，自动关闭对应账号的超额使用
func (h *Handler) checkOverageError(err error, accountID string) {
	if err == nil {
		return
	}
	errMsg := err.Error()
	if strings.Contains(errMsg, "402") && strings.Contains(errMsg, "OVERAGE") {
		logger.Warnf("[Overage] Detected overage limit error for account %s, disabling AllowOverage", accountID)
		config.DisableAccountOverage(accountID)
	}
}

// handleClaudeNonStream Claude 非流式响应. Returns (committed, err): committed
// is true once the response has been written to the client. On a pre-commit
// upstream failure it returns (false, err) so the dispatcher can fail over
// to another account; the dispatcher (not this function) records the error.
func (h *Handler) handleClaudeNonStream(ctx context.Context, w http.ResponseWriter, account *config.Account, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheUsage promptCacheUsage, cacheProfile *promptCacheProfile, apiKeyID string) (bool, error) {
	var content string
	var thinkingContent string
	var toolUses []KiroToolUse
	var inputTokens, outputTokens int
	var credits float64
	var realInputTokens int
	var upstreamStopReason string

	callback := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				thinkingContent += text
			} else {
				content += text
			}
		},
		OnToolUse: func(tu KiroToolUse) {
			toolUses = append(toolUses, tu)
		},
		OnComplete: func(inTok, outTok int) {
			inputTokens = inTok
			outputTokens = outTok
		},
		OnCredits: func(c float64) {
			credits = c
		},
		OnContextUsage: func(pct float64) {
			realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
		},
		OnStopReason: func(r string) { upstreamStopReason = r },
	}

	err := CallKiroAPIContext(ctx, account, payload, callback)
	if err != nil {
		// Nothing written to the client yet — let the dispatcher fail over.
		// Cool this account (+ overage flip) now; the dispatcher records the
		// single global failed-request count once all attempts are exhausted.
		h.recordAttemptError(err, account.ID)
		return false, err
	}

	// 合并 thinking 内容（如果有 reasoningContentEvent 的内容）
	thinkingFormat := thinkingOpts.Format
	finalContent, extractedReasoning := extractThinkingFromContent(content)
	rawThinkingContent := thinkingContent
	if thinking && rawThinkingContent == "" && extractedReasoning != "" {
		rawThinkingContent = extractedReasoning
	}
	if !thinking {
		rawThinkingContent = ""
	}

	// Token precedence (most → least accurate): the exact upstream count from
	// the event stream wins; if upstream sent none, fall back to the model's
	// own contextUsagePercentage × window (coarse, rounded to a percentage);
	// only estimate locally as a last resort.
	inputTokens = resolveInputTokens(inputTokens, realInputTokens, estimatedInputTokens)
	if outputTokens <= 0 {
		outputTokens = estimateClaudeOutputTokens(finalContent, rawThinkingContent, toolUses)
	}

	// Update per-account pool counters BEFORE recordSuccess so the realtime
	// dashboard broadcast (fired inside recordSuccess) already reflects this
	// request's credits/tokens for the account card — otherwise the per-account
	// numbers would lag one request behind the pushed snapshot.
	h.pool.RecordSuccess(account.ID)
	h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
	h.recordSuccess(model, apiKeyID, payload.ResolvedEffort, inputTokens, outputTokens, credits)
	h.triggerAccountRefresh(account.ID)
	if apiKeyID != "" {
		_, _ = config.ConsumeAPIKey(apiKeyID, inputTokens+outputTokens, credits, model)
	}
	h.promptCache.Update(account.ID, cacheProfile)

	responseThinkingContent := rawThinkingContent
	includeEmptyThinkingBlock := thinking && thinkingOpts.OmitDisplay && rawThinkingContent != ""
	if includeEmptyThinkingBlock {
		responseThinkingContent = ""
	}

	if thinking && responseThinkingContent != "" {
		switch thinkingFormat {
		case "think":
			finalContent = "<think>" + responseThinkingContent + "</think>" + finalContent
			responseThinkingContent = ""
		case "reasoning_content":
			finalContent = responseThinkingContent + finalContent // Claude 格式不支持 reasoning_content，直接拼接
			responseThinkingContent = ""
		default:
		}
	}

	resp := KiroToClaudeResponse(finalContent, responseThinkingContent, includeEmptyThinkingBlock, toolUses, inputTokens, outputTokens, model, upstreamStopReason)
	resp.Usage.InputTokens = billedClaudeInputTokens(inputTokens, cacheUsage)
	resp.Usage.CacheCreationInputTokens = cacheUsage.CacheCreationInputTokens
	resp.Usage.CacheReadInputTokens = cacheUsage.CacheReadInputTokens
	if cacheProfile != nil {
		resp.Usage.CacheCreation = &ClaudeCacheCreationUsage{
			Ephemeral5mInputTokens: cacheUsage.CacheCreation5mInputTokens,
			Ephemeral1hInputTokens: cacheUsage.CacheCreation1hInputTokens,
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(resp)
	return true, nil
}

// safeUpstreamError logs the real upstream error server-side and returns a
// generic, non-revealing message for the client. Raw upstream (AWS / Kiro)
// error strings routinely embed the profile ARN, endpoint host, account region,
// and other backend identifiers; surfacing err.Error() verbatim to a client who
// can deliberately trigger a failure leaks that infrastructure detail. Operators
// still get the full cause in the logs.
func safeUpstreamError(context string, err error) string {
	logger.Errorf("[upstream] %s: %v", context, err)
	return "Upstream service temporarily unavailable"
}

func (h *Handler) sendClaudeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}

// handleOpenAIChat OpenAI API 处理
func (h *Handler) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateOpenAIRequestShape(&req); msg != "" {
		h.sendOpenAIError(w, 400, "invalid_request_error", msg)
		return
	}

	// Per-key pre-flight gate (see handleClaude).
	if h.enforceAPIKeyLimit(w, r, req.Model) {
		return
	}

	// Snapshot thinking config once (see handleClaudeMessages for rationale).
	thinkingCfg := config.GetThinkingConfig()

	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	// Fold the OpenAI reasoning_effort knob into the thinking decision: an
	// explicit "minimal" turns reasoning off, low/medium/high/max turns it on,
	// and an unset value leaves the suffix-derived decision untouched. We map to
	// thinking on/off (not a graded upstream field) because the Kiro backend
	// rejects an explicit reasoning_effort payload — see reasoning_effort.go.
	thinking = resolveThinkingWithEffort(thinking, req.ReasoningEffort)
	// OpenAIToKiro maps the model internally for the Kiro upstream call;
	// keep req.Model as the original id so the response echoes what the client sent.
	estimatedInputTokens := estimateOpenAIRequestInputTokens(&req)

	kiroPayload := OpenAIToKiro(&req, thinking)

	// Forward graded reasoning effort natively when the resolved model supports
	// it (output_config.effort). Gated on the model's advertised schema; a
	// no-op for models without effort support, where the thinking on/off
	// mapping above already applied.
	h.applyReasoningEffort(kiroPayload, req.ReasoningEffort)

	matchedKeyID := ""
	if k := matchedAPIKey(r); k != nil {
		matchedKeyID = k.ID
	}

	worker := func(account *config.Account) (bool, error) {
		kiroPayload.ProfileArn = "" // re-resolve per attempt account
		if req.Stream {
			return h.handleOpenAIStream(r.Context(), w, account, kiroPayload, req.Model, thinking, estimatedInputTokens, matchedKeyID)
		}
		return h.handleOpenAINonStream(r.Context(), w, account, kiroPayload, req.Model, thinking, estimatedInputTokens, matchedKeyID)
	}

	committed, retryAfter, err := h.runWithFailover(actualModel, matchedKeyID, kiroPayload.ResolvedEffort, worker)
	if committed {
		return
	}
	if err == nil {
		if retryAfter > 0 {
			setRetryAfter(w, retryAfter)
			h.sendOpenAIError(w, 429, "rate_limit_exceeded", "All accounts are rate limited; retry after "+strconv.Itoa(retryAfterSeconds(retryAfter))+"s")
			return
		}
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}
	if retryAfter > 0 {
		setRetryAfter(w, retryAfter)
		h.sendOpenAIError(w, 429, "rate_limit_exceeded", "All accounts are rate limited; retry after "+strconv.Itoa(retryAfterSeconds(retryAfter))+"s")
		return
	}
	h.sendOpenAIError(w, 503, "server_error", safeUpstreamError("openai chat failover", err))
}

// handleOpenAIStream OpenAI 流式响应. Returns (committed, err): defers the
// first chunk so a pre-commit failure can fail over to another account.
func (h *Handler) handleOpenAIStream(ctx context.Context, w http.ResponseWriter, account *config.Account, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string) (bool, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return true, nil
	}

	// 获取 thinking 输出格式配置
	thinkingFormat := config.GetThinkingConfig().OpenAIFormat

	chatID := "chatcmpl-" + uuid.New().String()
	// Echo the canonical Anthropic dash form so SDKs / Claude Code resolve the
	// model id correctly on every chunk; the inbound dotted form (e.g.
	// "claude-opus-4.7") is kept in `model` for upstream routing.
	respModel := canonicalAnthropicModelID(model)
	var toolCalls []ToolCall
	var toolCallIndex int
	var inputTokens, outputTokens int
	var credits float64
	var realInputTokens int
	var rawContentBuilder strings.Builder
	var rawReasoningBuilder strings.Builder
	var upstreamStopReason string

	// committed flips true on the first chunk written to the client. emit is
	// the single choke point for streaming bytes: it sets the SSE headers +
	// committed on first use, then writes & flushes one data frame. Before
	// the first emit, a failure can be retried on a peer account.
	//
	// writeMu serializes every write to the client so the downstream heartbeat
	// goroutine (below) can never interleave a half-written frame with a
	// callback-driven chunk. All client writes — chunks, heartbeats, the [DONE]
	// frame — go through emit/emitRaw under this lock.
	var writeMu sync.Mutex
	committed := false
	emitLocked := func(data []byte) {
		if !committed {
			committed = true
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
		}
		writeOpenAIDataFrame(w, data)
		flusher.Flush()
	}
	emit := func(data []byte) {
		writeMu.Lock()
		emitLocked(data)
		writeMu.Unlock()
	}

	// Downstream heartbeat: once committed, emit an SSE comment every
	// streamHeartbeatInterval while the upstream is silent. A comment line
	// (": ...\n\n") is ignored by OpenAI SSE clients but keeps a quiet
	// generation (a long thinking pause, a slow tool gap) from looking like a
	// dead connection to the client's idle timer — matching the Claude path.
	// The tick writes under writeMu and only after commit, and the returned
	// stop joins the goroutine before any terminal frame is written, so a
	// heartbeat can never follow [DONE].
	stopHB := startSSEHeartbeat(streamHeartbeatInterval, func() {
		writeMu.Lock()
		if committed {
			rollWriteDeadline(w)
			w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		}
		writeMu.Unlock()
	})
	defer stopHB()

	// Thinking 标签解析状态由 thinkingTextProcessor 内部管理。

	// 发送 chunk 的辅助函数
	// thinkingState: 0=普通内容, 1=thinking开始, 2=thinking中间, 3=thinking结束
	sendChunk := func(content string, thinkingState int) {
		if content == "" && thinkingState == 2 {
			return
		}

		var chunk map[string]interface{}

		if thinkingState > 0 {
			if !thinking {
				return
			}
			// thinking 内容
			switch thinkingFormat {
			case "thinking":
				// 流式输出标签
				var text string
				switch thinkingState {
				case 1: // 开始
					text = "<thinking>" + content
				case 2: // 中间
					text = content
				case 3: // 结束
					text = content + "</thinking>"
				}
				if text == "" {
					return
				}
				chunk = map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   respModel,
					"choices": []map[string]interface{}{{
						"index":         0,
						"delta":         map[string]string{"content": text},
						"finish_reason": nil,
					}},
				}
			case "think":
				var text string
				switch thinkingState {
				case 1:
					text = "<think>" + content
				case 2:
					text = content
				case 3:
					text = content + "</think>"
				}
				if text == "" {
					return
				}
				chunk = map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   respModel,
					"choices": []map[string]interface{}{{
						"index":         0,
						"delta":         map[string]string{"content": text},
						"finish_reason": nil,
					}},
				}
			default: // "reasoning_content"
				if content == "" {
					return
				}
				chunk = map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   respModel,
					"choices": []map[string]interface{}{{
						"index":         0,
						"delta":         map[string]string{"reasoning_content": content},
						"finish_reason": nil,
					}},
				}
			}
		} else {
			// 普通内容
			if content == "" {
				return
			}
			chunk = map[string]interface{}{
				"id":      chatID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   respModel,
				"choices": []map[string]interface{}{{
					"index":         0,
					"delta":         map[string]string{"content": content},
					"finish_reason": nil,
				}},
			}
		}
		data, _ := json.Marshal(chunk)
		emit(data)
	}

	// 处理文本，解析 <thinking> 标签
	// thinkingStarted 用于跟踪是否已发送开始标签
	// 处理文本，解析 <thinking> 标签 — 与 Anthropic 路径共用 thinkingTextProcessor。
	processor := newThinkingProcessor(thinking, sendChunk, allowReasoningSource, allowTagSource)

	callback := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if text == "" {
				return
			}
			if isThinking {
				rawReasoningBuilder.WriteString(text)
			} else {
				rawContentBuilder.WriteString(text)
			}
			processor.Process(text, isThinking)
		},
		OnToolUse: func(tu KiroToolUse) {
			// 先刷新缓冲区
			processor.Finalize()

			args, _ := json.Marshal(tu.Input)
			rawContentBuilder.WriteString(tu.Name)
			rawContentBuilder.Write(args)
			tc := ToolCall{ID: tu.ToolUseID, Type: "function"}
			tc.Function.Name = tu.Name
			tc.Function.Arguments = string(args)
			toolCalls = append(toolCalls, tc)

			chunk := map[string]interface{}{
				"id":      chatID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   canonicalAnthropicModelID(model),
				"choices": []map[string]interface{}{{
					"index": 0,
					"delta": map[string]interface{}{
						"tool_calls": []map[string]interface{}{{
							"index": toolCallIndex,
							"id":    tu.ToolUseID,
							"type":  "function",
							"function": map[string]string{
								"name":      tu.Name,
								"arguments": string(args),
							},
						}},
					},
					"finish_reason": nil,
				}},
			}
			toolCallIndex++
			data, _ := json.Marshal(chunk)
			emit(data)
		},
		OnComplete: func(inTok, outTok int) {
			inputTokens = inTok
			outputTokens = outTok
		},
		OnCredits: func(c float64) {
			credits = c
		},
		OnContextUsage: func(pct float64) {
			realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
		},
		OnStopReason: func(r string) { upstreamStopReason = r },
	}

	err := CallKiroAPIContext(ctx, account, payload, callback)
	// Upstream is done (or failed). Stop the heartbeat BEFORE reading committed
	// or writing any terminal frame so a ping can never land after [DONE].
	stopHB()
	if err != nil {
		if !committed {
			// Nothing reached the client — let the dispatcher fail over.
			h.recordAttemptError(err, account.ID)
			return false, err
		}
		// Mid-stream failure on a committed response: record it and end the
		// (partial) stream with [DONE]. The OpenAI streaming wire format has
		// no error frame, so the client sees a truncated response.
		h.handleUpstreamError(err, account.ID, model, apiKeyID, payload.ResolvedEffort)
		w.Write(openAIDoneFrame)
		flusher.Flush()
		return true, nil
	}

	// 刷新剩余缓冲区
	processor.Finalize()

	// Token precedence (most → least accurate): the exact upstream count from
	// the event stream wins; if upstream sent none, fall back to the model's
	// own contextUsagePercentage × window (coarse, rounded to a percentage);
	// only estimate locally as a last resort.
	inputTokens = resolveInputTokens(inputTokens, realInputTokens, estimatedInputTokens)
	outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
	reasoningOutput := rawReasoningBuilder.String()
	if thinking && reasoningOutput == "" && extractedReasoning != "" {
		reasoningOutput = extractedReasoning
	}
	if !thinking {
		reasoningOutput = ""
	}
	if outputTokens <= 0 {
		outputTokens = estimateApproxTokens(outputContent) + estimateApproxTokens(reasoningOutput)
		for _, tc := range toolCalls {
			outputTokens += estimateApproxTokens(tc.Function.Name)
			outputTokens += estimateApproxTokens(tc.Function.Arguments)
		}
	}

	// Update per-account pool counters BEFORE recordSuccess so the realtime
	// dashboard broadcast (fired inside recordSuccess) already reflects this
	// request's credits/tokens for the account card — otherwise the per-account
	// numbers would lag one request behind the pushed snapshot.
	h.pool.RecordSuccess(account.ID)
	h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
	h.recordSuccess(model, apiKeyID, payload.ResolvedEffort, inputTokens, outputTokens, credits)
	h.triggerAccountRefresh(account.ID)
	if apiKeyID != "" {
		_, _ = config.ConsumeAPIKey(apiKeyID, inputTokens+outputTokens, credits, model)
	}

	// 发送结束
	finishReason := resolveOpenAIFinishReason(upstreamStopReason, len(toolCalls) > 0)

	chunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   canonicalAnthropicModelID(model),
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         map[string]interface{}{},
			"finish_reason": finishReason,
		}},
		"usage": map[string]int{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
		},
	}
	data, _ := json.Marshal(chunk)
	// Ensure headers are committed even for an empty-content success.
	emit(data)
	w.Write(openAIDoneFrame)
	flusher.Flush()
	return true, nil
}

// handleOpenAINonStream OpenAI 非流式响应. Returns (committed, err); on a
// pre-commit upstream failure returns (false, err) for dispatcher failover.
func (h *Handler) handleOpenAINonStream(ctx context.Context, w http.ResponseWriter, account *config.Account, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string) (bool, error) {
	var content string
	var reasoningContent string
	var toolUses []KiroToolUse
	var inputTokens, outputTokens int
	var credits float64
	var realInputTokens int
	var upstreamStopReason string

	callback := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				reasoningContent += text
			} else {
				content += text
			}
		},
		OnToolUse:  func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
		OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
		OnCredits:  func(c float64) { credits = c },
		OnContextUsage: func(pct float64) {
			realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
		},
		OnStopReason: func(r string) { upstreamStopReason = r },
	}

	err := CallKiroAPIContext(ctx, account, payload, callback)
	if err != nil {
		// Nothing written yet — cool this account and let the dispatcher
		// fail over (it records the single global failure on giving up).
		h.recordAttemptError(err, account.ID)
		return false, err
	}

	// 解析 content 中的 <thinking> 标签
	finalContent, extractedReasoning := extractThinkingFromContent(content)
	if thinking && reasoningContent == "" && extractedReasoning != "" {
		reasoningContent = extractedReasoning
	} else if !thinking {
		reasoningContent = ""
	}

	// Token precedence (most → least accurate): the exact upstream count from
	// the event stream wins; if upstream sent none, fall back to the model's
	// own contextUsagePercentage × window (coarse, rounded to a percentage);
	// only estimate locally as a last resort.
	inputTokens = resolveInputTokens(inputTokens, realInputTokens, estimatedInputTokens)
	if outputTokens <= 0 {
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)
	}

	// Update per-account pool counters BEFORE recordSuccess so the realtime
	// dashboard broadcast (fired inside recordSuccess) already reflects this
	// request's credits/tokens for the account card — otherwise the per-account
	// numbers would lag one request behind the pushed snapshot.
	h.pool.RecordSuccess(account.ID)
	h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
	h.recordSuccess(model, apiKeyID, payload.ResolvedEffort, inputTokens, outputTokens, credits)
	h.triggerAccountRefresh(account.ID)
	if apiKeyID != "" {
		_, _ = config.ConsumeAPIKey(apiKeyID, inputTokens+outputTokens, credits, model)
	}

	thinkingFormat := config.GetThinkingConfig().OpenAIFormat
	resp := KiroToOpenAIResponseWithReasoning(finalContent, reasoningContent, toolUses, inputTokens, outputTokens, model, thinkingFormat, upstreamStopReason)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(resp)
	return true, nil
}

func (h *Handler) sendOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// OpenAI's documented error envelope includes both `code` and `param`.
	// SDKs (langchain, openai-python, litellm) inspect them for retry / tool
	// validation logic, so we surface them even when they're nil.
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
			"code":    nil,
			"param":   nil,
		},
	})
}

// ensureValidToken 确保 token 有效
func (h *Handler) ensureValidToken(account *config.Account) error {
	if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
		return nil
	}

	// Per-account lock: only concurrent refreshes of the SAME account serialize;
	// different accounts refresh in parallel. The prior single Handler-wide mutex
	// meant one slow auth.RefreshToken blocked the refresh-check for every other
	// account on the pool.
	muAny, _ := h.tokenRefreshLocks.LoadOrStore(account.ID, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	// Another concurrent request may have refreshed this account while we waited.
	if latest := h.pool.GetByID(account.ID); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
		if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
			return nil
		}
	}

	accessToken, refreshToken, expiresAt, profileArn, err := auth.RefreshToken(account)
	if err != nil {
		return err
	}

	// 更新内存
	h.pool.UpdateToken(account.ID, accessToken, refreshToken, expiresAt)
	account.AccessToken = accessToken
	if refreshToken != "" {
		account.RefreshToken = refreshToken
	}
	account.ExpiresAt = expiresAt
	if profileArn != "" {
		account.ProfileArn = profileArn
		config.UpdateAccountProfileArn(account.ID, profileArn)
	}

	// 持久化
	config.UpdateAccountToken(account.ID, accessToken, refreshToken, expiresAt)

	return nil
}

// ==================== 管理 API ====================

func (h *Handler) handleAdminAPI(w http.ResponseWriter, r *http.Request) {
	// Header-only auth — explicitly drop the legacy cookie path. The cookie
	// branch was a CSRF surface (any sibling subdomain that could plant
	// admin_password gained admin), and the dashboard already stores the
	// password in localStorage and sends it as X-Admin-Password on every
	// fetch. Header auth forces preflight on cross-origin POSTs, killing
	// the CSRF vector.
	password := r.Header.Get("X-Admin-Password")

	// Per-IP failure rate-limit. After 10 wrong-password attempts within a
	// rolling 5-minute window, refuse further attempts from that IP for the
	// remainder of the window. In-memory only; resets on restart.
	if !h.allowAdminAttempt(r) {
		w.WriteHeader(429)
		json.NewEncoder(w).Encode(map[string]string{"error": "Too many failed attempts; try again later"})
		return
	}

	if !config.VerifyPassword(password) {
		h.recordAdminFailure(r)
		logger.Warnf("[Admin] failed auth from %s for %s", clientIP(r), r.URL.Path)
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}
	h.resetAdminFailures(r)

	// Bound every admin request body to maxRequestBodyBytes. Most admin handlers
	// decode r.Body with json.NewDecoder and no size cap, so without this an
	// authenticated request (or one past the brute-force gate) could POST a
	// multi-GB body and exhaust RAM. Capping once here covers all of them; the
	// import handler re-wraps with the same limit, so this never shrinks its
	// legitimate large-batch allowance.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)

	path := strings.TrimPrefix(r.URL.Path, "/admin/api")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch {
	case path == "/accounts" && r.Method == "GET":
		h.apiGetAccounts(w, r)
	case path == "/accounts" && r.Method == "POST":
		h.apiAddAccount(w, r)
	case path == "/accounts/batch" && r.Method == "POST":
		h.apiBatchAccounts(w, r)
	// models/refresh 必须在通用 /refresh 前匹配，否则会被误拦截
	case path == "/accounts/models/refresh" && r.Method == "POST":
		h.apiRefreshAllAccountsModels(w, r)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/refresh")
		h.apiRefreshAccountModels(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/refresh")
		h.apiRefreshAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/test") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/test")
		h.apiTestAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/overage") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/overage")
		h.apiGetAccountOverage(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/overage") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/overage")
		h.apiSetAccountOverage(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/cached") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/cached")
		h.apiGetAccountModelsCached(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models")
		h.apiGetAccountModels(w, r, id)

	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/full") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/full")
		h.apiGetAccountFull(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && r.Method == "DELETE":
		h.apiDeleteAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case strings.HasPrefix(path, "/accounts/") && r.Method == "PUT":
		h.apiUpdateAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case path == "/auth/iam-sso/start" && r.Method == "POST":
		h.apiStartIamSso(w, r)
	case path == "/auth/iam-sso/complete" && r.Method == "POST":
		h.apiCompleteIamSso(w, r)
	case path == "/auth/builderid/start" && r.Method == "POST":
		h.apiStartBuilderIdLogin(w, r)
	case path == "/auth/builderid/poll" && r.Method == "POST":
		h.apiPollBuilderIdAuth(w, r)
	case path == "/auth/sso-token" && r.Method == "POST":
		h.apiImportSsoToken(w, r)
	case path == "/auth/credentials" && r.Method == "POST":
		h.apiImportCredentials(w, r)
	case path == "/status" && r.Method == "GET":
		h.apiGetStatus(w, r)
	case path == "/settings" && r.Method == "GET":
		h.apiGetSettings(w, r)
	case path == "/settings" && r.Method == "POST":
		h.apiUpdateSettings(w, r)
	case path == "/stats" && r.Method == "GET":
		h.apiGetStats(w, r)
	case path == "/stats/reset" && r.Method == "POST":
		h.apiResetStats(w, r)
	case path == "/generate-machine-id" && r.Method == "GET":
		h.apiGenerateMachineId(w, r)
	case path == "/thinking" && r.Method == "GET":
		h.apiGetThinkingConfig(w, r)
	case path == "/thinking" && r.Method == "POST":
		h.apiUpdateThinkingConfig(w, r)
	case path == "/endpoint" && r.Method == "GET":
		h.apiGetEndpointConfig(w, r)
	case path == "/endpoint" && r.Method == "POST":
		h.apiUpdateEndpointConfig(w, r)
	case path == "/proxy" && r.Method == "GET":
		h.apiGetProxy(w, r)
	case path == "/proxy" && r.Method == "POST":
		h.apiUpdateProxy(w, r)
	case path == "/prompt-filter" && r.Method == "GET":
		h.apiGetPromptFilter(w, r)
	case path == "/prompt-filter" && r.Method == "POST":
		h.apiUpdatePromptFilter(w, r)
	case path == "/apikeys" && r.Method == "GET":
		h.apiListAPIKeys(w, r)
	case path == "/apikeys" && r.Method == "POST":
		h.apiCreateAPIKey(w, r)
	case strings.HasPrefix(path, "/apikeys/") && strings.HasSuffix(path, "/reveal") && r.Method == "GET":
		// /apikeys/<id>/reveal — fetch the full secret for a copy-to-clipboard
		// affordance. Same admin auth as everything else under /admin/api/*.
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/apikeys/"), "/reveal")
		h.apiRevealAPIKey(w, r, id)
	case strings.HasPrefix(path, "/apikeys/") && r.Method == "PUT":
		h.apiUpdateAPIKey(w, r, strings.TrimPrefix(path, "/apikeys/"))
	case strings.HasPrefix(path, "/apikeys/") && r.Method == "DELETE":
		h.apiDeleteAPIKey(w, r, strings.TrimPrefix(path, "/apikeys/"))
	case path == "/modelstats" && r.Method == "GET":
		h.apiGetModelStats(w, r)
	case path == "/available-models" && r.Method == "GET":
		h.apiGetAvailableModels(w, r)
	case path == "/stats/totals" && r.Method == "GET":
		h.apiGetStatsTotals(w, r)
	case path == "/stats/history" && r.Method == "GET":
		h.apiGetStatsHistory(w, r)
	case path == "/version" && r.Method == "GET":
		h.apiGetVersion(w, r)
	case path == "/export" && r.Method == "POST":
		h.apiExportAccounts(w, r)
	case path == "/import" && r.Method == "POST":
		h.apiImportAccounts(w, r)
	case path == "/websearch/probe" && r.Method == "POST":
		h.apiProbeWebSearch(w, r)
	default:
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Not Found"})
	}
}

func (h *Handler) apiGetAccounts(w http.ResponseWriter, r *http.Request) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// 合并运行时统计
	statsMap := make(map[string]config.Account)
	for _, a := range poolAccounts {
		statsMap[a.ID] = a
	}

	// 隐藏敏感信息
	result := make([]map[string]interface{}, len(accounts))
	for i, a := range accounts {
		// 获取运行时统计
		stats := statsMap[a.ID]
		inflight, concurrencyLimit := h.pool.ConcurrencyState(a.ID)
		pacedRate, observedRate := h.pool.RateState(a.ID)
		ttft := h.pool.TTFTState(a.ID)

		result[i] = map[string]interface{}{
			"id":                a.ID,
			"email":             a.Email,
			"userId":            a.UserId,
			"nickname":          a.Nickname,
			"authMethod":        a.AuthMethod,
			"provider":          a.Provider,
			"region":            a.Region,
			"enabled":           a.Enabled,
			"banStatus":         a.BanStatus,
			"banReason":         a.BanReason,
			"banTime":           a.BanTime,
			"expiresAt":         a.ExpiresAt,
			"hasToken":          a.AccessToken != "",
			"machineId":         a.MachineId,
			"weight":            a.Weight,
			"allowOverage":      a.AllowOverage,
			"overageWeight":     a.OverageWeight,
			"overageStatus":     a.OverageStatus,
			"overageCapability": a.OverageCapability,
			"overageCap":        a.OverageCap,
			"overageRate":       a.OverageRate,
			"currentOverages":   a.CurrentOverages,
			"overageCheckedAt":  a.OverageCheckedAt,
			"proxyURL":          a.ProxyURL,
			"subscriptionType":  a.SubscriptionType,
			"subscriptionTitle": a.SubscriptionTitle,
			"daysRemaining":     a.DaysRemaining,
			"usageCurrent":      a.UsageCurrent,
			"usageLimit":        a.UsageLimit,
			"usagePercent":      a.UsagePercent,
			"nextResetDate":     a.NextResetDate,
			"lastRefresh":       a.LastRefresh,
			"trialUsageCurrent": a.TrialUsageCurrent,
			"trialUsageLimit":   a.TrialUsageLimit,
			"trialUsagePercent": a.TrialUsagePercent,
			"trialStatus":       a.TrialStatus,
			"trialExpiresAt":    a.TrialExpiresAt,
			"requestCount":      stats.RequestCount,
			"errorCount":        stats.ErrorCount,
			"totalTokens":       stats.TotalTokens,
			"totalCredits":      stats.TotalCredits,
			"lastUsed":          stats.LastUsed,
			"inflight":          inflight,
			"concurrencyLimit":  concurrencyLimit,
			"pacedRate":         pacedRate,
			"observedRate":      observedRate,
			"ttftMs":            ttft,
			"cooldownSecs":      int(h.pool.CooldownRemaining(a.ID).Round(time.Second).Seconds()),
			"overQuota":         a.UsageLimit > 0 && a.UsageCurrent >= a.UsageLimit,
		}
	}
	json.NewEncoder(w).Encode(result)
}

func (h *Handler) apiAddAccount(w http.ResponseWriter, r *http.Request) {
	var account config.Account
	if err := json.NewDecoder(r.Body).Decode(&account); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if account.ID == "" {
		account.ID = auth.GenerateAccountID()
	}
	if account.Region == "" {
		account.Region = "us-east-1"
	}
	if account.ProxyURL != "" {
		if msg := validateProxyURL(account.ProxyURL); msg != "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": msg})
			return
		}
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 新账号若已启用且有 token，立即拉取并缓存模型列表
	if account.Enabled && account.AccessToken != "" {
		safeGoArg("addAccount-modelsRefresh", account, func(acc config.Account) {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for new account %s: %v", acc.Email, err)
			}
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": account.ID})
}

func (h *Handler) apiDeleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.DeleteAccount(id); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiUpdateAccount(w http.ResponseWriter, r *http.Request, id string) {
	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 获取现有账号
	accounts := config.GetAccounts()
	var existing *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			existing = &accounts[i]
			break
		}
	}
	if existing == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 只更新传入的字段
	oldEnabled := existing.Enabled
	if v, ok := updates["enabled"].(bool); ok {
		existing.Enabled = v
		// Operator made an explicit choice — clear the auto-disable marker
		// either way so the next refresh doesn't re-flip the state. (Manual
		// disable: don't auto-recover later. Manual enable: don't get
		// immediately re-disabled if quota is still full — the operator
		// might have just enabled overage or has a reason.)
		existing.AutoDisabledAtFull = false
	}
	if v, ok := updates["nickname"].(string); ok {
		existing.Nickname = v
	}
	if v, ok := updates["machineId"].(string); ok {
		existing.MachineId = v
	}
	if v, ok := updates["weight"].(float64); ok {
		existing.Weight = int(v)
	}
	if v, ok := updates["allowOverage"].(bool); ok {
		existing.AllowOverage = v
	}
	if v, ok := updates["overageWeight"].(float64); ok {
		existing.OverageWeight = clampInt(int(v), 1, 10)
	}
	if v, ok := updates["proxyURL"].(string); ok {
		if v != "" {
			if msg := validateProxyURL(v); msg != "" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": msg})
				return
			}
		}
		existing.ProxyURL = v
	}

	if err := config.UpdateAccount(id, *existing); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 账号从禁用→启用时，自动拉取并缓存模型列表
	if !oldEnabled && existing.Enabled && existing.AccessToken != "" {
		safeGoArg("updateAccount-modelsRefresh", *existing, func(acc config.Account) {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for re-enabled account %s: %v", acc.Email, err)
			}
		})
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiBatchAccounts 批量操作账号（启用/禁用/刷新/删除/超额开关）
func (h *Handler) apiBatchAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"` // "enable", "disable", "refresh", "delete", "overage-on", "overage-off"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if len(req.IDs) == 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "No account IDs provided"})
		return
	}

	switch req.Action {
	case "enable", "disable":
		enabled := req.Action == "enable"
		accounts := config.GetAccounts()
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var toRefreshModels []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				// 记录本次从禁用→启用、且有 token 的账号
				if enabled && !a.Enabled && a.AccessToken != "" {
					toRefreshModels = append(toRefreshModels, a)
				}
				a.Enabled = enabled
				// Operator made an explicit choice — clear auto-disable marker
				// so the next refresh doesn't re-flip the state. Mirrors the
				// single-account update path.
				a.AutoDisabledAtFull = false
				if enabled && a.BanStatus != "" && a.BanStatus != "ACTIVE" {
					a.BanStatus = "ACTIVE"
					a.BanReason = ""
					a.BanTime = 0
				}
				config.UpdateAccount(a.ID, a)
			}
		}
		h.pool.Reload()
		// 为本次新启用的账号异步拉取模型缓存
		for _, acc := range toRefreshModels {
			a := acc
			a.Enabled = true
			safeGoArg("batchEnable-modelsRefresh", a, func(account config.Account) {
				if err := h.fetchAndCacheAccountModels(&account); err != nil {
					logger.Warnf("[ModelsCache] Auto-refresh failed for batch-enabled account %s: %v", account.Email, err)
				}
			})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": len(req.IDs)})

	case "refresh":
		successCount := 0
		failCount := 0
		for _, id := range req.IDs {
			accounts := config.GetAccounts()
			var account *config.Account
			for i := range accounts {
				if accounts[i].ID == id {
					account = &accounts[i]
					break
				}
			}
			if account == nil {
				failCount++
				continue
			}
			// 刷新 token
			if account.RefreshToken != "" {
				if newAccess, newRefresh, newExpires, profileArn, err := auth.RefreshToken(account); err == nil {
					account.AccessToken = newAccess
					if newRefresh != "" {
						account.RefreshToken = newRefresh
					}
					account.ExpiresAt = newExpires
					config.UpdateAccountToken(id, newAccess, newRefresh, newExpires)
					if profileArn != "" {
						account.ProfileArn = profileArn
						config.UpdateAccountProfileArn(id, profileArn)
					}
					h.pool.UpdateToken(id, newAccess, newRefresh, newExpires)
				}
			}
			// 刷新账户信息
			info, err := RefreshAccountInfo(account)
			if err != nil {
				failCount++
				continue
			}
			config.UpdateAccountInfo(id, *info)
			successCount++
		}
		h.pool.Reload()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"refreshed": successCount,
			"failed":    failCount,
		})

	case "overage-on", "overage-off":
		allow := req.Action == "overage-on"
		idSet := make(map[string]bool, len(req.IDs))
		for _, id := range req.IDs {
			idSet[id] = true
		}
		updated := 0
		for _, a := range config.GetAccounts() {
			if !idSet[a.ID] {
				continue
			}
			a.AllowOverage = allow
			if err := config.UpdateAccount(a.ID, a); err != nil {
				logger.Warnf("[Batch] overage update failed for %s: %v", a.ID, err)
				continue
			}
			updated++
		}
		h.pool.Reload()
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": updated})

	case "delete":
		idSet := make(map[string]bool, len(req.IDs))
		for _, id := range req.IDs {
			idSet[id] = true
		}
		// Snapshot which requested IDs actually exist. config.DeleteAccount is
		// idempotent (returns nil for an unknown id), so we can't rely on its
		// error to tell deleted-from-missing apart — pre-check existence here.
		exists := make(map[string]bool, len(idSet))
		for _, a := range config.GetAccounts() {
			if idSet[a.ID] {
				exists[a.ID] = true
			}
		}
		deleted := 0
		failed := 0
		for id := range idSet {
			if !exists[id] {
				failed++ // requested id no longer present
				continue
			}
			if err := config.DeleteAccount(id); err != nil {
				logger.Warnf("[Batch] delete failed for %s: %v", id, err)
				failed++
				continue
			}
			deleted++
		}
		if deleted > 0 {
			h.pool.Reload()
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": failed == 0,
			"deleted": deleted,
			"failed":  failed,
		})

	default:
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid action: " + req.Action})
	}
}

func (h *Handler) apiStartIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StartUrl string `json:"startUrl"`
		Region   string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.StartUrl == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "startUrl is required"})
		return
	}

	sessionID, authorizeUrl, expiresIn, err := auth.StartIamSsoLogin(req.StartUrl, req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":    sessionID,
		"authorizeUrl": authorizeUrl,
		"expiresIn":    expiresIn,
	})
}

func (h *Handler) apiCompleteIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackUrl string `json:"callbackUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, err := auth.CompleteIamSsoLogin(req.SessionID, req.CallbackUrl)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiStartBuilderIdLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	session, err := auth.StartBuilderIdLogin(req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":       session.ID,
		"userCode":        session.UserCode,
		"verificationUri": session.VerificationUri,
		"interval":        session.Interval,
	})
}

func (h *Handler) apiPollBuilderIdAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, status, err := auth.PollBuilderIdAuth(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	if status == "pending" || status == "slow_down" {
		// 获取当前间隔
		interval := 5
		if session := auth.GetBuilderIdSession(req.SessionID); session != nil {
			interval = session.Interval
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"completed": false,
			"status":    status,
			"interval":  interval,
		})
		return
	}

	// 授权完成，获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"completed": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiImportSsoToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BearerToken string `json:"bearerToken"`
		Region      string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.BearerToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "bearerToken is required"})
		return
	}

	// 支持批量导入，按行分割
	tokens := strings.Split(strings.TrimSpace(req.BearerToken), "\n")
	var imported []map[string]interface{}
	var errors []string

	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}

		accessToken, refreshToken, clientID, clientSecret, expiresIn, err := auth.ImportFromSsoToken(token, req.Region)
		if err != nil {
			errors = append(errors, err.Error())
			continue
		}

		// 获取用户信息
		email, _, _ := auth.GetUserInfo(accessToken)

		// 创建账号
		account := config.Account{
			ID:           auth.GenerateAccountID(),
			Email:        email,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			ClientID:     clientID,
			ClientSecret: clientSecret,
			AuthMethod:   "idc",
			Region:       req.Region,
			ExpiresAt:    time.Now().Unix() + int64(expiresIn),
			Enabled:      true,
			MachineId:    config.GenerateMachineId(),
		}

		if err := config.AddAccount(account); err != nil {
			errors = append(errors, err.Error())
			continue
		}

		imported = append(imported, map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		})
	}

	h.pool.Reload()

	if len(imported) == 0 && len(errors) > 0 {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   strings.Join(errors, "; "),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"accounts": imported,
		"errors":   errors,
	})
}

func (h *Handler) apiImportCredentials(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
		AuthMethod   string `json:"authMethod"`
		Provider     string `json:"provider"`
		Region       string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.RefreshToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "refreshToken is required"})
		return
	}

	// 设置默认值
	if req.Region == "" {
		req.Region = "us-east-1"
	}
	if req.AuthMethod == "" {
		if req.ClientID != "" {
			req.AuthMethod = "idc"
		} else {
			req.AuthMethod = "social"
		}
	}
	// 标准化 authMethod
	switch strings.ToLower(req.AuthMethod) {
	case "idc", "builderid", "enterprise":
		req.AuthMethod = "idc"
	case "social", "google", "github":
		req.AuthMethod = "social"
	default:
		if req.ClientID != "" && req.ClientSecret != "" {
			req.AuthMethod = "idc"
		} else {
			req.AuthMethod = "social"
		}
	}

	// 始终尝试用 refreshToken 刷新获取新的 accessToken
	var accessToken string
	var expiresAt int64
	tempAccount := &config.Account{
		RefreshToken: req.RefreshToken,
		ClientID:     req.ClientID,
		ClientSecret: req.ClientSecret,
		AuthMethod:   req.AuthMethod,
		Region:       req.Region,
	}
	newAccessToken, newRefreshToken, newExpiresAt, newProfileArn, err := auth.RefreshToken(tempAccount)
	if err != nil {
		// 刷新失败，如果有传入的 accessToken 则尝试使用
		if req.AccessToken != "" {
			accessToken = req.AccessToken
			expiresAt = time.Now().Unix() + 300 // 可能已过期，设短一点
		} else {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
			return
		}
	} else {
		accessToken = newAccessToken
		if newRefreshToken != "" {
			req.RefreshToken = newRefreshToken
		}
		expiresAt = newExpiresAt
	}

	// 获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: req.RefreshToken,
		ClientID:     req.ClientID,
		ClientSecret: req.ClientSecret,
		AuthMethod:   req.AuthMethod,
		Provider:     req.Provider,
		Region:       req.Region,
		ExpiresAt:    expiresAt,
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
		ProfileArn:   newProfileArn,
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiGetStatus(w http.ResponseWriter, r *http.Request) {
	// Aggregate per-account subscription quotas so the dashboard can show
	// "Credits Total" / "Remaining" alongside the cumulative "Credits Used"
	// without having to refetch the full /accounts list every poll.
	//
	// Two parallel sums are exposed:
	//   - quotaTotal / quotaUsed: ALL accounts (includes disabled).
	//     Useful for capacity planning ("we have N credits across the
	//     fleet, however many we're actually using").
	//   - activeQuotaTotal / activeQuotaUsed / activeTokens / activeRequests:
	//     ENABLED accounts only. Matches the operator intuition that
	//     turning off an account should make its credits stop counting
	//     toward the live dashboard.
	var quotaTotal, quotaUsed float64
	var activeQuotaTotal, activeQuotaUsed float64
	var activeTokens int
	var activeRequests int
	for _, a := range config.GetAccounts() {
		if a.UsageLimit > 0 {
			quotaTotal += a.UsageLimit
		}
		if a.UsageCurrent > 0 {
			quotaUsed += a.UsageCurrent
		}
		if a.Enabled {
			if a.UsageLimit > 0 {
				activeQuotaTotal += a.UsageLimit
			}
			if a.UsageCurrent > 0 {
				activeQuotaUsed += a.UsageCurrent
			}
			activeTokens += a.TotalTokens
			activeRequests += a.RequestCount
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"accounts":         h.pool.Count(),
		"available":        h.pool.AvailableCount(),
		"totalRequests":    atomic.LoadInt64(&h.totalRequests),
		"successRequests":  atomic.LoadInt64(&h.successRequests),
		"failedRequests":   atomic.LoadInt64(&h.failedRequests),
		"totalTokens":      atomic.LoadInt64(&h.totalTokens),
		"totalCredits":     h.getCredits(),
		"quotaTotal":       quotaTotal,
		"quotaUsed":        quotaUsed,
		"activeQuotaTotal": activeQuotaTotal,
		"activeQuotaUsed":  activeQuotaUsed,
		"activeTokens":     activeTokens,
		"activeRequests":   activeRequests,
		"uptime":           time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"apiKey":                   config.GetApiKey(),
		"requireApiKey":            config.IsApiKeyRequired(),
		"port":                     config.GetPort(),
		"host":                     config.GetHost(),
		"allowOverUsage":           config.GetAllowOverUsage(),
		"webSearchEnabled":         config.GetWebSearchEnabled(),
		"toolSearchEnabled":        config.GetToolSearchEnabled(),
		"globalRateLimitPerMinute": config.GetGlobalRateLimitPerMinute(),
	})
}

// apiProbeWebSearch is a diagnostic that runs ONE native Kiro /mcp web_search
// against a chosen account and returns the raw outcome. It exists because the
// upstream MCP endpoint is opaque and not guaranteed on every account tier or
// region — this lets an operator confirm web search actually works for their
// accounts before enabling the feature, and surfaces the exact failure when it
// doesn't. Admin-authenticated (same gate as all /admin/api/*).
//
// Body: {"accountId":"<id>","query":"<text>"}. If accountId is omitted, the
// first enabled account is used.
func (h *Handler) apiProbeWebSearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID string `json:"accountId"`
		Query     string `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		req.Query = "what is the current date"
	}

	// Resolve the account: explicit id, else first enabled account.
	var account *config.Account
	if strings.TrimSpace(req.AccountID) != "" {
		account = h.pool.GetByID(req.AccountID)
		if account == nil {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]string{"error": "account not found"})
			return
		}
	} else {
		for _, a := range config.GetAccounts() {
			if a.Enabled && a.AccessToken != "" {
				acc := a
				account = &acc
				break
			}
		}
		if account == nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "no enabled account available"})
			return
		}
	}

	// Make sure the token is fresh so a probe failure reflects the endpoint,
	// not an expired token.
	if err := h.ensureValidToken(account); err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":    false,
			"stage": "token_refresh",
			"error": err.Error(),
		})
		return
	}

	results, err := performKiroWebSearch(r.Context(), account, req.Query)
	logWebSearch(req.Query, len(results), err)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":          false,
			"stage":       "mcp_call",
			"error":       err.Error(),
			"accountId":   account.ID,
			"region":      config.GetKiroAPIRegion(),
			"hint":        "If this is a 404/400, the /mcp web_search endpoint may not be enabled for this account tier or region.",
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":        true,
		"accountId": account.ID,
		"region":    config.GetKiroAPIRegion(),
		"query":     req.Query,
		"count":     len(results),
		"results":   results,
	})
}

func (h *Handler) apiGetPromptFilter(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetPromptFilterConfig())
}

func (h *Handler) apiUpdatePromptFilter(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FilterClaudeCode      *bool                      `json:"filterClaudeCode,omitempty"`
		FilterEnvNoise        *bool                      `json:"filterEnvNoise,omitempty"`
		FilterStripBoundaries *bool                      `json:"filterStripBoundaries,omitempty"`
		Rules                 *[]config.PromptFilterRule `json:"rules,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// Read current config to fill in any fields not provided in the request.
	current := config.GetPromptFilterConfig()
	fcc := current.FilterClaudeCode
	fen := current.FilterEnvNoise
	fsb := current.FilterStripBoundaries
	rules := current.Rules
	if req.FilterClaudeCode != nil {
		fcc = *req.FilterClaudeCode
	}
	if req.FilterEnvNoise != nil {
		fen = *req.FilterEnvNoise
	}
	if req.FilterStripBoundaries != nil {
		fsb = *req.FilterStripBoundaries
	}
	if req.Rules != nil {
		rules = *req.Rules
	}
	if err := config.UpdatePromptFilterConfig(fcc, fen, fsb, rules); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiUpdateSettings(w http.ResponseWriter, r *http.Request) {
	// All fields are pointers so the dashboard can PATCH a single field
	// (e.g. just AllowOverUsage when toggling over-usage) without clobbering
	// the others. Pre-A17 used a non-pointer struct, so saving "just the
	// password" silently set RequireApiKey=false and ApiKey="".
	var req struct {
		ApiKey                   *string `json:"apiKey,omitempty"`
		RequireApiKey            *bool   `json:"requireApiKey,omitempty"`
		Password                 *string `json:"password,omitempty"`
		CurrentPassword          *string `json:"currentPassword,omitempty"`
		AllowOverUsage           *bool   `json:"allowOverUsage,omitempty"`
		WebSearchEnabled         *bool   `json:"webSearchEnabled,omitempty"`
		ToolSearchEnabled        *bool   `json:"toolSearchEnabled,omitempty"`
		GlobalRateLimitPerMinute *int    `json:"globalRateLimitPerMinute,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// If the password is being changed, require the current password to be
	// supplied and verified — otherwise an XSS-stolen session cookie or a
	// CSRF-able admin endpoint could rotate the password silently.
	if req.Password != nil && *req.Password != "" {
		if req.CurrentPassword == nil || !config.VerifyPassword(*req.CurrentPassword) {
			w.WriteHeader(403)
			json.NewEncoder(w).Encode(map[string]string{"error": "Current password incorrect"})
			return
		}
	}

	if err := config.UpdateSettingsPartial(req.ApiKey, req.RequireApiKey, req.Password); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 更新超额使用设置
	if req.AllowOverUsage != nil {
		if err := config.UpdateAllowOverUsage(*req.AllowOverUsage); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// Web-search emulation toggle (opt-in; default off).
	if req.WebSearchEnabled != nil {
		if err := config.UpdateWebSearchEnabled(*req.WebSearchEnabled); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// Tool-search emulation toggle (default on; inert unless a request carries a
	// tool_search tool with deferred tools).
	if req.ToolSearchEnabled != nil {
		if err := config.UpdateToolSearchEnabled(*req.ToolSearchEnabled); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// Global rate limit (opt-in; 0 = off). Persist then reconfigure the live
	// limiter so the change takes effect immediately without a restart.
	if req.GlobalRateLimitPerMinute != nil {
		if err := config.UpdateGlobalRateLimitPerMinute(*req.GlobalRateLimitPerMinute); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		h.globalRL.Configure(config.GetGlobalRateLimitPerMinute())
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiGetStats(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) apiResetStats(w http.ResponseWriter, r *http.Request) {
	atomic.StoreInt64(&h.totalRequests, 0)
	atomic.StoreInt64(&h.successRequests, 0)
	atomic.StoreInt64(&h.failedRequests, 0)
	atomic.StoreInt64(&h.totalTokens, 0)
	h.creditsMu.Lock()
	h.totalCredits = 0
	h.creditsMu.Unlock()
	config.UpdateStats(0, 0, 0, 0, 0)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGenerateMachineId 生成新的机器码
func (h *Handler) apiGenerateMachineId(w http.ResponseWriter, r *http.Request) {
	machineId := config.GenerateMachineId()
	json.NewEncoder(w).Encode(map[string]string{"machineId": machineId})
}

// apiTestAccount tests a specific account by sending a real model request through its proxy.
func (h *Handler) apiTestAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	if err := h.ensureValidToken(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}

	// Parse test model from request body (optional)
	var req struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Model == "" {
		req.Model = "claude-sonnet-4"
	}

	// Build a minimal chat payload
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)

	openaiReq := &OpenAIRequest{
		Model:     actualModel,
		Messages:  []OpenAIMessage{{Role: "user", Content: "say ok"}},
		MaxTokens: 5,
		Stream:    false,
	}
	kiroPayload := OpenAIToKiro(openaiReq, thinking)

	var content string
	callback := &KiroStreamCallback{
		OnText:         func(text string, isThinking bool) { content += text },
		OnToolUse:      func(tu KiroToolUse) {},
		OnComplete:     func(inTok, outTok int) {},
		OnCredits:      func(c float64) {},
		OnContextUsage: func(pct float64) {},
		OnStopReason:   func(r string) {},
	}

	err := CallKiroAPI(account, kiroPayload, callback)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"reply":   content,
		"model":   req.Model,
	})
}

// apiGetAccountOverage GET /admin/api/accounts/{id}/overage — reads the REAL
// AWS user-level Overages switch + live billing figures (cap/rate/accumulated
// $) from upstream, persists the snapshot, and returns it. Read-only: this does
// NOT change any billing behavior.
func (h *Handler) apiGetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	account := h.findAccountByID(id)
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	if err := h.ensureValidToken(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}
	snap, err := FetchOverageStatus(account)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": "Fetch overage status failed: " + err.Error()})
		return
	}
	if err := PersistOverageSnapshot(id, snap); err != nil {
		logger.Warnf("[Overage] persist snapshot for %s failed: %v", redactForLog(account.Email), err)
	}
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"status":            snap.Status,
		"capability":        snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"checkedAt":         snap.CheckedAt,
	})
}

// apiSetAccountOverage POST /admin/api/accounts/{id}/overage — flips the REAL
// AWS user-level Overages billing switch. Body: {"enabled":true|false}.
// Enabling authorizes real overage billing, so the body must also carry
// {"confirm":true} as a guard against accidental clicks.
func (h *Handler) apiSetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	account := h.findAccountByID(id)
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
		Confirm bool `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	// Enabling spends real money — require an explicit confirm flag.
	if req.Enabled && !req.Confirm {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Enabling overage authorizes real AWS overage billing; resend with confirm=true"})
		return
	}
	if err := h.ensureValidToken(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}
	snap, err := SetOverageStatus(account, req.Enabled)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": "Set overage status failed: " + err.Error()})
		return
	}
	if err := PersistOverageSnapshot(id, snap); err != nil {
		logger.Warnf("[Overage] persist snapshot for %s failed: %v", redactForLog(account.Email), err)
	}
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         true,
		"status":          snap.Status,
		"overageCap":      snap.OverageCap,
		"overageRate":     snap.OverageRate,
		"currentOverages": snap.CurrentOverages,
		"checkedAt":       snap.CheckedAt,
	})
}

// findAccountByID returns a pointer to a copy of the named account, or nil.
func (h *Handler) findAccountByID(id string) *config.Account {
	accounts := config.GetAccounts()
	for i := range accounts {
		if accounts[i].ID == id {
			return &accounts[i]
		}
	}
	return nil
}
func (h *Handler) apiRefreshAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 先尝试刷新 token（不管是否过期，确保 token 有效）
	refreshTokenIfNeeded := func() error {
		if account.RefreshToken == "" {
			return nil
		}
		newAccessToken, newRefreshToken, newExpiresAt, profileArn, err := auth.RefreshToken(account)
		if err != nil {
			return err
		}
		account.AccessToken = newAccessToken
		if newRefreshToken != "" {
			account.RefreshToken = newRefreshToken
		}
		account.ExpiresAt = newExpiresAt
		config.UpdateAccountToken(id, newAccessToken, newRefreshToken, newExpiresAt)
		h.pool.UpdateToken(id, newAccessToken, newRefreshToken, newExpiresAt)
		if profileArn != "" {
			account.ProfileArn = profileArn
			config.UpdateAccountProfileArn(id, profileArn)
		}
		return nil
	}

	// 检查 token 是否快过期，先刷新
	if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
		if err := refreshTokenIfNeeded(); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
			return
		}
	}

	// 获取账户信息
	info, err := RefreshAccountInfo(account)
	if err != nil {
		// 检查是否为封禁相关错误
		errMsg := err.Error()
		if strings.Contains(errMsg, "TEMPORARILY_SUSPENDED") || strings.Contains(errMsg, "Account suspended") {
			// 封禁状态已在 RefreshAccountInfo 中处理，静默返回成功
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"message": "Account status updated",
			})
			return
		}

		// 如果是 403/401，说明 token 无效，尝试刷新后重试
		if strings.Contains(errMsg, "403") || strings.Contains(errMsg, "401") || strings.Contains(errMsg, "invalid") || strings.Contains(errMsg, "expired") {
			if refreshErr := refreshTokenIfNeeded(); refreshErr == nil {
				// 重试
				info, err = RefreshAccountInfo(account)
				if err != nil {
					// 重试后仍然失败，检查是否为封禁状态
					if strings.Contains(err.Error(), "TEMPORARILY_SUSPENDED") || strings.Contains(err.Error(), "Account suspended") {
						json.NewEncoder(w).Encode(map[string]interface{}{
							"success": true,
							"message": "Account status updated",
						})
						return
					}
				}
			}
		}

		// 其他错误才显示错误信息
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// 保存到配置
	flipped, err := config.UpdateAccountInfo(id, *info)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if flipped {
		h.pool.Reload()
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"info":    info,
	})
}

// apiGetAccountFull 获取单个账号的完整信息（包含敏感字段）
func (h *Handler) apiGetAccountFull(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// 查找指定账号
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 获取运行时统计
	var stats config.Account
	for _, a := range poolAccounts {
		if a.ID == id {
			stats = a
			break
		}
	}

	// 返回完整账号信息（包含敏感字段）
	result := map[string]interface{}{
		"id":                account.ID,
		"email":             account.Email,
		"userId":            account.UserId,
		"nickname":          account.Nickname,
		"accessToken":       account.AccessToken,
		"refreshToken":      account.RefreshToken,
		"clientId":          account.ClientID,
		"clientSecret":      account.ClientSecret,
		"authMethod":        account.AuthMethod,
		"provider":          account.Provider,
		"region":            account.Region,
		"expiresAt":         account.ExpiresAt,
		"machineId":         account.MachineId,
		"weight":            account.Weight,
		"allowOverage":      account.AllowOverage,
		"overageWeight":     account.OverageWeight,
		"proxyURL":          account.ProxyURL,
		"enabled":           account.Enabled,
		"banStatus":         account.BanStatus,
		"banReason":         account.BanReason,
		"banTime":           account.BanTime,
		"subscriptionType":  account.SubscriptionType,
		"subscriptionTitle": account.SubscriptionTitle,
		"daysRemaining":     account.DaysRemaining,
		"usageCurrent":      account.UsageCurrent,
		"usageLimit":        account.UsageLimit,
		"usagePercent":      account.UsagePercent,
		"nextResetDate":     account.NextResetDate,
		"lastRefresh":       account.LastRefresh,
		"trialUsageCurrent": account.TrialUsageCurrent,
		"trialUsageLimit":   account.TrialUsageLimit,
		"trialUsagePercent": account.TrialUsagePercent,
		"trialStatus":       account.TrialStatus,
		"trialExpiresAt":    account.TrialExpiresAt,
		"requestCount":      stats.RequestCount,
		"errorCount":        stats.ErrorCount,
		"totalTokens":       stats.TotalTokens,
		"totalCredits":      stats.TotalCredits,
		"lastUsed":          stats.LastUsed,
	}

	json.NewEncoder(w).Encode(result)
}

// apiGetAccountModels 获取账户可用模型
func (h *Handler) apiGetAccountModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	models, err := ListAvailableModels(account)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 同步更新路由缓存
	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ModelId)
	}
	h.pool.SetModelList(id, modelIDs)
	h.modelsCacheMu.Lock()
	h.cachedModels = mergeUniqueModels(h.cachedModels, models)
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

// apiGetAccountModelsCached 返回账号已缓存的模型列表（不实时拉取）
func (h *Handler) apiGetAccountModelsCached(w http.ResponseWriter, r *http.Request, id string) {
	models := h.pool.GetModelList(id)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

// ==================== 静态文件服务 ====================

func (h *Handler) serveAdminPage(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "web/index.html")
}

// serveLandingPage serves the public marketing landing page at "/".
func (h *Handler) serveLandingPage(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "web/landing.html")
}

// servePortalPage serves the public customer key-status portal at "/portal".
func (h *Handler) servePortalPage(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "web/portal.html")
}

// serveStaticFile serves admin assets from web/, jailed to the web/ directory.
// http.ServeFile already rejects ".." request paths with 400; serveJailedStaticFile
// adds a resolved-path containment check as defense-in-depth.
func (h *Handler) serveStaticFile(w http.ResponseWriter, r *http.Request) {
	h.serveJailedStaticFile(w, r, "/admin/")
}

// apiGetThinkingConfig 获取 thinking 配置
func (h *Handler) apiGetThinkingConfig(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetThinkingConfig()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"suffix":       cfg.Suffix,
		"openaiFormat": cfg.OpenAIFormat,
		"claudeFormat": cfg.ClaudeFormat,
	})
}

// apiUpdateThinkingConfig 更新 thinking 配置
func (h *Handler) apiUpdateThinkingConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Suffix       string `json:"suffix"`
		OpenAIFormat string `json:"openaiFormat"`
		ClaudeFormat string `json:"claudeFormat"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证格式
	validFormats := map[string]bool{"reasoning_content": true, "thinking": true, "think": true}
	if req.OpenAIFormat != "" && !validFormats[req.OpenAIFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid openaiFormat, must be: reasoning_content, thinking, or think"})
		return
	}
	if req.ClaudeFormat != "" && !validFormats[req.ClaudeFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid claudeFormat, must be: reasoning_content, thinking, or think"})
		return
	}

	if err := config.UpdateThinkingConfig(req.Suffix, req.OpenAIFormat, req.ClaudeFormat); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetEndpointConfig 获取端点配置
func (h *Handler) apiGetEndpointConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"preferredEndpoint": config.GetPreferredEndpoint(),
		"endpointFallback":  config.GetEndpointFallback(),
		"region":            config.GetKiroAPIRegion(),
		"regions":           config.GetKiroAPIRegions(),
		"poolStrategy":      config.GetPoolStrategy(),
	})
}

// apiUpdateEndpointConfig 更新端点配置
func (h *Handler) apiUpdateEndpointConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PreferredEndpoint string    `json:"preferredEndpoint"`
		EndpointFallback  *bool     `json:"endpointFallback"`
		Region            *string   `json:"region,omitempty"`
		Regions           *[]string `json:"regions,omitempty"`
		PoolStrategy      *string   `json:"poolStrategy,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	valid := map[string]bool{"auto": true, "kiro": true, "codewhisperer": true, "amazonq": true}
	if !valid[req.PreferredEndpoint] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid endpoint, must be: auto, kiro, codewhisperer, or amazonq"})
		return
	}

	if err := config.UpdatePreferredEndpoint(req.PreferredEndpoint); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if req.EndpointFallback != nil {
		config.UpdateEndpointFallback(*req.EndpointFallback)
	}

	if req.Region != nil {
		region := strings.ToLower(strings.TrimSpace(*req.Region))
		if region != "" && !isValidAWSRegion(region) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid AWS region (expected like 'us-east-1', 'eu-west-1', 'ap-northeast-1')"})
			return
		}
		if err := config.UpdateKiroAPIRegion(region); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	if req.Regions != nil {
		// Validate every region; reject the whole list if any entry is
		// malformed so the operator gets immediate feedback.
		clean := make([]string, 0, len(*req.Regions))
		for _, raw := range *req.Regions {
			s := strings.ToLower(strings.TrimSpace(raw))
			if s == "" {
				continue
			}
			if !isValidAWSRegion(s) {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "Invalid AWS region in failover list: " + s})
				return
			}
			clean = append(clean, s)
		}
		if err := config.UpdateKiroAPIRegions(clean); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	if req.PoolStrategy != nil {
		strat := strings.ToLower(strings.TrimSpace(*req.PoolStrategy))
		validStrats := map[string]bool{"": true, "least-request": true, "swr": true, "least-used": true, "random": true}
		if !validStrats[strat] {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid poolStrategy, must be: least-request, swr, least-used, or random"})
			return
		}
		if err := config.UpdatePoolStrategy(strat); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// isValidAWSRegion does a cheap shape check: two lowercase letters, dash,
// 1-2 lowercase region segments, dash, digits. Doesn't enforce the full
// list of real regions because AWS adds new ones routinely; we just reject
// obvious typos.
func isValidAWSRegion(s string) bool {
	if len(s) < 9 || len(s) > 32 {
		return false
	}
	parts := strings.Split(s, "-")
	if len(parts) < 3 {
		return false
	}
	// First part: 2 letters (us, eu, ap, ca, sa, af, me).
	if len(parts[0]) != 2 {
		return false
	}
	for _, c := range parts[0] {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	// Last part: digits only.
	if len(parts[len(parts)-1]) == 0 {
		return false
	}
	for _, c := range parts[len(parts)-1] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// applyProxyConfig 将代理配置应用到所有出站 HTTP 客户端（Kiro API + auth 模块）
func applyProxyConfig(proxyURL string) {
	InitKiroHttpClient(proxyURL)
	auth.InitHttpClient(proxyURL)
}

// apiGetProxy 获取当前代理配置
func (h *Handler) apiGetProxy(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"proxyURL": config.GetProxyURL(),
	})
}

// apiUpdateProxy 更新代理配置并立即生效
func (h *Handler) apiUpdateProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProxyURL string `json:"proxyURL"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证代理 URL 格式（非空时）
	if req.ProxyURL != "" {
		if msg := validateProxyURL(req.ProxyURL); msg != "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": msg})
			return
		}
	}

	if err := config.UpdateProxySettings(req.ProxyURL); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 立即应用新的代理配置
	applyProxyConfig(req.ProxyURL)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// validateProxyURL validates an outbound-proxy URL for both the global and the
// per-account proxy settings. It enforces a known scheme and, by default,
// rejects a LINK-LOCAL target (169.254.0.0/16 and fe80::/10). Link-local is
// never a legitimate forward proxy but IS where cloud instance-metadata lives
// (169.254.169.254) — the classic SSRF target that could be used to route the
// AWS bearer token through an attacker-reachable endpoint. Private/LAN and
// localhost proxies remain allowed because they are the common legitimate case
// (a local SOCKS5 proxy, a corporate proxy). Set KIRO_ALLOW_LINKLOCAL_PROXY=1
// to bypass the link-local guard for unusual setups. Returns "" when valid, or
// an operator-facing error message.
func validateProxyURL(raw string) string {
	if !strings.HasPrefix(raw, "http://") &&
		!strings.HasPrefix(raw, "https://") &&
		!strings.HasPrefix(raw, "socks5://") &&
		!strings.HasPrefix(raw, "socks5h://") {
		return "proxyURL must start with http://, https://, socks5://, or socks5h://"
	}
	if os.Getenv("KIRO_ALLOW_LINKLOCAL_PROXY") == "1" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "proxyURL is not a valid URL: " + err.Error()
	}
	host := u.Hostname()
	if host == "" {
		return "proxyURL has no host"
	}
	// A literal IP is checked directly. A hostname is resolved and EVERY
	// resolved address is checked, because net.ParseIP returns nil for a
	// hostname — so without resolution a name like "metadata.internal" (or an
	// attacker-controlled name that resolves to 169.254.169.254) would slip
	// past the link-local guard and the AWS bearer token would be routed
	// through it. Resolution is BEST-EFFORT: a host that doesn't resolve at
	// validation time is allowed through (the original permissive behavior for
	// hostnames — the proxy host may legitimately not resolve from this machine,
	// and a hard DNS gate would be flaky and itself a probe vector). DNS
	// rebinding between this check and connect time is the residual risk, best
	// closed at dial time; this blocks the straightforward hostname→metadata case.
	if ip := net.ParseIP(host); ip != nil {
		if msg := proxyIPDisallowed(ip); msg != "" {
			return msg
		}
		return ""
	}
	if addrs, err := net.LookupHost(host); err == nil {
		for _, addr := range addrs {
			ip := net.ParseIP(addr)
			if ip == nil {
				continue
			}
			if msg := proxyIPDisallowed(ip); msg != "" {
				return msg
			}
		}
	}
	return ""
}

// proxyIPDisallowed reports why an outbound-proxy IP is refused, or "" when it
// is allowed. Link-local (the cloud instance-metadata range, 169.254.0.0/16 and
// fe80::/10) is never a legitimate forward-proxy target and is the SSRF vector
// that could exfiltrate the AWS bearer token. Private/LAN and loopback addresses
// stay allowed — a local SOCKS5 or corporate proxy is the common legitimate
// case (preserving the pre-existing policy; only the hostname-resolution gap is
// being closed here).
func proxyIPDisallowed(ip net.IP) string {
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return "proxyURL resolves to a link-local address (e.g. cloud metadata 169.254.169.254); refused. Set KIRO_ALLOW_LINKLOCAL_PROXY=1 to override."
	}
	return ""
}

// apiGetVersion 获取版本信息
func (h *Handler) apiGetVersion(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"version": config.Version,
	})
}

// apiExportAccounts 导出账号凭证
func (h *Handler) apiExportAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"` // 为空则导出全部
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// 如果 body 为空或解析失败，导出全部
		req.IDs = nil
	}

	accounts := config.GetAccounts()

	// 如果指定了 ID，只导出指定的
	if len(req.IDs) > 0 {
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var filtered []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				filtered = append(filtered, a)
			}
		}
		accounts = filtered
	}

	// 构建兼容 Kiro Account Manager 的导出格式
	type ExportCredentials struct {
		AccessToken  string `json:"accessToken"`
		CsrfToken    string `json:"csrfToken"`
		RefreshToken string `json:"refreshToken"`
		ClientID     string `json:"clientId,omitempty"`
		ClientSecret string `json:"clientSecret,omitempty"`
		Region       string `json:"region,omitempty"`
		ExpiresAt    int64  `json:"expiresAt"`
		AuthMethod   string `json:"authMethod,omitempty"`
		Provider     string `json:"provider,omitempty"`
	}

	type ExportSubscription struct {
		Type  string `json:"type"`
		Title string `json:"title,omitempty"`
	}

	type ExportUsage struct {
		Current     float64 `json:"current"`
		Limit       float64 `json:"limit"`
		PercentUsed float64 `json:"percentUsed"`
		LastUpdated int64   `json:"lastUpdated"`
	}

	type ExportAccount struct {
		ID           string             `json:"id"`
		Email        string             `json:"email"`
		Nickname     string             `json:"nickname,omitempty"`
		Idp          string             `json:"idp"`
		UserId       string             `json:"userId,omitempty"`
		MachineId    string             `json:"machineId,omitempty"`
		Credentials  ExportCredentials  `json:"credentials"`
		Subscription ExportSubscription `json:"subscription"`
		Usage        ExportUsage        `json:"usage"`
		Tags         []string           `json:"tags"`
		Status       string             `json:"status"`
		CreatedAt    int64              `json:"createdAt"`
		LastUsedAt   int64              `json:"lastUsedAt"`
	}

	type ExportData struct {
		Version    string          `json:"version"`
		ExportedAt int64           `json:"exportedAt"`
		Accounts   []ExportAccount `json:"accounts"`
		Groups     []interface{}   `json:"groups"`
		Tags       []interface{}   `json:"tags"`
	}

	exportAccounts := make([]ExportAccount, 0, len(accounts))
	for _, a := range accounts {
		// 映射 provider 到 idp
		idp := a.Provider
		if idp == "" {
			if a.AuthMethod == "social" {
				idp = "Google"
			} else {
				idp = "BuilderId"
			}
		}

		// 映射 authMethod
		authMethod := a.AuthMethod
		if authMethod == "idc" {
			authMethod = "IdC"
		}

		// 映射订阅类型
		subType := "Free"
		rawType := strings.ToUpper(a.SubscriptionType)
		if strings.Contains(rawType, "PRO_PLUS") || strings.Contains(rawType, "PROPLUS") {
			subType = "Pro_Plus"
		} else if strings.Contains(rawType, "PRO") {
			subType = "Pro"
		} else if strings.Contains(rawType, "POWER") {
			subType = "Pro_Plus"
		}

		exportAccounts = append(exportAccounts, ExportAccount{
			ID:        a.ID,
			Email:     a.Email,
			Nickname:  a.Nickname,
			Idp:       idp,
			UserId:    a.UserId,
			MachineId: a.MachineId,
			Credentials: ExportCredentials{
				AccessToken:  a.AccessToken,
				CsrfToken:    "",
				RefreshToken: a.RefreshToken,
				ClientID:     a.ClientID,
				ClientSecret: a.ClientSecret,
				Region:       a.Region,
				ExpiresAt:    a.ExpiresAt * 1000, // 转为毫秒时间戳
				AuthMethod:   authMethod,
				Provider:     a.Provider,
			},
			Subscription: ExportSubscription{
				Type:  subType,
				Title: a.SubscriptionTitle,
			},
			Usage: ExportUsage{
				Current:     a.UsageCurrent,
				Limit:       a.UsageLimit,
				PercentUsed: a.UsagePercent,
				LastUpdated: time.Now().UnixMilli(),
			},
			Tags:       []string{},
			Status:     "active",
			CreatedAt:  time.Now().UnixMilli(),
			LastUsedAt: time.Now().UnixMilli(),
		})
	}

	data := ExportData{
		Version:    config.Version,
		ExportedAt: time.Now().UnixMilli(),
		Accounts:   exportAccounts,
		Groups:     []interface{}{},
		Tags:       []interface{}{},
	}

	json.NewEncoder(w).Encode(data)
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
