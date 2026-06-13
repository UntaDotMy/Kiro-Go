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
// Concurrency control (least-request strategy only): a per-account in-flight
// limit gates admission and self-tunes to AWS's unpublished bucket size using
// TCP-style slow-start + congestion-avoidance with a remembered ceiling
// (ssthresh). It starts WIDE (configurable initial window, default 12), grows
// MULTIPLICATIVELY while below the remembered ceiling (fast) and ADDITIVELY at
// or above it (cautious), and on a 429 backs off to ⌊limit×3/4⌋ while recording
// that point as ssthresh — so recovery snaps back near the known-good ceiling
// instead of re-climbing from scratch. Only a sustained run of 429s collapses
// the limit toward 1, which then gives a "half-open single probe" recovery for
// free. The other strategies (swr/least-used/random) do NOT reserve in-flight
// slots or apply this gate, so they behave exactly as before. See RFC 5681 /
// 6928, AWS SDK adaptive retry (CUBIC), and Netflix concurrency-limits.
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
	softCooldownBase      = 5 * time.Second
	softCooldownMax       = 5 * time.Minute
	retryAfterMin         = 5 * time.Second
	retryAfterMax         = 5 * time.Minute
	retryAfterAbsoluteMin = 1 * time.Second
	nonQuotaCooldown      = 60 * time.Second
	errorCounterDecay     = 5 * time.Minute

	// quotaExhaustedCooldown is the cooldown applied when the upstream
	// returns 402 OVERAGE / monthly-quota exhaustion. These are not
	// recoverable on a per-minute timescale — the quota resets at month
	// boundary — so we park the account for an hour and let the periodic
	// RefreshAccountInfo path notice the reset and re-enable.
	quotaExhaustedCooldown = 1 * time.Hour

	// ---- Adaptive concurrency (least-request strategy only) ----
	//
	// Each account carries a self-tuning in-flight limit that converges on AWS's
	// unpublished per-identity token bucket. The earliest revision used plain AIMD
	// (start at 2, +1 per success, ×3/4 on a 429); that was SAFE but SLOW — a
	// fresh account admitted only 2 parallel requests and climbed +1 per *streaming
	// completion*, so 2→12 took ~10 serialized completions (tens of seconds) and a
	// healthy identity sat throttled far below what AWS actually allowed.
	//
	// The concurrency limit now grows with TCP-style slow-start + congestion-
	// avoidance and a remembered ceiling (ssthresh): it opens FAST (geometric) on a
	// healthy account and, after a 429, snaps back near the last-known-good ceiling
	// (limit += limit/2 below ssthresh, +1 above) instead of re-climbing linearly
	// from the floor. It is the SOLE per-account throttle-guard for the
	// least-request strategy: both the rate gate and the burst gate (an earlier
	// revision had a separate GCRA rate pacer; that was removed, so the limit now
	// carries both roles). The soft 429 cooldown backs it up reactively.
	//
	// aimdInitialLimit / aimdMaxLimit are the BUILT-IN DEFAULTS used when config
	// supplies nothing (and by unit tests that build a pool without Reload). The
	// live values come from config (GetPoolInitialConcurrency /
	// GetPoolMaxConcurrency), cached onto the pool in Reload so the hot path never
	// nests the config lock under p.mu.
	//
	// See RFC 5681 (slow-start/CA), RFC 6928 (IW10 — start wide), AWS SDK adaptive
	// retry (CUBIC, β≈0.7), Netflix concurrency-limits, Google SRE "Handling
	// Overload".
	aimdInitialLimit = 12
	// aimdMinLimit is the floor — never drop below this on a 429. A 3-slot floor
	// keeps a recovered account productive immediately under a burst, while the
	// multiplicative-decrease path still lets the limit self-tune down toward this
	// floor; only a sustained run of 429s collapses it this far.
	aimdMinLimit = 3
	// aimdMaxLimit caps how far the ramp can climb. Generous (48) so a small pool
	// can use its burst headroom without a single account causing a storm; larger
	// pools rarely reach it because load spreads across accounts.
	aimdMaxLimit = 48
	// aimdDecreaseNum/Den is the multiplicative-decrease factor on a 429 (×3/4),
	// applied to BOTH the limit and the remembered ssthresh. A token bucket is a
	// wall, not TCP congestion, so we cut harder than Netflix's 0.9 default but
	// softer than TCP Reno's 0.5 (close to CUBIC's β≈0.7) to avoid over-shrinking
	// a pool that's only lightly throttled.
	aimdDecreaseNum = 3
	aimdDecreaseDen = 4

	// cooldownExpiryJitter caps the random extra time added to every soft
	// cooldown expiry so accounts that trip together don't all become eligible
	// on the same tick and re-stampede. Added on top of the computed backoff.
	cooldownExpiryJitter = 3 * time.Second

	// saturationPollInterval is the "try again very soon" hint the picker
	// returns when every eligible account is at its concurrency limit (not
	// cooling, just busy). The failover dispatcher waits this long for a slot to
	// free before re-selecting, up to its admission budget. 25ms (was 100ms)
	// shrinks the quantization waste — a slot that frees just after a poll is
	// reused ~4× sooner. Phase B replaces this poll with an event-driven wakeup
	// on Release; until then, a tighter poll is the cheap latency win.
	saturationPollInterval = 25 * time.Millisecond
)

