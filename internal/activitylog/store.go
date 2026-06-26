// Package activitylog provides SQLite-backed persistent storage for activity log entries.
// Uses a pure-Go SQLite implementation (modernc.org/sqlite) to avoid CGO.
package activitylog

import (
	"database/sql"
	"fmt"
	"sync"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// Entry is the persistent representation of an activity log entry.
type Entry struct {
	ID              int     `json:"id"`
	Timestamp       string  `json:"timestamp"`
	Model           string  `json:"model"`
	ReqPath         string  `json:"reqPath"`
	RespContentType string  `json:"respContentType"`
	RespStatusCode  int     `json:"respStatusCode"`
	CachedTokens    int     `json:"cachedTokens"`
	InputTokens     int     `json:"inputTokens"`
	OutputTokens    int     `json:"outputTokens"`
	PromptPerSecond float64 `json:"promptPerSecond"`
	TokensPerSecond float64 `json:"tokensPerSecond"`
	SpeedApprox     bool    `json:"speedApprox"`
	DurationMs      int     `json:"durationMs"`
}

// Store provides persistent storage for activity log entries.
type Store struct {
	db   *sql.DB
	mu   sync.Mutex
	path string
}

// New opens (or creates) the SQLite database and returns a Store.
// Returns nil, nil if dbPath is empty (no persistence).
func New(dbPath string) (*Store, error) {
	if dbPath == "" {
		return nil, nil
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db %s: %w", dbPath, err)
	}

	// Enable WAL mode for better concurrent read/write performance.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}

	// Set WAL auto-checkpoint to 1000 frames to prevent unbounded WAL growth.
	if _, err := db.Exec("PRAGMA wal_autocheckpoint=1000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL autocheckpoint: %w", err)
	}

	s := &Store{db: db, path: dbPath}

	if err := s.createTable(); err != nil {
		db.Close()
		return nil, err
	}

	return s, nil
}

// createTable creates the activity table if it doesn't exist.
func (s *Store) createTable() error {
	query := `
	CREATE TABLE IF NOT EXISTS activity (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp         TEXT    NOT NULL,
		model             TEXT    NOT NULL,
		req_path          TEXT    NOT NULL,
		resp_content_type TEXT    NOT NULL,
		resp_status_code  INTEGER NOT NULL DEFAULT 0,
		cached_tokens     INTEGER NOT NULL DEFAULT 0,
		input_tokens      INTEGER NOT NULL DEFAULT 0,
		output_tokens     INTEGER NOT NULL DEFAULT 0,
		prompt_per_second REAL    NOT NULL DEFAULT -1,
		tokens_per_second REAL    NOT NULL DEFAULT -1,
		speed_approx      INTEGER NOT NULL DEFAULT 0,
		duration_ms       INTEGER NOT NULL DEFAULT 0
	);`
	_, err := s.db.Exec(query)
	return err
}

// Insert adds a single entry to the database. Returns the assigned row ID.
func (s *Store) Insert(e *Entry) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var id int
	err := s.db.QueryRow(`
		INSERT INTO activity (
			timestamp, model, req_path, resp_content_type, resp_status_code,
			cached_tokens, input_tokens, output_tokens,
			prompt_per_second, tokens_per_second, speed_approx, duration_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		e.Timestamp, e.Model, e.ReqPath, e.RespContentType, e.RespStatusCode,
		e.CachedTokens, e.InputTokens, e.OutputTokens,
		e.PromptPerSecond, e.TokensPerSecond, boolToInt(e.SpeedApprox), e.DurationMs,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert activity entry: %w", err)
	}
	return id, nil
}

// Query returns paginated entries, ordered newest-first.
// offset is the number of entries to skip, limit is the max to return.
func (s *Store) Query(offset, limit int) ([]Entry, error) {
	if offset < 0 {
		offset = 0
	}
	if limit < 1 {
		limit = 50
	}

	rows, err := s.db.Query(`
		SELECT id, timestamp, model, req_path, resp_content_type,
		       resp_status_code, cached_tokens, input_tokens, output_tokens,
		       prompt_per_second, tokens_per_second, speed_approx, duration_ms
		FROM activity
		ORDER BY id DESC
		LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query activity entries: %w", err)
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var e Entry
		var speedApprox int
		err := rows.Scan(
			&e.ID, &e.Timestamp, &e.Model, &e.ReqPath, &e.RespContentType,
			&e.RespStatusCode, &e.CachedTokens, &e.InputTokens, &e.OutputTokens,
			&e.PromptPerSecond, &e.TokensPerSecond, &speedApprox, &e.DurationMs,
		)
		e.SpeedApprox = speedApprox != 0
		if err != nil {
			return nil, fmt.Errorf("scan activity row: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate activity rows: %w", err)
	}

	if entries == nil {
		entries = []Entry{}
	}
	return entries, nil
}

// Count returns the total number of stored entries.
func (s *Store) Count() (int, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM activity").Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count activity entries: %w", err)
	}
	return count, nil
}

// DeleteAll removes all entries from the database.
// Returns the number of rows deleted.
func (s *Store) DeleteAll() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec("DELETE FROM activity")
	if err != nil {
		return 0, fmt.Errorf("delete all activity entries: %w", err)
	}

	affected, _ := result.RowsAffected()
	return int(affected), nil
}

// DeleteOldest removes entries from the database, keeping only the most recent
// keep entries. Returns the number of rows deleted.
func (s *Store) DeleteOldest(keep int) (int, error) {
	if keep < 0 {
		keep = 0
	}

	// Keep=0 means delete everything.
	if keep == 0 {
		return s.DeleteAll()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Get total count first
	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM activity").Scan(&total); err != nil {
		return 0, fmt.Errorf("count activity entries: %w", err)
	}

	deleteCount := total - keep
	if deleteCount <= 0 {
		return 0, nil
	}

	// Find the cutoff ID: the oldest entry we want to keep.
	var cutoffID int
	if err := s.db.QueryRow(
		"SELECT id FROM activity ORDER BY id DESC LIMIT 1 OFFSET ?", keep,
	).Scan(&cutoffID); err != nil {
		return 0, fmt.Errorf("find cutoff ID: %w", err)
	}

	result, err := s.db.Exec("DELETE FROM activity WHERE id <= ?", cutoffID)
	if err != nil {
		return 0, fmt.Errorf("delete oldest activity entries: %w", err)
	}

	affected, _ := result.RowsAffected()
	return int(affected), nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// boolToInt converts a bool to 1 or 0 for SQLite storage.
func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
