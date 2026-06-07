// Realtime dashboard push over WebSocket.
//
// The admin dashboard previously polled /admin/api/status every 3 seconds.
// That meant every credit / token change took up to 3s to surface, and the
// dashboard burned an HTTP round-trip per tick whether or not anything had
// changed. The dashboard hub here pushes status snapshots to subscribed
// browsers as soon as they happen — RecordSuccess / RecordFailure on the
// request path, and RefreshAccountInfo on the periodic refresh path both
// trigger a broadcast.
//
// Auth: WebSockets cannot carry custom headers from a browser, so we
// piggy-back on the Sec-WebSocket-Protocol handshake. The dashboard
// passes the admin password as a subprotocol value:
//
//	new WebSocket(url, ['admin-password.<password>'])
//
// We verify against config.GetPassword() with constant-time compare and
// echo the protocol back so the handshake completes. The password stays in
// the request header (not the URL), so it doesn't end up in browser
// history or HTTP access logs.
//
// Backpressure: each subscriber has a 16-buffer channel. Slow consumers
// drop the connection rather than block the broadcast — every event is
// best-effort, and the next event re-syncs the full snapshot.
package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// dashboardSubscriber is one active WebSocket client receiving dashboard
// snapshots.
type dashboardSubscriber struct {
	send   chan []byte
	done   chan struct{}
	closed bool
}

// dashboardHub fans broadcasts out to every connected dashboard client.
// Add / remove are serialised through a mutex; broadcast walks the
// subscriber set under read-lock and tries a non-blocking send. A client
// that doesn't drain its buffer fast enough is removed from rotation
// (their goroutine sees the closed channel via context).
type dashboardHub struct {
	mu          sync.RWMutex
	subscribers map[*dashboardSubscriber]struct{}
}

func newDashboardHub() *dashboardHub {
	return &dashboardHub{
		subscribers: make(map[*dashboardSubscriber]struct{}),
	}
}

func (h *dashboardHub) add(s *dashboardSubscriber) {
	h.mu.Lock()
	h.subscribers[s] = struct{}{}
	h.mu.Unlock()
}

func (h *dashboardHub) remove(s *dashboardSubscriber) {
	h.mu.Lock()
	if _, ok := h.subscribers[s]; ok {
		delete(h.subscribers, s)
		if !s.closed {
			s.closed = true
			close(s.done)
		}
	}
	h.mu.Unlock()
}

// hasSubscribers reports whether any dashboard client is currently connected.
// Used by broadcastDashboardUpdate to skip building an (expensive) snapshot —
// full pool copy, config copy, N per-account locks, and a JSON marshal — on the
// request hot path when nobody is listening, which is the common case in
// production where the admin dashboard is usually closed.
func (h *dashboardHub) hasSubscribers() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subscribers) > 0
}

// broadcast sends payload to every active subscriber. Drops the message
// for any subscriber whose buffer is full — clients are responsible for
// keeping up; the snapshot model means a missed message is harmless.
func (h *dashboardHub) broadcast(payload []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subscribers {
		select {
		case s.send <- payload:
		default:
			// Drop slow consumer — they will eventually be evicted by
			// the heartbeat path when their write deadline trips.
		}
	}
}

// dashboardWsUpgrader matches the responses-WS upgrader's CheckOrigin
// rules so cross-origin upgrades are rejected unless KIRO_WS_ALLOW_ANY_ORIGIN
// is set.
var dashboardWsUpgrader = websocket.Upgrader{
	ReadBufferSize:  2048,
	WriteBufferSize: 2048,
	CheckOrigin:     checkResponsesWsOrigin,
	Subprotocols:    []string{"kiro-admin"},
}

// dashboardWSAuthSubprotocolPrefix is the prefix the browser sends in
// Sec-WebSocket-Protocol to carry the admin password through the
// upgrade handshake. Per RFC 6455 the value must be a token (no spaces,
// no commas), so the password is appended after the dot. The full
// protocol value is then echoed back in Sec-WebSocket-Protocol on the
// 101 response, which is required to complete the handshake.
const dashboardWSAuthSubprotocolPrefix = "admin-password."

