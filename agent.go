package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/adk/middlewares/plantask"
	"github.com/cloudwego/eino/adk/middlewares/summarization"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

func main() {
	cfg := LoadConfig()

	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	defer cancel()

	store, err := NewStore(cfg.DBPath)
	if err != nil {
		slog.Error("failed to open session store", "path", cfg.DBPath, "error", err)
		os.Exit(1)
	}
	defer store.Close()
	slog.Info("session store opened", "path", cfg.DBPath)

	chatModel, err := NewChatModel(ctx, cfg)
	if err != nil {
		slog.Error("failed to create chat model", "error", err)
		os.Exit(1)
	}

	sessionMgr := NewSessionManager(store)

	// Global callbacks: trace component invocations
	callbacks.AppendGlobalHandlers(newAgentCallback())

	// Start idle scanner: marks sessions idle if no heartbeat for 30s
	sessionMgr.StartIdleScanner(15*time.Second, 30*time.Second)

	switch {
	case strings.HasPrefix(cfg.Listen, "tcp://"):
		addr := strings.TrimPrefix(cfg.Listen, "tcp://")
		serveTCP(ctx, addr, cfg, chatModel, sessionMgr, store, logger)
	case strings.HasPrefix(cfg.Listen, "unix://"):
		path := strings.TrimPrefix(cfg.Listen, "unix://")
		serveUnix(ctx, path, cfg, chatModel, sessionMgr, store, logger)
	default:
		serveStdio(cfg, chatModel, sessionMgr, store, logger)
	}
	_ = store.MarkActiveAsIdle()
	slog.Info("agent server shutting down")
}

func serveStdio(cfg Config, chatModel model.ToolCallingChatModel, sessionMgr *SessionManager, store *Store, logger *slog.Logger) {
	ag := &EinoAgent{cfg: cfg, chatModel: chatModel, sessions: sessionMgr, store: store}
	conn := acp.NewAgentSideConnection(ag, os.Stdout, os.Stdin)
	conn.SetLogger(logger)
	ag.conn = conn

	slog.Info("agent server started (stdio)", "version", acp.ProtocolVersionNumber)
	<-conn.Done()
	slog.Info("agent server shutting down")
}

func serveTCP(ctx context.Context, addr string, cfg Config, chatModel model.ToolCallingChatModel, sessionMgr *SessionManager, store *Store, logger *slog.Logger) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("failed to listen", "addr", addr, "error", err)
		os.Exit(1)
	}
	defer ln.Close()

	slog.Info("agent server listening (tcp)", "addr", addr, "version", acp.ProtocolVersionNumber)

	var wg sync.WaitGroup
	for {
		raw, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				slog.Info("shutting down, waiting for active connections...")
				wg.Wait()
				return
			default:
				slog.Error("accept error", "error", err)
				continue
			}
		}

		wg.Add(1)
		slog.Info("client connected", "remote", raw.RemoteAddr())
		go func(c net.Conn) {
			defer wg.Done()
			ag := &EinoAgent{cfg: cfg, chatModel: chatModel, sessions: sessionMgr, store: store}
			conn := acp.NewAgentSideConnection(ag, c, c)
			conn.SetLogger(logger)
			ag.conn = conn
			<-conn.Done()
		}(raw)
	}
}

func serveUnix(ctx context.Context, path string, cfg Config, chatModel model.ToolCallingChatModel, sessionMgr *SessionManager, store *Store, logger *slog.Logger) {
	_ = os.Remove(path)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Error("failed to create socket dir", "dir", dir, "error", err)
		os.Exit(1)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		slog.Error("failed to listen", "path", path, "error", err)
		os.Exit(1)
	}
	defer ln.Close()
	defer os.Remove(path)

	slog.Info("agent server listening (unix)", "path", path, "version", acp.ProtocolVersionNumber)

	var wg sync.WaitGroup
	for {
		raw, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				slog.Info("shutting down, waiting for active connections...")
				wg.Wait()
				return
			default:
				slog.Error("accept error", "error", err)
				continue
			}
		}

		wg.Add(1)
		slog.Info("client connected", "path", path)
		go func(c net.Conn) {
			defer wg.Done()
			ag := &EinoAgent{cfg: cfg, chatModel: chatModel, sessions: sessionMgr, store: store}
			conn := acp.NewAgentSideConnection(ag, c, c)
			conn.SetLogger(logger)
			ag.conn = conn
			<-conn.Done()
		}(raw)
	}
}

