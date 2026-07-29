package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	_ "modernc.org/sqlite"
)

// =========================================================================
// Session — per-session runtime state
// =========================================================================

type Session struct {
	ID       string
	mu       sync.Mutex
	messages []*schema.Message
	cancel   context.CancelFunc
	store    *Store
	mode     string // "agent" (default) or "plan"

	mcpManager *Manager
	cmAgent    *adk.ChatModelAgent
	dirty      bool // true if agent needs rebuild (mode or maxIter changed)
}

func (s *Session) AppendMessages(msgs ...*schema.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, msgs...)
	if s.store != nil {
		_ = s.store.SaveMessages(s.ID, s.messages)
	}
}

func (s *Session) Messages() []*schema.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]*schema.Message, len(s.messages))
	copy(cp, s.messages)
	return cp
}

func (s *Session) SetCancel(cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel = cancel
}

func (s *Session) Cancel() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Session) SetMCAgent(cmAgent *adk.ChatModelAgent, mgr *Manager) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cmAgent = cmAgent
	s.mcpManager = mgr
	s.dirty = false
}

func (s *Session) RebuildAgent(cmAgent *adk.ChatModelAgent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cmAgent = cmAgent
	s.dirty = false
}

func (s *Session) GetAgent() *adk.ChatModelAgent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmAgent
}

func (s *Session) SetMode(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = mode
}

func (s *Session) GetMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode == "" {
		return "agent"
	}
	return s.mode
}

func (s *Session) SetDirty() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirty = true
}

func (s *Session) IsDirty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dirty
}

func (s *Session) CloseMCP() {
	s.mu.Lock()
	mgr := s.mcpManager
	s.mcpManager = nil
	s.mu.Unlock()
	if mgr != nil {
		mgr.Close()
	}
}

// =========================================================================
// SessionManager — in-memory map backed by SQLite Store
// =========================================================================

type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	store    *Store
}

func NewSessionManager(store *Store) *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*Session),
		store:    store,
	}
}

func (sm *SessionManager) Create(id string, initialMessages ...*schema.Message) (*Session, error) {
	if err := sm.store.CreateSession(id); err != nil {
		return nil, err
	}

	s := &Session{
		ID:       id,
		messages: initialMessages,
		store:    sm.store,
	}

	if len(initialMessages) > 0 {
		_ = sm.store.SaveMessages(id, initialMessages)
	}

	sm.mu.Lock()
	sm.sessions[id] = s
	sm.mu.Unlock()
	return s, nil
}

func (sm *SessionManager) Get(id string) (*Session, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s, ok := sm.sessions[id]
	return s, ok
}

func (sm *SessionManager) Load(id string) (*Session, error) {
	msgs, err := sm.store.LoadMessages(id)
	if err != nil {
		return nil, err
	}

	s := &Session{
		ID:       id,
		messages: msgs,
		store:    sm.store,
	}

	sm.mu.Lock()
	sm.sessions[id] = s
	sm.mu.Unlock()
	return s, nil
}

func (sm *SessionManager) Delete(id string) {
	sm.mu.Lock()
	delete(sm.sessions, id)
	sm.mu.Unlock()
	_ = sm.store.DeleteSession(id)
}

func (sm *SessionManager) List() ([]SessionMeta, error) {
	return sm.store.ListSessions()
}

func (sm *SessionManager) Exists(id string) (bool, error) {
	return sm.store.SessionExists(id)
}

// =========================================================================
// Store — SQLite persistence
// =========================================================================

type SessionMeta struct {
	ID        string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Store struct {
	db *sql.DB
}

func NewStore(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			messages TEXT NOT NULL DEFAULT '[]',
			tasks TEXT NOT NULL DEFAULT '{}',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	// Migration: add tasks column to existing databases
	db.Exec("ALTER TABLE sessions ADD COLUMN tasks TEXT NOT NULL DEFAULT '{}'")

	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateSession(id string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(
		"INSERT INTO sessions (id, messages, tasks, created_at, updated_at) VALUES (?, '[]', '{}', ?, ?)",
		id, now, now,
	)
	return err
}

func (s *Store) DeleteSession(id string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE id = ?", id)
	return err
}

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

func (s *Store) SessionExists(id string) (bool, error) {
	var exists bool
	err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM sessions WHERE id = ?)", id).Scan(&exists)
	return exists, err
}

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

func (s *Store) SaveTasks(id string, tasks map[string]string) error {
	data, err := json.Marshal(tasks)
	if err != nil {
		return fmt.Errorf("marshal tasks: %w", err)
	}
	_, err = s.db.Exec("UPDATE sessions SET tasks = ?, updated_at = ? WHERE id = ?",
		string(data), time.Now().Unix(), id)
	return err
}

func (s *Store) LoadTasks(id string) (map[string]string, error) {
	var raw string
	if err := s.db.QueryRow("SELECT tasks FROM sessions WHERE id = ?", id).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("session %s not found", id)
		}
		return nil, err
	}
	if raw == "" || raw == "{}" {
		return nil, nil
	}
	var tasks map[string]string
	if err := json.Unmarshal([]byte(raw), &tasks); err != nil {
		return nil, fmt.Errorf("unmarshal tasks: %w", err)
	}
	return tasks, nil
}

