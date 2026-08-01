package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"acp/ent"
	entSchema "acp/ent/schema"
	entSession "acp/ent/session"
	entMessage "acp/ent/sessionmessage"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
)

// =========================================================================
// RuntimeSession — per-session runtime state (cache backed by Ent/SQLite)
// =========================================================================

// NewSessionParams holds all parameters for creating a session.
type NewSessionParams struct {
	UserID                    int64
	Username                  string
	BusinessID                string
	BusinessType              string
	BusinessMeta              map[string]any
	Mode                      string
	HeartbeatInterval         int
	SummarizationTriggerRatio float64
	MaxIterations             int
	SystemPrompt              string
	OwnerAgent                string // connection identifier for ownership
}

// SessionMeta is a lightweight public view of a session.
type SessionMeta struct {
	ID           string
	Status       string
	Mode         string
	UserID       int64
	Username     string
	BusinessID   string
	BusinessType string
	BusinessMeta map[string]any
	MessageCount int
	CreateTime   time.Time
	UpdateTime   time.Time
}

type RuntimeSession struct {
	ID                        string
	Mode                      string
	Summary                   string
	HeartbeatInterval         int
	SummarizationTriggerRatio float64
	MaxIterations             int
	BusinessMeta              map[string]any

	mu             sync.Mutex
	messages       []*schema.Message
	seq            int
	cancel         context.CancelFunc
	ctx            context.Context
	mcpManager     *Manager
	cmAgent        *adk.ChatModelAgent
	heartbeatTimer *time.Timer
	lockedBy       string
	lockedAt       time.Time
	status         string
	promptMu       sync.Mutex // prevents concurrent prompts on same session
	ownerAgent     string     // connection that currently owns this session
}

func (s *RuntimeSession) AppendMessages(msgs ...*schema.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, msgs...)
	s.seq += len(msgs)
}

func (s *RuntimeSession) Messages() []*schema.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]*schema.Message, len(s.messages))
	copy(cp, s.messages)
	return cp
}

func (s *RuntimeSession) SetCancel(cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel = cancel
}

func (s *RuntimeSession) Cancel() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *RuntimeSession) SetMCAgent(cmAgent *adk.ChatModelAgent, mgr *Manager) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cmAgent = cmAgent
	s.mcpManager = mgr
}

func (s *RuntimeSession) GetAgent() *adk.ChatModelAgent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmAgent
}

func (s *RuntimeSession) GetMCPManager() *Manager {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mcpManager
}

func (s *RuntimeSession) CloseMCP() {
	s.mu.Lock()
	mgr := s.mcpManager
	s.mcpManager = nil
	s.mu.Unlock()
	if mgr != nil {
		mgr.Close()
	}
}

func (s *RuntimeSession) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *RuntimeSession) SetStatus(st string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = st
}

func (s *RuntimeSession) ResetHeartbeat() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.heartbeatTimer != nil {
		timeout := time.Duration(s.HeartbeatInterval*3) * time.Second
		s.heartbeatTimer.Reset(timeout)
	}
}

func (s *RuntimeSession) StopHeartbeat() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.heartbeatTimer != nil {
		s.heartbeatTimer.Stop()
		s.heartbeatTimer = nil
	}
}

// TryLockPrompt attempts to acquire the prompt lock. Returns false if busy.
func (s *RuntimeSession) TryLockPrompt() bool {
	return s.promptMu.TryLock()
}

// UnlockPrompt releases the prompt lock.
func (s *RuntimeSession) UnlockPrompt() {
	s.promptMu.Unlock()
}

// OwnedBy returns whether this session is owned by the given connection.
func (s *RuntimeSession) OwnedBy(agentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lockedBy == agentID
}

// LockOwnership sets the owning connection.
func (s *RuntimeSession) LockOwnership(agentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lockedBy = agentID
	s.lockedAt = time.Now()
	s.ownerAgent = agentID
}

// =========================================================================
// SessionManager — global singleton, in-memory cache backed by Ent
// =========================================================================

type SessionManager struct {
	mu        sync.Mutex
	sessions  map[string]*RuntimeSession
	entClient *ent.Client
}

var globalSessionManager *SessionManager

func NewSessionManager(entClient *ent.Client) *SessionManager {
	sm := &SessionManager{
		sessions:  make(map[string]*RuntimeSession),
		entClient: entClient,
	}
	globalSessionManager = sm
	return sm
}

func GetSessionManager() *SessionManager {
	return globalSessionManager
}

