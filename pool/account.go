// Package pool 账号池管理
//
// Implements smooth weighted round-robin (SWRR) account selection plus a
// per-account cooldown state machine that distinguishes soft throttles from
// hard quota exhaustion.
//
// Selection algorithm: the DEFAULT is least-outstanding-request (LOR), with
// smooth weighted round-robin (SWRR) and least-used / random as alternatives.
//
// Why LOR is the default: each account is one AWS identity rate-limited by a
// per-identity token bucket. SWRR balances the long-run *rate* of assignment
// but is blind to *concurrency* — when a client fires many parallel requests in
// the same few milliseconds (e.g. an agent fan-out), SWRR sprays them across
// every account before a single 429 returns, so the whole pool blows its burst
// allowance at once and throttles together. LOR instead steers each new request
// to the least-busy identity (Envoy's weighted form: score = weight/(inflight+1))
// and refuses to pile onto a saturated one, converting that all-at-once cliff
// into smooth backpressure. See the AWS jitter blog, Envoy least-request, and
// Google SRE "Handling Overload".
//
// Concurrency control (least-request strategy only): an AIMD per-account
// concurrency limit gates admission — it grows by 1 on a healthy success and is
// halved on a 429, so each account self-tunes to AWS's unpublished bucket size.
// After a cooldown the limit sits low (often 1), which gives a "half-open single
// probe" recovery for free: only one request is admitted until it succeeds. The
// other strategies (swr/least-used/random) do NOT reserve in-flight slots or
// apply the AIMD gate, so they behave exactly as before.
//
// SWRR (the nginx/Envoy variant), kept as an option: for each pick we add the
// effective weight to every account's runningWeight, pick the account with the
// highest runningWeight, then subtract the total effective weight from the
// winner. With weights {a:5, b:1, c:1} this produces a,a,b,a,c,a,a instead of
// bursting a,a,a,a,a then b then c.
//
// Cooldown tiers (RecordError):
//   - Retry-After header present → use it (clamped 5s..5min).
//   - 1st 429:                     decorrelated jitter starting at 5s
//   - Nth consecutive 429:         random(5s, prev × 3), capped at 5min
//   - 3 consecutive non-quota errs: 60s
//   - Hard UsageCurrent ≥ UsageLimit: handled separately by isOverUsageLimit.
//
// Decorrelated jitter (instead of plain full jitter) is preferred when many
// accounts can throttle in the same AWS bucket: it desynchronises retry
// windows because each account's next sleep is bounded by its OWN previous
// sleep, not a shared shift counter. See AWS Architecture Blog
// "Exponential Backoff and Jitter" for the comparison.
//
// The consecutive counter decays after errorCounterDecay of no errors so old
// failures don't keep amplifying backoff after a recovery.
package pool

import (
	"kiro-go/config"
	"math/rand"
	"strings"
	"sync"
	"time"
)

const (
	tokenRefreshSkewSeconds int64 = 120

	// Soft cooldown tiers.
	//
	// retryAfterMax bounds upstream-supplied Retry-After so a malformed or
	// oversized header can't park an account longer than ~5 min. AWS's own
	// SDK clamps server-supplied retry-after to [computed_backoff,
	// computed_backoff + 5s] for the same reason.
	//
	// retryAfterMin is the floor for synthesized backoff when upstream does
	// NOT provide a Retry-After. Real upstream hints (which arrive from
	// AWS's throttling layer with values as small as ~1s during light
	// throttling) bypass this floor — clamping a server-supplied 1s up to
	// 5s wasted four free seconds of capacity per recovery.
	softCooldownBase    = 5 * time.Second
	softCooldownMax     = 5 * time.Minute
	retryAfterMin       = 5 * time.Second
	retryAfterMax       = 5 * time.Minute
	retryAfterAbsoluteMin = 1 * time.Second
	nonQuotaCooldown    = 60 * time.Second
	errorCounterDecay   = 5 * time.Minute

	// quotaExhaustedCooldown is the cooldown applied when the upstream
	// returns 402 OVERAGE / monthly-quota exhaustion. These are not
	// recoverable on a per-minute timescale — the quota resets at month
	// boundary — so we park the account for an hour and let the periodic
	// RefreshAccountInfo path notice the reset and re-enable.
	quotaExhaustedCooldown = 1 * time.Hour

	// ---- Adaptive concurrency (least-request strategy only) ----
	//
	// Each account carries an AIMD in-flight limit that self-tunes to AWS's
	// unpublished per-identity token bucket: additive-increase on healthy
	// successes, multiplicative-decrease on a 429. The request handler reserves
	// a slot before the upstream call (Acquire) and releases it after (Release).
	//
	// aimdInitialLimit is where a fresh / just-recovered account starts. 2 lets
	// a little parallelism through immediately without spraying a burst.
	aimdInitialLimit = 2
	// aimdMinLimit is the floor — never drop below this on a 429. With
	// multiple accounts pooled, a 1-slot floor left no headroom: any
	// concurrent burst on a recovered account immediately re-throttled,
	// and the dispatcher's admission-wait budget turned into a stall.
	// A 2-slot floor keeps a recovered account productive immediately
	// while still letting it self-tune down toward AWS's actual bucket
	// size on the multiplicative-decrease path.
	aimdMinLimit = 2
	// aimdMaxLimit caps how far additive-increase can climb, so one account
	// can't absorb the entire burst and re-trigger the storm we're fixing.
	aimdMaxLimit = 12
	// aimdDecreaseNum/Den is the multiplicative-decrease factor on a 429 (×3/4).
	// A token bucket is a wall, not TCP congestion, so we cut harder than
	// Netflix's 0.9 default but softer than TCP's 0.5 to avoid over-shrinking a
	// pool that's only lightly throttled.
	aimdDecreaseNum = 3
	aimdDecreaseDen = 4

	// cooldownExpiryJitter caps the random extra time added to every soft
	// cooldown expiry so accounts that trip together don't all become eligible
	// on the same tick and re-stampede. Added on top of the computed backoff.
	cooldownExpiryJitter = 3 * time.Second

	// saturationPollInterval is the "try again very soon" hint the picker
	// returns when every eligible account is at its concurrency limit (not
	// cooling, just busy). The failover dispatcher waits this long for a slot to
	// free before re-selecting, up to its admission budget.
	saturationPollInterval = 100 * time.Millisecond
)

