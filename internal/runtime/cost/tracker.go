// Package cost calculates and tracks LLM API costs.
package cost

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Tracker persists cost data to SQLite.
type Tracker struct {
	db *sql.DB
}

// Record represents a single API call cost entry.
type Record struct {
	ID                  int64
	Timestamp           time.Time
	SessionID           string
	Model               string
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
	Cost                float64
}

// NewTracker creates or opens the cost tracking database at ~/.forge/costs.db
func NewTracker() (*Tracker, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("get home dir: %w", err)
	}

	forgeDir := filepath.Join(home, ".forge")
	if err := os.MkdirAll(forgeDir, 0755); err != nil {
		return nil, fmt.Errorf("create .forge dir: %w", err)
	}

	dbPath := filepath.Join(forgeDir, "costs.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Create table if not exists
	schema := `
	CREATE TABLE IF NOT EXISTS cost_records (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME NOT NULL,
		session_id TEXT NOT NULL,
		model TEXT NOT NULL,
		input_tokens INTEGER NOT NULL,
		output_tokens INTEGER NOT NULL,
		cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
		cache_read_tokens INTEGER NOT NULL DEFAULT 0,
		cost REAL NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_timestamp ON cost_records(timestamp);
	CREATE INDEX IF NOT EXISTS idx_session_id ON cost_records(session_id);
	`

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	return &Tracker{db: db}, nil
}

// Track records a single API call cost.
func (t *Tracker) Track(sessionID, model string, inputTokens, outputTokens, cacheCreation, cacheRead int, cost float64) error {
	query := `
	INSERT INTO cost_records 
		(timestamp, session_id, model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, cost)
	VALUES 
		(?, ?, ?, ?, ?, ?, ?, ?)
	`

	_, err := t.db.Exec(query, time.Now().UTC(), sessionID, model, inputTokens, outputTokens, cacheCreation, cacheRead, cost)
	if err != nil {
		return fmt.Errorf("insert record: %w", err)
	}

	return nil
}

// DailySummary aggregates costs by day.
type DailySummary struct {
	Date                time.Time
	TotalCost           float64
	SessionCount        int
	CallCount           int
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
}

