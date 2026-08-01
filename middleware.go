package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/adk/middlewares/plantask"
	"github.com/cloudwego/eino/adk/middlewares/summarization"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
)

// =========================================================================
// buildSessionAgent constructs a ChatModelAgent with middleware.
// =========================================================================

func buildSessionAgent(ctx context.Context, chatModel model.ToolCallingChatModel, tools []tool.BaseTool,
	session *RuntimeSession, contextWindow int) (*adk.ChatModelAgent, error) {

	toolsConfig := adk.ToolsConfig{}
	toolsConfig.Tools = tools

	triggerTokens := int(float64(contextWindow) * session.SummarizationTriggerRatio)
	if triggerTokens <= 0 {
		triggerTokens = int(float64(contextWindow) * 0.8)
	}

	// Summarization middleware
	sumMW, err := summarization.New(ctx, &summarization.Config{
		Model: chatModel,
		Trigger: &summarization.TriggerCondition{
			ContextTokens:   triggerTokens,
			ContextMessages: 100,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create summarization: %w", err)
	}

	// Plan-task middleware
	planMW, err := plantask.New(ctx, &plantask.Config{
		Backend: newTaskFS(),
		BaseDir: "/tasks",
	})
	if err != nil {
		return nil, fmt.Errorf("create plantask: %w", err)
	}

	// System prompt is the first message (seq=0, role=system)
	instruction := ""
	msgs := session.Messages()
	if len(msgs) > 0 && msgs[0].Role == "system" {
		instruction = msgs[0].Content
	}

	// Mode hint: plan mode gets additional instruction prefix
	if session.Mode == "plan" {
		instruction = "You are in PLANNING mode. " +
			"Before executing any tools, first use TaskCreate to break the user's request into clear steps. " +
			"Present the complete plan to the user. " +
			"Only begin executing when the user explicitly confirms to proceed.\n\n" + instruction
	}

	return adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:          "eino-agent",
		Description:   "A general-purpose AI agent. Tools are provided by the client via MCP servers.",
		Instruction:   instruction,
		Model:         chatModel,
		ToolsConfig:   toolsConfig,
		MaxIterations: session.MaxIterations,
		Handlers:      []adk.ChatModelAgentMiddleware{sumMW, planMW},
	})
}

// =========================================================================
// In-memory task filesystem for plantask middleware
// =========================================================================

type taskFS struct {
	mu    sync.Mutex
	files map[string]string
}

func newTaskFS() *taskFS {
	return &taskFS{files: make(map[string]string)}
}

func (t *taskFS) LsInfo(ctx context.Context, req *plantask.LsInfoRequest) ([]plantask.FileInfo, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []plantask.FileInfo
	for path, content := range t.files {
		out = append(out, plantask.FileInfo{Path: path, IsDir: false, Size: int64(len(content))})
	}
	return out, nil
}

func (t *taskFS) Read(ctx context.Context, req *plantask.ReadRequest) (*filesystem.FileContent, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	content, ok := t.files[req.FilePath]
	if !ok {
		return nil, fmt.Errorf("file not found: %s", req.FilePath)
	}
	return &filesystem.FileContent{Content: content}, nil
}

func (t *taskFS) Write(ctx context.Context, req *plantask.WriteRequest) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.files[req.FilePath] = req.Content
	return nil
}

func (t *taskFS) Delete(ctx context.Context, req *plantask.DeleteRequest) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.files, req.FilePath)
	return nil
}