// accountSlot is one entry in the SWRR scheduler. The slot is keyed
// positionally to AccountPool.accounts, so we don't store the account ID here.
type accountSlot struct {
	effectiveWeight int
	currentWeight   int
}

// cooldownEntry tracks per-account error state AND adaptive-concurrency state.
// One struct per account keeps all mutable per-account scheduling state behind
// the single pool lock, so Acquire/Release/RecordError stay consistent.
type cooldownEntry struct {
	until           time.Time     // soft cooldown expiry; zero = not cooling
	lastSleep       time.Duration // last cooldown duration we computed; seeds decorrelated jitter
	lastErrorAt     time.Time     // for decay
	consecutiveErrs int           // consecutive non-quota errors

	// Adaptive concurrency (least-request strategy). inflight is the number of
	// requests currently reserved on this account; limit is the AIMD ceiling.
	// limit==0 means "uninitialized" — treated as aimdInitialLimit on first use,
	// so an account with no cooldownEntry yet behaves as a fresh limit.
	inflight int
	limit    int

	// Adaptive RATE pacing (least-request strategy). See rate_pacer.go for the
	// full design. These implement a per-account GCRA pacer whose rate is learned
	// by AIMD ("discover then pace"): unpaced until the first 429, then paced just
	// below the observed achieved rate and probed back up on sustained success.
	//
	//   rateEstimate  — learned paced rate (req/sec); 0 means UNPACED (run full
	//                   speed, gated only by concurrency).
	//   tat           — GCRA Theoretical Arrival Time; the pacer admits a request
	//                   only when now >= tat - τ. Zero until first advance.
	//   observedRate  — EWMA of achieved success throughput, used to seed the
	//                   snap-down on a 429.
	//   lastSuccessAt — timestamp of the previous success, for the inter-success
	//                   interval that feeds observedRate.
	//   lastProbeAt   — last time the paced rate was probed upward, to rate-limit
	//                   probing to rateProbeInterval.
	rateEstimate  float64
	tat           time.Time
	observedRate  float64
	lastSuccessAt time.Time
	lastProbeAt   time.Time
}

// AccountPool 账号池
type AccountPool struct {
	// mu protects accounts, slots, cooldowns, modelLists. Read-only methods
	// (Count, AvailableCount, GetByID, GetAllAccounts, CooldownRemaining,
	// GetModelList) take RLock so admin/UI polling and per-request lookups
	// can fan in without serializing against each other. Methods that mutate
	// counters or SWRR state (GetNextForModel, RecordError/Success,
	// UpdateStats, UpdateToken, ClearSoftCooldownIfHealthy, SetModelList,
	// Reload) take the exclusive Lock.
	mu         sync.RWMutex
	accounts   []config.Account           // deduplicated, one entry per real account
	slots      []*accountSlot             // SWRR scheduler slots, parallel to accounts
	cooldowns  map[string]*cooldownEntry  // accountID → cooldown state
	modelLists map[string]map[string]bool // accountID → set of supported model IDs
}

var (
	pool     *AccountPool
	poolOnce sync.Once
)

// GetPool 获取全局账号池单例
func GetPool() *AccountPool {
	poolOnce.Do(func() {
		pool = &AccountPool{
			cooldowns:  make(map[string]*cooldownEntry),
			modelLists: make(map[string]map[string]bool),
		}
		pool.Reload()
	})
	return pool
}

// NewForTesting returns an empty pool for use in unit tests. Unlike GetPool,
// it does not call Reload (which would panic if config has not been initialized).
// Tests can populate state directly through the public mutator methods
// (RecordError, RecordSuccess, UpdateStats, ...) — these only touch the
// cooldown / stats maps, not the SWRR slots.
func NewForTesting() *AccountPool {
	return &AccountPool{
		cooldowns:  make(map[string]*cooldownEntry),
		modelLists: make(map[string]map[string]bool),
	}
}

// SetAccountsForTesting populates the pool's account list + SWRR slots
// directly (mirroring what Reload does from config), so tests in OTHER
// packages — e.g. proxy's failover dispatcher — can build a pool with known
// accounts without a config round-trip. Not for production use.
func (p *AccountPool) SetAccountsForTesting(accounts []config.Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accounts = accounts
	p.slots = make([]*accountSlot, 0, len(accounts))
	for _, a := range accounts {
		w := computeEffectiveWeight(a)
		if w <= 0 {
			continue
		}
		p.slots = append(p.slots, &accountSlot{effectiveWeight: w})
	}
}

// Reload rebuilds the account list and SWRR slots from configuration.
// Cooldown state and model lists are preserved for accounts that still exist.
func (p *AccountPool) Reload() {
	p.mu.Lock()
	defer p.mu.Unlock()

	enabled := config.GetEnabledAccounts()
	accounts := make([]config.Account, 0, len(enabled))
	slots := make([]*accountSlot, 0, len(enabled))

	for _, a := range enabled {
		w := computeEffectiveWeight(a)
		if w <= 0 {
			// Skip accounts that can't currently serve traffic (e.g. over-limit
			// without overage allowance). They re-enter rotation on the next
			// Reload after their state changes.
			continue
		}
		accounts = append(accounts, a)
		slots = append(slots, &accountSlot{
			effectiveWeight: w,
			currentWeight:   0,
		})
	}

	p.accounts = accounts
	p.slots = slots

	// Drop cooldowns and model lists for accounts that no longer exist.
	keep := make(map[string]bool, len(accounts))
	for _, a := range accounts {
		keep[a.ID] = true
	}
	for id := range p.cooldowns {
		if !keep[id] {
			delete(p.cooldowns, id)
		}
	}
	for id := range p.modelLists {
		if !keep[id] {
			delete(p.modelLists, id)
		}
	}
}

