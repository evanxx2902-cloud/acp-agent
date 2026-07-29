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
	"time"

	"github.com/coder/acp-go-sdk"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--mcp-server" {
		runMCPServer()
		return
	}

	autoYes := flag.Bool("y", false, "Auto-approve all tool permissions")
	mode := flag.String("mode", "agent", "Session mode: 'agent' or 'plan'")
	connect := flag.String("connect", "", "Connect to running agent: tcp://host:port or unix:///path/to/sock")
	sysPrompt := flag.String("system-prompt", "", "Override system prompt for the session")
	flag.Parse()
	runClient(*autoYes, *mode, *connect, *sysPrompt)
}

// =========================================================================
// ACP Client
// =========================================================================

func runClient(autoYes bool, mode string, connectAddr string, sysPrompt string) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	defer cancel()

	selfPath := selfExe()

	fmt.Print("=== ACP Agent Demo ===\n\n")
	if autoYes {
		fmt.Println("Auto-yes: enabled")
	}
	fmt.Printf("Mode:     %s\n\n", mode)

	client := &demoClient{autoYes: autoYes}

	var conn *acp.ClientSideConnection
	if connectAddr != "" {
		// Connect to running agent via TCP or Unix socket
		c, err := dialAgent(connectAddr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to connect: %v\n", err)
			os.Exit(1)
		}
		defer c.Close()
		fmt.Printf("Connected to agent at %s\n\n", connectAddr)
		conn = acp.NewClientSideConnection(client, c, c)
	} else {
		// Spawn agent as child process (stdio)
		agentPath := findAgentBinary()
		fmt.Printf("Agent:    %s\n", agentPath)
		fmt.Printf("MCP:      %s --mcp-server\n", selfPath)

		agentCmd := exec.CommandContext(ctx, agentPath, "-config", "config.json")
		agentCmd.Stderr = os.Stderr
		agentIn, _ := agentCmd.StdinPipe()
		agentOut, _ := agentCmd.StdoutPipe()
		if err := agentCmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to start agent: %v\n", err)
			os.Exit(1)
		}
		defer agentCmd.Process.Kill()
		fmt.Println()
		conn = acp.NewClientSideConnection(client, agentIn, agentOut)
	}

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

	sessReq := acp.NewSessionRequest{
		Cwd: mustCwd(),
		McpServers: []acp.McpServer{
			{Stdio: &acp.McpServerStdio{Command: selfPath, Args: []string{"--mcp-server"}}},
		},
	}
	if sysPrompt != "" {
		sessReq.Meta = map[string]any{"system_prompt": sysPrompt}
	}
	newSess, err := conn.NewSession(ctx, sessReq)
	if err != nil {
		die("newSession", err)
	}
	fmt.Printf("Session: %s\n\n", newSess.SessionId)

	if mode != "agent" {
		conn.SetSessionMode(ctx, acp.SetSessionModeRequest{
			SessionId: newSess.SessionId,
			ModeId:    acp.SessionModeId(mode),
		})
	}

	reader := bufio.NewReader(os.Stdin)
	for {
		if mode == "plan" {
			fmt.Print("[plan] > ")
		} else {
			fmt.Print("> ")
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" || line == "quit" || line == "exit" {
			break
		}

		fmt.Println()
		if mode == "plan" && (line == "g" || line == "go") {
			_, err = conn.ResumeSession(ctx, acp.ResumeSessionRequest{
				SessionId: newSess.SessionId,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "resume error: %v\n", err)
			}
			fmt.Println()
			continue
		}

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
	autoYes       bool
	thinkingStart time.Time
	thinking      bool
}

func (c *demoClient) clearThinking() {
	if c.thinking {
		fmt.Print("\r\033[K") // clear current line
		c.thinking = false
	}
}

func (c *demoClient) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	u := params.Update
	switch {
	case u.AgentMessageChunk != nil:
		c.clearThinking()
		if t := u.AgentMessageChunk.Content; t.Text != nil {
			fmt.Print(t.Text.Text)
		}
	case u.AgentThoughtChunk != nil:
		if !c.thinking {
			c.thinkingStart = time.Now()
			c.thinking = true
		}
		elapsed := time.Since(c.thinkingStart).Round(time.Second)
		// In-place refresh: \r goes to start of line, \033[K clears to end
		fmt.Printf("\r\033[K  thinking ... (%v)", elapsed)
	case u.ToolCall != nil:
		c.clearThinking()
		kind := ""
		if u.ToolCall.Kind != "" {
			kind = fmt.Sprintf(" [%s]", string(u.ToolCall.Kind))
		}
		fmt.Printf("\n🔧 %s%s\n", u.ToolCall.Title, kind)
	case u.Plan != nil:
		c.clearThinking()
		fmt.Println("\n📋 Plan:")
		for _, entry := range u.Plan.Entries {
			icon := "○"
			switch entry.Status {
			case "in_progress":
				icon = "◉"
			case "completed":
				icon = "●"
			}
			fmt.Printf("  %s %s\n", icon, entry.Content)
		}
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
