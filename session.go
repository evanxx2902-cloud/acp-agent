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
// Session — per-session runtime state (cache backed by SQLite)
// =========================================================================

type Session struct {
	ID       string
	mu       sync.Mutex
	messages []*schema.Message // in-memory cache, next seq for new messages
	seq      int               // next message sequence number
	cancel   context.CancelFunc
	store    *Store
	mode     string // "agent" (default) or "plan"
	status   string // "active" or "closed"

	mcpManager *Manager
	cmAgent    *adk.ChatModelAgent
	dirty      bool // true if agent needs rebuild
}

func (s *Session) AppendMessages(msgs ...*schema.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, msg := range msgs {
		s.messages = append(s.messages, msg)
		if s.store != nil {
			_ = s.store.AppendMessage(s.ID, s.seq, msg)
			s.seq++
		}
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

func (s *Session) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Session) SetStatus(st string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = st
	if s.store != nil {
		_ = s.store.SetSessionStatus(s.ID, st)
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

func (sm *SessionManager) Create(id string, meta map[string]any, initialMessages ...*schema.Message) (*Session, error) {
	userID := ""
	username := ""
	businessType := ""
	businessID := ""
	if meta != nil {
		if v, ok := meta["user_id"].(string); ok {
			userID = v
		}
		if v, ok := meta["username"].(string); ok {
			username = v
		}
		if v, ok := meta["business_type"].(string); ok {
			businessType = v
		}
		if v, ok := meta["business_id"].(string); ok {
			businessID = v
		}
	}

	if err := sm.store.CreateSession(id, userID, username, businessType, businessID, meta); err != nil {
		return nil, err
	}

	s := &Session{
		ID:       id,
		messages: initialMessages,
		seq:      len(initialMessages),
		store:    sm.store,
		status:   "active",
	}

	for i, msg := range initialMessages {
		_ = sm.store.AppendMessage(id, i, msg)
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
	meta, err := sm.store.GetSession(id)
	if err != nil {
		return nil, err
	}

	msgs, err := sm.store.LoadMessages(id)
	if err != nil {
		return nil, err
	}

	s := &Session{
		ID:       id,
		messages: msgs,
		seq:      len(msgs),
		store:    sm.store,
		mode:     meta.Mode,
		status:   meta.Status,
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
	ID           string
	Status       string
	Mode         string
	UserID       string
	Username     string
	BusinessType string
	BusinessID   string
	Metadata     map[string]any
	CreatedAt    time.Time
	UpdatedAt    time.Time
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
			id            TEXT PRIMARY KEY,
			status        TEXT NOT NULL DEFAULT 'active',
			mode          TEXT NOT NULL DEFAULT 'agent',
			user_id       TEXT NOT NULL DEFAULT '',
			username      TEXT NOT NULL DEFAULT '',
			business_type TEXT NOT NULL DEFAULT '',
			business_id   TEXT NOT NULL DEFAULT '',
			metadata      TEXT NOT NULL DEFAULT '{}',
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL
		)
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create sessions table: %w", err)
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS session_messages (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id   TEXT NOT NULL REFERENCES sessions(id),
			seq          INTEGER NOT NULL,
			role         TEXT NOT NULL,
			content      TEXT NOT NULL DEFAULT '',
			tool_calls   TEXT DEFAULT NULL,
			tool_call_id TEXT DEFAULT NULL,
			tool_name    TEXT DEFAULT NULL,
			created_at   INTEGER NOT NULL,
			UNIQUE(session_id, seq)
		)
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create session_messages table: %w", err)
	}

	if _, err := db.Exec(
		"CREATE INDEX IF NOT EXISTS idx_msg_session_seq ON session_messages(session_id, seq)",
	); err != nil {
		db.Close()
		return nil, fmt.Errorf("create index: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// --- Sessions ---

func (s *Store) CreateSession(id, userID, username, businessType, businessID string, meta map[string]any) error {
	now := time.Now().Unix()
	metaJSON := "{}"
	if meta != nil {
		b, _ := json.Marshal(meta)
		metaJSON = string(b)
	}
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, status, mode, user_id, username, business_type, business_id, metadata, created_at, updated_at)
		 VALUES (?, 'active', 'agent', ?, ?, ?, ?, ?, ?, ?)`,
		id, userID, username, businessType, businessID, metaJSON, now, now,
	)
	return err
}

func (s *Store) GetSession(id string) (*SessionMeta, error) {
	var m SessionMeta
	var ca, ua int64
	var metaJSON string
	if err := s.db.QueryRow(
		"SELECT id, status, mode, user_id, username, business_type, business_id, metadata, created_at, updated_at FROM sessions WHERE id = ?",
		id,
	).Scan(&m.ID, &m.Status, &m.Mode, &m.UserID, &m.Username, &m.BusinessType, &m.BusinessID, &metaJSON, &ca, &ua); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("session %s not found", id)
		}
		return nil, err
	}
	json.Unmarshal([]byte(metaJSON), &m.Metadata)
	m.CreatedAt = time.Unix(ca, 0)
	m.UpdatedAt = time.Unix(ua, 0)
	return &m, nil
}

func (s *Store) SetSessionStatus(id, status string) error {
	_, err := s.db.Exec(
		"UPDATE sessions SET status = ?, updated_at = ? WHERE id = ?",
		status, time.Now().Unix(), id,
	)
	return err
}

func (s *Store) MarkActiveAsIdle() error {
	_, err := s.db.Exec(
		"UPDATE sessions SET status = 'idle', updated_at = ? WHERE status = 'active'",
		time.Now().Unix(),
	)
	return err
}

func (s *Store) DeleteSession(id string) error {
	tx, _ := s.db.Begin()
	if tx != nil {
		tx.Exec("DELETE FROM session_messages WHERE session_id = ?", id)
		tx.Exec("DELETE FROM sessions WHERE id = ?", id)
		tx.Commit()
		return nil
	}
	return fmt.Errorf("failed to begin transaction")
}

func (s *Store) ListSessions() ([]SessionMeta, error) {
	rows, err := s.db.Query(
		"SELECT id, status, mode, user_id, username, business_type, business_id, metadata, created_at, updated_at FROM sessions ORDER BY updated_at DESC",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionMeta
	for rows.Next() {
		var m SessionMeta
		var ca, ua int64
		var metaJSON string
		if err := rows.Scan(&m.ID, &m.Status, &m.Mode, &m.UserID, &m.Username, &m.BusinessType, &m.BusinessID, &metaJSON, &ca, &ua); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(metaJSON), &m.Metadata)
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

// --- Messages ---

func (s *Store) AppendMessage(sessionID string, seq int, msg *schema.Message) error {
	toolCallsJSON := "null"
	if len(msg.ToolCalls) > 0 {
		b, _ := json.Marshal(msg.ToolCalls)
		toolCallsJSON = string(b)
	}
	toolCallID := "null"
	if msg.ToolCallID != "" {
		b, _ := json.Marshal(msg.ToolCallID)
		toolCallID = string(b)
	}
	toolName := "null"
	if msg.ToolName != "" {
		b, _ := json.Marshal(msg.ToolName)
		toolName = string(b)
	}

	// SQLite doesn't understand Go's "null" string — use raw SQL with proper NULL
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO session_messages (session_id, seq, role, content, tool_calls, tool_call_id, tool_name, created_at)
		 VALUES (?1, ?2, ?3, ?4,
		         CASE WHEN ?5 = 'null' THEN NULL ELSE ?5 END,
		         CASE WHEN ?6 = 'null' THEN NULL ELSE ?6 END,
		         CASE WHEN ?7 = 'null' THEN NULL ELSE ?7 END,
		         ?8)`,
		sessionID, seq, string(msg.Role), msg.Content,
		toolCallsJSON, toolCallID, toolName, time.Now().Unix(),
	)
	return err
}

func (s *Store) LoadMessages(sessionID string) ([]*schema.Message, error) {
	rows, err := s.db.Query(
		"SELECT role, content, tool_calls, tool_call_id, tool_name FROM session_messages WHERE session_id = ? ORDER BY seq",
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []*schema.Message
	for rows.Next() {
		var role, content string
		var tcJSON, tci, tn sql.NullString
		rows.Scan(&role, &content, &tcJSON, &tci, &tn)

		msg := &schema.Message{
			Role:    schema.RoleType(role),
			Content: content,
		}

		if tcJSON.Valid && tcJSON.String != "" {
			json.Unmarshal([]byte(tcJSON.String), &msg.ToolCalls)
		}
		if tci.Valid && tci.String != "" {
			_ = json.Unmarshal([]byte(tci.String), &msg.ToolCallID)
		}
		if tn.Valid && tn.String != "" {
			_ = json.Unmarshal([]byte(tn.String), &msg.ToolName)
		}

		msgs = append(msgs, msg)
	}
	return msgs, rows.Err()
}