// computeEffectiveWeight derives the SWRR weight from account config. Returns
// 0 to mean "do not include in rotation right now". The pool will skip it on
// every pick until the next Reload.
//
// Weights are scaled up by 10× so overage accounts (which have weight 1..10)
// can be expressed as fractions of a normal slot without losing precision.
func computeEffectiveWeight(a config.Account) int {
	if isOverUsageLimit(a) {
		if !a.AllowOverage && !config.GetAllowOverUsage() {
			return 0
		}
		// Overage weight 1..10, kept on the same scale so a healthy account
		// (weight 1 → 10) gets ≥10× the picks of an overage account (weight 1).
		w := a.OverageWeight
		if w < 1 {
			w = 1
		}
		if w > 10 {
			w = 10
		}
		return w
	}

	w := a.Weight
	if w < 1 {
		w = 1
	}
	return w * 10
}

// SetModelList 缓存账号支持的模型集合（由 handler 在刷新后调用）
func (p *AccountPool) SetModelList(accountID string, modelIDs []string) {
	set := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		set[strings.ToLower(strings.TrimSpace(id))] = true
	}
	p.mu.Lock()
	p.modelLists[accountID] = set
	p.mu.Unlock()
}

// GetModelList 返回该账号缓存的模型 ID 列表（供 admin API 使用）。
func (p *AccountPool) GetModelList(accountID string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	set, ok := p.modelLists[accountID]
	if !ok || len(set) == 0 {
		return []string{}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	return ids
}

// accountHasModel checks the cached model list. If unknown (cold start) we
// optimistically return true so the account is still considered.
//
// modelKey must be the already-normalized (lowercased + trimmed) model ID.
// Caller must hold p.mu.
func (p *AccountPool) accountHasModel(accountID, modelKey string) bool {
	list, ok := p.modelLists[accountID]
	if !ok || len(list) == 0 {
		return true
	}
	for _, key := range modelLookupKeys(modelKey) {
		if list[key] {
			return true
		}
	}
	return false
}

// strategyResolver returns the active pool selection strategy. Indirection
// via a package-level variable lets tests override the strategy without
// having to bring up a full config; production code points it at
// config.GetPoolStrategy.
var strategyResolver = func() string { return config.GetPoolStrategy() }

// SetStrategyResolverForTesting replaces the strategy resolver so unit
// tests can drive each branch of the picker without going through config.
// Tests are expected to restore the previous resolver in a defer.
func SetStrategyResolverForTesting(fn func() string) (restore func()) {
	prev := strategyResolver
	strategyResolver = fn
	return func() { strategyResolver = prev }
}

// modelLookupKeys returns the set of ids the model whitelist matcher
// should compare against. The first entry is the input as-is (lowercased
// + trimmed); the second, when applicable, is the dotted-vs-dashed twin
// for Claude family ids — Claude Code uses dashed (claude-opus-4-7),
// Kiro upstream uses dotted (claude-opus-4.7), and they refer to the
// same model. The transform is purely mechanical: swap "." and "-" in
// the version-number suffix only, so a future claude-opus-4-8 / 4.8 /
// 5-0 / 5-1 / etc. works without a code change.
//
// Non-Claude ids and ids without a version-number suffix return only
// the normalized input.
func modelLookupKeys(modelID string) []string {
	normalizedModelID := strings.ToLower(strings.TrimSpace(modelID))
	if normalizedModelID == "" {
		return nil
	}
	keys := []string{normalizedModelID}
	if twin := claudeAliasTwin(normalizedModelID); twin != "" && twin != normalizedModelID {
		keys = append(keys, twin)
	}
	return keys
}

// claudeAliasTwin returns the dotted-or-dashed twin of a Claude family
// id, or "" if the input doesn't match the family-version shape we know
// is interchangeable. Recognized families: opus, sonnet, haiku. Pattern:
// "claude-<family>-<digits><sep><digits>" where sep is "." or "-",
// each side is 1-2 digits. Anything else (bare family, dated suffix,
// non-claude id) returns "".
//
// Examples:
//
//	claude-opus-4-7      -> claude-opus-4.7
//	claude-opus-4.7      -> claude-opus-4-7
//	claude-opus-4-8      -> claude-opus-4.8     (works for new minor)
//	claude-opus-4-10     -> claude-opus-4.10    (works for double-digit minor)
//	claude-opus-10-2     -> claude-opus-10.2    (works for double-digit major)
//	claude-3-5-sonnet    -> ""                  (different family shape)
//	claude-sonnet-4-20250514 -> ""              (dated suffix, 8-digit "minor")
//	claude-sonnet-4      -> ""                  (no minor — not a twin pair)
//	gpt-4o               -> ""                  (not a Claude id)
func claudeAliasTwin(id string) string {
	const prefix = "claude-"
	if !strings.HasPrefix(id, prefix) {
		return ""
	}
	rest := id[len(prefix):]
	for _, fam := range claudeFamilies {
		famPrefix := fam + "-"
		if !strings.HasPrefix(rest, famPrefix) {
			continue
		}
		ver := rest[len(famPrefix):]
		// Find the version separator. We accept either form, but only
		// when both sides are 1-2 digits — that distinguishes the
		// version twin (4-7, 4-10, 10-2) from the dated suffix
		// (4-20250514, 8-digit right side) and from longer variants
		// we don't have evidence Anthropic uses.
		sepIdx := -1
		for i := 0; i < len(ver); i++ {
			if ver[i] == '.' || ver[i] == '-' {
				sepIdx = i
				break
			}
		}
		if sepIdx < 1 || sepIdx > 2 {
			return ""
		}
		major := ver[:sepIdx]
		minor := ver[sepIdx+1:]
		if len(minor) < 1 || len(minor) > 2 {
			return ""
		}
		if !allDigits(major) || !allDigits(minor) {
			return ""
		}
		// Emit the alternate separator. The whole tail after major+sep
		// must be exactly the minor digits — anything trailing means
		// this isn't a clean version pair (so we reject below).
		if sepIdx+1+len(minor) != len(ver) {
			return ""
		}
		var altSep byte
		if ver[sepIdx] == '.' {
			altSep = '-'
		} else {
			altSep = '.'
		}
		return prefix + famPrefix + major + string(altSep) + minor
	}
	return ""
}

var claudeFamilies = []string{"opus", "sonnet", "haiku"}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// GetNextForModel returns the next account that supports the requested
// model.
//
//   - acc == nil, ok == false                 → no account configured at all
//     (caller should return 503 / "No available accounts").
//   - acc == nil, ok == false, retryAfter > 0 → pool is non-empty but every
//     candidate is in cooldown (or, under least-request, saturated); retryAfter
//     is the time until the soonest account becomes eligible. Caller should
//     return 429 with Retry-After.
//   - acc != nil, ok == true                  → caller should proceed; the
//     account is eligible and not cooling.
//
// This is the NON-RESERVING picker: it selects by the active strategy but does
// NOT reserve an in-flight slot, so the AIMD concurrency gate is not applied.
// Used by the lower-volume single-account paths (Responses/Codex, the web-search
// MCP side-call) and by tests. The bursty main path goes through
// AcquireForModelExcluding (reserving) via the failover dispatcher.
//
// model may be empty to skip the model-list filter.
func (p *AccountPool) GetNextForModel(model string) (*config.Account, time.Duration, bool) {
	return p.pick(model, nil, false)
}

// GetNextForModelExcluding is the failover-aware NON-RESERVING variant. The
// exclude set holds account IDs the caller has already tried, so the picker
// skips them. Pass nil for the first pick.
func (p *AccountPool) GetNextForModelExcluding(model string, exclude map[string]bool) (*config.Account, time.Duration, bool) {
	return p.pick(model, exclude, false)
}

// AcquireForModelExcluding is the RESERVING picker used by the failover
// dispatcher on the main request path. For the least-request strategy it applies
// the AIMD per-account concurrency gate (skipping accounts at their limit) and,
// on a successful pick, atomically increments that account's in-flight counter —
// so the caller MUST call Release(account.ID) exactly once when the request
// finishes. For every other strategy it is identical to GetNextForModelExcluding
// (no slot is reserved) and Release is a harmless no-op.
//
// When every eligible account is at its concurrency limit (busy, not cooling),
// it returns ok=false with retryAfter=saturationPollInterval so the caller can
// wait briefly for a slot to free instead of shedding immediately.
func (p *AccountPool) AcquireForModelExcluding(model string, exclude map[string]bool) (*config.Account, time.Duration, bool) {
	return p.pick(model, exclude, true)
}

// Release frees an in-flight slot previously reserved by AcquireForModelExcluding.
// Safe to call unconditionally: it floors at zero and is a no-op for accounts
// that never reserved (non-least-request strategies, or an already-released
// slot). Must be called exactly once per successful Acquire.
func (p *AccountPool) Release(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cd, ok := p.cooldowns[id]; ok && cd.inflight > 0 {
		cd.inflight--
	}
}

// InflightCount reports the current reserved in-flight count for an account
// (0 if none). Exposed for admin diagnostics and tests.
func (p *AccountPool) InflightCount(id string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if cd, ok := p.cooldowns[id]; ok {
		return cd.inflight
	}
	return 0
}

// RateState reports an account's adaptive RATE-pacer state under a single lock,
// for the dashboard's realtime display and for tests. pacedRate is the learned
// GCRA rate in requests/sec (0 = UNPACED, running full speed gated only by
// concurrency); observedRate is the EWMA of achieved success throughput
// (req/sec) used to seed the snap-down on a 429. See rate_pacer.go.
func (p *AccountPool) RateState(id string) (pacedRate, observedRate float64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if cd, ok := p.cooldowns[id]; ok {
		return cd.rateEstimate, cd.observedRate
	}
	return 0, 0
}

// ConcurrencyState reports an account's live in-flight count and its current
// AIMD concurrency limit under a single lock, for the dashboard's per-account
// realtime display. The limit reflects what the picker would enforce right now:
// an account with no entry yet reports the initial limit. Exposed for admin
// diagnostics and tests.
func (p *AccountPool) ConcurrencyState(id string) (inflight, limit int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if cd, ok := p.cooldowns[id]; ok {
		limit = cd.limit
		if limit <= 0 {
			limit = aimdInitialLimit
		}
		return cd.inflight, limit
	}
	return 0, aimdInitialLimit
}

// pick is the shared selection core. reserve=true applies the AIMD concurrency
// gate and increments the winner's in-flight counter (least-request only).
func (p *AccountPool) pick(model string, exclude map[string]bool, reserve bool) (*config.Account, time.Duration, bool) {
	// Resolve the strategy BEFORE taking the pool lock. strategyResolver ->
	// config.GetPoolStrategy acquires the config lock; doing it under p.mu
	// nested two locks on every pick (a contention + lock-ordering hazard on
	// the request hot path). The strategy rarely changes, so a one-tick-stale
	// read is harmless.
	strategy := strategyResolver()

	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.slots) == 0 {
		return nil, 0, false
	}

	now := time.Now()
	p.decayCountersLocked(now)

	// Normalize once before the per-slot walk so accountHasModel doesn't pay
	// strings.ToLower + TrimSpace per candidate.
	modelKey := strings.ToLower(strings.TrimSpace(model))

	// Collect indices of slots that are currently eligible (model match, not
	// excluded, no cooldown, token not about to expire). Ineligible slots don't
	// accumulate SWRR currentWeight, so they don't bias future picks.
	var eligible []int
	var soonest time.Time
	for i := range p.slots {
		acc := &p.accounts[i]
		if exclude != nil && exclude[acc.ID] {
			continue
		}
		if modelKey != "" && !p.accountHasModel(acc.ID, modelKey) {
			continue
		}
		if acc.ExpiresAt > 0 && now.Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
			continue
		}
		if cd, ok := p.cooldowns[acc.ID]; ok && now.Before(cd.until) {
			if soonest.IsZero() || cd.until.Before(soonest) {
				soonest = cd.until
			}
			continue
		}
		eligible = append(eligible, i)
	}

	if len(eligible) == 0 {
		// Pool is non-empty but every account is cooling. Tell the caller how
		// long until the soonest account is eligible so they can surface a
		// real Retry-After to the client instead of burning quota on a known
		// throttled account.
		if !soonest.IsZero() {
			return nil, time.Until(soonest), false
		}
		return nil, 0, false
	}

	// Strategy dispatch among the eligible subset. The production default is
	// least-request (LOR) — see the package comment for why it beats the others
	// under bursty per-identity-rate-limited load. The cfg==nil test fallback
	// resolves to "swr" so the pool's SWRR tests keep their interleaving
	// guarantees.
	winner := -1
	switch strategy {
	case "least-used":
		// Pick the eligible account with the lowest RequestCount. When counts
		// tie, fall back to LastUsed (older = preferred).
		bestReq := -1
		var bestLastUsed int64
		for _, i := range eligible {
			rc := p.accounts[i].RequestCount
			lu := p.accounts[i].LastUsed
			if winner == -1 || rc < bestReq || (rc == bestReq && lu < bestLastUsed) {
				winner = i
				bestReq = rc
				bestLastUsed = lu
			}
		}
	case "random":
		// Uniform random pick among eligible — useful as a control or to break
		// unintended synchronisation.
		winner = eligible[rand.Intn(len(eligible))]
	case "swr":
		// SWRR (smooth weighted round-robin). Each pick adds the effective
		// weight to every eligible slot's running counter, picks the highest,
		// then subtracts the total eligible weight from the winner. With weights
		// {a:5, b:1, c:1} this produces a,a,b,a,c,a,a instead of bursting.
		totalEligibleWeight := 0
		for _, i := range eligible {
			totalEligibleWeight += p.slots[i].effectiveWeight
		}
		winner = eligible[0]
		for _, i := range eligible {
			p.slots[i].currentWeight += p.slots[i].effectiveWeight
			if p.slots[i].currentWeight > p.slots[winner].currentWeight {
				winner = i
			}
		}
		p.slots[winner].currentWeight -= totalEligibleWeight
	default:
		// least-request (LOR). Score each eligible account by
		// effectiveWeight/(inflight+1) — Envoy's weighted least-request form —
		// and pick the highest (least-busy, weight-adjusted). When reserving,
		// first filter out accounts already at their AIMD concurrency limit so a
		// burst can't pile onto a saturated identity.
		candidates := eligible
		if reserve {
			admittable := candidates[:0:0] // fresh slice; don't alias eligible
			rateSaturated := false
			for _, i := range eligible {
				id := p.accounts[i].ID
				// Concurrency gate: skip accounts already at their AIMD in-flight
				// limit so a burst can't pile onto a saturated identity.
				if p.inflightLocked(id) >= p.limitLocked(id) {
					continue
				}
				// Rate gate (GCRA): skip accounts whose paced bucket says "too
				// early". Unpaced accounts (rateEstimate==0) always pass — we only
				// pace an account after it has taught us its rate via a 429.
				if !p.rateAdmitLocked(id, now) {
					rateSaturated = true
					continue
				}
				admittable = append(admittable, i)
			}
			if len(admittable) == 0 {
				// Every eligible account is busy — either at its concurrency limit
				// or rate-paced (not cooling). Ask the caller to retry shortly; a
				// concurrency slot frees as requests complete, and a paced bucket
				// refills within an emission interval. Either way the existing
				// admission-wait budget in the dispatcher smooths this into a short
				// delay instead of a 429. rateSaturated is folded in implicitly:
				// the same saturationPollInterval hint applies.
				_ = rateSaturated
				return nil, saturationPollInterval, false
			}
			candidates = admittable
		}
		winner = candidates[0]
		bestScore := -1.0
		for _, i := range candidates {
			inflight := p.inflightLocked(p.accounts[i].ID)
			score := float64(p.slots[i].effectiveWeight) / float64(inflight+1)
			if score > bestScore {
				bestScore = score
				winner = i
			}
		}
		if reserve {
			p.reserveLocked(p.accounts[winner].ID)
			// Advance the GCRA TAT for the paced account so the next pick sees the
			// consumed token. No-op for an unpaced account (rateEstimate==0).
			p.rateAdvanceLocked(p.accounts[winner].ID, now)
		}
	}

	// Return a copy so the caller can't mutate pool state through the pointer.
	accCopy := p.accounts[winner]
	return &accCopy, 0, true
}