// =========================================================================
// EinoAgent — implements acp.Agent
// =========================================================================

var (
	_ acp.Agent       = (*EinoAgent)(nil)
	_ acp.AgentLoader = (*EinoAgent)(nil)
)

type EinoAgent struct {
	cfg       Config
	chatModel model.ToolCallingChatModel
	sessions  *SessionManager
	store     *Store
	conn      *acp.AgentSideConnection
}

func (a *EinoAgent) buildSessionAgent(ctx context.Context, sessionID string, tools []tool.BaseTool, systemPrompt string, maxIterations int, modeHint string) (*adk.ChatModelAgent, error) {
	toolsConfig := adk.ToolsConfig{}
	toolsConfig.Tools = tools

	// Summarization middleware: compress history when context grows too large
	sumMW, err := summarization.New(ctx, &summarization.Config{
		Model: a.chatModel,
		Trigger: &summarization.TriggerCondition{
			ContextTokens:   int(float64(a.cfg.ContextWindow) * a.cfg.SummarizationTrigger),
			ContextMessages: 100,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create summarization: %w", err)
	}

	// Plan-task middleware: inject task management tools
	planMW, err := plantask.New(ctx, &plantask.Config{
		Backend: newTaskFS(),
		BaseDir: "/tasks",
	})
	if err != nil {
		return nil, fmt.Errorf("create plantask: %w", err)
	}

	// Mode hint: inject plan-oriented prefix if in plan mode
	instruction := systemPrompt
	if modeHint == "plan" {
		instruction = "You are in PLANNING mode. " +
			"Before executing any tools, first use TaskCreate to break the user's request into clear steps. " +
			"Present the complete plan to the user. " +
			"Only begin executing when the user explicitly confirms to proceed.\n\n" + instruction
	}

	return adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:          "eino-agent",
		Description:   "A general-purpose AI agent. Tools are provided by the client via MCP servers.",
		Instruction:   instruction,
		Model:         a.chatModel,
		ToolsConfig:   toolsConfig,
		MaxIterations: maxIterations,
		Handlers:      []adk.ChatModelAgentMiddleware{sumMW, planMW},
	})
}

// --- acp.Agent methods ---

func (a *EinoAgent) Initialize(ctx context.Context, params acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession: true,
			McpCapabilities: acp.McpCapabilities{
				Http: true,
				Sse: true,
			},
			PromptCapabilities: acp.PromptCapabilities{
				Image:           true,
				Audio:           false,
				EmbeddedContext: false,
			},
			SessionCapabilities: acp.SessionCapabilities{
				Close:  &acp.SessionCloseCapabilities{},
				List:   &acp.SessionListCapabilities{},
				Resume: &acp.SessionResumeCapabilities{},
			},
		},
	}, nil
}

