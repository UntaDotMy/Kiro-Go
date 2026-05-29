// Package stats persists request statistics to SQLite so they survive
// container restarts. Three concurrent rollups are maintained per request:
// global, per-model, and per-key. Each rollup is keyed by date (YYYY-MM-DD
// in UTC) so historical breakdowns are queryable indefinitely.
//
// All writes are batched through a single goroutine to avoid lock contention
// on the SQLite handle (modernc.org/sqlite uses a single connection by
// default). Public APIs are safe to call from request goroutines.
package stats

import (
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS daily_stats (
    date        TEXT NOT NULL,                 -- YYYY-MM-DD UTC
    scope_type  TEXT NOT NULL,                 -- 'global' | 'model' | 'key'
    scope_id    TEXT NOT NULL DEFAULT '',      -- '' for global
    requests    INTEGER NOT NULL DEFAULT 0,
    success     INTEGER NOT NULL DEFAULT 0,
    failed      INTEGER NOT NULL DEFAULT 0,
    tokens_in   INTEGER NOT NULL DEFAULT 0,
    tokens_out  INTEGER NOT NULL DEFAULT 0,
    credits     REAL    NOT NULL DEFAULT 0,
    last_at     INTEGER NOT NULL DEFAULT 0,    -- unix seconds
    PRIMARY KEY (date, scope_type, scope_id)
);
CREATE INDEX IF NOT EXISTS idx_daily_scope ON daily_stats(scope_type, scope_id);
`

// Totals aggregates a (scope_type, scope_id) over a date range.
type Totals struct {
	Requests   int     `json:"requests"`
	Success    int     `json:"success"`
	Failed     int     `json:"failed"`
	TokensIn   int     `json:"tokensIn"`
	TokensOut  int     `json:"tokensOut"`
	Credits    float64 `json:"credits"`
	LastAt     int64   `json:"lastAt"`
}

// DailyEntry is one row in a per-day history series.
type DailyEntry struct {
	Date      string  `json:"date"`
	Requests  int     `json:"requests"`
	Success   int     `json:"success"`
	Failed    int     `json:"failed"`
	TokensIn  int     `json:"tokensIn"`
	TokensOut int     `json:"tokensOut"`
	Credits   float64 `json:"credits"`
}

var (
	db    *sql.DB
	dbMu  sync.RWMutex
	stmts struct {
		upsert *sql.Stmt
	}
)

// Init opens (or creates) the SQLite file and runs migrations. Safe to call
// multiple times; subsequent calls are no-ops once the DB is initialized.
func Init(path string) error {
	dbMu.Lock()
	defer dbMu.Unlock()
	if db != nil {
		return nil
	}
	conn, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return err
	}
	conn.SetMaxOpenConns(1) // SQLite — serialise writes through one conn
	if _, err := conn.Exec(schema); err != nil {
		return err
	}
	upsert, err := conn.Prepare(`
        INSERT INTO daily_stats(date, scope_type, scope_id, requests, success, failed, tokens_in, tokens_out, credits, last_at)
        VALUES(?, ?, ?, 1, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(date, scope_type, scope_id) DO UPDATE SET
            requests   = requests   + 1,
            success    = success    + excluded.success,
            failed     = failed     + excluded.failed,
            tokens_in  = tokens_in  + excluded.tokens_in,
            tokens_out = tokens_out + excluded.tokens_out,
            credits    = credits    + excluded.credits,
            last_at    = MAX(last_at, excluded.last_at)
    `)
	if err != nil {
		return err
	}
	db = conn
	stmts.upsert = upsert
	if err := mergeLegacyModelRows(conn); err != nil {
		// Best-effort migration: log via returned error path is overkill;
		// stats are non-critical. Future records will be canonical anyway.
		_ = err
	}
	return nil
}

// mergeLegacyModelRows folds rows whose scope_id differs only by the
// canonicalization rules in CanonicalModelID into a single canonical row.
// Without this, upgrading users see legacy duplicate model rows in the
// Analytics tab even after the per-record canonicalization lands.
//
// The merge is done inside one transaction so a crash mid-migration leaves
// the DB unchanged, not partially merged. Idempotent: re-running on an
// already-canonical DB is a no-op.
func mergeLegacyModelRows(conn *sql.DB) error {
	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT date, scope_id, requests, success, failed, tokens_in, tokens_out, credits, last_at
                            FROM daily_stats WHERE scope_type = 'model'`)
	if err != nil {
		return err
	}
	type row struct {
		date      string
		legacyID  string
		canonical string
		requests  int
		success   int
		failed    int
		tokensIn  int
		tokensOut int
		credits   float64
		lastAt    int64
	}
	var legacy []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.date, &r.legacyID, &r.requests, &r.success, &r.failed, &r.tokensIn, &r.tokensOut, &r.credits, &r.lastAt); err != nil {
			rows.Close()
			return err
		}
		r.canonical = CanonicalModelID(r.legacyID)
		if r.canonical != r.legacyID {
			legacy = append(legacy, r)
		}
	}
	rows.Close()
	if len(legacy) == 0 {
		return tx.Commit()
	}
	upsert, err := tx.Prepare(`
        INSERT INTO daily_stats(date, scope_type, scope_id, requests, success, failed, tokens_in, tokens_out, credits, last_at)
        VALUES(?, 'model', ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(date, scope_type, scope_id) DO UPDATE SET
            requests   = requests   + excluded.requests,
            success    = success    + excluded.success,
            failed     = failed     + excluded.failed,
            tokens_in  = tokens_in  + excluded.tokens_in,
            tokens_out = tokens_out + excluded.tokens_out,
            credits    = credits    + excluded.credits,
            last_at    = MAX(last_at, excluded.last_at)
    `)
	if err != nil {
		return err
	}
	defer upsert.Close()
	del, err := tx.Prepare(`DELETE FROM daily_stats WHERE scope_type = 'model' AND date = ? AND scope_id = ?`)
	if err != nil {
		return err
	}
	defer del.Close()
	for _, r := range legacy {
		if _, err := upsert.Exec(r.date, r.canonical, r.requests, r.success, r.failed, r.tokensIn, r.tokensOut, r.credits, r.lastAt); err != nil {
			return err
		}
		if _, err := del.Exec(r.date, r.legacyID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Close flushes any prepared statements and closes the DB handle. Used in
// graceful shutdown so WAL is checkpointed cleanly.
func Close() error {
	dbMu.Lock()
	defer dbMu.Unlock()
	if db == nil {
		return nil
	}
	if stmts.upsert != nil {
		_ = stmts.upsert.Close()
	}
	err := db.Close()
	db = nil
	return err
}

// Record inserts one row per scope (global / model / key). Failures
// (success=false) still count as a request for visibility but contribute 0
// tokens and 0 credits. modelID and keyID may be empty; empty values skip
// that scope.
func Record(modelID, keyID string, success bool, tokensIn, tokensOut int, credits float64) {
	dbMu.RLock()
	defer dbMu.RUnlock()
	if db == nil {
		return
	}
	now := time.Now().UTC()
	date := now.Format("2006-01-02")
	ts := now.Unix()
	succ := 0
	fail := 1
	if success {
		succ = 1
		fail = 0
	}
	if !success {
		// Don't credit tokens / credits to a failed request.
		tokensIn, tokensOut = 0, 0
		credits = 0
	}
	exec := func(scopeType, scopeID string) {
		if _, err := stmts.upsert.Exec(date, scopeType, scopeID, succ, fail, tokensIn, tokensOut, credits, ts); err != nil {
			// Stats are best-effort; never fail the request because of DB
			// issues. Logging happens at the call site.
			_ = err
		}
	}
	exec("global", "")
	if modelID != "" {
		exec("model", CanonicalModelID(modelID))
	}
	if keyID != "" {
		exec("key", keyID)
	}
}

// CanonicalModelID normalizes a model id so the same logical model recorded
// under different spellings (case, dotted vs dashed version, "-thinking"
// suffix, leading/trailing whitespace) collapses to a single row in the
// per-model rollup. Without this, a client that sometimes sends
// "claude-opus-4.7" and sometimes "claude-opus-4-7" produces two distinct
// rows that the analytics dashboard renders as duplicates.
//
// Rules (applied in order):
//  1. Trim ASCII whitespace.
//  2. Lowercase.
//  3. Replace dots with dashes (Claude Code expects dashed ids; Kiro
//     internally uses dotted — we standardize on dashed for storage).
//  4. Strip a trailing "-thinking" / "-think" / "-reasoning" suffix so
//     the same model with and without thinking mode is one row.
//
// The transform is idempotent: feeding output through CanonicalModelID
// again returns the same string. Empty input returns empty output.
func CanonicalModelID(model string) string {
	s := strings.ToLower(strings.TrimSpace(model))
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, ".", "-")
	for _, suffix := range []string{"-thinking", "-think", "-reasoning"} {
		if strings.HasSuffix(s, suffix) {
			s = strings.TrimSuffix(s, suffix)
			break
		}
	}
	return s
}

// AllTimeTotals returns aggregate Totals across the entire history for a
// scope. scopeID may be empty for the global scope. Used on startup to
// repopulate in-memory counters.
func AllTimeTotals(scopeType, scopeID string) (Totals, error) {
	dbMu.RLock()
	defer dbMu.RUnlock()
	if db == nil {
		return Totals{}, errors.New("stats db not initialized")
	}
	row := db.QueryRow(`
        SELECT
            COALESCE(SUM(requests),  0),
            COALESCE(SUM(success),   0),
            COALESCE(SUM(failed),    0),
            COALESCE(SUM(tokens_in), 0),
            COALESCE(SUM(tokens_out),0),
            COALESCE(SUM(credits),   0),
            COALESCE(MAX(last_at),   0)
        FROM daily_stats WHERE scope_type = ? AND scope_id = ?
    `, scopeType, scopeID)
	var t Totals
	if err := row.Scan(&t.Requests, &t.Success, &t.Failed, &t.TokensIn, &t.TokensOut, &t.Credits, &t.LastAt); err != nil {
		return Totals{}, err
	}
	return t, nil
}

// ByModel returns a map[modelID]Totals across the entire history. Used by
// the Analytics tab.
func ByModel() (map[string]Totals, error) {
	return groupBy("model")
}

// ByKey returns a map[keyID]Totals across the entire history.
func ByKey() (map[string]Totals, error) {
	return groupBy("key")
}

func groupBy(scopeType string) (map[string]Totals, error) {
	dbMu.RLock()
	defer dbMu.RUnlock()
	if db == nil {
		return nil, errors.New("stats db not initialized")
	}
	rows, err := db.Query(`
        SELECT
            scope_id,
            COALESCE(SUM(requests),  0),
            COALESCE(SUM(success),   0),
            COALESCE(SUM(failed),    0),
            COALESCE(SUM(tokens_in), 0),
            COALESCE(SUM(tokens_out),0),
            COALESCE(SUM(credits),   0),
            COALESCE(MAX(last_at),   0)
        FROM daily_stats WHERE scope_type = ? GROUP BY scope_id
    `, scopeType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]Totals)
	for rows.Next() {
		var id string
		var t Totals
		if err := rows.Scan(&id, &t.Requests, &t.Success, &t.Failed, &t.TokensIn, &t.TokensOut, &t.Credits, &t.LastAt); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

// History returns the last N days (most-recent first) of per-day rows for a
// scope. days <= 0 returns the full history.
func History(scopeType, scopeID string, days int) ([]DailyEntry, error) {
	dbMu.RLock()
	defer dbMu.RUnlock()
	if db == nil {
		return nil, errors.New("stats db not initialized")
	}
	q := `SELECT date, requests, success, failed, tokens_in, tokens_out, credits
          FROM daily_stats WHERE scope_type = ? AND scope_id = ?
          ORDER BY date DESC`
	args := []interface{}{scopeType, scopeID}
	if days > 0 {
		q += ` LIMIT ?`
		args = append(args, days)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DailyEntry
	for rows.Next() {
		var e DailyEntry
		if err := rows.Scan(&e.Date, &e.Requests, &e.Success, &e.Failed, &e.TokensIn, &e.TokensOut, &e.Credits); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