// NewSession creates a new session with server-generated UUID v4.
func (sm *SessionManager) NewSession(ctx context.Context, params NewSessionParams) (*RuntimeSession, error) {
	id := uuid.New().String()
	now := time.Now()

	mode := params.Mode
	if mode == "" {
		mode = "agent"
	}
	hi := params.HeartbeatInterval
	if hi <= 0 {
		hi = 10
	}
	maxIter := params.MaxIterations
	if maxIter <= 0 {
		maxIter = 20
	}
	str := params.SummarizationTriggerRatio
	if str <= 0 {
		str = 0.8
	}
	businessMeta := params.BusinessMeta
	if businessMeta == nil {
		businessMeta = map[string]any{}
	}

	// Insert session record
	_, err := sm.entClient.Session.Create().
		SetID(id).
		SetStatus(entSession.StatusActive).
		SetUserID(params.UserID).
		SetUsername(params.Username).
		SetBusinessID(params.BusinessID).
		SetBusinessType(params.BusinessType).
		SetBusinessMeta(businessMeta).
		SetMode(mode).
		SetHeartbeatInterval(hi).
		SetLockedBy(params.OwnerAgent).
		SetLockedAt(now).
		SetCreateTime(now).
		SetUpdateTime(now).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	var initMsgs []*schema.Message
	seq := 0

	// System prompt is persisted as seq=0 message
	if params.SystemPrompt != "" {
		initMsgs = append(initMsgs, schema.SystemMessage(params.SystemPrompt))
		_, err = sm.entClient.SessionMessage.Create().
			SetSessionID(id).
			SetSeq(seq).
			SetRole(entMessage.RoleSystem).
			SetContent(params.SystemPrompt).
			SetCreateTime(now).
			Save(ctx)
		if err != nil {
			return nil, fmt.Errorf("save system message: %w", err)
		}
		seq++
	}

	timeout := time.Duration(hi*3) * time.Second
	timer := time.AfterFunc(timeout, func() {
		sm.onIdleTimeout(id)
	})

	s := &RuntimeSession{
		ID:                        id,
		Mode:                      mode,
		HeartbeatInterval:         hi,
		SummarizationTriggerRatio: str,
		MaxIterations:             maxIter,
		BusinessMeta:              businessMeta,
		messages:                  initMsgs,
		seq:                       seq,
		status:                    "active",
		heartbeatTimer:            timer,
		lockedBy:                  params.OwnerAgent,
		lockedAt:                  now,
		ownerAgent:                params.OwnerAgent,
	}

	sm.mu.Lock()
	sm.sessions[id] = s
	sm.mu.Unlock()

	slog.Info("session created", "id", id, "mode", mode)
	return s, nil
}

// GetCached returns a session from the in-memory cache.
func (sm *SessionManager) GetCached(id string) (*RuntimeSession, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s, ok := sm.sessions[id]
	return s, ok
}

// Resume loads a session from the database and reconnects MCP.
func (sm *SessionManager) Resume(ctx context.Context, id string, mcpServers []any, ownerAgent string) (*RuntimeSession, error) {
	// Load session from DB
	sess, err := sm.entClient.Session.Get(ctx, id)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("session %s not found", id)
		}
		return nil, fmt.Errorf("load session: %w", err)
	}

	if sess.Status == entSession.StatusClosed {
		return nil, fmt.Errorf("session %s is closed", id)
	}
	if sess.Status != entSession.StatusIdle {
		return nil, fmt.Errorf("session %s is not idle (status: %s)", id, sess.Status)
	}

	// Load messages from DB
	msgs, err := sm.entClient.SessionMessage.Query().
		Where(entMessage.SessionID(id)).
		Order(ent.Asc(entMessage.FieldSeq)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}

	messages := make([]*schema.Message, 0, len(msgs))
	for _, m := range msgs {
		msg := &schema.Message{
			Role:    schema.RoleType(m.Role),
			Content: m.Content,
		}
		if m.ToolCallID != "" {
			msg.ToolCallID = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, schema.ToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: schema.FunctionCall{
						Name:      tc.Name,
						Arguments: fmt.Sprintf("%v", tc.Arguments),
					},
				})
			}
		}
		messages = append(messages, msg)
	}

	// Update status to active
	now := time.Now()
	_, err = sm.entClient.Session.UpdateOneID(id).
		SetStatus(entSession.StatusActive).
		SetLockedBy(ownerAgent).
		SetLockedAt(now).
		SetUpdateTime(now).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("update session status: %w", err)
	}

	heartbeatInterval := sess.HeartbeatInterval
	timeout := time.Duration(heartbeatInterval*3) * time.Second
	timer := time.AfterFunc(timeout, func() {
		sm.onIdleTimeout(id)
	})

	s := &RuntimeSession{
		ID:                        id,
		Mode:                      sess.Mode,
		Summary:                   sess.Summary,
		HeartbeatInterval:         heartbeatInterval,
		SummarizationTriggerRatio: 0.8,
		MaxIterations:             20,
		BusinessMeta:              sess.BusinessMeta,
		messages:                  messages,
		seq:                       len(messages),
		status:                    "active",
		heartbeatTimer:            timer,
		lockedBy:                  ownerAgent,
		lockedAt:                  now,
		ownerAgent:                ownerAgent,
	}

	sm.mu.Lock()
	sm.sessions[id] = s
	sm.mu.Unlock()

	slog.Info("session resumed", "id", id)
	return s, nil
}

