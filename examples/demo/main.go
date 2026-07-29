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
	connect := flag.String("connect", "", "Connect to running agent: tcp://host:port or unix:///path/to/sock")
	sysPrompt := flag.String("system-prompt", "", "Override system prompt for the session")
	mode := flag.String("mode", "agent", "Session mode: 'agent' or 'plan'")
	maxIter := flag.Int("max-iter", 0, "Override max ReAct iterations (0=use default)")
	flag.Parse()
	runClient(*autoYes, *connect, *sysPrompt, *mode, *maxIter)
}

// =========================================================================
// ACP Client
// =========================================================================

func runClient(autoYes bool, connectAddr string, sysPrompt string, mode string, maxIter int) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	defer cancel()

	selfPath := selfExe()

	fmt.Print("=== ACP Agent Demo ===\n\n")
	if autoYes {
		fmt.Println("Auto-yes: enabled")
	}
	if maxIter > 0 {
		fmt.Printf("Max-iter: %d\n", maxIter)
	}
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
	if sysPrompt != "" || maxIter > 0 {
		sessReq.Meta = make(map[string]any)
		if sysPrompt != "" {
			sessReq.Meta["system_prompt"] = sysPrompt
		}
		if maxIter > 0 {
			sessReq.Meta["max_iterations"] = float64(maxIter)
		}
	}
	newSess, err := conn.NewSession(ctx, sessReq)
	if err != nil {
		die("newSession", err)
	}
	fmt.Printf("Session: %s\n", newSess.SessionId)
	if mode == "plan" {
		conn.SetSessionMode(ctx, acp.SetSessionModeRequest{
			SessionId: newSess.SessionId,
			ModeId:    acp.SessionModeId(mode),
		})
		fmt.Printf("Mode:     plan\n")
	}
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	turn := 0
	for {
		fmt.Print("> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "quit" || line == "exit" {
			break
		}

		// Client-side commands
		switch {
		case line == "/help":
			fmt.Print("\nCommands: /help  /tools  /quit\n")
			fmt.Print("Multi-line: end a line with \\ to continue.\n\n")
			continue
		case line == "/tools":
			fmt.Print("\nTools are provided by MCP servers — use the LLM to discover them.\n\n")
			continue
		}

		// Multi-line input: trailing \ appends next line
		for strings.HasSuffix(line, "\\") {
			line = line[:len(line)-1] + "\n"
			fmt.Print("  ")
			next, _ := reader.ReadString('\n')
			line += next
		}

		if turn > 0 {
			fmt.Println("\n───")
		}
		fmt.Println()
		turn++

		_, err = conn.Prompt(ctx, acp.PromptRequest{
			SessionId: newSess.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock(line)},
		})
		if err != nil {
			errMsg := fmt.Sprintf("%v", err)
			if re, ok := err.(*acp.RequestError); ok {
				errMsg = re.Message
				if re.Data != nil {
					if s, ok := re.Data.(map[string]any); ok {
						if e, ok := s["error"].(string); ok && e != "" {
							errMsg = e
						}
					}
				}
			}
			fmt.Fprintf(os.Stderr, "\n✗ %s\n", errMsg)
		}
		fmt.Println()
	}
}

type demoClient struct {
	autoYes       bool
	thinkingStart time.Time
	thinking      bool
	lastToolTitle string // track current tool for result display
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
		fmt.Printf("\r\033[K  thinking ... (%v)", elapsed)
	case u.ToolCall != nil:
		c.clearThinking()
		kind := ""
		if u.ToolCall.Kind != "" {
			kind = fmt.Sprintf(" [%s]", string(u.ToolCall.Kind))
		}
		c.lastToolTitle = u.ToolCall.Title
		fmt.Printf("\n🔧 %s%s", u.ToolCall.Title, kind)
	case u.ToolCallUpdate != nil:
		status := "?"
		if u.ToolCallUpdate.Status != nil {
			status = string(*u.ToolCallUpdate.Status)
		}
		switch status {
		case "pending":
		case "in_progress":
			fmt.Print(" ...")
		case "completed":
			fmt.Print(" ✓")
			if ro, ok := u.ToolCallUpdate.RawOutput.(map[string]any); ok {
				if s, _ := ro["result"].(string); s != "" {
					fmt.Printf(" (%s)", summarizeLine(s, 60))
				}
			}
		case "failed":
			fmt.Print(" ✗")
		}
		if status != "pending" {
			fmt.Println()
		}
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
	}
	return nil
}

func summarizeLine(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	// Take first line only
	if idx := strings.Index(s, "\n"); idx >= 0 {
		s = s[:idx]
	}
	if len(s) > maxLen {
		s = s[:maxLen-1] + "…"
	}
	return s
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
