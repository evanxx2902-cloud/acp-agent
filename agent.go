package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

)

// Ensure EinoAgent satisfies the required interfaces.
var (
	_ acp.Agent       = (*EinoAgent)(nil)
	_ acp.AgentLoader = (*EinoAgent)(nil)
)

// EinoAgent implements acp.Agent, backed by eino's ChatModelAgent.
type EinoAgent struct {
	cfg       Config
	chatModel model.ToolCallingChatModel
	sessions  *SessionManager
	conn      *acp.AgentSideConnection
	connMu    sync.Mutex
}

// NewEinoAgent creates a new EinoAgent with the given config, chat model, and session store.
func NewEinoAgent(cfg Config, chatModel model.ToolCallingChatModel, store *Store) *EinoAgent {
	return &EinoAgent{
		cfg:       cfg,
		chatModel: chatModel,
		sessions:  NewSessionManager(store),
	}
}

// SetAgentConnection stores the ACP connection reference.
func (a *EinoAgent) SetAgentConnection(conn *acp.AgentSideConnection) {
	a.connMu.Lock()
	a.conn = conn
	a.connMu.Unlock()
}

func (a *EinoAgent) getConn() *acp.AgentSideConnection {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	return a.conn
}

// buildSessionAgent constructs a ChatModelAgent for a specific session,
// combining base tools with MCP-discovered tools.
func (a *EinoAgent) buildSessionAgent(tools []tool.BaseTool) (*adk.ChatModelAgent, error) {
	toolsConfig := adk.ToolsConfig{}
	toolsConfig.Tools = tools

	return adk.NewChatModelAgent(context.Background(), &adk.ChatModelAgentConfig{
		Name:          "eino-agent",
		Description:   "A general-purpose AI agent with filesystem and shell access",
		Instruction:   a.cfg.SystemPrompt,
		Model:         a.chatModel,
		ToolsConfig:   toolsConfig,
		MaxIterations: a.cfg.MaxIterations,
	})
}

// --------------------------------------------------------------------------
// acp.Agent interface
// --------------------------------------------------------------------------

func (a *EinoAgent) Initialize(ctx context.Context, params acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession: true,
			PromptCapabilities: acp.PromptCapabilities{
				Image:           true,
				Audio:           false,
				EmbeddedContext: false,
			},
		},
	}, nil
}

func (a *EinoAgent) Authenticate(ctx context.Context, params acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

func (a *EinoAgent) NewSession(ctx context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	sid := randomID()

	var initMsgs []*schema.Message
	if a.cfg.SystemPrompt != "" {
		initMsgs = append(initMsgs, schema.SystemMessage(a.cfg.SystemPrompt))
	}

	s, err := a.sessions.Create(sid, initMsgs...)
	if err != nil {
		return acp.NewSessionResponse{}, fmt.Errorf("create session: %w", err)
	}

	// Connect MCP servers and discover tools
	mcpMgr := &Manager{}
	mcpTools, _ := mcpMgr.Connect(ctx, params.McpServers)

	// Build session-specific agent: all tools come from MCP
	allTools := mcpTools
	cmAgent, err := a.buildSessionAgent(allTools)
	if err != nil {
		mcpMgr.Close()
		return acp.NewSessionResponse{}, fmt.Errorf("build agent: %w", err)
	}

	s.SetMCAgent(cmAgent, mcpMgr)

	return acp.NewSessionResponse{SessionId: acp.SessionId(sid)}, nil
}

func (a *EinoAgent) LoadSession(ctx context.Context, params acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	// If the session is not yet in memory, load it from the DB.
	// Note: MCP tools are NOT reconnected on load (they're ephemeral per original session creation).
	if _, ok := a.sessions.Get(string(params.SessionId)); !ok {
		_, err := a.sessions.Load(string(params.SessionId))
		if err != nil {
			return acp.LoadSessionResponse{}, fmt.Errorf("load session: %w", err)
		}
	}
	return acp.LoadSessionResponse{}, nil
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
	s, ok := a.sessions.Get(sid)
	if !ok {
		return acp.PromptResponse{}, fmt.Errorf("session %s not found", sid)
	}

	// Cancel any previous turn for this session
	s.Cancel()

	ctx, cancel := context.WithCancel(context.Background())
	s.SetCancel(cancel)
	defer cancel()

	conn := a.getConn()
	if conn == nil {
		return acp.PromptResponse{}, fmt.Errorf("agent connection not set")
	}

	// Inject ACP connection context so tools can access the connection
	ctx = ContextWithACP(ctx, conn, acp.SessionId(sid))

	// Convert ACP content blocks to eino message and append to history
	userMsg := ContentBlocksToMessage(params.Prompt)
	s.AppendMessages(userMsg)

	// Build the full message list for this turn
	messages := s.Messages()

	// Get the session-specific agent
	cmAgent := s.GetAgent()
	if cmAgent == nil {
		return acp.PromptResponse{}, fmt.Errorf("agent not initialized for session %s", sid)
	}

	// Create runner and execute
	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent:           cmAgent,
		EnableStreaming: true,
	})

	iter := runner.Run(ctx, messages)

	// Process agent events - streaming + message collection
	var finalContent strings.Builder
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}

		if event.Err != nil {
			if ctx.Err() != nil {
				return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
			}
			slog.Error("agent error", "error", event.Err)
			return acp.PromptResponse{}, event.Err
		}

		if event.Output != nil && event.Output.MessageOutput != nil {
			err := ProcessAgentEvent(ctx, conn, sid, event, &finalContent)
			if err != nil {
				slog.Error("failed to process agent event", "error", err)
			}
		}
	}

	// Store the assistant's final response in session history (auto-persists via write-through)
	responseText := finalContent.String()
	if responseText != "" {
		s.AppendMessages(schema.AssistantMessage(responseText, nil))
	}

	return acp.PromptResponse{
		StopReason: acp.StopReasonEndTurn,
	}, nil
}

func (a *EinoAgent) CloseSession(ctx context.Context, params acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	sid := string(params.SessionId)
	if s, ok := a.sessions.Get(sid); ok {
		s.CloseMCP()
	}
	a.sessions.Delete(sid)
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
		items = append(items, acp.SessionInfo{
			SessionId: acp.SessionId(m.ID),
			Cwd:       "/",
			UpdatedAt: &updatedAt,
		})
	}
	return acp.ListSessionsResponse{Sessions: items}, nil
}

func (a *EinoAgent) ResumeSession(ctx context.Context, params acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionResume)
}

func (a *EinoAgent) SetSessionConfigOption(ctx context.Context, params acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, nil
}

func (a *EinoAgent) SetSessionMode(ctx context.Context, params acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}

func (a *EinoAgent) Logout(ctx context.Context, params acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, acp.NewMethodNotFound(acp.AgentMethodLogout)
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func randomID() string {
	var b [12]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return fmt.Sprintf("sess_%d", time.Now().UnixNano())
	}
	return "sess_" + hex.EncodeToString(b[:])
}
