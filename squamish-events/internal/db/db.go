package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/tbarnesjr/squamish-events/internal/models"
	_ "modernc.org/sqlite"
)

// DB wraps *sql.DB for SQLite access.
type DB struct {
	db *sql.DB
}

// EventFilter controls which events are returned by ListEvents.
type EventFilter struct {
	Search   string
	Category string
	Source   string // source name
	From     *time.Time
	To       *time.Time
	Limit    int
	Offset   int
}

// New opens a SQLite database at dbPath with WAL mode and busy timeout,
// creates tables and indexes, and returns a ready-to-use DB.
func New(dbPath string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// Enable WAL mode and set busy timeout.
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
	}
	for _, p := range pragmas {
		if _, err := sqlDB.Exec(p); err != nil {
			sqlDB.Close()
			return nil, fmt.Errorf("exec %q: %w", p, err)
		}
	}

	if err := createTables(sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("create tables: %w", err)
	}

	return &DB{db: sqlDB}, nil
}

func createTables(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS sources (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			type TEXT NOT NULL,
			url TEXT NOT NULL,
			scrape_interval_seconds INTEGER NOT NULL DEFAULT 3600,
			last_scraped_at DATETIME,
			enabled BOOLEAN NOT NULL DEFAULT 1,
			config TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE TABLE IF NOT EXISTS events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id INTEGER NOT NULL REFERENCES sources(id),
			external_id TEXT NOT NULL,
			title TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			location TEXT NOT NULL DEFAULT '',
			url TEXT NOT NULL DEFAULT '',
			start_time DATETIME NOT NULL,
			end_time DATETIME,
			category TEXT NOT NULL DEFAULT '',
			image_url TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(source_id, external_id)
		)`,
		`CREATE TABLE IF NOT EXISTS submissions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			title TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			location TEXT NOT NULL DEFAULT '',
			url TEXT NOT NULL DEFAULT '',
			start_time DATETIME NOT NULL,
			end_time DATETIME,
			category TEXT NOT NULL DEFAULT '',
			contact_email TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_start_time ON events(start_time)`,
		`CREATE INDEX IF NOT EXISTS idx_events_category ON events(category)`,
		`CREATE INDEX IF NOT EXISTS idx_events_source_id ON events(source_id)`,
		`CREATE INDEX IF NOT EXISTS idx_submissions_status ON submissions(status)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("exec %q: %w", stmt[:40], err)
		}
	}
	return nil
}