func (a *EinoAgent) Authenticate(ctx context.Context, params acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

func (a *EinoAgent) NewSession(ctx context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	sid := randomID()
	if s, ok := a.sessions.Get(sid); ok && s.Status() != "closed" {
		return acp.NewSessionResponse{}, fmt.Errorf("session %s already exists and is %s", sid, s.Status())
	}

	// System prompt: client can override via _meta.system_prompt
	systemPrompt := a.cfg.SystemPrompt
	if v, ok := params.Meta["system_prompt"].(string); ok && v != "" {
		systemPrompt = v
	}

	// Max iterations: client can override via _meta.max_iterations
	maxIterations := a.cfg.MaxIterations
	if v, ok := params.Meta["max_iterations"].(float64); ok && v > 0 {
		maxIterations = int(v)
	}

	var initMsgs []*schema.Message
	if systemPrompt != "" {
		initMsgs = append(initMsgs, schema.SystemMessage(systemPrompt))
	}

	s, err := a.sessions.Create(sid, params.Meta, initMsgs...)
	if err != nil {
		return acp.NewSessionResponse{}, fmt.Errorf("create session: %w", err)
	}

	mcpMgr := &Manager{}
	mcpTools, mcpErr := mcpMgr.Connect(ctx, params.McpServers)
	if mcpErr != nil {
		slog.Warn("some MCP servers failed to connect", "error", mcpErr)
	}

	cmAgent, err := a.buildSessionAgent(ctx, sid, mcpTools, systemPrompt, maxIterations, "")
	if err != nil {
		mcpMgr.Close()
		return acp.NewSessionResponse{}, fmt.Errorf("build agent: %w", err)
	}

	s.SetMCAgent(cmAgent, mcpMgr)
	s.Touch()

	return acp.NewSessionResponse{SessionId: acp.SessionId(sid)}, nil
}

func (a *EinoAgent) LoadSession(ctx context.Context, params acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	sid := string(params.SessionId)
	if _, ok := a.sessions.Get(sid); ok {
		return acp.LoadSessionResponse{}, nil
	}

	s, err := a.sessions.Load(sid)
	if err != nil {
		return acp.LoadSessionResponse{}, fmt.Errorf("load session: %w", err)
	}

	// Reconnect MCP servers if provided via _meta
	var mcpTools []tool.BaseTool
	var mcpMgr *Manager
	if servers, ok := params.Meta["mcpServers"].([]any); ok && len(servers) > 0 {
		// Convert from JSON generic format
		data, _ := json.Marshal(servers)
		var acpServers []acp.McpServer
		if json.Unmarshal(data, &acpServers) == nil {
			mcpMgr = &Manager{}
			mcpTools, _ = mcpMgr.Connect(ctx, acpServers)
		}
	}

	cmAgent, err := a.buildSessionAgent(ctx, sid, mcpTools, a.cfg.SystemPrompt, a.cfg.MaxIterations, "")
	if err != nil {
		if mcpMgr != nil {
			mcpMgr.Close()
		}
		return acp.LoadSessionResponse{}, fmt.Errorf("build agent: %w", err)
	}
	s.SetMCAgent(cmAgent, mcpMgr)
	s.SetStatus("active")
	s.Touch()

	return acp.LoadSessionResponse{}, nil
}

func (a *EinoAgent) requireActive(sid string) (*Session, error) {
	s, ok := a.sessions.Get(sid)
	if !ok {
		return nil, fmt.Errorf("session %s not found", sid)
	}
	st := s.Status()
	if st == "closed" {
		return nil, fmt.Errorf("session %s is closed", sid)
	}
	if st == "idle" {
		return nil, fmt.Errorf("session %s is idle, use resume to re-activate", sid)
	}
	return s, nil
}

func (a *EinoAgent) Cancel(ctx context.Context, params acp.CancelNotification) error {
	s, ok := a.sessions.Get(string(params.SessionId))
	if ok {
		s.Cancel()
	}
	return nil
}

func (a *EinoAgent) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	sid := string(params.SessionId)
	s, err := a.requireActive(sid)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	s.Cancel()
	s.Touch()

	ctx, cancel := context.WithCancel(ctx)
	s.SetCancel(cancel)
	defer cancel()

	conn := a.conn
	if conn == nil {
		return acp.PromptResponse{}, fmt.Errorf("agent connection not set")
	}

	ctx = ContextWithACP(ctx, conn, acp.SessionId(sid))

	userMsg := ContentBlocksToMessage(params.Prompt)
	s.AppendMessages(userMsg)

	// Rebuild agent lazily if mode or maxIter changed (preserves MCP manager)
	if s.IsDirty() {
		cmAgent, err := a.buildSessionAgent(ctx, sid, nil, a.cfg.SystemPrompt, a.cfg.MaxIterations, s.GetMode())
		if err != nil {
			slog.Error("failed to rebuild agent", "error", err)
		} else {
			s.RebuildAgent(cmAgent)
		}
	}

	if err := a.runReAct(ctx, conn, s); err != nil {
		slog.Error("prompt execution failed", "error", err)
		return acp.PromptResponse{}, err
	}

	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

// runReAct runs the eino ReAct loop, streaming output to the ACP client.
// Returns an error if the ReAct loop fails.
func (a *EinoAgent) runReAct(ctx context.Context, conn *acp.AgentSideConnection, s *Session) error {
	messages := s.Messages()

	cmAgent := s.GetAgent()
	if cmAgent == nil {
		return fmt.Errorf("agent not initialized")
	}

	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent:           cmAgent,
		EnableStreaming: true,
	})

	iter := runner.Run(ctx, messages)

	var finalContent strings.Builder
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			return fmt.Errorf("agent error: %w", event.Err)
		}
		if event.Output != nil && event.Output.MessageOutput != nil {
			if err := ProcessAgentEvent(ctx, conn, s, event, &finalContent); err != nil {
				slog.Error("failed to process agent event", "error", err)
			}
		}
	}

	responseText := finalContent.String()
	if responseText != "" {
		s.AppendMessages(schema.AssistantMessage(responseText, nil))
	}

	return nil
}

