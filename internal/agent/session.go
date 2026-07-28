package agent

import (
	"context"
	"sync"

	"github.com/cloudwego/eino/schema"
)

// Session holds per-session runtime state.
// Messages are persisted to SQLite via the Store after every mutation.
type Session struct {
	ID       string
	mu       sync.Mutex
	messages []*schema.Message
	cancel   context.CancelFunc
	store    *Store
}

// AppendMessages appends messages and persists to the database.
func (s *Session) AppendMessages(msgs ...*schema.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, msgs...)
	// Write-through: persist immediately
	if s.store != nil {
		_ = s.store.SaveMessages(s.ID, s.messages)
	}
}

// Messages returns a copy of the session's message history.
func (s *Session) Messages() []*schema.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]*schema.Message, len(s.messages))
	copy(cp, s.messages)
	return cp
}

// SetCancel stores the cancel function for the current turn.
func (s *Session) SetCancel(cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel = cancel
}

// Cancel calls the stored cancel function (if any).
func (s *Session) Cancel() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// SessionManager manages the in-memory session map backed by a SQLite Store.
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	store    *Store
}

// NewSessionManager creates a new session manager.
func NewSessionManager(store *Store) *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*Session),
		store:    store,
	}
}

// Create creates a new session, persists it to the DB, and adds the initial messages.
func (sm *SessionManager) Create(id string, initialMessages ...*schema.Message) (*Session, error) {
	if err := sm.store.CreateSession(id); err != nil {
		return nil, err
	}

	s := &Session{
		ID:       id,
		messages: initialMessages,
		store:    sm.store,
	}

	// Persist initial messages
	if len(initialMessages) > 0 {
		_ = sm.store.SaveMessages(id, initialMessages)
	}

	sm.mu.Lock()
	sm.sessions[id] = s
	sm.mu.Unlock()
	return s, nil
}

// Get retrieves an active session from memory.
func (sm *SessionManager) Get(id string) (*Session, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s, ok := sm.sessions[id]
	return s, ok
}

// Load restores a session from the DB and brings it into memory.
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

// Delete removes a session from memory and DB.
func (sm *SessionManager) Delete(id string) {
	sm.mu.Lock()
	delete(sm.sessions, id)
	sm.mu.Unlock()
	_ = sm.store.DeleteSession(id)
}

// List returns metadata for all persisted sessions.
func (sm *SessionManager) List() ([]SessionMeta, error) {
	return sm.store.ListSessions()
}

// Exists checks if a session exists in the DB.
func (sm *SessionManager) Exists(id string) (bool, error) {
	return sm.store.SessionExists(id)
}
