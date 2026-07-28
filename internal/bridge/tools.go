package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/coder/acp-go-sdk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// BuildTools returns the list of eino tools for the agent.
func BuildTools() []tool.BaseTool {
	return []tool.BaseTool{
		newReadFileTool(),
		newWriteFileTool(),
	}
}

// --------------------------------------------------------------------------
// read_file tool
// --------------------------------------------------------------------------

type readFileArgs struct {
	Path string `json:"path"`
}

type readFileTool struct {
	info *schema.ToolInfo
}

func newReadFileTool() *readFileTool {
	return &readFileTool{
		info: &schema.ToolInfo{
			Name: "acp_read_file",
			Desc: "Read the contents of a file from the client's filesystem. Returns the file content as text.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"path": {
					Type:     schema.String,
					Desc:     "Absolute path to the file to read",
					Required: true,
				},
			}),
		},
	}
}

func (t *readFileTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.info, nil
}

func (t *readFileTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	conn, sessionID := getACPContext(ctx)
	if conn == nil {
		return "", fmt.Errorf("acp connection not available in context")
	}

	var args readFileArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	toolCallID := acp.ToolCallId("read_" + randomShortID())
	path := args.Path

	// Notify client: tool call started
	if err := conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sessionID,
		Update: acp.StartToolCall(
			toolCallID,
			fmt.Sprintf("Reading %s", path),
			acp.WithStartKind(acp.ToolKindRead),
			acp.WithStartStatus(acp.ToolCallStatusInProgress),
			acp.WithStartLocations([]acp.ToolCallLocation{{Path: path}}),
			acp.WithStartRawInput(map[string]any{"path": path}),
		),
	}); err != nil {
		slog.Error("failed to send tool call start", "error", err)
	}

	// Execute via ACP
	resp, err := conn.ReadTextFile(ctx, acp.ReadTextFileRequest{
		SessionId: sessionID,
		Path:      path,
	})
	if err != nil {
		// Notify client: tool call failed
		_ = conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: sessionID,
			Update: acp.UpdateToolCall(
				toolCallID,
				acp.WithUpdateStatus(acp.ToolCallStatusFailed),
				acp.WithUpdateTitle(fmt.Sprintf("Error reading %s", path)),
			),
		})
		return "", fmt.Errorf("read file failed: %w", err)
	}

	// Notify client: tool call completed
	if err := conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sessionID,
		Update: acp.UpdateToolCall(
			toolCallID,
			acp.WithUpdateStatus(acp.ToolCallStatusCompleted),
			acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.TextBlock(resp.Content))}),
			acp.WithUpdateRawOutput(map[string]any{"content": resp.Content}),
		),
	}); err != nil {
		slog.Error("failed to send tool call complete", "error", err)
	}

	return resp.Content, nil
}

// --------------------------------------------------------------------------
// write_file tool
// --------------------------------------------------------------------------

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type writeFileTool struct {
	info *schema.ToolInfo
}

func newWriteFileTool() *writeFileTool {
	return &writeFileTool{
		info: &schema.ToolInfo{
			Name: "acp_write_file",
			Desc: "Write or overwrite a file on the client's filesystem with the given content.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"path": {
					Type:     schema.String,
					Desc:     "Absolute path to the file to write",
					Required: true,
				},
				"content": {
					Type:     schema.String,
					Desc:     "Content to write to the file",
					Required: true,
				},
			}),
		},
	}
}

func (t *writeFileTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.info, nil
}

func (t *writeFileTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	conn, sessionID := getACPContext(ctx)
	if conn == nil {
		return "", fmt.Errorf("acp connection not available in context")
	}

	var args writeFileArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	toolCallID := acp.ToolCallId("write_" + randomShortID())
	path := args.Path

	// Notify client: tool call started
	if err := conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sessionID,
		Update: acp.StartToolCall(
			toolCallID,
			fmt.Sprintf("Writing %s", path),
			acp.WithStartKind(acp.ToolKindEdit),
			acp.WithStartStatus(acp.ToolCallStatusInProgress),
			acp.WithStartLocations([]acp.ToolCallLocation{{Path: path}}),
			acp.WithStartRawInput(map[string]any{"path": path, "content": args.Content}),
		),
	}); err != nil {
		slog.Error("failed to send tool call start", "error", err)
	}

	// Execute via ACP
	resp, err := conn.WriteTextFile(ctx, acp.WriteTextFileRequest{
		SessionId: sessionID,
		Path:      path,
		Content:   args.Content,
	})
	if err != nil {
		_ = conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: sessionID,
			Update: acp.UpdateToolCall(
				toolCallID,
				acp.WithUpdateStatus(acp.ToolCallStatusFailed),
				acp.WithUpdateTitle(fmt.Sprintf("Error writing %s", path)),
			),
		})
		return "", fmt.Errorf("write file failed: %w", err)
	}

	_ = resp

	// Notify client: tool call completed
	if err := conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sessionID,
		Update: acp.UpdateToolCall(
			toolCallID,
			acp.WithUpdateStatus(acp.ToolCallStatusCompleted),
			acp.WithUpdateTitle(fmt.Sprintf("Wrote %s", path)),
			acp.WithUpdateRawOutput(map[string]any{"path": path, "success": true}),
		),
	}); err != nil {
		slog.Error("failed to send tool call complete", "error", err)
	}

	return fmt.Sprintf("Successfully wrote to %s", path), nil
}

// --------------------------------------------------------------------------
// ACP context helpers
// --------------------------------------------------------------------------

type acpContextKey struct{}

type acpContext struct {
	Conn      *acp.AgentSideConnection
	SessionID acp.SessionId
}

// ContextWithACP stores the ACP connection and session ID in the context.
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

func randomShortID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b[:])
}
