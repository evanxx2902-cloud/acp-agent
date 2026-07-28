package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// BuildTools returns the list of eino tools for the agent.
func BuildTools() []tool.BaseTool {
	return []tool.BaseTool{
		newReadFileTool(),
		newWriteFileTool(),
		newRunCommandTool(),
	}
}

// --------------------------------------------------------------------------
// ACP notification helpers
// --------------------------------------------------------------------------

func notifyToolStart(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId,
	id acp.ToolCallId, title string, kind acp.ToolKind, locations []acp.ToolCallLocation, rawInput map[string]any) {
	if err := conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sid,
		Update: acp.StartToolCall(
			id,
			title,
			acp.WithStartKind(kind),
			acp.WithStartStatus(acp.ToolCallStatusInProgress),
			acp.WithStartLocations(locations),
			acp.WithStartRawInput(rawInput),
		),
	}); err != nil {
		slog.Error("failed to send tool call start", "error", err)
	}
}

func notifyToolComplete(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId,
	id acp.ToolCallId, title, content string, rawOutput map[string]any) {
	var updateContent []acp.ToolCallContent
	if content != "" {
		updateContent = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(content))}
	}
	if err := conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sid,
		Update: acp.UpdateToolCall(
			id,
			acp.WithUpdateStatus(acp.ToolCallStatusCompleted),
			acp.WithUpdateTitle(title),
			acp.WithUpdateContent(updateContent),
			acp.WithUpdateRawOutput(rawOutput),
		),
	}); err != nil {
		slog.Error("failed to send tool call complete", "error", err)
	}
}

func notifyToolFailed(ctx context.Context, conn *acp.AgentSideConnection, sid acp.SessionId,
	id acp.ToolCallId, title string) {
	if err := conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sid,
		Update: acp.UpdateToolCall(
			id,
			acp.WithUpdateStatus(acp.ToolCallStatusFailed),
			acp.WithUpdateTitle(title),
		),
	}); err != nil {
		slog.Error("failed to send tool call failed", "error", err)
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
			Desc: "Read the contents of a file from the client's filesystem. " +
				"Returns the file content as text. The path must point to a regular file, not a directory.",
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

	notifyToolStart(ctx, conn, sessionID, toolCallID,
		fmt.Sprintf("Reading %s", args.Path), acp.ToolKindRead,
		[]acp.ToolCallLocation{{Path: args.Path}},
		map[string]any{"path": args.Path})

	resp, err := conn.ReadTextFile(ctx, acp.ReadTextFileRequest{
		SessionId: sessionID,
		Path:      args.Path,
	})
	if err != nil {
		notifyToolFailed(ctx, conn, sessionID, toolCallID,
			fmt.Sprintf("Error reading %s", args.Path))
		return "", fmt.Errorf("read file failed: %w", err)
	}

	notifyToolComplete(ctx, conn, sessionID, toolCallID,
		fmt.Sprintf("Read %s", args.Path), resp.Content,
		map[string]any{"path": args.Path, "content": resp.Content})

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

	notifyToolStart(ctx, conn, sessionID, toolCallID,
		fmt.Sprintf("Writing %s", args.Path), acp.ToolKindEdit,
		[]acp.ToolCallLocation{{Path: args.Path}},
		map[string]any{"path": args.Path, "content": args.Content})

	_, err := conn.WriteTextFile(ctx, acp.WriteTextFileRequest{
		SessionId: sessionID,
		Path:      args.Path,
		Content:   args.Content,
	})
	if err != nil {
		notifyToolFailed(ctx, conn, sessionID, toolCallID,
			fmt.Sprintf("Error writing %s", args.Path))
		return "", fmt.Errorf("write file failed: %w", err)
	}

	notifyToolComplete(ctx, conn, sessionID, toolCallID,
		fmt.Sprintf("Wrote %s", args.Path), "",
		map[string]any{"path": args.Path, "success": true})

	return fmt.Sprintf("Successfully wrote to %s", args.Path), nil
}

