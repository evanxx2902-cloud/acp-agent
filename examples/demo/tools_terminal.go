package main

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerTerminalTools(s *server.MCPServer) {
	s.AddTool(mcp.NewTool("run_command",
		mcp.WithDescription("Execute a shell command and return the output."),
		mcp.WithString("command", mcp.Description("Shell command to execute"), mcp.Required()),
		mcp.WithString("workdir", mcp.Description("Working directory (optional)")),
	), terminalRun)
}

func terminalRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