// SaturationPollInterval is the exported value of saturationPollInterval — the
// "busy, slot will free shortly" retryAfter the reserving picker returns when
// every eligible account is at its concurrency cap. The proxy's failover
// dispatcher uses it to keep its own saturation threshold (saturationPollHint)
// at or above this value via a compile-time invariant, so a future change to one
// constant can't silently break the "wait for a slot" path.
const SaturationPollInterval = saturationPollInterval

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

	// quotaExhaustedUntil marks a HARD quota / OVERAGE park (HTTP 402 OVERAGE),
	// set by RecordQuotaExhaustion, distinct from the short soft-429 `until`. A
	// 402 OVERAGE means the upstream REJECTED the request because the account is
	// billed past its cap, so re-routing there just produces more 402s for the
	// whole park window. Unlike the soft cooldown, this park is honored by EVERY
	// strategy — including "fast" — because fast's cooldown bypass (route by free
	// capacity) only makes sense for a transient throttle, not an exhausted
	// account. Cleared on a successful request (RecordSuccess) or an explicit
	// health clear. Zero = not parked.
	quotaExhaustedUntil time.Time

	// Adaptive concurrency (least-request strategy). inflight is the number of
	// requests currently reserved on this account; limit is the live ceiling the
	// picker enforces. limit==0 means "uninitialized" — treated as the pool's
	// initial limit on first use, so an account with no cooldownEntry yet behaves
	// as a fresh limit.
	//
	// ssthresh is the slow-start threshold: the remembered concurrency ceiling at
	// which the last 429 occurred. Below ssthresh the limit grows multiplicatively
	// (slow-start, fast); at/above it the limit grows additively (congestion-
	// avoidance, cautious). ssthresh==0 means "not yet discovered" — the account
	// stays in slow-start all the way to the pool max until its first 429 teaches
	// it where the bucket ceiling is.
	inflight int
	limit    int
	ssthresh int

	// lastPickSeq is the value of AccountPool.pickSeq at the moment the "fast"
	// strategy last selected this account. Used for pick-time LRU rotation (see
	// pickFastLocked) so a burst rotates across accounts even before any request
	// completes. 0 = never picked by the fast path (sorts oldest -> filled first).
	lastPickSeq uint64
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

	// advisoryModelLists holds DISPLAY-ONLY model catalogs for providers that do
	// not expose a working GET /models endpoint (e.g. Tencent CodeBuddy, iFlow,
	// the Alibaba "alicode" coding hosts, GitLab Duo, Perplexity). These ids are a
	// best-effort static catalog: they populate the dashboard model count and the
	// /v1/models advert so the provider doesn't read as "0 models", but they are
	// DELIBERATELY NOT consulted by accountHasModel — a hardcoded list can omit a
	// model the provider later adds, and treating it as a strict routing filter
	// would shed a valid request as "no available account". Routing therefore stays
	// optimistic (the upstream validates the id at call time), exactly as it does
	// for an account with no cached list. An account has EITHER a strict modelLists
	// entry (from a live /models fetch) OR an advisory one, never both.
	advisoryModelLists map[string]map[string]bool // accountID → display-only model IDs

	// releaseCh wakes an admission waiter the instant an in-flight slot frees,
	// so the failover dispatcher no longer has to poll for a free slot (a freed
	// slot was previously unused until the next ~25ms poll tick). Buffered-1 with
	// a non-blocking send in Release: concurrent releases coalesce into a single
	// wakeup (the woken waiter re-attempts Acquire and re-waits if it still can't
	// get one), and a send that finds the buffer full is simply dropped. Waiters
	// still use a bounded fallback timer as a safety net in case a wakeup is
	// missed.
	releaseCh chan struct{}

	// initialLimit / maxLimit are the per-account adaptive-concurrency bounds,
	// cached from config on Reload (and on first use) so the request hot path
	// reads them under p.mu without nesting the config lock. Zero means "not yet
	// loaded" — resolved lazily to the built-in defaults via concurrencyBounds.
	initialLimit int
	maxLimit     int

	// fastConcurrency is the per-account concurrent-request cap for the "fast"
	// strategy — how many requests one account handles at once before the next
	// request fans out to a free peer (and waits when all are at the cap). Cached
	// from config on Reload (and lazily on first use). 0 => resolved lazily;
	// floored at 1.
	fastConcurrency int

	// pickSeq is a monotonic counter stamped onto an account's cooldownEntry each
	// time the "fast" strategy selects it (cd.lastPickSeq). It breaks ties between
	// equally-loaded accounts by least-recently-PICKED, so a burst rotates evenly
	// across free accounts at selection time (before any request completes), not
	// pinning to the lowest-index account.
	pickSeq uint64
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
			releaseCh:  make(chan struct{}, 1),
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
		releaseCh:  make(chan struct{}, 1),
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

	// Refresh the cached adaptive-concurrency bounds from config so an operator
	// change (admin UI) takes effect on Reload. Resolved here, under p.mu but
	// NOT under the config lock-on-hot-path, since Reload is a cold operation.
	p.initialLimit, p.maxLimit = concurrencyResolver()
	if p.initialLimit <= 0 {
		p.initialLimit = aimdInitialLimit
	}
	if p.maxLimit < p.initialLimit {
		p.maxLimit = p.initialLimit
	}
	// Cache the "fast" strategy per-account concurrency cap too, so the hot path
	// reads it under p.mu without nesting the config lock. Resolved lazily (see
	// fastConcurrencyLocked) when zero, e.g. a pool built in tests without Reload.
	p.fastConcurrency = fastConcurrencyResolver()

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
	for id := range p.advisoryModelLists {
		if !keep[id] {
			delete(p.advisoryModelLists, id)
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
	// Operator-set per-account token cap (api-key providers). When exhausted the
	// account is dropped from rotation entirely — there is no "overage" for a
	// bring-your-own upstream key (unlike Kiro's AWS billing switch), so a spent
	// key is simply skipped and the burst STACKS onto keys that still have budget.
	// 0 = unlimited, so Kiro accounts and every account without a configured limit
	// are unaffected.
	if isTokenLimitExhausted(a) {
		return 0
	}

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
	// A live, authoritative list supersedes any advisory (static) one.
	delete(p.advisoryModelLists, accountID)
	p.mu.Unlock()
}

// SetAdvisoryModelList records a DISPLAY-ONLY model catalog for an account whose
// provider has no working GET /models endpoint (a hardcoded static list). Unlike
// SetModelList it does NOT gate routing — accountHasModel ignores it — so an id
// missing from this best-effort list is never shed; the upstream validates the id
// at call time. It only feeds the dashboard count and the /v1/models advert (via
// GetModelList). A real /models fetch later (SetModelList) supersedes it.
func (p *AccountPool) SetAdvisoryModelList(accountID string, modelIDs []string) {
	set := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		if k := strings.ToLower(strings.TrimSpace(id)); k != "" {
			set[k] = true
		}
	}
	p.mu.Lock()
	if p.advisoryModelLists == nil {
		p.advisoryModelLists = make(map[string]map[string]bool)
	}
	p.advisoryModelLists[accountID] = set
	p.mu.Unlock()
}