// inflightLocked returns the reserved in-flight count for an account. Caller
// must hold p.mu.
func (p *AccountPool) inflightLocked(id string) int {
	if cd, ok := p.cooldowns[id]; ok {
		return cd.inflight
	}
	return 0
}

// limitLocked returns the account's current AIMD concurrency limit, treating an
// absent/zero limit as the initial limit. Caller must hold p.mu.
func (p *AccountPool) limitLocked(id string) int {
	if cd, ok := p.cooldowns[id]; ok && cd.limit > 0 {
		return cd.limit
	}
	return aimdInitialLimit
}

// reserveLocked increments an account's in-flight counter, creating the entry
// (seeded with the initial AIMD limit) if needed. Caller must hold p.mu.
func (p *AccountPool) reserveLocked(id string) {
	cd := p.cooldowns[id]
	if cd == nil {
		cd = &cooldownEntry{limit: aimdInitialLimit}
		p.cooldowns[id] = cd
	}
	if cd.limit <= 0 {
		cd.limit = aimdInitialLimit
	}
	cd.inflight++
}

// rateAdmitLocked reports whether the GCRA pacer admits a request for this
// account at `now`. An account with no learned rate (rateEstimate <= 0) is
// UNPACED and always admitted — we only pace after a 429 has taught us the
// rate. Caller must hold p.mu.
func (p *AccountPool) rateAdmitLocked(id string, now time.Time) bool {
	cd := p.cooldowns[id]
	if cd == nil || cd.rateEstimate <= 0 {
		return true
	}
	emission := emissionInterval(cd.rateEstimate)
	tau := time.Duration(rateBurstTolerance * float64(emission))
	return gcraAdmit(now, cd.tat, tau)
}

