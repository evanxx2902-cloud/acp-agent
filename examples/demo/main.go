package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	"github.com/coder/acp-go-sdk"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--mcp-server" {
		runMCPServer()
		return
	}

	autoYes := flag.Bool("y", false, "Auto-approve all tool permissions")
	flag.Parse()
	runClient(*autoYes)
}

// =========================================================================
// ACP Client
// =========================================================================

func runClient(autoYes bool) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	defer cancel()

	selfPath := selfExe()
	agentPath := findAgentBinary()

	fmt.Print("=== ACP Agent Demo ===\n\n")
	fmt.Printf("Agent:    %s\n", agentPath)
	fmt.Printf("MCP:      %s --mcp-server\n", selfPath)
	if autoYes {
		fmt.Println("Auto-yes: enabled")
	}
	fmt.Println()

	agentCmd := exec.CommandContext(ctx, agentPath, "-config", "config.json")
	agentCmd.Stderr = os.Stderr
	agentIn, _ := agentCmd.StdinPipe()
	agentOut, _ := agentCmd.StdoutPipe()
	if err := agentCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start agent: %v\n", err)
		os.Exit(1)
	}
	defer agentCmd.Process.Kill()

	client := &demoClient{autoYes: autoYes}
	conn := acp.NewClientSideConnection(client, agentIn, agentOut)

	initResp, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Fs:       acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
			Terminal: true,
		},
	})
	if err != nil {
		die("initialize", err)
	}
	fmt.Printf("Connected (protocol v%v)\n\n", initResp.ProtocolVersion)

	newSess, err := conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd: mustCwd(),
		McpServers: []acp.McpServer{
			{Stdio: &acp.McpServerStdio{Command: selfPath, Args: []string{"--mcp-server"}}},
		},
	})
	if err != nil {
		die("newSession", err)
	}
	fmt.Printf("Session: %s\n\n", newSess.SessionId)

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" || line == "quit" || line == "exit" {
			break
		}

		fmt.Println()
		_, err = conn.Prompt(ctx, acp.PromptRequest{
			SessionId: newSess.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock(line)},
		})
		if err != nil {
			if re, ok := err.(*acp.RequestError); ok {
				b, _ := json.MarshalIndent(re, "", "  ")
				fmt.Fprintf(os.Stderr, "Error: %s\n", string(b))
			} else {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			}
		}
		fmt.Println()
	}
}

type demoClient struct {
	autoYes bool
}

func (c *demoClient) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	u := params.Update
	switch {
	case u.AgentMessageChunk != nil:
		if t := u.AgentMessageChunk.Content; t.Text != nil {
			fmt.Print(t.Text.Text)
		}
	case u.AgentThoughtChunk != nil:
		if t := u.AgentThoughtChunk.Content; t.Text != nil {
			fmt.Printf("\n[thought] %s", t.Text.Text)
		}
	case u.ToolCall != nil:
		kind := ""
		if u.ToolCall.Kind != "" {
			kind = fmt.Sprintf(" [%s]", string(u.ToolCall.Kind))
		}
		fmt.Printf("\n🔧 %s%s\n", u.ToolCall.Title, kind)
	case u.ToolCallUpdate != nil:
		status := "?"
		if u.ToolCallUpdate.Status != nil {
			status = string(*u.ToolCallUpdate.Status)
		}
		switch status {
		case "pending":
		case "in_progress":
			fmt.Print("  ⏳ running...")
		case "completed":
			fmt.Print("  ✅ done")
		case "failed":
			fmt.Print("  ❌ failed/rejected")
		}
		fmt.Println()
	}
	return nil
}

func (c *demoClient) RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	title := ""
	if params.ToolCall.Title != nil {
		title = *params.ToolCall.Title
	}
	fmt.Printf("\n┌─ Tool Permission ───────────────────┐\n")
	fmt.Printf("│ %s\n", title)
	if params.ToolCall.RawInput != nil {
		b, _ := json.Marshal(params.ToolCall.RawInput)
		fmt.Printf("│ args: %s\n", string(b))
	}
	fmt.Printf("└──────────────────────────────────────┘\n")

	if c.autoYes {
		fmt.Print("✓ Auto-allowed (--yes)\n\n")
		return acp.RequestPermissionResponse{
			Outcome: acp.NewRequestPermissionOutcomeSelected("allow"),
		}, nil
	}

	fmt.Print("Allow? [y/N] ")
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))

	if line == "y" || line == "yes" {
		fmt.Print("✓ Allowed\n\n")
		return acp.RequestPermissionResponse{
			Outcome: acp.NewRequestPermissionOutcomeSelected("allow"),
		}, nil
	}

	fmt.Print("✗ Rejected\n\n")
	return acp.RequestPermissionResponse{
		Outcome: acp.NewRequestPermissionOutcomeSelected("reject"),
	}, nil
}

// Stubs — agent uses MCP tools, these are no longer called.
func (c *demoClient) ReadTextFile(ctx context.Context, p acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, fmt.Errorf("not implemented")
}
func (c *demoClient) WriteTextFile(ctx context.Context, p acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, fmt.Errorf("not implemented")
}
func (c *demoClient) CreateTerminal(ctx context.Context, p acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, fmt.Errorf("not implemented")
}
func (c *demoClient) TerminalOutput(ctx context.Context, p acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, nil
}
func (c *demoClient) ReleaseTerminal(ctx context.Context, p acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, nil
}
func (c *demoClient) WaitForTerminalExit(ctx context.Context, p acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}
func (c *demoClient) KillTerminal(ctx context.Context, p acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, nil
}