// Close closes the underlying database connection.
func (d *DB) Close() error {
	return d.db.Close()
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// UpsertEvent inserts or updates an event keyed by (source_id, external_id).
func (d *DB) UpsertEvent(ctx context.Context, event models.Event) error {
	query := `
		INSERT INTO events (source_id, external_id, title, description, location, url, start_time, end_time, category, image_url, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(source_id, external_id) DO UPDATE SET
			title = excluded.title,
			description = excluded.description,
			location = excluded.location,
			url = excluded.url,
			start_time = excluded.start_time,
			end_time = excluded.end_time,
			category = excluded.category,
			image_url = excluded.image_url,
			updated_at = CURRENT_TIMESTAMP
	`
	_, err := d.db.ExecContext(ctx, query,
		event.SourceID,
		event.ExternalID,
		event.Title,
		event.Description,
		event.Location,
		event.URL,
		event.StartTime.UTC(),
		nullTime(event.EndTime),
		event.Category,
		event.ImageURL,
	)
	if err != nil {
		return fmt.Errorf("upsert event: %w", err)
	}
	return nil
}

// ListEvents returns events matching the given filter. Results are joined with
// the sources table to populate SourceName and SourceType. Events default to
// those starting from now unless From is specified. Default limit is 100.
func (d *DB) ListEvents(ctx context.Context, filter EventFilter) ([]models.Event, error) {
	var (
		where []string
		args  []any
	)

	if filter.Search != "" {
		where = append(where, "(e.title LIKE ? OR e.description LIKE ?)")
		pat := "%" + filter.Search + "%"
		args = append(args, pat, pat)
	}
	if filter.Category != "" {
		where = append(where, "e.category = ?")
		args = append(args, filter.Category)
	}
	if filter.Source != "" {
		where = append(where, "s.name = ?")
		args = append(args, filter.Source)
	}
	if filter.From != nil {
		where = append(where, "e.start_time >= ?")
		args = append(args, filter.From.UTC())
	} else {
		where = append(where, "e.start_time >= ?")
		args = append(args, time.Now().UTC())
	}
	if filter.To != nil {
		where = append(where, "e.start_time <= ?")
		args = append(args, filter.To.UTC())
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}

	query := `
		SELECT e.id, e.source_id, e.external_id, e.title, e.description, e.location,
		       e.url, e.start_time, e.end_time, e.category, e.image_url,
		       e.created_at, e.updated_at, s.name, s.type
		FROM events e
		JOIN sources s ON s.id = e.source_id
	`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY e.start_time ASC LIMIT ? OFFSET ?"
	args = append(args, limit, filter.Offset)

	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var events []models.Event
	for rows.Next() {
		var e models.Event
		var endTime sql.NullTime
		if err := rows.Scan(
			&e.ID, &e.SourceID, &e.ExternalID, &e.Title, &e.Description, &e.Location,
			&e.URL, &e.StartTime, &endTime, &e.Category, &e.ImageURL,
			&e.CreatedAt, &e.UpdatedAt, &e.SourceName, &e.SourceType,
		); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		if endTime.Valid {
			e.EndTime = endTime.Time
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// GetEvent returns a single event by ID, joined with its source.
func (d *DB) GetEvent(ctx context.Context, id int64) (*models.Event, error) {
	query := `
		SELECT e.id, e.source_id, e.external_id, e.title, e.description, e.location,
		       e.url, e.start_time, e.end_time, e.category, e.image_url,
		       e.created_at, e.updated_at, s.name, s.type
		FROM events e
		JOIN sources s ON s.id = e.source_id
		WHERE e.id = ?
	`
	var e models.Event
	var endTime sql.NullTime
	err := d.db.QueryRowContext(ctx, query, id).Scan(
		&e.ID, &e.SourceID, &e.ExternalID, &e.Title, &e.Description, &e.Location,
		&e.URL, &e.StartTime, &endTime, &e.Category, &e.ImageURL,
		&e.CreatedAt, &e.UpdatedAt, &e.SourceName, &e.SourceType,
	)
	if err != nil {
		return nil, fmt.Errorf("get event %d: %w", id, err)
	}
	if endTime.Valid {
		e.EndTime = endTime.Time
	}
	return &e, nil
}

// ---------------------------------------------------------------------------
// Sources
// ---------------------------------------------------------------------------

// ListSources returns all sources.
func (d *DB) ListSources(ctx context.Context) ([]models.Source, error) {
	query := `SELECT id, name, type, url, scrape_interval_seconds, last_scraped_at, enabled, config FROM sources`
	rows, err := d.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()

	var sources []models.Source
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		sources = append(sources, s)
	}
	return sources, rows.Err()
}

// GetSource returns a single source by ID.
func (d *DB) GetSource(ctx context.Context, id int64) (*models.Source, error) {
	query := `SELECT id, name, type, url, scrape_interval_seconds, last_scraped_at, enabled, config FROM sources WHERE id = ?`
	row := d.db.QueryRowContext(ctx, query, id)

	var s models.Source
	var intervalSecs int64
	var lastScraped sql.NullTime
	err := row.Scan(&s.ID, &s.Name, &s.Type, &s.URL, &intervalSecs, &lastScraped, &s.Enabled, &s.Config)
	if err != nil {
		return nil, fmt.Errorf("get source %d: %w", id, err)
	}
	s.ScrapeInterval = time.Duration(intervalSecs) * time.Second
	if lastScraped.Valid {
		s.LastScrapedAt = &lastScraped.Time
	}
	return &s, nil
}

// CreateSource inserts a new source and returns its ID.
func (d *DB) CreateSource(ctx context.Context, source models.Source) (int64, error) {
	query := `INSERT INTO sources (name, type, url, scrape_interval_seconds, last_scraped_at, enabled, config) VALUES (?, ?, ?, ?, ?, ?, ?)`
	result, err := d.db.ExecContext(ctx, query,
		source.Name,
		source.Type,
		source.URL,
		int64(source.ScrapeInterval.Seconds()),
		nullTimePtr(source.LastScrapedAt),
		source.Enabled,
		source.Config,
	)
	if err != nil {
		return 0, fmt.Errorf("create source: %w", err)
	}
	return result.LastInsertId()
}

// UpdateSourceLastScraped sets the last_scraped_at timestamp for a source.
func (d *DB) UpdateSourceLastScraped(ctx context.Context, sourceID int64, t time.Time) error {
	query := `UPDATE sources SET last_scraped_at = ? WHERE id = ?`
	_, err := d.db.ExecContext(ctx, query, t.UTC(), sourceID)
	if err != nil {
		return fmt.Errorf("update source last scraped: %w", err)
	}
	return nil
}

// GetEnabledSources returns all sources where enabled = 1.
func (d *DB) GetEnabledSources(ctx context.Context) ([]models.Source, error) {
	query := `SELECT id, name, type, url, scrape_interval_seconds, last_scraped_at, enabled, config FROM sources WHERE enabled = 1`
	rows, err := d.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("get enabled sources: %w", err)
	}
	defer rows.Close()

	var sources []models.Source
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		sources = append(sources, s)
	}
	return sources, rows.Err()
}

// ---------------------------------------------------------------------------
// Submissions
// ---------------------------------------------------------------------------

// CreateSubmission inserts a new submission and returns its ID.
func (d *DB) CreateSubmission(ctx context.Context, sub models.Submission) (int64, error) {
	query := `INSERT INTO submissions (title, description, location, url, start_time, end_time, category, contact_email, status) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	status := sub.Status
	if status == "" {
		status = "pending"
	}
	result, err := d.db.ExecContext(ctx, query,
		sub.Title,
		sub.Description,
		sub.Location,
		sub.URL,
		sub.StartTime.UTC(),
		nullTime(sub.EndTime),
		sub.Category,
		sub.ContactEmail,
		status,
	)
	if err != nil {
		return 0, fmt.Errorf("create submission: %w", err)
	}
	return result.LastInsertId()
}

// ListSubmissions returns submissions filtered by status. Pass empty string for all.
func (d *DB) ListSubmissions(ctx context.Context, status string) ([]models.Submission, error) {
	query := `SELECT id, title, description, location, url, start_time, end_time, category, contact_email, status, created_at, updated_at FROM submissions`
	var args []any
	if status != "" {
		query += " WHERE status = ?"
		args = append(args, status)
	}
	query += " ORDER BY created_at DESC"

	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list submissions: %w", err)
	}
	defer rows.Close()

	var subs []models.Submission
	for rows.Next() {
		var s models.Submission
		var endTime sql.NullTime
		if err := rows.Scan(
			&s.ID, &s.Title, &s.Description, &s.Location, &s.URL,
			&s.StartTime, &endTime, &s.Category, &s.ContactEmail,
			&s.Status, &s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan submission: %w", err)
		}
		if endTime.Valid {
			s.EndTime = endTime.Time
		}
		subs = append(subs, s)
	}
	return subs, rows.Err()
}

// UpdateSubmissionStatus updates the status and updated_at of a submission.
func (d *DB) UpdateSubmissionStatus(ctx context.Context, id int64, status string) error {
	query := `UPDATE submissions SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`
	_, err := d.db.ExecContext(ctx, query, status, id)
	if err != nil {
		return fmt.Errorf("update submission status: %w", err)
	}
	return nil
}

// GetSubmission returns a single submission by ID.
func (d *DB) GetSubmission(ctx context.Context, id int64) (*models.Submission, error) {
	query := `SELECT id, title, description, location, url, start_time, end_time, category, contact_email, status, created_at, updated_at FROM submissions WHERE id = ?`
	var s models.Submission
	var endTime sql.NullTime
	err := d.db.QueryRowContext(ctx, query, id).Scan(
		&s.ID, &s.Title, &s.Description, &s.Location, &s.URL,
		&s.StartTime, &endTime, &s.Category, &s.ContactEmail,
		&s.Status, &s.CreatedAt, &s.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("get submission %d: %w", id, err)
	}
	if endTime.Valid {
		s.EndTime = endTime.Time
	}
	return &s, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// scanner is satisfied by both *sql.Rows and *sql.Row.
type scanner interface {
	Scan(dest ...any) error
}

func scanSource(rows scanner) (models.Source, error) {
	var s models.Source
	var intervalSecs int64
	var lastScraped sql.NullTime
	err := rows.Scan(&s.ID, &s.Name, &s.Type, &s.URL, &intervalSecs, &lastScraped, &s.Enabled, &s.Config)
	if err != nil {
		return s, fmt.Errorf("scan source: %w", err)
	}
	s.ScrapeInterval = time.Duration(intervalSecs) * time.Second
	if lastScraped.Valid {
		s.LastScrapedAt = &lastScraped.Time
	}
	return s, nil
}

// nullTime returns a sql.NullTime. A zero time is treated as NULL.
func nullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t.UTC(), Valid: true}
}

// nullTimePtr returns nil (NULL) for a nil pointer, otherwise the UTC time.
func nullTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}