// rateAdvanceLocked steps the GCRA TAT forward by one emission interval for a
// paced account, recording that a token was consumed. A per-account phase
// offset is applied on the FIRST advance (when tat is zero) so two accounts'
// pacing cycles are staggered and a synchronized client burst doesn't drain
// both buckets at the same instant. No-op for an unpaced account. Caller must
// hold p.mu.
func (p *AccountPool) rateAdvanceLocked(id string, now time.Time) {
	cd := p.cooldowns[id]
	if cd == nil || cd.rateEstimate <= 0 {
		return
	}
	emission := emissionInterval(cd.rateEstimate)
	if cd.tat.IsZero() {
		// Seed the cycle at a staggered phase so peers don't fire in lockstep.
		cd.tat = now.Add(phaseOffset(id, emission))
	}
	cd.tat = gcraAdvance(now, cd.tat, emission)
}

// decayCountersLocked drops cooldown state for accounts whose last error is
// older than errorCounterDecay AND whose cooldown timer has already expired.
// Without this, an account that flaps with intermittent 429s separated by
// hours would keep climbing the decorrelated-jitter ladder via lastSleep,
// even though earlier failures should have aged out. Caller must hold p.mu.
func (p *AccountPool) decayCountersLocked(now time.Time) {
	for id, cd := range p.cooldowns {
		if cd.lastErrorAt.IsZero() {
			continue
		}
		if now.Sub(cd.lastErrorAt) <= errorCounterDecay {
			continue
		}
		// Don't drop while still cooling — that would make the account
		// look eligible mid-cooldown.
		if !cd.until.IsZero() && now.Before(cd.until) {
			continue
		}
		// Don't drop an entry that still has reserved in-flight slots — that
		// would lose the count and let a burst overshoot the AIMD limit. Just
		// age out the error history instead so backoff resets but the live
		// concurrency state survives.
		if cd.inflight > 0 {
			cd.lastErrorAt = time.Time{}
			cd.lastSleep = 0
			cd.consecutiveErrs = 0
			continue
		}
		// Don't drop an entry that carries a LEARNED PACED RATE. Deleting it
		// would reset the account to unpaced (full-speed), so the next burst
		// would re-trip the 429 we already learned to avoid — a sawtooth at the
		// decay period. The probe-up path (RecordSuccess) keeps climbing the
		// rate back toward the true ceiling at +5%/probe, so a stale-but-low
		// estimate self-corrects without ever needing to re-discover via a 429.
		// Age out only the error history.
		if cd.rateEstimate > 0 {
			cd.lastErrorAt = time.Time{}
			cd.lastSleep = 0
			cd.consecutiveErrs = 0
			continue
		}
		delete(p.cooldowns, id)
	}
}