func (a *EinoAgent) CloseSession(ctx context.Context, params acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	sid := string(params.SessionId)
	if s, ok := a.sessions.Get(sid); ok {
		s.CloseMCP()
		s.SetStatus("closed")
	}
	return acp.CloseSessionResponse{}, nil
}

func (a *EinoAgent) ListSessions(ctx context.Context, params acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	metas, err := a.sessions.List()
	if err != nil {
		return acp.ListSessionsResponse{}, fmt.Errorf("list sessions: %w", err)
	}

	items := make([]acp.SessionInfo, 0, len(metas))
	for _, m := range metas {
		updatedAt := m.UpdatedAt.Format(time.RFC3339)
		title := fmt.Sprintf("%s@%s", m.Username, m.BusinessType)
		items = append(items, acp.SessionInfo{
			SessionId: acp.SessionId(m.ID),
			Cwd:       "/",
			Title:     &title,
			UpdatedAt: &updatedAt,
		})
	}
	return acp.ListSessionsResponse{Sessions: items}, nil
}

func (a *EinoAgent) SetSessionConfigOption(ctx context.Context, params acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, nil
}

func (a *EinoAgent) SetSessionMode(ctx context.Context, params acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	sid := string(params.SessionId)
	s, err := a.requireActive(sid)
	if err != nil {
		return acp.SetSessionModeResponse{}, err
	}
	mode := string(params.ModeId)
	s.SetMode(mode)
	s.SetDirty()
	slog.Info("session mode set", "session", sid, "mode", mode)
	return acp.SetSessionModeResponse{}, nil
}

func (a *EinoAgent) ResumeSession(ctx context.Context, params acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	sid := string(params.SessionId)
	s, ok := a.sessions.Get(sid)
	if !ok {
		// Try loading from DB if not in memory
		var err error
		s, err = a.sessions.Load(sid)
		if err != nil {
			return acp.ResumeSessionResponse{}, fmt.Errorf("session %s not found", sid)
		}
		// Rebuild agent for loaded session
		cmAgent, err := a.buildSessionAgent(ctx, sid, nil, a.cfg.SystemPrompt, a.cfg.MaxIterations, s.GetMode())
		if err != nil {
			return acp.ResumeSessionResponse{}, fmt.Errorf("rebuild agent: %w", err)
		}
		s.SetMCAgent(cmAgent, nil)
	}

	s.SetStatus("active")
	s.Touch()

	conn := a.conn
	if conn == nil {
		return acp.ResumeSessionResponse{}, fmt.Errorf("agent connection not set")
	}

	ctx = ContextWithACP(ctx, conn, acp.SessionId(sid))

	// Send a continuation prompt so the LLM knows we're resuming
	continueMsg := schema.UserMessage("(session resumed — continue where you left off)")
	s.AppendMessages(continueMsg)

	if err := a.runReAct(ctx, conn, s); err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	return acp.ResumeSessionResponse{}, nil
}

func (a *EinoAgent) Logout(ctx context.Context, params acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, nil
}

// =========================================================================
// Extension methods — ACP _ prefixed custom methods
// =========================================================================

func (a *EinoAgent) HandleExtensionMethod(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "_heartbeat":
		var req struct {
			SessionIDs []string `json:"sessionIds"`
		}
		json.Unmarshal(params, &req)
		for _, sid := range req.SessionIDs {
			if s, ok := a.sessions.Get(sid); ok && s.Status() == "active" {
				s.Touch()
			}
		}
		return map[string]any{"ok": true, "ts": time.Now().Unix()}, nil

	case "_release":
		var req struct {
			SessionIDs []string `json:"sessionIds"`
		}
		json.Unmarshal(params, &req)
		for _, sid := range req.SessionIDs {
			if s, ok := a.sessions.Get(sid); ok && s.Status() != "closed" {
				s.SetStatus("idle")
			}
		}
		return map[string]any{"ok": true}, nil

	default:
		return nil, acp.NewMethodNotFound(method)
	}
}