// GetModelList 返回该账号缓存的模型 ID 列表（供 admin API 使用）。It returns the
// strict (live-fetched) list when present, else the advisory (static) one, so the
// dashboard count and /v1/models advert reflect a no-/models provider's catalog.
func (p *AccountPool) GetModelList(accountID string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	set, ok := p.modelLists[accountID]
	if !ok || len(set) == 0 {
		set = p.advisoryModelLists[accountID]
	}
	if len(set) == 0 {
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

// HasModelForTesting exposes the routing-gate model check (accountHasModel) to
// tests in other packages, taking the lock and normalizing the key the same way
// the picker does. It deliberately reflects ONLY the strict modelLists gate, not
// the advisory (display-only) catalog — so a test can assert that an advisory list
// never sheds an unlisted model. Not for production use.
func (p *AccountPool) HasModelForTesting(accountID, model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.accountHasModel(accountID, strings.ToLower(strings.TrimSpace(model)))
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

// concurrencyResolver returns the configured (initial, max) per-account
// concurrency bounds. Indirection via a package-level variable lets tests pin
// the bounds without bringing up a full config; production points it at config.
// The values are clamped/defaulted inside config, so callers can trust them.
var concurrencyResolver = func() (initial, max int) {
	return config.GetPoolInitialConcurrency(), config.GetPoolMaxConcurrency()
}

// SetConcurrencyResolverForTesting replaces the concurrency-bounds resolver so
// unit tests can pin specific (initial, max) values. Tests restore the previous
// resolver in a defer/cleanup.
func SetConcurrencyResolverForTesting(fn func() (int, int)) (restore func()) {
	prev := concurrencyResolver
	concurrencyResolver = fn
	return func() { concurrencyResolver = prev }
}

// fastConcurrencyResolver returns the configured per-account concurrency cap for
// the "fast" strategy. Indirection via a package-level variable lets tests pin
// the cap without bringing up a full config; production points it at config.
var fastConcurrencyResolver = func() int { return config.GetPoolFastConcurrency() }

// SetFastConcurrencyResolverForTesting replaces the fast-concurrency resolver so
// unit tests can drive the "fast" strategy's fan-out deterministically. Tests
// restore the previous resolver in a defer/cleanup.
func SetFastConcurrencyResolverForTesting(fn func() int) (restore func()) {
	prev := fastConcurrencyResolver
	fastConcurrencyResolver = fn
	return func() { fastConcurrencyResolver = prev }
}

// concurrencyBounds returns the pool's cached (initial, max) concurrency limits,
// lazily resolving them from config on first use so a pool built without Reload
// (unit tests, NewForTesting) still gets sane non-zero bounds. Caller must hold
// p.mu.
func (p *AccountPool) concurrencyBounds() (initial, max int) {
	if p.initialLimit <= 0 || p.maxLimit <= 0 {
		p.initialLimit, p.maxLimit = concurrencyResolver()
		if p.initialLimit <= 0 {
			p.initialLimit = aimdInitialLimit
		}
		if p.maxLimit < p.initialLimit {
			p.maxLimit = p.initialLimit
		}
	}
	return p.initialLimit, p.maxLimit
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
	return p.pick("", model, nil, false)
}

// GetNextForModelExcluding is the failover-aware NON-RESERVING variant. The
// exclude set holds account IDs the caller has already tried, so the picker
// skips them. Pass nil for the first pick.
func (p *AccountPool) GetNextForModelExcluding(model string, exclude map[string]bool) (*config.Account, time.Duration, bool) {
	return p.pick("", model, exclude, false)
}

// GetNextForBackendModelExcluding is the backend-scoped NON-RESERVING variant:
// it only considers accounts whose resolved backend matches `backend` (""
// = no constraint, identical to GetNextForModelExcluding). Used by non-Kiro
// provider request paths so a Codex/OpenAI/etc. request never lands on a Kiro
// account.
func (p *AccountPool) GetNextForBackendModelExcluding(backend, model string, exclude map[string]bool) (*config.Account, time.Duration, bool) {
	return p.pick(backend, model, exclude, false)
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
	return p.pick("", model, exclude, true)
}

// AcquireForBackendModelExcluding is the backend-scoped RESERVING picker: it
// only considers accounts whose resolved backend matches `backend` ("" = no
// constraint, identical to AcquireForModelExcluding). The failover dispatcher
// uses this so a request for a given provider reserves a slot on (and fails over
// among) only that provider's accounts.
func (p *AccountPool) AcquireForBackendModelExcluding(backend, model string, exclude map[string]bool) (*config.Account, time.Duration, bool) {
	return p.pick(backend, model, exclude, true)
}

// Release frees an in-flight slot previously reserved by AcquireForModelExcluding.
// Safe to call unconditionally: it floors at zero and is a no-op for accounts
// that never reserved (non-least-request strategies, or an already-released
// slot). Must be called exactly once per successful Acquire.
//
// On a real slot free it signals releaseCh (non-blocking, coalesced) so an
// admission waiter wakes immediately instead of after the next poll tick.
func (p *AccountPool) Release(id string) {
	p.mu.Lock()
	freed := false
	if cd, ok := p.cooldowns[id]; ok && cd.inflight > 0 {
		cd.inflight--
		freed = true
	}
	p.mu.Unlock()
	if freed {
		p.signalRelease()
	}
}

// signalRelease does a non-blocking send on releaseCh so a waiting acquirer
// wakes at once. Buffered-1 + non-blocking: concurrent releases coalesce into a
// single pending wakeup, and a send that finds the buffer full is dropped (the
// already-pending signal will wake a waiter, who re-attempts and re-waits if a
// slot still isn't available). Nil-safe for pools built without the channel.
func (p *AccountPool) signalRelease() {
	if p.releaseCh == nil {
		return
	}
	select {
	case p.releaseCh <- struct{}{}:
	default:
	}
}

// ReleaseSignal returns the channel an admission waiter selects on to be woken
// the instant an in-flight slot frees. Pair a receive on it with a bounded
// fallback timer as a safety net in case a wakeup is missed. Returns nil for a
// pool built without the channel, in which case the caller falls back to pure
// polling.
func (p *AccountPool) ReleaseSignal() <-chan struct{} {
	return p.releaseCh
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

// ConcurrencyState reports an account's live in-flight count and its current
// AIMD concurrency limit under a single lock, for the dashboard's per-account
// realtime display. The limit reflects what the picker would enforce right now:
// an account with no entry yet reports the initial limit. Exposed for admin
// diagnostics and tests.
func (p *AccountPool) ConcurrencyState(id string) (inflight, limit int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	// Read-only fallback: this method holds RLock, so it can't lazily populate
	// the cached bounds via concurrencyBounds (which mutates). Use the cached
	// initial limit if Reload has run, else the built-in default.
	initial := p.initialLimit
	if initial <= 0 {
		initial = aimdInitialLimit
	}
	if cd, ok := p.cooldowns[id]; ok {
		limit = cd.limit
		if limit <= 0 {
			limit = initial
		}
		return cd.inflight, limit
	}
	return 0, initial
}

// pick is the shared selection core. reserve=true applies the AIMD concurrency
// gate and increments the winner's in-flight counter (least-request only).
//
// backend scopes selection to accounts whose resolved backend matches: ""
// means "no constraint" (legacy behavior — every account is eligible), while a
// concrete value (e.g. "kiro", "codex", "openai") restricts the eligible set so
// a request for one provider never lands on another provider's account. Because
// a Backend-less account resolves to "kiro" (config.GetAccountBackend), a pool
// of only pre-existing Kiro accounts behaves identically whether backend is ""
// or "kiro".
func (p *AccountPool) pick(backend, model string, exclude map[string]bool, reserve bool) (*config.Account, time.Duration, bool) {
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
	backendKey := strings.ToLower(strings.TrimSpace(backend))

	// Collect indices of slots that are currently eligible (backend match, model
	// match, not excluded, no cooldown, token not about to expire). Ineligible
	// slots don't accumulate SWRR currentWeight, so they don't bias future picks.
	var eligible []int
	var soonest time.Time
	for i := range p.slots {
		acc := &p.accounts[i]
		if exclude != nil && exclude[acc.ID] {
			continue
		}
		if backendKey != "" && config.GetAccountBackend(acc) != backendKey {
			continue
		}
		if modelKey != "" && !p.accountHasModel(acc.ID, modelKey) {
			continue
		}
		if acc.ExpiresAt > 0 && now.Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
			continue
		}
		// Live per-account token-cap check. computeEffectiveWeight already drops an
		// exhausted account at Reload, but TotalTokens keeps growing mid-session via
		// UpdateStats (which mutates the in-memory p.accounts copy directly), so a key
		// can cross its limit BETWEEN reloads. Skip it here too so the burst stacks
		// onto keys that still have budget the instant one is spent — every strategy,
		// including "fast". 0 = unlimited, so Kiro / unconfigured accounts are
		// unaffected.
		if isTokenLimitExhausted(*acc) {
			continue
		}
		// Hard quota / OVERAGE park is honored by EVERY strategy, INCLUDING
		// "fast". A 402 OVERAGE means the upstream rejected the request because
		// the account is billed past its cap; routing there again just yields
		// more 402s for the whole park window. This is distinct from the soft
		// 429 cooldown below, which fast deliberately bypasses (route by free
		// capacity). Cleared by RecordSuccess / ClearSoftCooldownIfHealthy.
		if cd, ok := p.cooldowns[acc.ID]; ok && now.Before(cd.quotaExhaustedUntil) {
			if soonest.IsZero() || cd.quotaExhaustedUntil.Before(soonest) {
				soonest = cd.quotaExhaustedUntil
			}
			continue
		}
		// Soft 429 cooldown is honored by every strategy EXCEPT "fast", which
		// routes purely by free capacity — an in-use account simply isn't picked
		// until it frees, and a 429'd account is steered around by load, not by a
		// timed cooldown. This bypass is bounded because hard-disable is enforced
		// independently of cd.until: an account whose quota is exhausted WITHOUT an
		// overage allowance gets computeEffectiveWeight==0 and is dropped from
		// p.slots on the next Reload, so it never reaches this eligible walk at all.
		// The remaining case — an OVERAGE-ALLOWED account that returned 402 — is
		// deliberately kept in fast rotation (overage is permitted), so fast will
		// route to it again before its 1h RecordQuotaExhaustion cooldown expires;
		// the other strategies still skip it for that hour.
		if strategy != "fast" {
			if cd, ok := p.cooldowns[acc.ID]; ok && now.Before(cd.until) {
				if soonest.IsZero() || cd.until.Before(soonest) {
					soonest = cd.until
				}
				continue
			}
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
	case "fast":
		// Parallel-spread selection (WiFi-7 multi-link). Each eligible account
		// holds up to fastConcurrency concurrent requests (the per-account "rotate
		// number", default 1 = send-and-ack). A burst fans OUT across the
		// least-loaded free accounts so N concurrent requests land on N distinct
		// accounts in parallel instead of queueing on one. When reserving and EVERY
		// eligible account is at its cap, we return the saturation poll hint so the
		// dispatcher WAITS for a slot to free (woken the instant one does) and
		// routes there — no request is dropped just because the pool is momentarily
		// full. No cooldown gate and no AIMD limit on this path: an in-use account
		// simply isn't picked until it frees.
		w, saturated := p.pickFastLocked(eligible, reserve)
		if saturated {
			return nil, saturationPollInterval, false
		}
		winner = w
		if winner >= 0 && reserve {
			p.reserveLocked(p.accounts[winner].ID)
		}
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
			for _, i := range eligible {
				id := p.accounts[i].ID
				// Concurrency gate: skip accounts already at their AIMD in-flight
				// limit so a burst can't pile onto a saturated identity.
				if p.inflightLocked(id) >= p.limitLocked(id) {
					continue
				}
				admittable = append(admittable, i)
			}
			if len(admittable) == 0 {
				// Every eligible account is at its concurrency limit (busy, not
				// cooling). Ask the caller to retry shortly; a slot frees as
				// requests complete, and the dispatcher's admission-wait budget
				// smooths this into a short delay instead of a 429.
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

// fastConcurrencyLocked returns the cached "fast" strategy per-account
// concurrency cap, lazily resolving it from config on first use so a pool built
// without Reload (tests, NewForTesting) still gets a sane non-zero value.
// Floored at 1 (send-and-ack — one request per account at a time). Caller must
// hold p.mu.
func (p *AccountPool) fastConcurrencyLocked() int {
	if p.fastConcurrency <= 0 {
		p.fastConcurrency = fastConcurrencyResolver()
	}
	if p.fastConcurrency < 1 {
		p.fastConcurrency = 1
	}
	return p.fastConcurrency
}

// pickFastLocked implements the "fast" strategy's parallel-spread selection over
// the eligible slot indices. It models WiFi-7 multi-link: a burst of N requests
// fans OUT across the N least-loaded free accounts so each goes to a distinct
// account in parallel, instead of queueing on one.
//
// Selection, among accounts BELOW their per-account fast-concurrency cap:
//   - fewest in-flight wins (spread the load) — this is what turns 4 concurrent
//     requests across 4 free accounts into one-each;
//   - ties broken by higher effective weight (prefer stronger accounts);
//   - then by least-recently-PICKED (cd.lastPickSeq) so equal accounts rotate at
//     SELECTION time, before any request completes, instead of pinning to slot 0.
//
// Returns (winningIndex, saturated):
//   - (i, false): account i was selected.
//   - (-1, true): reserve was requested and EVERY eligible account is already at
//     its fast cap (all busy, not cooling). The caller turns this into the
//     saturation poll hint so the dispatcher waits for a slot to free.
//   - (i, false) with reserve=false always returns a pick (the non-reserving
//     picker doesn't gate on the cap — it's used by diagnostics / single-account
//     paths where there's no slot to reserve).
//
// Caller must hold p.mu and must have ensured len(eligible) > 0.
func (p *AccountPool) pickFastLocked(eligible []int, reserve bool) (int, bool) {
	if len(eligible) == 0 {
		return -1, false
	}
	fastCap := p.fastConcurrencyLocked()

	// pickRecency returns the account's last fast-pick sequence (0 = never).
	pickRecency := func(i int) uint64 {
		if cd, ok := p.cooldowns[p.accounts[i].ID]; ok {
			return cd.lastPickSeq
		}
		return 0
	}
	// stamp records that index i was just selected, advancing the pool's pick
	// sequence so the next pick rotates off it under a same-instant burst.
	stamp := func(i int) (int, bool) {
		p.pickSeq++
		id := p.accounts[i].ID
		cd := p.cooldowns[id]
		if cd == nil {
			cd = &cooldownEntry{}
			p.cooldowns[id] = cd
		}
		cd.lastPickSeq = p.pickSeq
		return i, false
	}

	// better reports whether candidate i should beat the current best j:
	// fewer in-flight, then higher weight, then least-recently-picked.
	better := func(i, j int) bool {
		ii, ij := p.inflightLocked(p.accounts[i].ID), p.inflightLocked(p.accounts[j].ID)
		if ii != ij {
			return ii < ij
		}
		wi, wj := p.slots[i].effectiveWeight, p.slots[j].effectiveWeight
		if wi != wj {
			return wi > wj
		}
		return pickRecency(i) < pickRecency(j)
	}

	best := -1
	for _, i := range eligible {
		// When reserving, only accounts BELOW their fast cap are admittable, so a
		// burst spreads to a free peer instead of doubling up on a busy account.
		if reserve && p.inflightLocked(p.accounts[i].ID) >= fastCap {
			continue
		}
		if best == -1 || better(i, best) {
			best = i
		}
	}
	if best == -1 {
		// reserve was requested and every eligible account is at its cap.
		return -1, true
	}
	return stamp(best)
}

// limitLocked returns the account's current concurrency limit, treating an
// absent/zero limit as the pool's initial limit. Caller must hold p.mu.
func (p *AccountPool) limitLocked(id string) int {
	if cd, ok := p.cooldowns[id]; ok && cd.limit > 0 {
		return cd.limit
	}
	initial, _ := p.concurrencyBounds()
	return initial
}

// reserveLocked increments an account's in-flight counter, creating the entry
// (seeded with the pool's initial limit) if needed. Caller must hold p.mu.
func (p *AccountPool) reserveLocked(id string) {
	initial, _ := p.concurrencyBounds()
	cd := p.cooldowns[id]
	if cd == nil {
		cd = &cooldownEntry{limit: initial}
		p.cooldowns[id] = cd
	}
	if cd.limit <= 0 {
		cd.limit = initial
	}
	cd.inflight++
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
// It also grows the account's concurrency limit using TCP-style slow-start /
// congestion-avoidance, but only while the account is actually USING its limit
// (inflight at the ceiling) — growing an idle account's limit would just let the
// next burst overshoot. Below the remembered ssthresh the limit grows
// MULTIPLICATIVELY (limit += limit/2, fast slow-start); at/above ssthresh it
// grows ADDITIVELY (+1, cautious congestion-avoidance). Both are capped at the
// pool max. An account that has never 429ed (ssthresh==0) stays in fast
// slow-start all the way to the max. Hard quota state (UsageCurrent ≥ UsageLimit)
// is unaffected and continues to be enforced by computeEffectiveWeight on the
// next Reload.
//
// We preserve the cooldown entry (rather than deleting it) so the concurrency
// limit and the remembered ssthresh survive across requests; the soft-cooldown
// fields are cleared instead.
func (p *AccountPool) RecordSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cd := p.cooldowns[id]
	if cd == nil {
		// No prior state: nothing to clear, and a fresh account starts at the
		// initial limit implicitly (limitLocked treats absent as initial).
		cd = &cooldownEntry{limit: aimdInitialLimit}
		p.cooldowns[id] = cd
	}
	initial, max := p.concurrencyBounds()
	// Clear soft-cooldown / error state.
	cd.until = time.Time{}
	cd.quotaExhaustedUntil = time.Time{} // a success means the account is serving again
	cd.consecutiveErrs = 0
	cd.lastSleep = 0
	// Grow the limit, gated on actually using it so an idle account doesn't
	// accumulate headroom for the next burst to overshoot. The gate is relaxed to
	// inflight >= limit-1 so growth keeps up under a steady burst that sits just
	// below the ceiling. Below the remembered ssthresh the limit climbs FAST
	// (slow-start, geometric); at/above it the limit grows by +1 (congestion-
	// avoidance). The rate pacer remains the proactive 429-guard, so a fast
	// concurrency climb doesn't itself induce throttling.
	limit := cd.limit
	if limit <= 0 {
		limit = initial
	}
	if cd.inflight >= limit-1 && limit < max {
		if cd.ssthresh > 0 && limit >= cd.ssthresh {
			// Congestion-avoidance: gently probe above a known ceiling.
			limit++
		} else {
			// Slow-start: climb fast toward the ceiling (or the max if no
			// ceiling has been discovered yet). limit/2 makes it geometric;
			// the max(1, …) guarantees forward progress at small limits.
			step := limit / 2
			if step < 1 {
				step = 1
			}
			limit += step
		}
		if limit > max {
			limit = max
		}
	}
	cd.limit = limit
	// Drop the entry only when it carries NO useful state at all. A remembered
	// ssthresh must survive so recovery snaps back to the known-good ceiling.
	// Idle measurement-only entries are reaped later by decayCountersLocked.
	if cd.inflight == 0 && cd.limit == initial && cd.ssthresh == 0 && cd.lastErrorAt.IsZero() {
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

// aimdDecreaseLocked applies the multiplicative-decrease on a quota error and
// REMEMBERS the ceiling. It sets ssthresh = ⌊limit×3/4⌋ (the slow-start
// threshold — where this account started 429ing) and drops the live limit to
// that same value, floored at aimdMinLimit. This is TCP/CUBIC fast-recovery, not
// a collapse to 1: a single 429 backs the account off to 3/4 of where it was and
// records that point, so the next recovery snaps back via slow-start to just
// below the known ceiling instead of re-climbing from the initial window. Only a
// sustained run of 429s walks both limit and ssthresh down toward the floor —
// which is precisely when the half-open single-probe recovery is wanted. An
// uninitialized limit is seeded from the pool initial limit first so the cut is
// meaningful. Caller must hold p.mu.
func (p *AccountPool) aimdDecreaseLocked(cd *cooldownEntry) {
	initial, _ := p.concurrencyBounds()
	limit := cd.limit
	if limit <= 0 {
		limit = initial
	}
	limit = limit * aimdDecreaseNum / aimdDecreaseDen
	if limit < aimdMinLimit {
		limit = aimdMinLimit
	}
	// Remember the backed-off point as the slow-start threshold so recovery
	// returns near the discovered ceiling and then probes additively above it.
	cd.ssthresh = limit
	cd.limit = limit
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
	// Mark the HARD quota park so EVERY strategy (incl. "fast") skips this
	// account until the park expires — a 402 OVERAGE means the request was
	// rejected, so fast's "route by free capacity" cooldown bypass must not apply.
	cd.quotaExhaustedUntil = now.Add(quotaExhaustedCooldown)
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
// rotation immediately rather than waiting for the cooldown to expire. The
// hard quota/OVERAGE park is cleared too: a refresh that confirms the account
// is under its limit means the overage condition is gone, so the account should
// rejoin rotation without waiting out the full park window.
func (p *AccountPool) ClearSoftCooldownIfHealthy(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cd, ok := p.cooldowns[id]; ok {
		cd.until = time.Time{}
		cd.quotaExhaustedUntil = time.Time{}
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

// EligibleCountForBackendModel reports how many DISTINCT accounts a request for
// (backend, model) could be routed to right now, applying the same eligibility
// predicate as the picker EXCEPT the per-account concurrency gate (which is a
// transient "busy", not "can't serve"): backend match, model match, token not
// about to expire, per-account token cap not exhausted, and — for every strategy
// except "fast" — not currently in soft 429 cooldown. backend == "" / model == ""
// mean "no constraint" on that dimension, matching pick().
//
// The failover dispatcher uses this to size its attempt budget to the real
// addressable pool instead of a fixed constant, so a large pool isn't capped at
// 3 tries while many healthy accounts go untried. It intentionally counts
// cooled accounts as eligible under "fast" (fast routes by free capacity, not
// cooldown), exactly as pick() would consider them.
func (p *AccountPool) EligibleCountForBackendModel(backend, model string) int {
	strategy := strategyResolver()

	p.mu.RLock()
	defer p.mu.RUnlock()

	now := time.Now()
	modelKey := strings.ToLower(strings.TrimSpace(model))
	backendKey := strings.ToLower(strings.TrimSpace(backend))

	count := 0
	for i := range p.accounts {
		acc := &p.accounts[i]
		if backendKey != "" && config.GetAccountBackend(acc) != backendKey {
			continue
		}
		if modelKey != "" && !p.accountHasModel(acc.ID, modelKey) {
			continue
		}
		if acc.ExpiresAt > 0 && now.Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
			continue
		}
		if isTokenLimitExhausted(*acc) {
			continue
		}
		// Hard quota / OVERAGE park is honored by every strategy (incl. fast).
		if cd, ok := p.cooldowns[acc.ID]; ok && now.Before(cd.quotaExhaustedUntil) {
			continue
		}
		if strategy != "fast" {
			if cd, ok := p.cooldowns[acc.ID]; ok && now.Before(cd.until) {
				continue
			}
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

// isTokenLimitExhausted reports whether an operator-set per-account token cap has
// been reached. 0 = unlimited (the default), so this is a no-op for Kiro accounts
// and any account without a configured limit. The counter is the same cumulative
// Account.TotalTokens the dashboard already tracks, incremented by pool.UpdateStats
// after each successful request.
func isTokenLimitExhausted(acc config.Account) bool {
	return acc.TokenLimit > 0 && acc.TotalTokens >= acc.TokenLimit
}