// GetByID 根据 ID 获取账号
func (p *AccountPool) GetByID(id string) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			acc := p.accounts[i]
			return &acc
		}
	}
	return nil
}

// RecordSuccess clears soft cooldown and resets the consecutive error counter.
// It also applies the AIMD additive-increase: a healthy response nudges the
// account's concurrency limit up by 1 (capped at aimdMaxLimit), but only while
// the account is actually using its current limit (inflight near the ceiling) —
// growing an idle account's limit would just let the next burst overshoot. Hard
// quota state (UsageCurrent ≥ UsageLimit) is unaffected and continues to be
// enforced by computeEffectiveWeight on the next Reload.
//
// It also feeds the RATE pacer (see rate_pacer.go): every success folds the
// inter-success interval into the achieved-throughput EWMA (used to seed the
// snap-down on a future 429), and — if the account is currently paced — probes
// the paced rate upward by +5% once per rateProbeInterval so it climbs back
// toward the true ceiling instead of staying pinned at half.
//
// We preserve the cooldown entry (rather than deleting it) so the AIMD limit
// AND the learned paced rate survive across requests; the soft-cooldown fields
// are cleared instead.
func (p *AccountPool) RecordSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cd := p.cooldowns[id]
	if cd == nil {
		// No prior state: nothing to clear, and a fresh account starts at the
		// initial limit implicitly (limitLocked treats absent as initial). We
		// create an entry to begin observing throughput so a later 429 has a
		// measured rate to snap down from instead of falling to the floor blind.
		cd = &cooldownEntry{limit: aimdInitialLimit}
		p.cooldowns[id] = cd
	}
	now := time.Now()
	// Observe achieved throughput: fold the inter-success interval into the EWMA.
	if !cd.lastSuccessAt.IsZero() {
		cd.observedRate = updateObservedRate(cd.observedRate, now.Sub(cd.lastSuccessAt))
	}
	cd.lastSuccessAt = now
	// Probe the paced rate upward on sustained success (AIMD additive-increase),
	// rate-limited to once per rateProbeInterval. Only when actually paced.
	if cd.rateEstimate > 0 && now.Sub(cd.lastProbeAt) >= rateProbeInterval {
		cd.rateEstimate = aimdRateProbe(cd.rateEstimate)
		cd.lastProbeAt = now
	}
	// Clear soft-cooldown / error state.
	cd.until = time.Time{}
	cd.consecutiveErrs = 0
	cd.lastSleep = 0
	// AIMD additive increase, gated on actually using the limit so an idle
	// account doesn't accumulate headroom for the next burst to overshoot.
	limit := cd.limit
	if limit <= 0 {
		limit = aimdInitialLimit
	}
	if cd.inflight >= limit && limit < aimdMaxLimit {
		limit++
	}
	cd.limit = limit
	// Drop the entry only when it carries NO useful state at all. A
	// lastSuccessAt measurement must survive so the NEXT success can compute the
	// inter-success interval that feeds observedRate (the foundation of "discover
	// then pace") — deleting it here was silently resetting the measurement
	// between every pair of successes. Idle measurement-only entries are reaped
	// later by decayCountersLocked.
	if cd.inflight == 0 && cd.limit == aimdInitialLimit && cd.lastErrorAt.IsZero() &&
		cd.rateEstimate == 0 && cd.observedRate == 0 && cd.lastSuccessAt.IsZero() {
		delete(p.cooldowns, id)
	}
}