// =========================================================================
// ACP context helpers (injected into context for tool access)
// =========================================================================

type acpContextKey struct{}

type acpContext struct {
	Conn      *acp.AgentSideConnection
	SessionID acp.SessionId
}

func ContextWithACP(ctx context.Context, conn *acp.AgentSideConnection, sessionID acp.SessionId) context.Context {
	return context.WithValue(ctx, acpContextKey{}, &acpContext{
		Conn:      conn,
		SessionID: sessionID,
	})
}

func getACPContext(ctx context.Context) (*acp.AgentSideConnection, acp.SessionId) {
	if v, ok := ctx.Value(acpContextKey{}).(*acpContext); ok {
		return v.Conn, v.SessionID
	}
	return nil, ""
}

// =========================================================================
// ACP ContentBlock → eino Message
// =========================================================================

func ContentBlocksToMessage(blocks []acp.ContentBlock) *schema.Message {
	var textParts []string
	var imageParts []schema.MessageInputPart

	for _, block := range blocks {
		switch {
		case block.Text != nil:
			textParts = append(textParts, block.Text.Text)
		case block.Image != nil:
			imageParts = append(imageParts, schema.MessageInputPart{
				Type: schema.ChatMessagePartTypeImageURL,
				Image: &schema.MessageInputImage{
					MessagePartCommon: schema.MessagePartCommon{
						Base64Data: &block.Image.Data,
						MIMEType:   block.Image.MimeType,
					},
				},
			})
		case block.ResourceLink != nil:
			textParts = append(textParts,
				fmt.Sprintf("[Resource: %s](%s)", block.ResourceLink.Name, block.ResourceLink.Uri))
		case block.Resource != nil:
			if block.Resource.Resource.TextResourceContents != nil {
				textParts = append(textParts, block.Resource.Resource.TextResourceContents.Text)
			}
		case block.Audio != nil:
			textParts = append(textParts, "[Audio content attached]")
		}
	}

	textContent := strings.Join(textParts, "\n")

	if len(imageParts) > 0 {
		parts := make([]schema.MessageInputPart, 0, len(imageParts)+1)
		if textContent != "" {
			parts = append(parts, schema.MessageInputPart{
				Type: schema.ChatMessagePartTypeText,
				Text: textContent,
			})
		}
		parts = append(parts, imageParts...)
		return &schema.Message{
			Role:                schema.User,
			UserInputMultiContent: parts,
		}
	}

	return schema.UserMessage(textContent)
}

// =========================================================================
// eino AgentEvent → ACP SessionUpdate streaming
// =========================================================================

func ProcessAgentEvent(
	ctx context.Context,
	conn *acp.AgentSideConnection,
	s *Session,
	event *adk.AgentEvent,
	finalContent *strings.Builder,
) error {
	mv := event.Output.MessageOutput
	if mv.IsStreaming {
		return processStreaming(ctx, conn, s, mv, finalContent)
	}
	return processNonStreaming(ctx, conn, s, mv, finalContent)
}

func processStreaming(
	ctx context.Context,
	conn *acp.AgentSideConnection,
	s *Session,
	mv *adk.MessageVariant,
	finalContent *strings.Builder,
) error {
	msgStream := mv.MessageStream
	if msgStream == nil {
		return nil
	}

	for {
		chunk, err := msgStream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if chunk == nil {
			continue
		}

		switch mv.Role {
		case schema.Assistant:
			if chunk.Content != "" {
				if err := conn.SessionUpdate(ctx, acp.SessionNotification{
					SessionId: acp.SessionId(s.ID),
					Update:    acp.UpdateAgentMessageText(chunk.Content),
				}); err != nil {
					return err
				}
				finalContent.WriteString(chunk.Content)
			}
			if chunk.ReasoningContent != "" {
				if err := conn.SessionUpdate(ctx, acp.SessionNotification{
					SessionId: acp.SessionId(s.ID),
					Update:    acp.UpdateAgentThoughtText(chunk.ReasoningContent),
				}); err != nil {
					return err
				}
			}
			// Save tool calls (small — just names and args)
			if len(chunk.ToolCalls) > 0 {
				s.AppendMessages(chunk)
			}
		case schema.Tool:
			// Stream tool result to client, store lightweight status only
			s.AppendMessages(compactToolMsg(chunk))
		}
	}
	return nil
}

