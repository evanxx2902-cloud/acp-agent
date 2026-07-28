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
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	autoYes := flag.Bool("y", false, "Auto-approve all tool permissions")
	flag.Parse()

	if flag.Arg(0) == "--mcp-server" {
		runMCPServer()
		return
	}
	runDemo(*autoYes)
}

// =========================================================================
// Mode 1: MCP Server (--mcp-server)
// =========================================================================

func runMCPServer() {
	s := server.NewMCPServer("demo-mcp-server", "1.0.0",
		server.WithToolCapabilities(true),
		server.WithRecovery(),
	)

	registerTools(s)

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "MCP server error: %v\n", err)
		os.Exit(1)
	}
}

func registerTools(s *server.MCPServer) {
	// Filesystem tools
	s.AddTool(mcp.NewTool("read_file",
		mcp.WithDescription("Read the contents of a file. Returns the file content as text."),
		mcp.WithString("path", mcp.Description("Absolute path to the file to read"), mcp.Required()),
		mcp.WithNumber("offset", mcp.Description("Line number to start reading from (1-based)")),
		mcp.WithNumber("limit", mcp.Description("Maximum number of lines to read")),
	), handleReadFile)

	s.AddTool(mcp.NewTool("write_file",
		mcp.WithDescription("Write or overwrite a file with the given content."),
		mcp.WithString("path", mcp.Description("Absolute path to the file"), mcp.Required()),
		mcp.WithString("content", mcp.Description("Content to write"), mcp.Required()),
	), handleWriteFile)

	s.AddTool(mcp.NewTool("list_directory",
		mcp.WithDescription("List the contents of a directory. Returns file names, sizes, types, and permissions."),
		mcp.WithString("path", mcp.Description("Absolute path to the directory"), mcp.Required()),
	), handleListDirectory)

	s.AddTool(mcp.NewTool("make_directory",
		mcp.WithDescription("Create a new directory, including any necessary parent directories."),
		mcp.WithString("path", mcp.Description("Absolute path to create"), mcp.Required()),
	), handleMakeDirectory)

	s.AddTool(mcp.NewTool("move_file",
		mcp.WithDescription("Move or rename a file or directory."),
		mcp.WithString("source", mcp.Description("Source path"), mcp.Required()),
		mcp.WithString("destination", mcp.Description("Destination path"), mcp.Required()),
	), handleMoveFile)

	s.AddTool(mcp.NewTool("delete_file",
		mcp.WithDescription("Delete a file or directory. Directories are removed recursively."),
		mcp.WithString("path", mcp.Description("Path to delete"), mcp.Required()),
	), handleDeleteFile)

	s.AddTool(mcp.NewTool("get_file_info",
		mcp.WithDescription("Get metadata about a file or directory: size, permissions, modification time."),
		mcp.WithString("path", mcp.Description("Absolute path"), mcp.Required()),
	), handleGetFileInfo)

	s.AddTool(mcp.NewTool("search_files",
		mcp.WithDescription("Search for files matching a glob pattern in a directory."),
		mcp.WithString("directory", mcp.Description("Directory to search in"), mcp.Required()),
		mcp.WithString("pattern", mcp.Description("Glob pattern (e.g., '*.go', '**/*.md')"), mcp.Required()),
	), handleSearchFiles)

	// Terminal tool
	s.AddTool(mcp.NewTool("run_command",
		mcp.WithDescription("Execute a shell command and return the output."),
		mcp.WithString("command", mcp.Description("Shell command to execute"), mcp.Required()),
		mcp.WithString("workdir", mcp.Description("Working directory (optional)")),
	), handleRunCommand)

	// Database tools
	dbPool := connectDB()
	s.AddTool(mcp.NewTool("query_database",
		mcp.WithDescription("Execute a read-only SQL query against the PostgreSQL database and return results."),
		mcp.WithString("sql", mcp.Description("SQL SELECT query to execute"), mcp.Required()),
	), makeHandleQueryDB(dbPool))

	s.AddTool(mcp.NewTool("list_database_tables",
		mcp.WithDescription("List all tables in the PostgreSQL database with their schemas."),
	), makeHandleListTables(dbPool))
}

// =========================================================================
// Database
// =========================================================================

func connectDB() *pgxpool.Pool {
	password, err := os.ReadFile("/run/secure/service")
	if err != nil {
		fmt.Fprintf(os.Stderr, "MCP server: failed to read DB password: %v (db tools disabled)\n", err)
		return nil
	}
	pass := strings.TrimSpace(string(password))

	// Connect via Unix socket at /tmp
	connStr := fmt.Sprintf("host=/tmp user=pam password=%s dbname=postgres sslmode=disable", pass)
	pool, err := pgxpool.New(context.Background(), connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "MCP server: failed to connect to DB: %v (db tools disabled)\n", err)
		return nil
	}
	return pool
}

