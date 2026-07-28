package main

import (
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/server"
)

func runMCPServer() {
	s := server.NewMCPServer("demo-mcp-server", "1.0.0",
		server.WithToolCapabilities(true),
		server.WithRecovery(),
	)

	registerAllTools(s)

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "MCP server error: %v\n", err)
		os.Exit(1)
	}
}

// registerAllTools is the central registry. Add new tool categories here.
func registerAllTools(s *server.MCPServer) {
	registerFSTools(s)
	registerTerminalTools(s)
	registerDBTools(s)
}
