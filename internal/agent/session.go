package agent

import (
	"context"
	"sync"

	"github.com/cloudwego/eino/schema"
)

// Session holds per-session state including conversation history.
type Session struct {
	id       string
	messages []*schema.Message // full conversation history
	cancel   context.CancelFunc // current turn cancellation
	mu       sync.Mutex
}

// AppendMessages safely appends messages to the session history.
func (s *Session) AppendMessages(msgs ...*schema.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, msgs...)
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

// SessionStore is a thread-safe map of session IDs to Session objects.
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]*Session
}

// NewSessionStore creates a new empty session store.
func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: make(map[string]*Session)}
}

// Get retrieves a session by ID.
func (ss *SessionStore) Get(id string) (*Session, bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s, ok := ss.sessions[id]
	return s, ok
}

// Put stores a session by ID.
func (ss *SessionStore) Put(id string, s *Session) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.sessions[id] = s
}

// Delete removes a session by ID.
func (ss *SessionStore) Delete(id string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	delete(ss.sessions, id)
}