// dashboardWSAcceptSubprotocol is the non-sensitive value the server echoes in
// the 101 Sec-WebSocket-Protocol response. We must NOT echo the password-bearing
// "admin-password.<pw>" token: response headers land in reverse-proxy access
// logs (nginx/ALB) and browser DevTools, which would leak the plaintext admin
// password. The browser only requires the echoed subprotocol to be one it
// offered, so the client offers this static token alongside the auth token and
// the server selects this one.
const dashboardWSAcceptSubprotocol = "kiro-admin-v1"

// handleDashboardWS upgrades an admin dashboard WebSocket and starts a
// goroutine that pumps hub broadcasts to the client. Heartbeat ping every
// 30s; missed pong within 60s closes the connection.
func (h *Handler) handleDashboardWS(w http.ResponseWriter, r *http.Request) {
	// Extract password from Sec-WebSocket-Protocol. The header is a
	// comma-separated list per RFC 6455; we accept any element with the
	// expected prefix.
	var password string
	for _, v := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, raw := range strings.Split(v, ",") {
			tok := strings.TrimSpace(raw)
			if strings.HasPrefix(tok, dashboardWSAuthSubprotocolPrefix) {
				password = strings.TrimPrefix(tok, dashboardWSAuthSubprotocolPrefix)
				break
			}
		}
		if password != "" {
			break
		}
	}
	if !config.VerifyPassword(password) {
		// Don't reveal whether the issue was missing protocol vs wrong
		// password — both surface as 401 to the upgrade attempt.
		w.WriteHeader(401)
		return
	}

	// Echo a STATIC, non-sensitive subprotocol back so the browser handshake
	// completes without leaking the password in the 101 response header. The
	// client offers this token alongside the password-bearing auth token; the
	// browser only requires the echoed value to be one it offered.
	upgrader := dashboardWsUpgrader
	upgrader.Subprotocols = []string{dashboardWSAcceptSubprotocol}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Warnf("[DashboardWS] upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	sub := &dashboardSubscriber{
		send: make(chan []byte, 16),
		done: make(chan struct{}),
	}
	h.dashboardHub.add(sub)
	defer h.dashboardHub.remove(sub)

	// Send an initial snapshot so the dashboard renders immediately
	// without waiting for the first state-change broadcast.
	if snapshot := h.dashboardSnapshot(); snapshot != nil {
		select {
		case sub.send <- snapshot:
		default:
		}
	}

	// Read pump: discard incoming messages but use them as keepalive
	// signals. We also need ReadMessage to detect a client-side close so
	// the connection is reaped promptly.
	go func() {
		// Recover so a panic in the read pump can't crash the whole process —
		// this is a spawned goroutine, which net/http does not protect.
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[DashboardWS] read pump panic recovered: %v", r)
				h.dashboardHub.remove(sub)
			}
		}()
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			return nil
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				h.dashboardHub.remove(sub)
				return
			}
		}
	}()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-sub.done:
			return
		case msg, ok := <-sub.send:
			if !ok {
				return
			}
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// dashboardSnapshot computes a JSON payload mirroring apiGetStatus's body.
// The admin dashboard's onmessage handler dispatches purely on the JSON
// shape, so push and poll responses share one renderer.
func (h *Handler) dashboardSnapshot() []byte {
	var quotaTotal, quotaUsed float64
	var activeQuotaTotal, activeQuotaUsed float64
	var activeTokens int
	var activeRequests int

	// Per-account live counters, keyed by id, so the dashboard can update each
	// account card's credits / tokens / requests in realtime instead of waiting
	// for the operator to hit refresh. We merge config (quota/subscription) with
	// the pool's runtime stats (request/error/token/credit counters), mirroring
	// what apiGetAccounts returns but limited to the fields the cards display
	// live. Keeping it small keeps every broadcast cheap (this runs on every
	// recordSuccess).
	poolStats := make(map[string]config.Account)
	for _, a := range h.pool.GetAllAccounts() {
		poolStats[a.ID] = a
	}
	accountList := make([]map[string]interface{}, 0)
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
		st := poolStats[a.ID]
		inflight, concurrencyLimit := h.pool.ConcurrencyState(a.ID)
		pacedRate, observedRate := h.pool.RateState(a.ID)
		ttft := h.pool.TTFTState(a.ID)
		cooldownSecs := int(h.pool.CooldownRemaining(a.ID).Round(time.Second).Seconds())
		overQuota := a.UsageLimit > 0 && a.UsageCurrent >= a.UsageLimit
		accountList = append(accountList, map[string]interface{}{
			"id":                a.ID,
			"enabled":           a.Enabled,
			"banStatus":         a.BanStatus,
			"expiresAt":         a.ExpiresAt,
			"hasToken":          a.AccessToken != "",
			"usageCurrent":      a.UsageCurrent,
			"usageLimit":        a.UsageLimit,
			"usagePercent":      a.UsagePercent,
			"trialUsageCurrent": a.TrialUsageCurrent,
			"trialUsageLimit":   a.TrialUsageLimit,
			"trialUsagePercent": a.TrialUsagePercent,
			"requestCount":      st.RequestCount,
			"errorCount":        st.ErrorCount,
			"totalTokens":       st.TotalTokens,
			"totalCredits":      st.TotalCredits,
			"lastUsed":          st.LastUsed,
			"lastRefresh":       a.LastRefresh,
			"inflight":          inflight,
			"concurrencyLimit":  concurrencyLimit,
			"pacedRate":         pacedRate,
			"observedRate":      observedRate,
			"ttftMs":            ttft,
			"cooldownSecs":      cooldownSecs,
			"overQuota":         overQuota,
		})
	}
	payload := map[string]interface{}{
		"type":             "status",
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
		"accountStats":     accountList,
		"uptime":           time.Now().Unix() - h.startTime,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return b
}