// GetDailySummaries returns daily cost summaries for the given time range.
func (t *Tracker) GetDailySummaries(start, end time.Time) ([]DailySummary, error) {
	query := `
	SELECT 
		DATE(timestamp, 'localtime') as date,
		SUM(cost) as total_cost,
		COUNT(DISTINCT session_id) as session_count,
		COUNT(*) as call_count,
		SUM(input_tokens) as input_tokens,
		SUM(output_tokens) as output_tokens,
		SUM(cache_creation_tokens) as cache_creation_tokens,
		SUM(cache_read_tokens) as cache_read_tokens
	FROM cost_records
	WHERE timestamp >= ? AND timestamp < ?
	GROUP BY DATE(timestamp, 'localtime')
	ORDER BY date DESC
	`

	rows, err := t.db.Query(query, start, end)
	if err != nil {
		return nil, fmt.Errorf("query summaries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var summaries []DailySummary
	for rows.Next() {
		var s DailySummary
		var dateStr string
		if err := rows.Scan(&dateStr, &s.TotalCost, &s.SessionCount, &s.CallCount, &s.InputTokens, &s.OutputTokens, &s.CacheCreationTokens, &s.CacheReadTokens); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}

		s.Date, err = time.Parse("2006-01-02", dateStr)
		if err != nil {
			return nil, fmt.Errorf("parse date %s: %w", dateStr, err)
		}

		summaries = append(summaries, s)
	}

	return summaries, rows.Err()
}

// MonthlyTotal returns the total cost for a given month.
func (t *Tracker) MonthlyTotal(year int, month time.Month) (float64, error) {
	start := time.Date(year, month, 1, 0, 0, 0, 0, time.Local)
	end := start.AddDate(0, 1, 0)

	var total float64
	query := `SELECT COALESCE(SUM(cost), 0) FROM cost_records WHERE timestamp >= ? AND timestamp < ?`
	if err := t.db.QueryRow(query, start.UTC(), end.UTC()).Scan(&total); err != nil {
		return 0, fmt.Errorf("query monthly total: %w", err)
	}

	return total, nil
}

// SessionBreakdown returns per-session cost breakdown for a time range.
type SessionBreakdown struct {
	SessionID           string
	TotalCost           float64
	CallCount           int
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
	FirstCall           time.Time
	LastCall            time.Time
}

func (t *Tracker) GetSessionBreakdown(start, end time.Time) ([]SessionBreakdown, error) {
	query := `
	SELECT 
		session_id,
		SUM(cost) as total_cost,
		COUNT(*) as call_count,
		SUM(input_tokens) as input_tokens,
		SUM(output_tokens) as output_tokens,
		SUM(cache_creation_tokens) as cache_creation_tokens,
		SUM(cache_read_tokens) as cache_read_tokens,
		MIN(timestamp) as first_call,
		MAX(timestamp) as last_call
	FROM cost_records
	WHERE timestamp >= ? AND timestamp < ?
	GROUP BY session_id
	ORDER BY total_cost DESC
	`

	rows, err := t.db.Query(query, start, end)
	if err != nil {
		return nil, fmt.Errorf("query session breakdown: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var breakdowns []SessionBreakdown
	for rows.Next() {
		var b SessionBreakdown
		var firstCallStr, lastCallStr string
		if err := rows.Scan(&b.SessionID, &b.TotalCost, &b.CallCount, &b.InputTokens, &b.OutputTokens, &b.CacheCreationTokens, &b.CacheReadTokens, &firstCallStr, &lastCallStr); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}

		// Parse SQLite datetime strings (try multiple formats)
		b.FirstCall, err = parseTimestamp(firstCallStr)
		if err != nil {
			return nil, fmt.Errorf("parse first_call %s: %w", firstCallStr, err)
		}

		b.LastCall, err = parseTimestamp(lastCallStr)
		if err != nil {
			return nil, fmt.Errorf("parse last_call %s: %w", lastCallStr, err)
		}

		breakdowns = append(breakdowns, b)
	}

	return breakdowns, rows.Err()
}

// NewReadOnlyTracker opens the cost database in read-only mode.
// This avoids write contention with agent processes that own the DB.
func NewReadOnlyTracker(dbPath string) (*Tracker, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("cost db not found: %w", err)
	}
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open cost db (read-only): %w", err)
	}
	// Verify connection works
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping cost db: %w", err)
	}
	return &Tracker{db: db}, nil
}

// GetSessionCost returns cost breakdown for a single session.
func (t *Tracker) GetSessionCost(sessionID string) (*SessionBreakdown, error) {
	query := `
	SELECT 
		session_id,
		COALESCE(SUM(cost), 0) as total_cost,
		COUNT(*) as call_count,
		COALESCE(SUM(input_tokens), 0) as input_tokens,
		COALESCE(SUM(output_tokens), 0) as output_tokens,
		COALESCE(SUM(cache_creation_tokens), 0) as cache_creation_tokens,
		COALESCE(SUM(cache_read_tokens), 0) as cache_read_tokens,
		MIN(timestamp) as first_call,
		MAX(timestamp) as last_call
	FROM cost_records
	WHERE session_id = ?
	GROUP BY session_id
	`

	var b SessionBreakdown
	var firstCallStr, lastCallStr sql.NullString
	err := t.db.QueryRow(query, sessionID).Scan(
		&b.SessionID, &b.TotalCost, &b.CallCount,
		&b.InputTokens, &b.OutputTokens,
		&b.CacheCreationTokens, &b.CacheReadTokens,
		&firstCallStr, &lastCallStr,
	)
	if err == sql.ErrNoRows {
		return &SessionBreakdown{SessionID: sessionID}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query session cost: %w", err)
	}

	if firstCallStr.Valid {
		b.FirstCall, _ = parseTimestamp(firstCallStr.String)
	}
	if lastCallStr.Valid {
		b.LastCall, _ = parseTimestamp(lastCallStr.String)
	}

	return &b, nil
}

// Close closes the database connection.
func (t *Tracker) Close() error {
	if t.db != nil {
		return t.db.Close()
	}
	return nil
}

// parseTimestamp tries multiple timestamp formats that SQLite might use.
func parseTimestamp(s string) (time.Time, error) {
	formats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05.999999-07:00",
		time.RFC3339,
		time.RFC3339Nano,
	}

	for _, format := range formats {
		t, err := time.Parse(format, s)
		if err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("no matching time format for: %s", s)
}
