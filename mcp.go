package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// =========================================================================
// MCP Client Manager — connect to MCP servers, discover tools
// =========================================================================

type Manager struct {
	clients []*mcpclient.Client
}

func (m *Manager) Connect(ctx context.Context, servers []acp.McpServer) ([]tool.BaseTool, error) {
	var allTools []tool.BaseTool

	for i, server := range servers {
		cli, tools, err := connectOne(ctx, i, server)
		if err != nil {
			slog.Error("failed to connect to MCP server", "index", i, "error", err)
			continue
		}
		m.clients = append(m.clients, cli)
		allTools = append(allTools, tools...)
	}

	return allTools, nil
}

func connectOne(ctx context.Context, idx int, srv acp.McpServer) (*mcpclient.Client, []tool.BaseTool, error) {
	switch {
	case srv.Stdio != nil:
		return connectStdio(ctx, srv.Stdio)
	case srv.Http != nil:
		return connectHTTP(ctx, srv.Http)
	case srv.Sse != nil:
		return connectSSE(ctx, srv.Sse)
	case srv.Acp != nil:
		return connectACP(ctx, srv.Acp)
	default:
		return nil, nil, fmt.Errorf("server %d: no supported transport", idx)
	}
}

func connectStdio(ctx context.Context, stdio *acp.McpServerStdio) (*mcpclient.Client, []tool.BaseTool, error) {
	cli, err := mcpclient.NewStdioMCPClient(stdio.Command, nil, stdio.Args...)
	if err != nil {
		return nil, nil, fmt.Errorf("create stdio client: %w", err)
	}

	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "acp-agent", Version: "0.1.0"}
	initReq.Params.Capabilities = mcpgo.ClientCapabilities{}

	if _, err := cli.Initialize(ctx, initReq); err != nil {
		cli.Close()
		return nil, nil, fmt.Errorf("initialize: %w", err)
	}

	tools, err := discoverTools(ctx, cli)
	if err != nil {
		cli.Close()
		return nil, nil, err
	}

	slog.Info("connected to MCP server (stdio)", "command", stdio.Command, "tools", len(tools))
	return cli, tools, nil
}

func connectHTTP(ctx context.Context, http *acp.McpServerHttpInline) (*mcpclient.Client, []tool.BaseTool, error) {
	cli, err := mcpclient.NewStreamableHttpClient(http.Url)
	if err != nil {
		return nil, nil, fmt.Errorf("create http client: %w", err)
	}

	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "acp-agent", Version: "0.1.0"}
	initReq.Params.Capabilities = mcpgo.ClientCapabilities{}

	if _, err := cli.Initialize(ctx, initReq); err != nil {
		cli.Close()
		return nil, nil, fmt.Errorf("initialize: %w", err)
	}

	tools, err := discoverTools(ctx, cli)
	if err != nil {
		cli.Close()
		return nil, nil, err
	}

	slog.Info("connected to MCP server (http)", "url", http.Url, "tools", len(tools))
	return cli, tools, nil
}

func connectSSE(ctx context.Context, sse *acp.McpServerSseInline) (*mcpclient.Client, []tool.BaseTool, error) {
	cli, err := mcpclient.NewSSEMCPClient(sse.Url)
	if err != nil {
		return nil, nil, fmt.Errorf("create sse client: %w", err)
	}

	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "acp-agent", Version: "0.1.0"}
	initReq.Params.Capabilities = mcpgo.ClientCapabilities{}

	if _, err := cli.Initialize(ctx, initReq); err != nil {
		cli.Close()
		return nil, nil, fmt.Errorf("initialize: %w", err)
	}

	tools, err := discoverTools(ctx, cli)
	if err != nil {
		cli.Close()
		return nil, nil, err
	}

	slog.Info("connected to MCP server (sse)", "url", sse.Url, "tools", len(tools))
	return cli, tools, nil
}

func connectACP(ctx context.Context, acpTransport *acp.McpServerAcpInline) (*mcpclient.Client, []tool.BaseTool, error) {
	// ACP transport: the MCP server is provided by an ACP component and communicates
	// over the ACP channel. Implementation requires ACP component support.
	// See: https://agentclientprotocol.com/protocol/mcp
	return nil, nil, fmt.Errorf("acp transport not yet implemented (server id: %s)", acpTransport.Id)
}

func discoverTools(ctx context.Context, cli *mcpclient.Client) ([]tool.BaseTool, error) {
	toolsResp, err := cli.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}

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

	return einoTools, nil
}

func (m *Manager) Close() {
	for _, cli := range m.clients {
		if err := cli.Close(); err != nil {
			slog.Debug("error closing MCP client", "error", err)
		}
	}
	m.clients = nil
}

// =========================================================================
// ToolAdapter — wraps MCP tool as eino BaseTool
// =========================================================================