// RecordError records an error for the account and applies a tiered cooldown.
//
//   - retryAfter > 0 (from upstream Retry-After header): use it, clamped to
//     [retryAfterMin, retryAfterMax].
//   - isQuotaError && retryAfter == 0: decorrelated jitter — the next sleep
//     is drawn from random(retryAfterMin, prev × 3), capped at softCooldownMax.
//     The first 429 uses random(retryAfterMin, softCooldownBase × 3) so it
//     can't be near-zero. Decorrelated jitter desynchronises retries across
//     accounts that share an AWS identity better than full jitter does.
//   - !isQuotaError: only cool down after 3 consecutive non-quota errors.
//
// On any QUOTA error it also applies the AIMD multiplicative-decrease: the
// account's concurrency limit drops to ⌊limit × 3/4⌋ (floored at aimdMinLimit),
// so a throttled identity immediately accepts fewer concurrent requests and
// self-tunes toward AWS's actual bucket size. Every soft cooldown expiry gets a
// small random jitter (cooldownExpiryJitter) added on top so accounts that trip
// together don't all become eligible on the same tick and re-stampede.
//
// Pass retryAfter = 0 if no header was present.
func (p *AccountPool) RecordError(id string, isQuotaError bool, retryAfter time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	cd := p.cooldowns[id]
	if cd == nil {
		cd = &cooldownEntry{}
		p.cooldowns[id] = cd
	}
	now := time.Now()
	cd.lastErrorAt = now

	switch {
	case isQuotaError && retryAfter > 0:
		// Honor upstream Retry-After down to retryAfterAbsoluteMin (1s).
		// Earlier revisions clamped to 5s, which threw away free capacity
		// whenever AWS hinted at a fast recovery. We still cap the upper
		// bound so a malformed header can't park the account indefinitely.
		clamped := retryAfter
		if clamped < retryAfterAbsoluteMin {
			clamped = retryAfterAbsoluteMin
		}
		if clamped > retryAfterMax {
			clamped = retryAfterMax
		}
		cd.until = now.Add(clamped + cooldownExpiryJitterDuration())
		cd.lastSleep = clamped
		cd.consecutiveErrs = 0
		p.aimdDecreaseLocked(cd)
		p.rateDecreaseLocked(cd)

	case isQuotaError:
		// Decorrelated jitter: sleep = random(base, min(cap, prev × 3)).
		prev := cd.lastSleep
		if prev <= 0 {
			prev = softCooldownBase
		}
		ceiling := prev * 3
		if ceiling > softCooldownMax {
			ceiling = softCooldownMax
		}
		var jittered time.Duration
		if ceiling <= retryAfterMin {
			jittered = retryAfterMin
		} else {
			width := int64(ceiling - retryAfterMin)
			jittered = retryAfterMin + time.Duration(rand.Int63n(width))
		}
		cd.until = now.Add(jittered + cooldownExpiryJitterDuration())
		cd.lastSleep = jittered
		cd.consecutiveErrs = 0
		p.aimdDecreaseLocked(cd)
		p.rateDecreaseLocked(cd)

	default:
		cd.consecutiveErrs++
		if cd.consecutiveErrs >= 3 {
			cd.until = now.Add(nonQuotaCooldown + cooldownExpiryJitterDuration())
			cd.lastSleep = nonQuotaCooldown
		}
	}

	// Bump the persisted ErrorCount so the admin UI reflects reality.
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].ErrorCount++
			break
		}
	}
}

// aimdDecreaseLocked applies the multiplicative-decrease to a cooldown entry's
// concurrency limit on a quota error: limit = max(aimdMinLimit, ⌊limit×3/4⌋).
// An uninitialized limit is seeded from the initial limit first so the cut is
// meaningful. Caller must hold p.mu.
func (p *AccountPool) aimdDecreaseLocked(cd *cooldownEntry) {
	limit := cd.limit
	if limit <= 0 {
		limit = aimdInitialLimit
	}
	limit = limit * aimdDecreaseNum / aimdDecreaseDen
	if limit < aimdMinLimit {
		limit = aimdMinLimit
	}
	cd.limit = limit
}