func makeHandleQueryDB(pool *pgxpool.Pool) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if pool == nil {
			return mcp.NewToolResultError("database not available"), nil
		}
		sql := req.GetString("sql", "")
		if sql == "" {
			return mcp.NewToolResultError("sql is required"), nil
		}

		rows, err := pool.Query(ctx, sql)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("query failed: %v", err)), nil
		}
		defer rows.Close()

		// Build table output
		descriptions := rows.FieldDescriptions()
		var headers []string
		for _, d := range descriptions {
			headers = append(headers, string(d.Name))
		}

		var lines []string
		lines = append(lines, strings.Join(headers, "\t"))

		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				continue
			}
			var cols []string
			for _, v := range vals {
				cols = append(cols, fmt.Sprintf("%v", v))
			}
			lines = append(lines, strings.Join(cols, "\t"))
		}

		if len(lines) <= 1 {
			return mcp.NewToolResultText("(empty result)"), nil
		}
		return mcp.NewToolResultText(strings.Join(lines, "\n")), nil
	}
}

func makeHandleListTables(pool *pgxpool.Pool) func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if pool == nil {
			return mcp.NewToolResultError("database not available"), nil
		}
		rows, err := pool.Query(ctx, `
			SELECT table_schema, table_name
			FROM information_schema.tables
			WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
			ORDER BY table_schema, table_name
		`)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("list tables failed: %v", err)), nil
		}
		defer rows.Close()

		var lines []string
		for rows.Next() {
			var schema, name string
			rows.Scan(&schema, &name)
			lines = append(lines, fmt.Sprintf("%s.%s", schema, name))
		}
		if len(lines) == 0 {
			return mcp.NewToolResultText("(no tables)"), nil
		}
		return mcp.NewToolResultText(strings.Join(lines, "\n")), nil
	}
}

// =========================================================================
// Tool Handlers
// =========================================================================

func handleReadFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	data, err := os.ReadFile(path)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read %s: %v", path, err)), nil
	}
	lines := strings.Split(string(data), "\n")
	args := req.GetArguments()
	offset := getIntArg(args, "offset", 1) - 1
	if offset < 0 {
		offset = 0
	}
	if offset >= len(lines) {
		return mcp.NewToolResultText(""), nil
	}
	lines = lines[offset:]
	limit := getIntArg(args, "limit", 0)
	if limit > 0 && limit < len(lines) {
		lines = lines[:limit]
	}
	return mcp.NewToolResultText(strings.Join(lines, "\n")), nil
}

func handleWriteFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	content := req.GetString("content", "")
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("mkdir %s: %v", dir, err)), nil
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("write %s: %v", path, err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Wrote %d bytes to %s", len(content), path)), nil
}

func handleListDirectory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	entries, err := os.ReadDir(path)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read dir %s: %v", path, err)), nil
	}
	var lines []string
	for _, e := range entries {
		info, _ := e.Info()
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		size := ""
		mode := ""
		if info != nil {
			size = fmt.Sprintf("%8d", info.Size())
			mode = info.Mode().String()
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", mode, size, name))
	}
	if len(lines) == 0 {
		return mcp.NewToolResultText("(empty directory)"), nil
	}
	return mcp.NewToolResultText(strings.Join(lines, "\n")), nil
}

func handleMakeDirectory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	if err := os.MkdirAll(path, 0755); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("mkdir %s: %v", path, err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Created directory %s", path)), nil
}

func handleMoveFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	src := req.GetString("source", "")
	dst := req.GetString("destination", "")
	if err := os.Rename(src, dst); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("move %s -> %s: %v", src, dst, err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Moved %s to %s", src, dst)), nil
}

func handleDeleteFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	info, err := os.Stat(path)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("stat %s: %v", path, err)), nil
	}
	if info.IsDir() {
		err = os.RemoveAll(path)
	} else {
		err = os.Remove(path)
	}
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("delete %s: %v", path, err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Deleted %s", path)), nil
}

func handleGetFileInfo(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := req.GetString("path", "")
	info, err := os.Stat(path)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("stat %s: %v", path, err)), nil
	}
	result := fmt.Sprintf("Path: %s\nSize: %d bytes\nMode: %s\nIsDir: %v\nModTime: %s",
		path, info.Size(), info.Mode().String(), info.IsDir(),
		info.ModTime().Format(time.RFC3339))
	return mcp.NewToolResultText(result), nil
}