func processNonStreaming(
	ctx context.Context,
	conn *acp.AgentSideConnection,
	s *Session,
	mv *adk.MessageVariant,
	finalContent *strings.Builder,
) error {
	msg := mv.Message
	if msg == nil {
		return nil
	}

	switch mv.Role {
	case schema.Assistant:
		if msg.Content != "" {
			if err := conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: acp.SessionId(s.ID),
				Update:    acp.UpdateAgentMessageText(msg.Content),
			}); err != nil {
				return err
			}
			finalContent.WriteString(msg.Content)
		}
		if msg.ReasoningContent != "" {
			if err := conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: acp.SessionId(s.ID),
				Update:    acp.UpdateAgentThoughtText(msg.ReasoningContent),
			}); err != nil {
				return err
			}
		}
		// Save assistant messages that have tool calls
		if len(msg.ToolCalls) > 0 {
			s.AppendMessages(msg)
		}
	case schema.Tool:
		// Store lightweight status only (not full content)
		s.AppendMessages(compactToolMsg(msg))
	}
	return nil
}

// compactToolMsg creates a lightweight tool message: only name + status, no content.
func compactToolMsg(msg *schema.Message) *schema.Message {
	return &schema.Message{
		Role:       schema.Tool,
		ToolCallID: msg.ToolCallID,
		ToolName:   msg.ToolName,
		Content:    "(completed)",
	}
}

// =========================================================================
// In-memory task filesystem for plantask
// =========================================================================

type taskFS struct {
	mu    sync.Mutex
	files map[string]string
}

func newTaskFS() *taskFS {
	return &taskFS{files: make(map[string]string)}
}

func (t *taskFS) LsInfo(ctx context.Context, req *plantask.LsInfoRequest) ([]plantask.FileInfo, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []plantask.FileInfo
	for path, content := range t.files {
		out = append(out, plantask.FileInfo{Path: path, IsDir: false, Size: int64(len(content))})
	}
	return out, nil
}

func (t *taskFS) Read(ctx context.Context, req *plantask.ReadRequest) (*filesystem.FileContent, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	content, ok := t.files[req.FilePath]
	if !ok {
		return nil, fmt.Errorf("file not found: %s", req.FilePath)
	}
	return &filesystem.FileContent{Content: content}, nil
}

func (t *taskFS) Write(ctx context.Context, req *plantask.WriteRequest) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.files[req.FilePath] = req.Content
	return nil
}

func (t *taskFS) Delete(ctx context.Context, req *plantask.DeleteRequest) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.files, req.FilePath)
	return nil
}

// =========================================================================
// Callbacks — observability hooks
// =========================================================================

type agentCallback struct {
	callbacks.Handler
}

func newAgentCallback() callbacks.Handler {
	return &agentCallback{}
}

func (c *agentCallback) OnStart(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
	slog.Debug("callback: start", "component", info.Component, "name", info.Name, "type", info.Type)
	return ctx
}

func (c *agentCallback) OnEnd(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
	slog.Debug("callback: end", "component", info.Component, "name", info.Name, "type", info.Type)
	return ctx
}

func (c *agentCallback) OnError(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
	slog.Warn("callback: error", "component", info.Component, "name", info.Name, "type", info.Type, "error", err)
	return ctx
}

func (c *agentCallback) OnStartWithStreamInput(ctx context.Context, info *callbacks.RunInfo, input *schema.StreamReader[callbacks.CallbackInput]) context.Context {
	return ctx
}

func (c *agentCallback) OnEndWithStreamOutput(ctx context.Context, info *callbacks.RunInfo, output *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
	return ctx
}

// Tell eino which timings we actually handle (avoids stream copying overhead)
func (c *agentCallback) Needed(_ context.Context, _ *callbacks.RunInfo, timing callbacks.CallbackTiming) bool {
	return timing == callbacks.TimingOnStart || timing == callbacks.TimingOnEnd || timing == callbacks.TimingOnError
}

// =========================================================================
// Helpers
// =========================================================================

func randomID() string {
	var b [12]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return fmt.Sprintf("sess_%d", time.Now().UnixNano())
	}
	return "sess_" + hex.EncodeToString(b[:])
}