// broadcastDashboardUpdate is called from the request and refresh paths
// whenever something the dashboard would care about changes (counters,
// account info, credit balance). Coalesces is the caller's job — this
// function publishes whatever the current state is.
func (h *Handler) broadcastDashboardUpdate() {
	if h.dashboardHub == nil {
		return
	}
	// Fast path: when no dashboard is connected (the common production case),
	// skip building the snapshot entirely. dashboardSnapshot copies the whole
	// pool + config, takes a per-account lock for each, and JSON-marshals the
	// result — pure waste on every successful request if nobody is listening.
	if !h.dashboardHub.hasSubscribers() {
		return
	}
	if snapshot := h.dashboardSnapshot(); snapshot != nil {
		h.dashboardHub.broadcast(snapshot)
	}
}

// dashboardPusher periodically broadcasts a fresh snapshot so the dashboard's
// purely-LIVE fields update in realtime, not just on request completion.
//
// The event-driven broadcasts (recordSuccess / recordFailure / account refresh)
// fire only AFTER a request finishes — by which point the failover dispatcher's
// deferred Release has already decremented the account's in-flight count. So a
// rising `inflight` (incremented at request START, in pick→reserveLocked) was
// never pushed while requests were actually in flight, and the same was true
// for the cooldown countdown, paced rate, and AIMD limit — all of which only
// move between completion events. A single shared ~1s tick covers every such
// live-only field at once.
//
// Cost: broadcastDashboardUpdate's hasSubscribers() fast-path makes each tick a
// single RLock+len check when no dashboard is connected (the common production
// case), so this is effectively free when nobody is watching. It coexists with
// the event-driven broadcasts (both are kept): completions still push instantly;
// this fills the gaps. Exactly one goroutine for the process lifetime, torn down
// via stopDashboardPusher in Stop().
func (h *Handler) dashboardPusher() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopDashboardPusher:
			return
		case <-ticker.C:
			h.broadcastDashboardUpdate()
		}
	}
}