type MCPCaller interface {
	CallTool(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
}

type ToolAdapter struct {
	info   *schema.ToolInfo
	client MCPCaller
}

func NewToolAdapter(mcpTool mcpgo.Tool, client MCPCaller) (*ToolAdapter, error) {
	info := &schema.ToolInfo{
		Name: "mcp__" + mcpTool.Name,
		Desc: mcpTool.Description,
	}

	if mcpTool.RawInputSchema != nil && len(mcpTool.RawInputSchema) > 0 {
		var js jsonschema.Schema
		if err := json.Unmarshal(mcpTool.RawInputSchema, &js); err != nil {
			return nil, fmt.Errorf("mcp tool %s: invalid input schema: %w", mcpTool.Name, err)
		}
		info.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(&js)
	}

	return &ToolAdapter{info: info, client: client}, nil
}

func (t *ToolAdapter) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.info, nil
}

func (t *ToolAdapter) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	var args map[string]any
	if argumentsInJSON != "" && argumentsInJSON != "{}" {
		if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
			return "", fmt.Errorf("invalid tool arguments: %w", err)
		}
	}

	toolName := t.info.Name[5:] // strip "mcp__" prefix
	conn, sid := getACPContext(ctx)
	toolCallID := acp.ToolCallId("mcp_" + mcpShortID())
	title := fmt.Sprintf("%s(%s)", toolName, summarizeArgs(args, 60))

	// ACP notifications + permission
	if conn != nil {
		_ = conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: sid,
			Update: acp.StartToolCall(toolCallID, title,
				acp.WithStartKind(acp.ToolKindOther),
				acp.WithStartStatus(acp.ToolCallStatusPending),
				acp.WithStartRawInput(args),
			),
		})

		permResp, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
			SessionId: sid,
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: toolCallID,
				Title:      &title,
				Kind:       acp.Ptr(acp.ToolKindOther),
				Status:     acp.Ptr(acp.ToolCallStatusPending),
				RawInput:   args,
			},
			Options: []acp.PermissionOption{
				{Kind: acp.PermissionOptionKindAllowOnce, Name: "Allow", OptionId: acp.PermissionOptionId("allow")},
				{Kind: acp.PermissionOptionKindRejectOnce, Name: "Reject", OptionId: acp.PermissionOptionId("reject")},
			},
		})
		if err != nil {
			slog.Error("permission request failed", "error", err)
		} else if permResp.Outcome.Selected == nil || string(permResp.Outcome.Selected.OptionId) != "allow" {
			_ = conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: sid,
				Update: acp.UpdateToolCall(toolCallID,
					acp.WithUpdateStatus(acp.ToolCallStatusFailed),
					acp.WithUpdateTitle(title+" (rejected)"),
				),
			})
			return "rejected by user", nil
		}

		_ = conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: sid,
			Update:    acp.UpdateToolCall(toolCallID, acp.WithUpdateStatus(acp.ToolCallStatusInProgress)),
		})
	}

	// Execute via MCP
	result, err := t.client.CallTool(ctx, mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{Name: toolName, Arguments: args},
	})

	if conn != nil {
		if err != nil {
			_ = conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: sid,
				Update: acp.UpdateToolCall(toolCallID,
					acp.WithUpdateStatus(acp.ToolCallStatusFailed),
					acp.WithUpdateTitle(title+" (failed)"),
				),
			})
		} else {
			output := extractResult(result)
			_ = conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: sid,
				Update: acp.UpdateToolCall(toolCallID,
					acp.WithUpdateStatus(acp.ToolCallStatusCompleted),
					acp.WithUpdateTitle(title),
					acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.TextBlock(trunc(output, 500)))}),
					acp.WithUpdateRawOutput(map[string]any{"result": output}),
				),
			})
		}
	}

	if err != nil {
		return "", fmt.Errorf("mcp tool call failed: %w", err)
	}
	return extractResult(result), nil
}

func extractResult(r *mcpgo.CallToolResult) string {
	if r == nil {
		return "(empty)"
	}
	var texts []string
	for _, c := range r.Content {
		switch c := c.(type) {
		case mcpgo.TextContent:
			texts = append(texts, c.Text)
		case mcpgo.ImageContent:
			texts = append(texts, "[image]")
		default:
			texts = append(texts, fmt.Sprintf("[%T]", c))
		}
	}
	if len(texts) == 0 {
		if r.IsError {
			return "(error)"
		}
		return "(ok)"
	}
	return strings.Join(texts, "\n")
}

func mcpShortID() string {
	var b [4]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func summarizeArgs(args map[string]any, maxLen int) string {
	if len(args) == 0 {
		return ""
	}
	var parts []string
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 40 {
			s = s[:37] + "..."
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, s))
	}
	joined := strings.Join(parts, ", ")
	if len(joined) > maxLen {
		return joined[:maxLen-3] + "..."
	}
	return joined
}

func trunc(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
