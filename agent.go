package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

func main() {
	cfg := LoadConfig()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
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

	ag := NewEinoAgent(cfg, chatModel, store)
	conn := acp.NewAgentSideConnection(ag, os.Stdout, os.Stdin)
	conn.SetLogger(logger)
	ag.SetAgentConnection(conn)

	slog.Info("agent server started", "version", acp.ProtocolVersionNumber)

	<-conn.Done()
	slog.Info("agent server shutting down")
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
	conn      *acp.AgentSideConnection
	connMu    sync.Mutex
}

func NewEinoAgent(cfg Config, chatModel model.ToolCallingChatModel, store *Store) *EinoAgent {
	return &EinoAgent{
		cfg:       cfg,
		chatModel: chatModel,
		sessions:  NewSessionManager(store),
	}
}

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

// --- acp.Agent methods ---

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

	mcpMgr := &Manager{}
	mcpTools, _ := mcpMgr.Connect(ctx, params.McpServers)

	cmAgent, err := a.buildSessionAgent(mcpTools)
	if err != nil {
		mcpMgr.Close()
		return acp.NewSessionResponse{}, fmt.Errorf("build agent: %w", err)
	}

	s.SetMCAgent(cmAgent, mcpMgr)

	return acp.NewSessionResponse{SessionId: acp.SessionId(sid)}, nil
}

func (a *EinoAgent) LoadSession(ctx context.Context, params acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
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

	s.Cancel()

	ctx, cancel := context.WithCancel(context.Background())
	s.SetCancel(cancel)
	defer cancel()

	conn := a.getConn()
	if conn == nil {
		return acp.PromptResponse{}, fmt.Errorf("agent connection not set")
	}

	ctx = ContextWithACP(ctx, conn, acp.SessionId(sid))

	userMsg := ContentBlocksToMessage(params.Prompt)
	s.AppendMessages(userMsg)

	messages := s.Messages()

	cmAgent := s.GetAgent()
	if cmAgent == nil {
		return acp.PromptResponse{}, fmt.Errorf("agent not initialized for session %s", sid)
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
			if ctx.Err() != nil {
				return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
			}
			slog.Error("agent error", "error", event.Err)
			return acp.PromptResponse{}, event.Err
		}

		if event.Output != nil && event.Output.MessageOutput != nil {
			if err := ProcessAgentEvent(ctx, conn, sid, event, &finalContent); err != nil {
				slog.Error("failed to process agent event", "error", err)
			}
		}
	}

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
	sid string,
	event *adk.AgentEvent,
	finalContent *strings.Builder,
) error {
	mv := event.Output.MessageOutput
	if mv.IsStreaming {
		return processStreaming(ctx, conn, sid, mv, finalContent)
	}
	return processNonStreaming(ctx, conn, sid, mv, finalContent)
}

func processStreaming(
	ctx context.Context,
	conn *acp.AgentSideConnection,
	sid string,
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
					SessionId: acp.SessionId(sid),
					Update:    acp.UpdateAgentMessageText(chunk.Content),
				}); err != nil {
					return err
				}
				finalContent.WriteString(chunk.Content)
			}
			if chunk.ReasoningContent != "" {
				if err := conn.SessionUpdate(ctx, acp.SessionNotification{
					SessionId: acp.SessionId(sid),
					Update:    acp.UpdateAgentThoughtText(chunk.ReasoningContent),
				}); err != nil {
					return err
				}
			}
		case schema.Tool:
			slog.Debug("streaming tool result", "toolName", mv.ToolName, "content", chunk.Content)
		}
	}
	return nil
}

func processNonStreaming(
	ctx context.Context,
	conn *acp.AgentSideConnection,
	sid string,
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
				SessionId: acp.SessionId(sid),
				Update:    acp.UpdateAgentMessageText(msg.Content),
			}); err != nil {
				return err
			}
			finalContent.WriteString(msg.Content)
		}
		if msg.ReasoningContent != "" {
			if err := conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: acp.SessionId(sid),
				Update:    acp.UpdateAgentThoughtText(msg.ReasoningContent),
			}); err != nil {
				return err
			}
		}
	case schema.Tool:
		slog.Debug("non-streaming tool result", "toolName", mv.ToolName, "content", msg.Content)
	}
	return nil
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
