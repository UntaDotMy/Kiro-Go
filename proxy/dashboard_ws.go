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
	"crypto/subtle"
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

// handleDashboardWS upgrades an admin dashboard WebSocket and starts a
// goroutine that pumps hub broadcasts to the client. Heartbeat ping every
// 30s; missed pong within 60s closes the connection.
func (h *Handler) handleDashboardWS(w http.ResponseWriter, r *http.Request) {
	// Extract password from Sec-WebSocket-Protocol. The header is a
	// comma-separated list per RFC 6455; we accept any element with the
	// expected prefix.
	var password string
	var matchedProto string
	for _, v := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, raw := range strings.Split(v, ",") {
			tok := strings.TrimSpace(raw)
			if strings.HasPrefix(tok, dashboardWSAuthSubprotocolPrefix) {
				password = strings.TrimPrefix(tok, dashboardWSAuthSubprotocolPrefix)
				matchedProto = tok
				break
			}
		}
		if password != "" {
			break
		}
	}
	if subtle.ConstantTimeCompare([]byte(password), []byte(config.GetPassword())) != 1 {
		// Don't reveal whether the issue was missing protocol vs wrong
		// password — both surface as 401 to the upgrade attempt.
		w.WriteHeader(401)
		return
	}

	// Echo the matched subprotocol back so the browser handshake
	// completes. Without this, browsers reject the upgrade.
	upgrader := dashboardWsUpgrader
	upgrader.Subprotocols = []string{matchedProto}
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
	if snapshot := h.dashboardSnapshot(); snapshot != nil {
		h.dashboardHub.broadcast(snapshot)
	}
}
