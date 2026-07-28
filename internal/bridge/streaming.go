package bridge

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// ProcessAgentEvent handles a single AgentEvent from eino, streaming output to the ACP client
// via SessionUpdate notifications, and accumulating final text in finalContent.
func ProcessAgentEvent(
	ctx context.Context,
	conn *acp.AgentSideConnection,
	sid string,
	event *adk.AgentEvent,
	finalContent *strings.Builder,
) error {
	mv := event.Output.MessageOutput

	if mv.IsStreaming {
		return processStreaming(ctx, conn, sid, mv, finalContent)
	}
	return processNonStreaming(ctx, conn, sid, mv, finalContent)
}

func processStreaming(
	ctx context.Context,
	conn *acp.AgentSideConnection,
	sid string,
	mv *adk.MessageVariant,
	finalContent *strings.Builder,
) error {
	msgStream := mv.MessageStream
	if msgStream == nil {
		return nil
	}

	for {
		chunk, err := msgStream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		if chunk == nil {
			continue
		}

		switch mv.Role {
		case schema.Assistant:
			// Send text content as AgentMessage chunks
			if chunk.Content != "" {
				if err := conn.SessionUpdate(ctx, acp.SessionNotification{
					SessionId: acp.SessionId(sid),
					Update:    acp.UpdateAgentMessageText(chunk.Content),
				}); err != nil {
					return err
				}
				finalContent.WriteString(chunk.Content)
			}
			// Send reasoning content as AgentThought chunks
			if chunk.ReasoningContent != "" {
				if err := conn.SessionUpdate(ctx, acp.SessionNotification{
					SessionId: acp.SessionId(sid),
					Update:    acp.UpdateAgentThoughtText(chunk.ReasoningContent),
				}); err != nil {
					return err
				}
			}
		case schema.Tool:
			// Tool results are sent by the tool bridge itself via ACPBackedTool
			slog.Debug("streaming tool result", "toolName", mv.ToolName, "content", chunk.Content)
		}
	}

	return nil
}

func processNonStreaming(
	ctx context.Context,
	conn *acp.AgentSideConnection,
	sid string,
	mv *adk.MessageVariant,
	finalContent *strings.Builder,
) error {
	msg := mv.Message
	if msg == nil {
		return nil
	}

	switch mv.Role {
	case schema.Assistant:
		if msg.Content != "" {
			if err := conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: acp.SessionId(sid),
				Update:    acp.UpdateAgentMessageText(msg.Content),
			}); err != nil {
				return err
			}
			finalContent.WriteString(msg.Content)
		}
		if msg.ReasoningContent != "" {
			if err := conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: acp.SessionId(sid),
				Update:    acp.UpdateAgentThoughtText(msg.ReasoningContent),
			}); err != nil {
				return err
			}
		}
	case schema.Tool:
		// Tool results are sent by the tool bridge itself via ACPBackedTool
		slog.Debug("non-streaming tool result", "toolName", mv.ToolName, "content", msg.Content)
	}

	return nil
}
