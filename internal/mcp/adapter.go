package mcp

import (
	"context"
	"encoding/json"
	"fmt"

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

	// Convert MCP input schema to eino ParamsOneOf
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

// InvokableRun calls the MCP tool and returns the result as a string.
func (t *ToolAdapter) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	var args map[string]any
	if argumentsInJSON != "" && argumentsInJSON != "{}" {
		if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
			return "", fmt.Errorf("invalid tool arguments: %w", err)
		}
	}

	result, err := t.client.CallTool(ctx, mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{
			Name:      t.info.Name[5:], // strip "mcp__" prefix
			Arguments: args,
		},
	})
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
			texts = append(texts, fmt.Sprintf("[image: %s, mime: %s]", c.Data, c.MIMEType))
		case mcpgo.EmbeddedResource:
			texts = append(texts, fmt.Sprintf("[embedded resource: %+v]", c.Resource))
		default:
			texts = append(texts, fmt.Sprintf("[unknown content type: %T]", c))
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
