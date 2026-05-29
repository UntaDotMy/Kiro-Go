// Package pool 账号池管理
//
// Implements smooth weighted round-robin (SWRR) account selection plus a
// per-account cooldown state machine that distinguishes soft throttles from
// hard quota exhaustion.
//
// Selection algorithm: smooth weighted round-robin (the nginx/Envoy variant).
// For each pick we add the effective weight to every account's runningWeight,
// pick the account with the highest runningWeight, then subtract the total
// effective weight from the winner. With weights {a:5, b:1, c:1} this
// produces the interleaved sequence a,a,b,a,c,a,a instead of bursting
// a,a,a,a,a then b then c — which matters because each account is one AWS
// identity and AWS rate-limits per identity.
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
)

// accountSlot is one entry in the SWRR scheduler. The slot is keyed
// positionally to AccountPool.accounts, so we don't store the account ID here.
type accountSlot struct {
	effectiveWeight int
	currentWeight   int
}

// cooldownEntry tracks per-account error state.
type cooldownEntry struct {
	until           time.Time     // soft cooldown expiry; zero = not cooling
	lastSleep       time.Duration // last cooldown duration we computed; seeds decorrelated jitter
	lastErrorAt     time.Time     // for decay
	consecutiveErrs int           // consecutive non-quota errors
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

// accountInGroup reports whether the given account belongs to the named
// group. Empty group is treated as "no restriction" by the caller; this
// helper assumes group is non-empty and lower-cased. An account with no
// Groups configured is universal — it participates in every grouped
// call so existing deployments that don't use grouping keep working
// without configuration changes.
func accountInGroup(a config.Account, group string) bool {
	if len(a.Groups) == 0 {
		return true
	}
	for _, ag := range a.Groups {
		if strings.EqualFold(strings.TrimSpace(ag), group) {
			return true
		}
	}
	return false
}

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
// model and belongs to the requested group.
//
//   - acc == nil, ok == false                 → no account configured at all
//     (caller should return 503 / "No available accounts").
//   - acc == nil, ok == false, retryAfter > 0 → pool is non-empty but every
//     candidate is in cooldown; retryAfter is the time until the soonest
//     account becomes eligible. Caller should return 429 with Retry-After.
//   - acc != nil, ok == true                  → caller should proceed; the
//     account is eligible and not cooling.
//
// model may be empty to skip the model-list filter. group may be empty
// to skip the group filter. When non-empty, group restricts the pool to
// accounts whose Groups list includes the named group; accounts with
// empty Groups participate in every group-restricted call (back-compat
// for deployments that don't use grouping). The group is supplied by the
// per-API-key configuration: the handler resolves the inbound API key
// to a group and passes it here.
func (p *AccountPool) GetNextForModel(model string) (*config.Account, time.Duration, bool) {
	return p.GetNextForModelInGroup(model, "")
}

// GetNextForModelInGroup is the group-aware variant of GetNextForModel.
// New callers should use this directly; GetNextForModel is preserved as a
// thin wrapper so existing call sites that don't yet thread an API-key
// group don't have to change.
func (p *AccountPool) GetNextForModelInGroup(model, group string) (*config.Account, time.Duration, bool) {
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
	groupKey := strings.ToLower(strings.TrimSpace(group))

	// SWRR walk over eligible slots only. We collect indices of slots that
	// are currently eligible (model match, group match, no cooldown, token
	// not about to expire) and run SWRR on that subset. Ineligible slots
	// don't accumulate currentWeight, so they don't bias future picks
	// toward themselves.
	var eligible []int
	var soonest time.Time
	for i := range p.slots {
		acc := &p.accounts[i]
		if groupKey != "" && !accountInGroup(*acc, groupKey) {
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

	// Strategy dispatch among the eligible subset. The default is SWRR
	// (smooth weighted round-robin) — see the package comment for why we
	// prefer it over plain RR for AWS-identity-bound traffic. The other
	// strategies trade SWRR's interleaving guarantee for either per-account
	// "freshness" weighting (least-used) or pure jitter (random); both can
	// help when the pool is heterogeneous (mixed quotas) or when the
	// operator wants to break a synchronisation that SWRR alone can't.
	winner := -1
	switch strategyResolver() {
	case "least-used":
		// Pick the eligible account with the lowest RequestCount. When
		// counts tie, fall back to LastUsed (older = preferred) so we
		// drain freshly-added accounts before re-hitting the pool's
		// long-tenured ones. This naturally tilts traffic toward less
		// burned accounts without needing an explicit weight.
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
		// Uniform random pick among eligible — useful as a control or to
		// break unintended synchronisation.
		winner = eligible[rand.Intn(len(eligible))]
	default:
		// SWRR (smooth weighted round-robin) — original behavior. Each
		// pick adds the effective weight to every eligible slot's running
		// counter, picks the slot with the highest counter, then subtracts
		// the total eligible weight from the winner. With weights
		// {a:5, b:1, c:1} this produces a,a,b,a,c,a,a instead of bursting
		// a,a,a,a,a then b then c.
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
	}

	// Return a copy so the caller can't mutate pool state through the pointer.
	accCopy := p.accounts[winner]
	return &accCopy, 0, true
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
// Hard quota state (UsageCurrent ≥ UsageLimit) is unaffected and continues to
// be enforced by computeEffectiveWeight on the next Reload.
func (p *AccountPool) RecordSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cooldowns, id)
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
		cd.until = now.Add(clamped)
		cd.lastSleep = clamped
		cd.consecutiveErrs = 0

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
		cd.until = now.Add(jittered)
		cd.lastSleep = jittered
		cd.consecutiveErrs = 0

	default:
		cd.consecutiveErrs++
		if cd.consecutiveErrs >= 3 {
			cd.until = now.Add(nonQuotaCooldown)
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
			go config.UpdateAccountStats(id, requestCount, errorCount, totalTokens, totalCredits, lastUsed)
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