// rateDecreaseLocked applies the AIMD multiplicative-decrease to the learned
// PACED RATE on a quota error: rate = max(rateMinPaced, observedRate × 0.5).
// This is the "discover then pace" snap-down — the first 429 AFTER we have
// measured throughput turns an unpaced account into a paced one at half the
// rate it was actually achieving; a subsequent 429 cuts further. When no
// throughput was ever measured (observedRate == 0) aimdRateDecrease returns 0
// and the account stays UNPACED — the cooldown + concurrency AIMD handle that
// case, exactly as before the pacer existed. The TAT is reset so pacing starts
// cleanly from the new rate on the next pick. Caller must hold p.mu.
func (p *AccountPool) rateDecreaseLocked(cd *cooldownEntry) {
	newRate := aimdRateDecrease(cd.observedRate)
	if newRate <= 0 {
		return // unmeasured: stay unpaced
	}
	cd.rateEstimate = newRate
	cd.tat = time.Time{}
	cd.lastProbeAt = time.Now()
}

// cooldownExpiryJitterDuration returns a random duration in [0, cooldownExpiryJitter)
// added to every cooldown expiry so a fleet that trips together doesn't recover
// in lockstep. Returns 0 if the cap is non-positive.
func cooldownExpiryJitterDuration() time.Duration {
	if cooldownExpiryJitter <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(cooldownExpiryJitter)))
}

// RecordQuotaExhaustion is RecordError's heavier sibling for monthly /
// hard-quota exhaustion (HTTP 402 OVERAGE from upstream). The recoverable
// timescale here is hours-to-days, not seconds, so we apply a long
// cooldown (1h) so the SWRR walk skips this account until the periodic
// RefreshAccountInfo path either notices a quota reset or the operator
// manually intervenes. Callers that already record the same error via
// RecordError should NOT also call this — the longer cooldown wins.
func (p *AccountPool) RecordQuotaExhaustion(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cd := p.cooldowns[id]
	if cd == nil {
		cd = &cooldownEntry{}
		p.cooldowns[id] = cd
	}
	now := time.Now()
	cd.lastErrorAt = now
	cd.until = now.Add(quotaExhaustedCooldown)
	cd.lastSleep = quotaExhaustedCooldown
	cd.consecutiveErrs = 0
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].ErrorCount++
			break
		}
	}
}

// ClearSoftCooldownIfHealthy is called from RefreshAccountInfo after a
// successful refresh confirms the account is still healthy (well under its
// usage limit). It clears the soft cooldown so the account can re-enter
// rotation immediately rather than waiting for the cooldown to expire.
func (p *AccountPool) ClearSoftCooldownIfHealthy(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cd, ok := p.cooldowns[id]; ok {
		cd.until = time.Time{}
		cd.consecutiveErrs = 0
		cd.lastSleep = 0
	}
}

// UpdateToken 更新账号 Token
func (p *AccountPool) UpdateToken(id, accessToken, refreshToken string, expiresAt int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				p.accounts[i].RefreshToken = refreshToken
			}
			p.accounts[i].ExpiresAt = expiresAt
			return
		}
	}
}

// Count 返回账号总数
func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.accounts)
}

// AvailableCount 返回当前可用（未冷却且 token 未过期）的账号数
func (p *AccountPool) AvailableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	count := 0
	for i := range p.accounts {
		acc := &p.accounts[i]
		if cd, ok := p.cooldowns[acc.ID]; ok && now.Before(cd.until) {
			continue
		}
		if acc.ExpiresAt > 0 && now.Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
			continue
		}
		count++
	}
	return count
}

// UpdateStats 更新账号统计
func (p *AccountPool) UpdateStats(id string, tokens int, credits float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].RequestCount++
			p.accounts[i].TotalTokens += tokens
			p.accounts[i].TotalCredits += credits
			p.accounts[i].LastUsed = time.Now().Unix()

			requestCount := p.accounts[i].RequestCount
			errorCount := p.accounts[i].ErrorCount
			totalTokens := p.accounts[i].TotalTokens
			totalCredits := p.accounts[i].TotalCredits
			lastUsed := p.accounts[i].LastUsed
			// Synchronous: UpdateAccountStats now only takes cfgLock and sets a
			// dirty flag (the background StatsSaver does the actual disk write),
			// so there is no I/O to offload. Spawning a goroutine per successful
			// request bought nothing and was un-recovered — a panic there would
			// crash the process (net/http only recovers the request goroutine).
			// The pool.mu -> cfgLock order here matches the established ordering
			// (Reload -> GetEnabledAccounts), so no deadlock; config never calls
			// back into the pool.
			_ = config.UpdateAccountStats(id, requestCount, errorCount, totalTokens, totalCredits, lastUsed)
			return
		}
	}
}

// GetAllAccounts returns a deduplicated copy of every account in the pool.
func (p *AccountPool) GetAllAccounts() []config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]config.Account, len(p.accounts))
	copy(result, p.accounts)
	return result
}

// CooldownRemaining reports the time remaining on the soft cooldown for the
// given account, or 0 if not cooling. Useful for tests and admin diagnostics.
func (p *AccountPool) CooldownRemaining(id string) time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if cd, ok := p.cooldowns[id]; ok {
		if d := time.Until(cd.until); d > 0 {
			return d
		}
	}
	return 0
}

func isOverUsageLimit(acc config.Account) bool {
	return acc.UsageLimit > 0 && acc.UsageCurrent >= acc.UsageLimit
}
