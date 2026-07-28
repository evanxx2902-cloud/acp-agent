package agent

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
	"github.com/cloudwego/eino/schema"

	"acp/internal/bridge"
	"acp/internal/config"
)

// Ensure EinoAgent satisfies the required interfaces.
var (
	_ acp.Agent = (*EinoAgent)(nil)
)

// EinoAgent implements acp.Agent, backed by eino's ChatModelAgent.
type EinoAgent struct {
	cfg       config.Config
	cmAgent   *adk.ChatModelAgent
	chatModel model.ToolCallingChatModel
	sessions  *SessionStore
	conn      *acp.AgentSideConnection
	connMu    sync.Mutex
}

// NewEinoAgent creates a new EinoAgent with the given config and chat model.
func NewEinoAgent(cfg config.Config, chatModel model.ToolCallingChatModel) *EinoAgent {
	return &EinoAgent{
		cfg:       cfg,
		chatModel: chatModel,
		sessions:  NewSessionStore(),
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

// getOrCreateAgent returns the ChatModelAgent, creating it once on first use.
func (a *EinoAgent) getOrCreateAgent() *adk.ChatModelAgent {
	if a.cmAgent != nil {
		return a.cmAgent
	}

	tools := bridge.BuildTools()
	toolsConfig := adk.ToolsConfig{}
	toolsConfig.Tools = tools

	cmAgent, err := adk.NewChatModelAgent(context.Background(), &adk.ChatModelAgentConfig{
		Name:          "eino-agent",
		Description:   "A general-purpose AI agent with filesystem access",
		Instruction:   a.cfg.SystemPrompt,
		Model:         a.chatModel,
		ToolsConfig:   toolsConfig,
		MaxIterations: a.cfg.MaxIterations,
	})
	if err != nil {
		panic(fmt.Sprintf("failed to create ChatModelAgent: %v", err))
	}

	a.cmAgent = cmAgent
	return cmAgent
}

// --------------------------------------------------------------------------
// acp.Agent interface
// --------------------------------------------------------------------------

func (a *EinoAgent) Initialize(ctx context.Context, params acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentCapabilities: acp.AgentCapabilities{
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
	s := &Session{id: sid}

	// Prepend system message if configured
	if a.cfg.SystemPrompt != "" {
		s.messages = append(s.messages, schema.SystemMessage(a.cfg.SystemPrompt))
	}

	a.sessions.Put(sid, s)
	return acp.NewSessionResponse{SessionId: acp.SessionId(sid)}, nil
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
	ctx = bridge.ContextWithACP(ctx, conn, acp.SessionId(sid))

	// Convert ACP content blocks to eino message and append to history
	userMsg := bridge.ContentBlocksToMessage(params.Prompt)
	s.AppendMessages(userMsg)

	// Build the full message list for this turn
	messages := s.Messages()

	// Get or create the ChatModelAgent
	cmAgent := a.getOrCreateAgent()

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
			err := bridge.ProcessAgentEvent(ctx, conn, sid, event, &finalContent)
			if err != nil {
				slog.Error("failed to process agent event", "error", err)
			}
		}
	}

	// Store the assistant's final response in session history
	responseText := finalContent.String()
	if responseText != "" {
		s.AppendMessages(schema.AssistantMessage(responseText, nil))
	}

	return acp.PromptResponse{
		StopReason: acp.StopReasonEndTurn,
	}, nil
}

func (a *EinoAgent) CloseSession(ctx context.Context, params acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	a.sessions.Delete(string(params.SessionId))
	return acp.CloseSessionResponse{}, nil
}

func (a *EinoAgent) ListSessions(ctx context.Context, params acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionList)
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