func handleSearchFiles(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	dir := req.GetString("directory", "")
	pattern := req.GetString("pattern", "")
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("glob %s: %v", pattern, err)), nil
	}
	if len(matches) == 0 {
		return mcp.NewToolResultText("(no matches)"), nil
	}
	return mcp.NewToolResultText(strings.Join(matches, "\n")), nil
}

func handleRunCommand(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	command := req.GetString("command", "")
	workdir := req.GetString("workdir", "")

	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	if workdir != "" {
		cmd.Dir = workdir
	}
	output, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return mcp.NewToolResultError(fmt.Sprintf("run command: %v", err)), nil
		}
	}
	return mcp.NewToolResultText(fmt.Sprintf("exit_code: %d\n%s", exitCode, string(output))), nil
}

// =========================================================================
// Mode 2: Demo Client
// =========================================================================

func runDemo(autoYes bool) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	defer cancel()

	selfPath, _ := os.Executable()
	if selfPath == "" {
		selfPath, _ = filepath.Abs(os.Args[0])
	}

	agentPath := findAgentBinary()

	fmt.Println("=== ACP Agent Demo ===\n")
	fmt.Printf("Agent:    %s\n", agentPath)
	fmt.Printf("MCP:      %s --mcp-server\n", selfPath)
	if autoYes {
		fmt.Println("Auto-yes: enabled\n")
	} else {
		fmt.Println()
	}

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
	fmt.Printf("Connected to agent (protocol v%v)\n\n", initResp.ProtocolVersion)

	newSess, err := conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd: mustCwd(),
		McpServers: []acp.McpServer{
			{
				Stdio: &acp.McpServerStdio{
					Command: selfPath,
					Args:    []string{"--mcp-server"},
				},
			},
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

		client.reset()
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
	output  strings.Builder
	autoYes bool
}

func (c *demoClient) reset() { c.output.Reset() }

func (c *demoClient) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	u := params.Update
	switch {
	case u.AgentMessageChunk != nil:
		text := u.AgentMessageChunk.Content
		if text.Text != nil {
			fmt.Print(text.Text.Text)
		}
	case u.AgentThoughtChunk != nil:
		text := u.AgentThoughtChunk.Content
		if text.Text != nil {
			fmt.Printf("\n[thought] %s", text.Text.Text)
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
		fmt.Println("✓ Auto-allowed (--yes)\n")
		return acp.RequestPermissionResponse{
			Outcome: acp.NewRequestPermissionOutcomeSelected("allow"),
		}, nil
	}

	fmt.Print("Allow? [y/N] ")
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))

	if line == "y" || line == "yes" {
		fmt.Println("✓ Allowed\n")
		return acp.RequestPermissionResponse{
			Outcome: acp.NewRequestPermissionOutcomeSelected("allow"),
		}, nil
	}

	fmt.Println("✗ Rejected\n")
	return acp.RequestPermissionResponse{
		Outcome: acp.NewRequestPermissionOutcomeSelected("reject"),
	}, nil
}

// Stub implementations
func (c *demoClient) ReadTextFile(ctx context.Context, params acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, fmt.Errorf("not implemented (use MCP tools)")
}
func (c *demoClient) WriteTextFile(ctx context.Context, params acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, fmt.Errorf("not implemented (use MCP tools)")
}
func (c *demoClient) CreateTerminal(ctx context.Context, params acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, fmt.Errorf("not implemented (use MCP tools)")
}
func (c *demoClient) TerminalOutput(ctx context.Context, params acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, nil
}
func (c *demoClient) ReleaseTerminal(ctx context.Context, params acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, nil
}
func (c *demoClient) WaitForTerminalExit(ctx context.Context, params acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}
func (c *demoClient) KillTerminal(ctx context.Context, params acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, nil
}

// --- Helpers ---

func findAgentBinary() string {
	candidates := []string{"./agent-server", "../agent-server", "./acp", "../acp"}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	fmt.Println("Building agent...")
	cmd := exec.Command("go", "build", "-o", "/tmp/agent-server", ".")
	cmd.Dir = findProjectRoot()
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "Build failed: %v\n%s\n", err, string(out))
		os.Exit(1)
	}
	return "/tmp/agent-server"
}

func findProjectRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

func mustCwd() string {
	wd, _ := os.Getwd()
	return wd
}

func die(method string, err error) {
	if re, ok := err.(*acp.RequestError); ok {
		b, _ := json.MarshalIndent(re, "", "  ")
		fmt.Fprintf(os.Stderr, "[%s] %s\n", method, string(b))
	} else {
		fmt.Fprintf(os.Stderr, "[%s] %v\n", method, err)
	}
	os.Exit(1)
}

func getIntArg(args map[string]any, key string, defaultVal int) int {
	v, ok := args[key]
	if !ok {
		return defaultVal
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return defaultVal
}
