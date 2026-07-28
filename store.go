package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cloudwego/eino/schema"
	_ "modernc.org/sqlite"
)

// SessionMeta is lightweight session metadata (no message content).
type SessionMeta struct {
	ID        string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store persists sessions to SQLite.
type Store struct {
	db *sql.DB
}

// NewStore opens (or creates) the SQLite database and ensures the schema exists.
func NewStore(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// WAL mode for concurrent reads
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}

	if err := initSchema(db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

func initSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			messages TEXT NOT NULL DEFAULT '[]',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)
	`)
	if err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	return nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// CreateSession inserts a new session record.
func (s *Store) CreateSession(id string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(
		"INSERT INTO sessions (id, messages, created_at, updated_at) VALUES (?, '[]', ?, ?)",
		id, now, now,
	)
	return err
}

// DeleteSession removes a session from the database.
func (s *Store) DeleteSession(id string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE id = ?", id)
	return err
}

// ListSessions returns metadata for all sessions, newest first.
func (s *Store) ListSessions() ([]SessionMeta, error) {
	rows, err := s.db.Query("SELECT id, created_at, updated_at FROM sessions ORDER BY updated_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionMeta
	for rows.Next() {
		var m SessionMeta
		var ca, ua int64
		if err := rows.Scan(&m.ID, &ca, &ua); err != nil {
			return nil, err
		}
		m.CreatedAt = time.Unix(ca, 0)
		m.UpdatedAt = time.Unix(ua, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

// SessionExists checks whether a session ID exists in the database.
func (s *Store) SessionExists(id string) (bool, error) {
	var exists bool
	err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM sessions WHERE id = ?)", id).Scan(&exists)
	return exists, err
}

// LoadMessages reads the message history for a session.
func (s *Store) LoadMessages(id string) ([]*schema.Message, error) {
	var raw string
	if err := s.db.QueryRow("SELECT messages FROM sessions WHERE id = ?", id).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("session %s not found", id)
		}
		return nil, err
	}
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	var msgs []*schema.Message
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		return nil, fmt.Errorf("unmarshal messages for session %s: %w", id, err)
	}
	return msgs, nil
}

// SaveMessages persists the message history and bumps updated_at.
func (s *Store) SaveMessages(id string, msgs []*schema.Message) error {
	data, err := json.Marshal(msgs)
	if err != nil {
		return fmt.Errorf("marshal messages: %w", err)
	}
	now := time.Now().Unix()
	_, err = s.db.Exec(
		"UPDATE sessions SET messages = ?, updated_at = ? WHERE id = ?",
		string(data), now, id,
	)
	return err
}
