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
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// ToolAdapter wraps an MCP tool as an eino tool.InvokableTool.
type ToolAdapter struct {
	info   *schema.ToolInfo
	client MCPCaller
}

// MCPCaller is the subset of MCP client methods we need for tool calls.
type MCPCaller interface {
	CallTool(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
}

// NewToolAdapter creates an eino tool from an MCP tool definition.
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

	return &ToolAdapter{
		info:   info,
		client: client,
	}, nil
}

// Info returns the eino tool metadata.
func (t *ToolAdapter) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.info, nil
}

// InvokableRun calls the MCP tool with ACP notifications and permission checks.
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

	// If ACP connection is available, notify the client and ask for permission
	if conn != nil {
		// Notify: tool call starting
		if err := conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: sid,
			Update: acp.StartToolCall(
				toolCallID,
				title,
				acp.WithStartKind(acp.ToolKindOther),
				acp.WithStartStatus(acp.ToolCallStatusPending),
				acp.WithStartRawInput(args),
			),
		}); err != nil {
			slog.Error("mcp tool start notification failed", "error", err)
		}

		// Ask permission
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
		} else if permResp.Outcome.Selected == nil {
			// Cancelled
			_ = conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: sid,
				Update: acp.UpdateToolCall(toolCallID,
					acp.WithUpdateStatus(acp.ToolCallStatusFailed),
					acp.WithUpdateTitle(title+" (cancelled)"),
				),
			})
			return "cancelled by user", nil
		} else if string(permResp.Outcome.Selected.OptionId) != "allow" {
			// Rejected
			_ = conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: sid,
				Update: acp.UpdateToolCall(toolCallID,
					acp.WithUpdateStatus(acp.ToolCallStatusFailed),
					acp.WithUpdateTitle(title+" (rejected)"),
				),
			})
			return "rejected by user", nil
		}

		// Allowed — update status to in-progress
		_ = conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: sid,
			Update: acp.UpdateToolCall(toolCallID,
				acp.WithUpdateStatus(acp.ToolCallStatusInProgress),
			),
		})
	}

	// Execute via MCP
	result, err := t.client.CallTool(ctx, mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{
			Name:      toolName,
			Arguments: args,
		},
	})

	// Notify result
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
			output := extractToolResult(result)
			_ = conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: sid,
				Update: acp.UpdateToolCall(toolCallID,
					acp.WithUpdateStatus(acp.ToolCallStatusCompleted),
					acp.WithUpdateTitle(title),
					acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.TextBlock(truncateText(output, 500)))}),
					acp.WithUpdateRawOutput(map[string]any{"result": output}),
				),
			})
		}
	}

	if err != nil {
		return "", fmt.Errorf("mcp tool call failed: %w", err)
	}
	return extractToolResult(result), nil
}

// extractToolResult extracts human-readable text from an MCP tool result.
func extractToolResult(result *mcpgo.CallToolResult) string {
	if result == nil {
		return "(empty result)"
	}

	var texts []string
	for _, content := range result.Content {
		switch c := content.(type) {
		case mcpgo.TextContent:
			texts = append(texts, c.Text)
		case mcpgo.ImageContent:
			texts = append(texts, fmt.Sprintf("[image: %s]", c.MIMEType))
		case mcpgo.EmbeddedResource:
			texts = append(texts, fmt.Sprintf("[embedded resource]"))
		default:
			texts = append(texts, fmt.Sprintf("[content: %T]", c))
		}
	}

	if len(texts) == 0 {
		if result.IsError {
			return "(tool error, no message)"
		}
		return "(ok)"
	}

	return joinNonEmpty(texts, "\n")
}

func joinNonEmpty(ss []string, sep string) string {
	var filtered []string
	for _, s := range ss {
		if s != "" {
			filtered = append(filtered, s)
		}
	}
	if len(filtered) == 0 {
		return ""
	}
	result := filtered[0]
	for _, s := range filtered[1:] {
		result += sep + s
	}
	return result
}

func mcpShortID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "deadbeef"
	}
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

func truncateText(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
