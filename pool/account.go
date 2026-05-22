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
	softCooldownBase  = 5 * time.Second
	softCooldownMax   = 5 * time.Minute
	retryAfterMin     = 5 * time.Second
	retryAfterMax     = 5 * time.Minute
	nonQuotaCooldown  = 60 * time.Second
	errorCounterDecay = 5 * time.Minute
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
	mu         sync.Mutex
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
	p.mu.Lock()
	defer p.mu.Unlock()
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
	return list[modelKey]
}

// GetNextForModel returns the next account that supports the requested model.
//
//   - acc == nil, ok == false                 → no account configured at all
//     (caller should return 503 / "No available accounts").
//   - acc == nil, ok == false, retryAfter > 0 → pool is non-empty but every
//     candidate is in cooldown; retryAfter is the time until the soonest
//     account becomes eligible. Caller should return 429 with Retry-After.
//   - acc != nil, ok == true                  → caller should proceed; the
//     account is eligible and not cooling.
//
// model may be empty to skip the model-list filter.
func (p *AccountPool) GetNextForModel(model string) (*config.Account, time.Duration, bool) {
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

	// SWRR walk over eligible slots only. We collect indices of slots that
	// are currently eligible (model match, no cooldown, token not about to
	// expire) and run SWRR on that subset. Ineligible slots don't accumulate
	// currentWeight, so they don't bias future picks toward themselves.
	var eligible []int
	var soonest time.Time
	for i := range p.slots {
		acc := &p.accounts[i]
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

	// SWRR pick among the eligible subset.
	totalEligibleWeight := 0
	for _, i := range eligible {
		totalEligibleWeight += p.slots[i].effectiveWeight
	}

	winner := eligible[0]
	for _, i := range eligible {
		p.slots[i].currentWeight += p.slots[i].effectiveWeight
		if p.slots[i].currentWeight > p.slots[winner].currentWeight {
			winner = i
		}
	}
	p.slots[winner].currentWeight -= totalEligibleWeight

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
	p.mu.Lock()
	defer p.mu.Unlock()
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
		clamped := retryAfter
		if clamped < retryAfterMin {
			clamped = retryAfterMin
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
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.accounts)
}

// AvailableCount 返回当前可用（未冷却且 token 未过期）的账号数
func (p *AccountPool) AvailableCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
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
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]config.Account, len(p.accounts))
	copy(result, p.accounts)
	return result
}

// CooldownRemaining reports the time remaining on the soft cooldown for the
// given account, or 0 if not cooling. Useful for tests and admin diagnostics.
func (p *AccountPool) CooldownRemaining(id string) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
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
