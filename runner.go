package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// =========================================================================
// runReAct executes the eino ReAct loop, streaming output to the ACP client.
// =========================================================================

func runReAct(ctx context.Context, conn *acp.AgentSideConnection, s *RuntimeSession) error {
	messages := s.Messages()

	cmAgent := s.GetAgent()
	if cmAgent == nil {
		return fmt.Errorf("agent not initialized")
	}

	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent:           cmAgent,
		EnableStreaming: true,
	})

	iter := runner.Run(ctx, messages)

	var finalContent strings.Builder
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			return fmt.Errorf("agent error: %w", event.Err)
		}
		if event.Output != nil && event.Output.MessageOutput != nil {
			if err := ProcessAgentEvent(ctx, conn, s, event, &finalContent); err != nil {
				slog.Error("failed to process agent event", "error", err)
			}
		}
	}

	responseText := finalContent.String()
	if responseText != "" {
		s.AppendMessages(schema.AssistantMessage(responseText, nil))
	}

	return nil
}

// =========================================================================
// ProcessAgentEvent — eino AgentEvent -> ACP SessionUpdate streaming
// =========================================================================

func ProcessAgentEvent(
	ctx context.Context,
	conn *acp.AgentSideConnection,
	s *RuntimeSession,
	event *adk.AgentEvent,
	finalContent *strings.Builder,
) error {
	mv := event.Output.MessageOutput
	if mv.IsStreaming {
		return processStreaming(ctx, conn, s, mv, finalContent)
	}
	return processNonStreaming(ctx, conn, s, mv, finalContent)
}

func processStreaming(
	ctx context.Context,
	conn *acp.AgentSideConnection,
	s *RuntimeSession,
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
			if chunk.Content != "" {
				if err := conn.SessionUpdate(ctx, acp.SessionNotification{
					SessionId: acp.SessionId(s.ID),
					Update:    acp.UpdateAgentMessageText(chunk.Content),
				}); err != nil {
					return err
				}
				finalContent.WriteString(chunk.Content)
			}
			if chunk.ReasoningContent != "" {
				if err := conn.SessionUpdate(ctx, acp.SessionNotification{
					SessionId: acp.SessionId(s.ID),
					Update:    acp.UpdateAgentThoughtText(chunk.ReasoningContent),
				}); err != nil {
					return err
				}
			}
			if len(chunk.ToolCalls) > 0 {
				s.AppendMessages(chunk)
			}
		case schema.Tool:
			s.AppendMessages(compactToolMsg(chunk))
		}
	}
	return nil
}

func processNonStreaming(
	ctx context.Context,
	conn *acp.AgentSideConnection,
	s *RuntimeSession,
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
				SessionId: acp.SessionId(s.ID),
				Update:    acp.UpdateAgentMessageText(msg.Content),
			}); err != nil {
				return err
			}
			finalContent.WriteString(msg.Content)
		}
		if msg.ReasoningContent != "" {
			if err := conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: acp.SessionId(s.ID),
				Update:    acp.UpdateAgentThoughtText(msg.ReasoningContent),
			}); err != nil {
				return err
			}
		}
		if len(msg.ToolCalls) > 0 {
			s.AppendMessages(msg)
		}
	case schema.Tool:
		s.AppendMessages(compactToolMsg(msg))
	}
	return nil
}

// compactToolMsg creates a lightweight tool message: only name + status, no content.
func compactToolMsg(msg *schema.Message) *schema.Message {
	return &schema.Message{
		Role:       schema.Tool,
		ToolCallID: msg.ToolCallID,
		ToolName:   msg.ToolName,
		Content:    "(completed)",
	}
}