// --------------------------------------------------------------------------
// run_command tool
// --------------------------------------------------------------------------

type runCommandArgs struct {
	Command string `json:"command"`
	Workdir string `json:"workdir,omitempty"`
}

type runCommandTool struct {
	info *schema.ToolInfo
}

func newRunCommandTool() *runCommandTool {
	return &runCommandTool{
		info: &schema.ToolInfo{
			Name: "acp_run_command",
			Desc: "Execute a shell command on the client's system and return the output. " +
				"Use this for listing files, searching code, running build commands, etc. " +
				"The command runs in a bash shell.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"command": {
					Type:     schema.String,
					Desc:     "The shell command to execute",
					Required: true,
				},
				"workdir": {
					Type:     schema.String,
					Desc:     "Working directory for the command (absolute path). Defaults to the client's current directory.",
					Required: false,
				},
			}),
		},
	}
}

func (t *runCommandTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.info, nil
}

func (t *runCommandTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	conn, sessionID := getACPContext(ctx)
	if conn == nil {
		return "", fmt.Errorf("acp connection not available in context")
	}

	var args runCommandArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	toolCallID := acp.ToolCallId("run_" + randomShortID())
	title := fmt.Sprintf("Run: %s", truncate(args.Command, 60))

	notifyToolStart(ctx, conn, sessionID, toolCallID, title,
		acp.ToolKindExecute, nil,
		map[string]any{"command": args.Command, "workdir": args.Workdir})

	// Prepare terminal request
	var cwd *string
	if args.Workdir != "" {
		cwd = &args.Workdir
	}

	// Create terminal
	termResp, err := conn.CreateTerminal(ctx, acp.CreateTerminalRequest{
		SessionId: sessionID,
		Command:   args.Command,
		Args:      []string{"bash", "-c", args.Command},
		Cwd:       cwd,
	})
	if err != nil {
		notifyToolFailed(ctx, conn, sessionID, toolCallID,
			fmt.Sprintf("Error creating terminal: %s", truncate(args.Command, 40)))
		return "", fmt.Errorf("create terminal: %w", err)
	}

	// Poll for output in background
	done := make(chan struct{})
	var outputBuilder strings.Builder
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				out, err := conn.TerminalOutput(ctx, acp.TerminalOutputRequest{
					SessionId:  sessionID,
					TerminalId: termResp.TerminalId,
				})
				if err != nil {
					slog.Debug("terminal output poll error", "error", err)
					continue
				}
				if out.Output != "" {
					outputBuilder.WriteString(out.Output)
				}
			}
		}
	}()

	// Wait for exit
	exitResp, err := conn.WaitForTerminalExit(ctx, acp.WaitForTerminalExitRequest{
		SessionId:  sessionID,
		TerminalId: termResp.TerminalId,
	})
	close(done)

	// Final output drain
	finalOut, err := conn.TerminalOutput(ctx, acp.TerminalOutputRequest{
		SessionId:  sessionID,
		TerminalId: termResp.TerminalId,
	})
	if err == nil && finalOut.Output != "" {
		outputBuilder.WriteString(finalOut.Output)
	}

	// Release terminal (best-effort)
	if _, err := conn.ReleaseTerminal(ctx, acp.ReleaseTerminalRequest{
		SessionId:  sessionID,
		TerminalId: termResp.TerminalId,
	}); err != nil {
		slog.Debug("release terminal error", "error", err)
	}

	// Build result
	output := outputBuilder.String()
	exitCode := -1
	if exitResp.ExitCode != nil {
		exitCode = *exitResp.ExitCode
	}

	result := fmt.Sprintf("exit_code: %d\n", exitCode)
	if output != "" {
		result += output
	} else {
		result += "(no output)"
	}

	notifyToolComplete(ctx, conn, sessionID, toolCallID,
		fmt.Sprintf("Completed: %s (exit %d)", truncate(args.Command, 40), exitCode),
		output, map[string]any{"exit_code": exitCode})

	return result, nil
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

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