// Close marks a session as closed, disconnects MCP, removes from cache.
func (sm *SessionManager) Close(ctx context.Context, id string) error {
	s, ok := sm.GetCached(id)
	if !ok {
		return fmt.Errorf("session %s not found in cache", id)
	}

	s.StopHeartbeat()
	s.CloseMCP()

	_, err := sm.entClient.Session.UpdateOneID(id).
		SetStatus(entSession.StatusClosed).
		SetUpdateTime(time.Now()).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("close session: %w", err)
	}

	sm.mu.Lock()
	delete(sm.sessions, id)
	sm.mu.Unlock()

	slog.Info("session closed", "id", id)
	return nil
}

// MarkIdle transitions a session to idle: disconnect MCP, stop timer, update DB.
func (sm *SessionManager) MarkIdle(ctx context.Context, id string) error {
	s, ok := sm.GetCached(id)
	if !ok {
		return nil // already gone
	}

	s.StopHeartbeat()
	s.CloseMCP()
	s.SetStatus("idle")

	_, err := sm.entClient.Session.UpdateOneID(id).
		SetStatus(entSession.StatusIdle).
		SetUpdateTime(time.Now()).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("mark idle: %w", err)
	}

	slog.Info("session marked idle", "id", id)
	return nil
}

// UpdateSummary updates the conversation summary in memory and DB.
func (sm *SessionManager) UpdateSummary(ctx context.Context, id, summary string) error {
	s, ok := sm.GetCached(id)
	if ok {
		s.mu.Lock()
		s.Summary = summary
		s.mu.Unlock()
	}

	_, err := sm.entClient.Session.UpdateOneID(id).
		SetSummary(summary).
		SetUpdateTime(time.Now()).
		Save(ctx)
	return err
}

// List returns sessions matching the given filters.
func (sm *SessionManager) List(ctx context.Context) ([]SessionMeta, error) {
	sessions, err := sm.entClient.Session.Query().
		Order(ent.Desc(entSession.FieldUpdateTime)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]SessionMeta, 0, len(sessions))
	for _, s := range sessions {
		count, _ := sm.entClient.SessionMessage.Query().
			Where(entMessage.SessionID(s.ID), entMessage.RoleNEQ(entMessage.RoleSystem)).
			Count(ctx)

		out = append(out, SessionMeta{
			ID:           s.ID,
			Status:       string(s.Status),
			Mode:         s.Mode,
			UserID:       s.UserID,
			Username:     s.Username,
			BusinessID:   s.BusinessID,
			BusinessType: s.BusinessType,
			BusinessMeta: s.BusinessMeta,
			MessageCount: count,
			CreateTime:   s.CreateTime,
			UpdateTime:   s.UpdateTime,
		})
	}
	return out, nil
}

// MarkActiveAsIdle marks all active sessions as idle (called on shutdown).
func (sm *SessionManager) MarkActiveAsIdle(ctx context.Context) error {
	_, err := sm.entClient.Session.Update().
		Where(entSession.StatusEQ(entSession.StatusActive)).
		SetStatus(entSession.StatusIdle).
		SetUpdateTime(time.Now()).
		Save(ctx)
	return err
}

// PersistMessages saves in-memory messages to the database.
func (sm *SessionManager) PersistMessages(ctx context.Context, sessionID string, startSeq int, messages []*schema.Message) error {
	now := time.Now()
	for i, msg := range messages {
		seq := startSeq + i
		var toolCalls []entSchema.ToolCall
		for _, tc := range msg.ToolCalls {
			toolCalls = append(toolCalls, entSchema.ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: parseArgsMap(tc.Function.Arguments),
			})
		}

		role := entMessage.RoleUser
		switch msg.Role {
		case "system":
			role = entMessage.RoleSystem
		case "user":
			role = entMessage.RoleUser
		case "assistant":
			role = entMessage.RoleAssistant
		case "tool":
			role = entMessage.RoleTool
		}

		_, err := sm.entClient.SessionMessage.Create().
			SetSessionID(sessionID).
			SetSeq(seq).
			SetRole(role).
			SetContent(msg.Content).
			SetToolCalls(toolCalls).
			SetToolCallID(msg.ToolCallID).
			SetCreateTime(now).
			Save(ctx)
		if err != nil {
			// Ignore duplicates (UNIQUE on session_id, seq)
			slog.Debug("message persist skipped (duplicate)", "session", sessionID, "seq", seq)
		}
	}
	return nil
}

// onIdleTimeout is called when the heartbeat timer fires.
func (sm *SessionManager) onIdleTimeout(id string) {
	s, ok := sm.GetCached(id)
	if !ok {
		return
	}
	s.SetStatus("idle")
	s.CloseMCP()

	ctx := context.Background()
	_, err := sm.entClient.Session.UpdateOneID(id).
		SetStatus(entSession.StatusIdle).
		SetUpdateTime(time.Now()).
		Save(ctx)
	if err != nil {
		slog.Error("failed to mark session idle on timeout", "id", id, "error", err)
		return
	}

	slog.Info("session marked idle (heartbeat timeout)", "id", id)
}

// parseArgsMap parses a JSON-encoded arguments string back to a map.
func parseArgsMap(argsJSON string) map[string]any {
	if argsJSON == "" {
		return nil
	}
	result := map[string]any{"_raw": argsJSON}
	return result
}
