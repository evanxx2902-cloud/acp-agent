package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/coder/acp-go-sdk"
	"github.com/cloudwego/eino/components/tool"
	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// Manager manages MCP client connections for a session.
type Manager struct {
	clients []*mcpclient.Client
}

// Connect connects to MCP servers, discovers tools, and returns eino tool wrappers.
func (m *Manager) Connect(ctx context.Context, servers []acp.McpServer) ([]tool.BaseTool, error) {
	var allTools []tool.BaseTool

	for i, server := range servers {
		if server.Stdio == nil {
			continue
		}

		cli, tools, err := connectStdio(ctx, server.Stdio)
		if err != nil {
			slog.Error("failed to connect to MCP server",
				"index", i,
				"command", server.Stdio.Command,
				"error", err,
			)
			continue
		}

		m.clients = append(m.clients, cli)
		allTools = append(allTools, tools...)
		slog.Info("connected to MCP server",
			"command", server.Stdio.Command,
			"tools", len(tools),
		)
	}

	return allTools, nil
}

func connectStdio(ctx context.Context, stdio *acp.McpServerStdio) (*mcpclient.Client, []tool.BaseTool, error) {
	// Create MCP client with stdio transport
	cli, err := mcpclient.NewStdioMCPClient(stdio.Command, nil, stdio.Args...)
	if err != nil {
		return nil, nil, fmt.Errorf("create stdio client: %w", err)
	}

	// Initialize
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{
		Name:    "acp-agent",
		Version: "0.1.0",
	}
	initReq.Params.Capabilities = mcpgo.ClientCapabilities{}

	if _, err := cli.Initialize(ctx, initReq); err != nil {
		cli.Close()
		return nil, nil, fmt.Errorf("initialize: %w", err)
	}

	// Discover tools
	toolsResp, err := cli.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		cli.Close()
		return nil, nil, fmt.Errorf("list tools: %w", err)
	}

	// Wrap as eino tools
	var einoTools []tool.BaseTool
	for _, mcpTool := range toolsResp.Tools {
		t := mcpTool
		adapter, err := NewToolAdapter(t, cli)
		if err != nil {
			slog.Warn("skipping MCP tool with bad schema", "name", t.Name, "error", err)
			continue
		}
		einoTools = append(einoTools, adapter)
	}

	return cli, einoTools, nil
}

// Close shuts down all MCP client connections.
func (m *Manager) Close() {
	for _, cli := range m.clients {
		if err := cli.Close(); err != nil {
			slog.Debug("error closing MCP client", "error", err)
		}
	}
	m.clients = nil
}
